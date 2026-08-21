package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

const (
	UnifiedAccountMigrationStateApplied    = "applied"
	UnifiedAccountMigrationStateRolledBack = "rolled_back"
)

var (
	ErrUnifiedAccountMigrationInvalidManifest = errors.New("unified account migration manifest is invalid")
	ErrUnifiedAccountMigrationSnapshotDrift   = errors.New("unified account migration snapshot drift")
	ErrUnifiedAccountMigrationConflict        = errors.New("unified account migration identity conflict")
	ErrUnifiedAccountMigrationRollbackUnsafe  = errors.New("unified account migration rollback is unsafe")
)

var unifiedAccountMigrationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

// UnifiedAccountMigrationAccount is one verified, active Authentik identity
// and the WildCloud balance snapshot authorized for copying. It is accepted
// only by the offline migration command and is never exposed through HTTP.
type UnifiedAccountMigrationAccount struct {
	Subject            string `json:"subject"`
	PreferredUsername  string `json:"preferred_username"`
	DisplayName        string `json:"display_name"`
	Email              string `json:"email"`
	SourceBalanceCents int64  `json:"source_balance_cents"`
}

type UnifiedAccountMigrationManifest struct {
	MigrationID                string                           `json:"migration_id"`
	QuotaPerUnit               int64                            `json:"quota_per_unit"`
	USDToCNYCents              int64                            `json:"usd_to_cny_cents"`
	ExpectedAccountCount       int                              `json:"expected_account_count"`
	ExpectedSourceBalanceCents int64                            `json:"expected_source_balance_cents"`
	Accounts                   []UnifiedAccountMigrationAccount `json:"accounts"`
}

// UnifiedAccountMigrationRecord is the durable, per-identity idempotency and
// rollback ledger. Subjects are represented by a one-way hash so the ledger
// does not duplicate external identity data already stored on the user.
type UnifiedAccountMigrationRecord struct {
	Id                 int64      `json:"id"`
	MigrationID        string     `json:"migration_id" gorm:"type:varchar(80);not null;uniqueIndex:idx_unified_account_migration,priority:1"`
	SubjectHash        string     `json:"subject_hash" gorm:"type:char(64);not null;uniqueIndex:idx_unified_account_migration,priority:2"`
	UserId             int        `json:"user_id" gorm:"not null;index"`
	SourceBalanceCents int64      `json:"source_balance_cents" gorm:"not null"`
	QuotaDelta         int64      `json:"quota_delta" gorm:"not null"`
	BaselineQuota      int64      `json:"baseline_quota" gorm:"not null"`
	CreatedUser        bool       `json:"created_user" gorm:"not null"`
	State              string     `json:"state" gorm:"type:varchar(16);not null;index"`
	CreatedAt          time.Time  `json:"created_at"`
	RolledBackAt       *time.Time `json:"rolled_back_at"`
}

type UnifiedAccountMigrationPlan struct {
	AccountCount        int   `json:"account_count"`
	CreateCount         int   `json:"create_count"`
	ExistingCount       int   `json:"existing_count"`
	AlreadyAppliedCount int   `json:"already_applied_count"`
	SourceBalanceCents  int64 `json:"source_balance_cents"`
	QuotaDelta          int64 `json:"quota_delta"`
}

type UnifiedAccountMigrationResult struct {
	AccountCount       int   `json:"account_count"`
	CreatedCount       int   `json:"created_count"`
	ExistingCount      int   `json:"existing_count"`
	IdempotentCount    int   `json:"idempotent_count"`
	SourceBalanceCents int64 `json:"source_balance_cents"`
	QuotaDelta         int64 `json:"quota_delta"`
}

type UnifiedAccountMigrationRollbackResult struct {
	RecordCount     int `json:"record_count"`
	RolledBackCount int `json:"rolled_back_count"`
	IdempotentCount int `json:"idempotent_count"`
}

type validatedUnifiedAccountMigration struct {
	manifest UnifiedAccountMigrationManifest
	accounts []validatedUnifiedAccountMigrationAccount
	total    int64
	quota    int64
}

