package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func migrationManifest(accounts ...UnifiedAccountMigrationAccount) UnifiedAccountMigrationManifest {
	var total int64
	for _, account := range accounts {
		total += account.SourceBalanceCents
	}
	return UnifiedAccountMigrationManifest{
		MigrationID:                "wildcloud-balance-copy-2026-08-21-v1",
		QuotaPerUnit:               500_000,
		USDToCNYCents:              730,
		ExpectedAccountCount:       len(accounts),
		ExpectedSourceBalanceCents: total,
		Accounts:                   accounts,
	}
}

func TestPlanUnifiedAccountMigrationPreservesLargeCNYBalance(t *testing.T) {
	truncateTables(t)
	manifest := migrationManifest(
		UnifiedAccountMigrationAccount{
			Subject:            "authentik-large-balance",
			PreferredUsername:  "large-owner",
			DisplayName:        "Large Owner",
			Email:              "large@example.com",
			SourceBalanceCents: 1_120_006_920,
		},
		UnifiedAccountMigrationAccount{
			Subject:            "authentik-normal-balance",
			PreferredUsername:  "normal-owner",
			DisplayName:        "Normal Owner",
			Email:              "normal@example.com",
			SourceBalanceCents: 92_535,
		},
	)

	plan, err := PlanUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	assert.Equal(t, 2, plan.AccountCount)
	assert.Equal(t, int64(1_120_099_455), plan.SourceBalanceCents)
	assert.Equal(t, int64(767_191_407_534), plan.QuotaDelta)
	assert.Equal(t, 2, plan.CreateCount)
	assert.Zero(t, plan.ExistingCount)
}

func TestApplyUnifiedAccountMigrationCreatesOIDCOnlyUserAndLedger(t *testing.T) {
	truncateTables(t)
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            "authentik-new-user",
		PreferredUsername:  "new-user",
		DisplayName:        "New User",
		Email:              "NEW@Example.com",
		SourceBalanceCents: 730,
	})

	result, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	assert.Equal(t, 1, result.CreatedCount)
	assert.Zero(t, result.ExistingCount)
	assert.Zero(t, result.IdempotentCount)
	assert.Equal(t, int64(500_000), result.QuotaDelta)

	var user User
	require.NoError(t, DB.Where("oidc_id = ?", "authentik-new-user").First(&user).Error)
	assert.Equal(t, "new-user", user.Username)
	assert.Equal(t, "new@example.com", user.Email)
	assert.Empty(t, user.Password)
	assert.Equal(t, common.UserStatusEnabled, user.Status)
	assert.Equal(t, 500_000, user.Quota)

	var claim ExternalIdentityClaim
	require.NoError(t, DB.Where("provider = ? AND subject = ?", ExternalIdentityProviderOIDC, "authentik-new-user").First(&claim).Error)
	assert.Equal(t, user.Id, claim.UserId)

	var record UnifiedAccountMigrationRecord
	require.NoError(t, DB.Where("migration_id = ?", manifest.MigrationID).First(&record).Error)
	assert.Equal(t, user.Id, record.UserId)
	assert.Equal(t, int64(730), record.SourceBalanceCents)
	assert.Equal(t, int64(500_000), record.QuotaDelta)
	assert.True(t, record.CreatedUser)
	assert.Equal(t, UnifiedAccountMigrationStateApplied, record.State)
}

func TestApplyUnifiedAccountMigrationStoresConfirmedLargeBalance(t *testing.T) {
	truncateTables(t)
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            "authentik-confirmed-large-balance",
		PreferredUsername:  "large-balance",
		DisplayName:        "Large Balance",
		Email:              "large-balance@example.com",
		SourceBalanceCents: 1_120_006_920,
	})

	result, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	assert.Equal(t, int64(767_128_027_397), result.QuotaDelta)

	var quota int64
	require.NoError(t, DB.Model(&User{}).Where("oidc_id = ?", "authentik-confirmed-large-balance").Select("quota").Scan(&quota).Error)
	assert.Equal(t, result.QuotaDelta, quota)
}

func TestApplyUnifiedAccountMigrationCreditsExistingBindingExactlyOnce(t *testing.T) {
	truncateTables(t)
	user := User{
		Username: "existing-user",
		Email:    "existing@example.com",
		OidcId:   "authentik-existing",
		Status:   common.UserStatusEnabled,
		Role:     common.RoleCommonUser,
		Quota:    123,
		AffCode:  "existing-user",
	}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		return ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderOIDC, user.OidcId, user.Id)
	}))
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            user.OidcId,
		PreferredUsername:  user.Username,
		DisplayName:        "Existing User",
		Email:              user.Email,
		SourceBalanceCents: 730,
	})

	first, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	assert.Equal(t, 1, first.ExistingCount)
	second, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	assert.Equal(t, 1, second.IdempotentCount)

	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 500_123, user.Quota)
	var records int64
	require.NoError(t, DB.Model(&UnifiedAccountMigrationRecord{}).Count(&records).Error)
	assert.Equal(t, int64(1), records)
}