type validatedUnifiedAccountMigrationAccount struct {
	account     UnifiedAccountMigrationAccount
	subjectHash string
	quotaDelta  int64
}

func (UnifiedAccountMigrationRecord) TableName() string {
	return "unified_account_migration_records"
}

func unifiedAccountSubjectHash(subject string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(subject)))
}

func quotaDeltaFromCNYCents(cents, quotaPerUnit, usdToCNYCents int64) (int64, error) {
	if cents < 0 || quotaPerUnit <= 0 || usdToCNYCents <= 0 {
		return 0, ErrUnifiedAccountMigrationInvalidManifest
	}
	if cents != 0 && quotaPerUnit > math.MaxInt64/cents {
		return 0, ErrUnifiedAccountMigrationInvalidManifest
	}
	numerator := cents * quotaPerUnit
	if numerator > math.MaxInt64-usdToCNYCents/2 {
		return 0, ErrUnifiedAccountMigrationInvalidManifest
	}
	return (numerator + usdToCNYCents/2) / usdToCNYCents, nil
}

func validateUnifiedAccountMigrationManifest(manifest UnifiedAccountMigrationManifest) (*validatedUnifiedAccountMigration, error) {
	manifest.MigrationID = strings.TrimSpace(manifest.MigrationID)
	if !unifiedAccountMigrationIDPattern.MatchString(manifest.MigrationID) ||
		manifest.QuotaPerUnit <= 0 || manifest.USDToCNYCents <= 0 ||
		manifest.ExpectedAccountCount <= 0 || len(manifest.Accounts) != manifest.ExpectedAccountCount {
		return nil, ErrUnifiedAccountMigrationInvalidManifest
	}

	validated := &validatedUnifiedAccountMigration{manifest: manifest}
	seenSubjects := make(map[string]struct{}, len(manifest.Accounts))
	seenEmails := make(map[string]struct{}, len(manifest.Accounts))
	for _, raw := range manifest.Accounts {
		raw.Subject = strings.TrimSpace(raw.Subject)
		raw.PreferredUsername = strings.TrimSpace(raw.PreferredUsername)
		raw.DisplayName = strings.TrimSpace(raw.DisplayName)
		raw.Email = NormalizeEmail(raw.Email)
		parsedEmail, err := mail.ParseAddress(raw.Email)
		if raw.Subject == "" || len(raw.Subject) > 128 || raw.Email == "" || len(raw.Email) > 50 ||
			strings.ContainsAny(raw.Email, "\r\n") || err != nil || parsedEmail.Address != raw.Email ||
			raw.SourceBalanceCents < 0 {
			return nil, ErrUnifiedAccountMigrationInvalidManifest
		}
		hash := unifiedAccountSubjectHash(raw.Subject)
		if _, exists := seenSubjects[hash]; exists {
			return nil, ErrUnifiedAccountMigrationInvalidManifest
		}
		if _, exists := seenEmails[raw.Email]; exists {
			return nil, ErrUnifiedAccountMigrationInvalidManifest
		}
		seenSubjects[hash] = struct{}{}
		seenEmails[raw.Email] = struct{}{}

		delta, err := quotaDeltaFromCNYCents(raw.SourceBalanceCents, manifest.QuotaPerUnit, manifest.USDToCNYCents)
		if err != nil || delta > int64(^uint(0)>>1) {
			return nil, ErrUnifiedAccountMigrationInvalidManifest
		}
		if validated.total > math.MaxInt64-raw.SourceBalanceCents || validated.quota > math.MaxInt64-delta {
			return nil, ErrUnifiedAccountMigrationInvalidManifest
		}
		validated.total += raw.SourceBalanceCents
		validated.quota += delta
		validated.accounts = append(validated.accounts, validatedUnifiedAccountMigrationAccount{
			account: raw, subjectHash: hash, quotaDelta: delta,
		})
	}
	if validated.total != manifest.ExpectedSourceBalanceCents {
		return nil, ErrUnifiedAccountMigrationSnapshotDrift
	}
	sort.Slice(validated.accounts, func(i, j int) bool {
		return validated.accounts[i].subjectHash < validated.accounts[j].subjectHash
	})
	return validated, nil
}

func findUnifiedAccountMigrationUser(tx *gorm.DB, subject string) (*User, error) {
	var claim ExternalIdentityClaim
	err := tx.Where("provider = ? AND subject = ?", ExternalIdentityProviderOIDC, subject).First(&claim).Error
	if err == nil {
		var user User
		if err := tx.Unscoped().First(&user, claim.UserId).Error; err != nil {
			return nil, ErrUnifiedAccountMigrationConflict
		}
		if user.DeletedAt.Valid || user.Status != common.UserStatusEnabled || user.OidcId != subject {
			return nil, ErrUnifiedAccountMigrationConflict
		}
		return &user, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var directCount int64
	if err := tx.Unscoped().Model(&User{}).Where("oidc_id = ?", subject).Count(&directCount).Error; err != nil {
		return nil, err
	}
	if directCount != 0 {
		return nil, ErrUnifiedAccountMigrationConflict
	}
	return nil, nil
}

func PlanUnifiedAccountMigration(manifest UnifiedAccountMigrationManifest) (*UnifiedAccountMigrationPlan, error) {
	validated, err := validateUnifiedAccountMigrationManifest(manifest)
	if err != nil {
		return nil, err
	}
	plan := &UnifiedAccountMigrationPlan{
		AccountCount: len(validated.accounts), SourceBalanceCents: validated.total, QuotaDelta: validated.quota,
	}
	hasLedger := DB.Migrator().HasTable(&UnifiedAccountMigrationRecord{})
	for _, item := range validated.accounts {
		if hasLedger {
			var record UnifiedAccountMigrationRecord
			err := DB.Where("migration_id = ? AND subject_hash = ?", validated.manifest.MigrationID, item.subjectHash).First(&record).Error
			if err == nil {
				if record.SourceBalanceCents != item.account.SourceBalanceCents || record.QuotaDelta != item.quotaDelta ||
					record.State != UnifiedAccountMigrationStateApplied {
					return nil, ErrUnifiedAccountMigrationSnapshotDrift
				}
				if err := validateUnifiedAccountMigrationReplay(DB, &record, item); err != nil {
					return nil, fmt.Errorf("%w: identity %s", err, item.subjectHash[:12])
				}
				plan.AlreadyAppliedCount++
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
		}
		user, err := findUnifiedAccountMigrationUser(DB, item.account.Subject)
		if err != nil {
			return nil, fmt.Errorf("%w: identity %s", err, item.subjectHash[:12])
		}
		if user != nil {
			plan.ExistingCount++
			continue
		}
		var emailCount int64
		if err := emailQuery(DB.Unscoped().Model(&User{}), item.account.Email).Count(&emailCount).Error; err != nil {
			return nil, err
		}
		if emailCount != 0 {
			return nil, fmt.Errorf("%w: identity %s", ErrUnifiedAccountMigrationConflict, item.subjectHash[:12])
		}
		plan.CreateCount++
	}
	return plan, nil
}

func sanitizeUnifiedAccountMigrationUsername(value, subjectHash string) string {
	runes := make([]rune, 0, UserNameMaxLength)
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			runes = append(runes, r)
			if len(runes) == UserNameMaxLength {
				break
			}
		}
	}
	if len(runes) == 0 {
		return "oidc-" + subjectHash[:12]
	}
	return string(runes)
}

func truncateUnifiedAccountMigrationDisplayName(value, fallback string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return fallback
	}
	if len(runes) > 20 {
		runes = runes[:20]
	}
	return string(runes)
}