func TestApplyUnifiedAccountMigrationFailsClosedOnEmailCollision(t *testing.T) {
	truncateTables(t)
	existing := User{
		Username: "email-owner",
		Email:    "claimed@example.com",
		Status:   common.UserStatusEnabled,
		AffCode:  "email-owner",
	}
	require.NoError(t, DB.Create(&existing).Error)
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            "different-authentik-user",
		PreferredUsername:  "different-user",
		DisplayName:        "Different User",
		Email:              "CLAIMED@example.com",
		SourceBalanceCents: 730,
	})

	_, err := ApplyUnifiedAccountMigration(manifest)
	assert.ErrorIs(t, err, ErrUnifiedAccountMigrationConflict)
	require.NoError(t, DB.First(&existing, existing.Id).Error)
	assert.Zero(t, existing.Quota)
	var records int64
	require.NoError(t, DB.Model(&UnifiedAccountMigrationRecord{}).Count(&records).Error)
	assert.Zero(t, records)
}

func TestPlanUnifiedAccountMigrationRejectsSnapshotDriftAndDuplicateSubject(t *testing.T) {
	truncateTables(t)
	account := UnifiedAccountMigrationAccount{
		Subject:            "authentik-duplicate",
		PreferredUsername:  "duplicate-user",
		DisplayName:        "Duplicate User",
		Email:              "duplicate@example.com",
		SourceBalanceCents: 100,
	}
	manifest := migrationManifest(account, account)

	_, err := PlanUnifiedAccountMigration(manifest)
	assert.ErrorIs(t, err, ErrUnifiedAccountMigrationInvalidManifest)

	manifest = migrationManifest(account)
	manifest.ExpectedSourceBalanceCents++
	_, err = PlanUnifiedAccountMigration(manifest)
	assert.ErrorIs(t, err, ErrUnifiedAccountMigrationSnapshotDrift)
}

func TestRollbackUnifiedAccountMigrationSubtractsCreditAndDisablesCreatedUser(t *testing.T) {
	truncateTables(t)
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            "authentik-rollback",
		PreferredUsername:  "rollback-user",
		DisplayName:        "Rollback User",
		Email:              "rollback@example.com",
		SourceBalanceCents: 730,
	})
	require.NoError(t, DB.AutoMigrate(&UnifiedAccountMigrationRecord{}))
	_, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)

	first, err := RollbackUnifiedAccountMigration(manifest.MigrationID)
	require.NoError(t, err)
	assert.Equal(t, 1, first.RolledBackCount)
	second, err := RollbackUnifiedAccountMigration(manifest.MigrationID)
	require.NoError(t, err)
	assert.Equal(t, 1, second.IdempotentCount)

	var user User
	require.NoError(t, DB.Where("oidc_id = ?", "authentik-rollback").First(&user).Error)
	assert.Zero(t, user.Quota)
	assert.Equal(t, common.UserStatusDisabled, user.Status)
	var record UnifiedAccountMigrationRecord
	require.NoError(t, DB.Where("migration_id = ?", manifest.MigrationID).First(&record).Error)
	assert.Equal(t, UnifiedAccountMigrationStateRolledBack, record.State)
}

func TestRollbackUnifiedAccountMigrationRefusesSpentCredit(t *testing.T) {
	truncateTables(t)
	manifest := migrationManifest(UnifiedAccountMigrationAccount{
		Subject:            "authentik-spent",
		PreferredUsername:  "spent-user",
		DisplayName:        "Spent User",
		Email:              "spent@example.com",
		SourceBalanceCents: 730,
	})
	_, err := ApplyUnifiedAccountMigration(manifest)
	require.NoError(t, err)
	var user User
	require.NoError(t, DB.Where("oidc_id = ?", "authentik-spent").First(&user).Error)
	require.NoError(t, DB.Model(&user).Update("quota", 499_999).Error)

	_, err = RollbackUnifiedAccountMigration(manifest.MigrationID)
	assert.ErrorIs(t, err, ErrUnifiedAccountMigrationRollbackUnsafe)
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 499_999, user.Quota)
}