func uniqueUnifiedAccountMigrationUsername(tx *gorm.DB, preferred, subjectHash string) (string, error) {
	base := sanitizeUnifiedAccountMigrationUsername(preferred, subjectHash)
	for attempt := 0; attempt < 100; attempt++ {
		candidate := base
		if attempt > 0 {
			suffix := fmt.Sprintf("-%s", subjectHash[:6])
			if attempt > 1 {
				suffix = fmt.Sprintf("-%s-%d", subjectHash[:6], attempt)
			}
			baseRunes := []rune(base)
			maxBase := UserNameMaxLength - len([]rune(suffix))
			if len(baseRunes) > maxBase {
				baseRunes = baseRunes[:maxBase]
			}
			candidate = string(baseRunes) + suffix
		}
		var count int64
		if err := tx.Unscoped().Model(&User{}).Where("username = ?", candidate).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
	return "", ErrUnifiedAccountMigrationConflict
}

func validateUnifiedAccountMigrationReplay(tx *gorm.DB, record *UnifiedAccountMigrationRecord, item validatedUnifiedAccountMigrationAccount) error {
	if record.SourceBalanceCents != item.account.SourceBalanceCents || record.QuotaDelta != item.quotaDelta ||
		record.State != UnifiedAccountMigrationStateApplied {
		return ErrUnifiedAccountMigrationSnapshotDrift
	}
	var user User
	if err := tx.Unscoped().First(&user, record.UserId).Error; err != nil || user.DeletedAt.Valid || user.OidcId != item.account.Subject {
		return ErrUnifiedAccountMigrationConflict
	}
	return nil
}

func ApplyUnifiedAccountMigration(manifest UnifiedAccountMigrationManifest) (*UnifiedAccountMigrationResult, error) {
	validated, err := validateUnifiedAccountMigrationManifest(manifest)
	if err != nil {
		return nil, err
	}
	if _, err := PlanUnifiedAccountMigration(manifest); err != nil {
		return nil, err
	}
	if err := DB.AutoMigrate(&UnifiedAccountMigrationRecord{}); err != nil {
		return nil, err
	}
	result := &UnifiedAccountMigrationResult{
		AccountCount: len(validated.accounts), SourceBalanceCents: validated.total, QuotaDelta: validated.quota,
	}
	for _, item := range validated.accounts {
		created := false
		idempotent := false
		userID := 0
		err := DB.Transaction(func(tx *gorm.DB) error {
			var record UnifiedAccountMigrationRecord
			recordResult := lockForUpdate(tx).Where(
				"migration_id = ? AND subject_hash = ?", validated.manifest.MigrationID, item.subjectHash,
			).First(&record)
			if recordResult.Error == nil {
				if err := validateUnifiedAccountMigrationReplay(tx, &record, item); err != nil {
					return err
				}
				userID = record.UserId
				idempotent = true
				return nil
			}
			if !errors.Is(recordResult.Error, gorm.ErrRecordNotFound) {
				return recordResult.Error
			}

			user, err := findUnifiedAccountMigrationUser(tx, item.account.Subject)
			if err != nil {
				return err
			}
			if user == nil {
				if err := ensureEmailAvailableWithTx(tx, item.account.Email, 0); err != nil {
					return ErrUnifiedAccountMigrationConflict
				}
				username, err := uniqueUnifiedAccountMigrationUsername(tx, item.account.PreferredUsername, item.subjectHash)
				if err != nil {
					return err
				}
				user = &User{
					Username:    username,
					DisplayName: truncateUnifiedAccountMigrationDisplayName(item.account.DisplayName, username),
					Email:       item.account.Email,
					Role:        common.RoleCommonUser,
					Status:      common.UserStatusEnabled,
					OidcId:      item.account.Subject,
				}
				user.SetSetting(dto.UserSetting{
					SidebarModules: generateDefaultSidebarConfigForRole(user.Role),
				})
				if err := user.InsertWithTx(tx, 0); err != nil {
					return err
				}
				if err := ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderOIDC, item.account.Subject, user.Id); err != nil {
					return err
				}
				if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("oidc_id", item.account.Subject).Error; err != nil {
					return err
				}
				created = true
			}
			userID = user.Id
			baseline := int64(user.Quota)
			if item.quotaDelta > 0 {
				update := tx.Model(&User{}).Where("id = ?", user.Id).
					Update("quota", gorm.Expr("quota + ?", item.quotaDelta))
				if update.Error != nil {
					return update.Error
				}
				if update.RowsAffected != 1 {
					return ErrUnifiedAccountMigrationConflict
				}
			}
			record = UnifiedAccountMigrationRecord{
				MigrationID: validated.manifest.MigrationID, SubjectHash: item.subjectHash,
				UserId: user.Id, SourceBalanceCents: item.account.SourceBalanceCents,
				QuotaDelta: item.quotaDelta, BaselineQuota: baseline, CreatedUser: created,
				State: UnifiedAccountMigrationStateApplied,
			}
			return tx.Create(&record).Error
		})
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", item.subjectHash[:12], err)
		}
		// A previous attempt can commit successfully and then lose the Redis
		// invalidation response. Repeat invalidation on idempotent replays so the
		// database-authoritative quota is eventually visible to the API process.
		if err := invalidateUserCache(userID); err != nil {
			return nil, err
		}
		if idempotent {
			result.IdempotentCount++
			continue
		}
		if created {
			result.CreatedCount++
		} else {
			result.ExistingCount++
		}
	}
	return result, nil
}

func RollbackUnifiedAccountMigration(migrationID string) (*UnifiedAccountMigrationRollbackResult, error) {
	migrationID = strings.TrimSpace(migrationID)
	if !unifiedAccountMigrationIDPattern.MatchString(migrationID) || !DB.Migrator().HasTable(&UnifiedAccountMigrationRecord{}) {
		return nil, ErrUnifiedAccountMigrationInvalidManifest
	}
	var records []UnifiedAccountMigrationRecord
	if err := DB.Where("migration_id = ?", migrationID).Order("id asc").Find(&records).Error; err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, ErrUnifiedAccountMigrationInvalidManifest
	}
	result := &UnifiedAccountMigrationRollbackResult{RecordCount: len(records)}
	for _, listed := range records {
		created := false
		idempotent := false
		userID := listed.UserId
		err := DB.Transaction(func(tx *gorm.DB) error {
			var record UnifiedAccountMigrationRecord
			if err := lockForUpdate(tx).First(&record, listed.Id).Error; err != nil {
				return err
			}
			if record.State == UnifiedAccountMigrationStateRolledBack {
				idempotent = true
				return nil
			}
			if record.State != UnifiedAccountMigrationStateApplied {
				return ErrUnifiedAccountMigrationRollbackUnsafe
			}
			var user User
			if err := lockForUpdate(tx).Unscoped().First(&user, record.UserId).Error; err != nil {
				return ErrUnifiedAccountMigrationRollbackUnsafe
			}
			if int64(user.Quota) < record.QuotaDelta {
				return ErrUnifiedAccountMigrationRollbackUnsafe
			}
			updates := map[string]any{"quota": gorm.Expr("quota - ?", record.QuotaDelta)}
			created = record.CreatedUser
			if created {
				updates["status"] = common.UserStatusDisabled
				nextVersion, err := IncrementUserAuthVersionWithTx(tx, user.Id)
				if err != nil {
					return err
				}
				updates["auth_version"] = nextVersion
			}
			if err := tx.Model(&User{}).Where("id = ?", user.Id).Updates(updates).Error; err != nil {
				return err
			}
			now := time.Now()
			return tx.Model(&record).Updates(map[string]any{
				"state": UnifiedAccountMigrationStateRolledBack, "rolled_back_at": &now,
			}).Error
		})
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", listed.Id, err)
		}
		if idempotent {
			result.IdempotentCount++
			continue
		}
		result.RolledBackCount++
		if err := invalidateUserCache(userID); err != nil {
			return nil, err
		}
		if created {
			if _, err := RevokeAllUserSessions(userID, "unified_account_migration_rollback"); err != nil {
				return nil, err
			}
			if err := InvalidateUserTokensCache(userID); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
