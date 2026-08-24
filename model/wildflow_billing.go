package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	WildFlowBillingStatePending   = "pending"
	WildFlowBillingStateReserved  = "reserved"
	WildFlowBillingStateRefunding = "refunding"
	WildFlowBillingStateSettled   = "settled"
	WildFlowBillingStateRefunded  = "refunded"

	WildFlowBillingSourceWallet       = "wallet"
	WildFlowBillingSourceSubscription = "subscription"
	WildFlowBillingSourceTeamTrial    = "team_trial"
)

var (
	ErrWildFlowBillingStateConflict   = errors.New("WildFlow billing state conflict")
	ErrWildFlowInsufficientUserQuota  = errors.New("insufficient user quota")
	ErrWildFlowInsufficientTokenQuota = errors.New("insufficient token quota")
)

type WildFlowBillingQuote struct {
	Currency        string
	AmountMicros    int64
	Unit            string
	BillableUnits   int64
	Quota           int
	QuotaPerUnit    string
	USDExchangeRate string
	PriceVersion    string
}

func (quote WildFlowBillingQuote) Validate() error {
	if quote.Currency != "CNY" || quote.AmountMicros <= 0 || quote.Unit == "" || quote.BillableUnits <= 0 || quote.Quota <= 0 {
		return fmt.Errorf("invalid WildFlow billing quote")
	}
	if quote.QuotaPerUnit == "" || quote.USDExchangeRate == "" || quote.PriceVersion == "" {
		return fmt.Errorf("incomplete WildFlow billing quote")
	}
	return nil
}

func billingQuoteMatchesOperation(operation *WildFlowOperation, quote WildFlowBillingQuote) bool {
	return operation.BillingCurrency == quote.Currency &&
		operation.BillingAmountMicros == quote.AmountMicros &&
		operation.BillingUnit == quote.Unit &&
		operation.BillingBillableUnits == quote.BillableUnits &&
		operation.BillingQuota == quote.Quota &&
		operation.BillingQuotaPerUnit == quote.QuotaPerUnit &&
		operation.BillingUSDExchangeRate == quote.USDExchangeRate &&
		operation.BillingPriceVersion == quote.PriceVersion
}

func loadWildFlowOperationForBilling(tx *gorm.DB, operationID string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	if err := lockForUpdate(tx).Where("operation_id = ?", operationID).First(&operation).Error; err != nil {
		return nil, err
	}
	return &operation, nil
}

func wildFlowBillingSnapshotUpdates(quote WildFlowBillingQuote, source string, subscriptionID int, tokenQuota int) map[string]any {
	return map[string]any{
		"billing_state":             WildFlowBillingStateReserved,
		"billing_source":            source,
		"billing_subscription_id":   subscriptionID,
		"billing_quota":             quote.Quota,
		"billing_token_quota":       tokenQuota,
		"billing_currency":          quote.Currency,
		"billing_amount_micros":     quote.AmountMicros,
		"billing_unit":              quote.Unit,
		"billing_billable_units":    quote.BillableUnits,
		"billing_quota_per_unit":    quote.QuotaPerUnit,
		"billing_usd_exchange_rate": quote.USDExchangeRate,
		"billing_price_version":     quote.PriceVersion,
		"updated_time":              time.Now().Unix(),
	}
}

func ensureWildFlowBillingCanReserve(operation *WildFlowOperation, quote WildFlowBillingQuote) (bool, error) {
	switch operation.BillingState {
	case WildFlowBillingStateReserved, WildFlowBillingStateSettled:
		if !billingQuoteMatchesOperation(operation, quote) {
			return false, ErrWildFlowBillingStateConflict
		}
		return false, nil
	case "", WildFlowBillingStatePending:
		return true, nil
	default:
		return false, ErrWildFlowBillingStateConflict
	}
}

func ReserveWildFlowWalletBilling(operationID string, quote WildFlowBillingQuote) (*WildFlowOperation, error) {
	if err := quote.Validate(); err != nil {
		return nil, err
	}
	var result *WildFlowOperation
	var tokenKey string
	var tokenQuota int
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		shouldReserve, err := ensureWildFlowBillingCanReserve(operation, quote)
		if err != nil || !shouldReserve {
			result = operation
			return err
		}

		var user User
		if err := lockForUpdate(tx).Where("id = ?", operation.UserID).First(&user).Error; err != nil {
			return err
		}
		if user.Quota < quote.Quota {
			return ErrWildFlowInsufficientUserQuota
		}

		if operation.TokenID > 0 {
			var token Token
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", operation.TokenID, operation.UserID).First(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
			if !token.UnlimitedQuota {
				if token.RemainQuota < quote.Quota {
					return ErrWildFlowInsufficientTokenQuota
				}
				tokenQuota = quote.Quota
				if err := tx.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota - ?", tokenQuota),
					"used_quota":    gorm.Expr("used_quota + ?", tokenQuota),
					"accessed_time": common.GetTimestamp(),
				}).Error; err != nil {
					return err
				}
			}
		}

		if err := tx.Model(&User{}).Where("id = ?", operation.UserID).
			Update("quota", gorm.Expr("quota - ?", quote.Quota)).Error; err != nil {
			return err
		}
		if err := tx.Model(&WildFlowOperation{}).Where("id = ?", operation.ID).
			Updates(wildFlowBillingSnapshotUpdates(quote, WildFlowBillingSourceWallet, 0, tokenQuota)).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
			return err
		}
		result = operation
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if changed {
		syncWildFlowBillingQuotaCache(result.UserID, tokenKey, -quote.Quota, -tokenQuota)
	}
	return result, nil
}

// ReserveWildFlowSubscriptionBilling atomically reserves subscription and token
// quota and persists the operation billing snapshot.
func ReserveWildFlowSubscriptionBilling(operationID string, quote WildFlowBillingQuote) (*WildFlowOperation, error) {
	if err := quote.Validate(); err != nil {
		return nil, err
	}
	var result *WildFlowOperation
	var tokenKey string
	var tokenQuota int
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		shouldReserve, err := ensureWildFlowBillingCanReserve(operation, quote)
		if err != nil || !shouldReserve {
			result = operation
			return err
		}
		preConsume, err := preConsumeUserSubscriptionTx(
			tx,
			operation.OperationID,
			operation.UserID,
			int64(quote.Quota),
		)
		if err != nil {
			return err
		}
		if operation.TokenID > 0 {
			var token Token
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", operation.TokenID, operation.UserID).First(&token).Error; err != nil {
				return err
			}
			tokenKey = token.Key
			if !token.UnlimitedQuota {
				if token.RemainQuota < quote.Quota {
					return ErrWildFlowInsufficientTokenQuota
				}
				tokenQuota = quote.Quota
				if err := tx.Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota - ?", tokenQuota),
					"used_quota":    gorm.Expr("used_quota + ?", tokenQuota),
					"accessed_time": common.GetTimestamp(),
				}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&WildFlowOperation{}).Where("id = ?", operation.ID).
			Updates(wildFlowBillingSnapshotUpdates(
				quote,
				WildFlowBillingSourceSubscription,
				preConsume.UserSubscriptionId,
				tokenQuota,
			)).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
			return err
		}
		result = operation
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if changed {
		syncWildFlowBillingQuotaCache(0, tokenKey, 0, -tokenQuota)
	}
	return result, nil
}

func SettleWildFlowOperationBilling(operationID string) (*WildFlowOperation, bool, error) {
	var result *WildFlowOperation
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		if operation.BillingState == WildFlowBillingStateSettled {
			if _, err := ensureWildFlowCanonicalBillingLogTx(tx, operation, LogTypeConsume, "WildFlow job settled"); err != nil {
				return err
			}
			result = operation
			return nil
		}
		if operation.BillingState != WildFlowBillingStateReserved || operation.State != "succeeded" ||
			operation.ResultJSON == "" || operation.ResultValidatedTime == 0 {
			result = operation
			return nil
		}
		var usageEvent WildFlowUsageEvent
		eventErr := tx.Where(
			"operation_id = ? AND job_id = ? AND model_version_ref = ?",
			operation.OperationID,
			operation.JobID,
			operation.ModelVersionRef,
		).Order("created_time asc, event_id asc").First(&usageEvent).Error
		if errors.Is(eventErr, gorm.ErrRecordNotFound) {
			result = operation
			return nil
		}
		if eventErr != nil {
			return eventErr
		}
		if !wildFlowUsageEventMatchesOperation(operation, &usageEvent) {
			return ErrWildFlowBillingStateConflict
		}
		now := time.Now().Unix()
		update := tx.Model(&WildFlowOperation{}).
			Where("id = ? AND billing_state = ? AND state = ? AND result_json <> ? AND result_validated_time > 0",
				operation.ID, WildFlowBillingStateReserved, "succeeded", "").
			Updates(map[string]any{
				"billing_state":          WildFlowBillingStateSettled,
				"billing_usage_event_id": usageEvent.EventID,
				"billing_settled_time":   now,
				"updated_time":           now,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
				return err
			}
			result = operation
			return nil
		}
		if err := tx.Model(&User{}).Where("id = ?", operation.UserID).Updates(map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", operation.BillingQuota),
			"request_count": gorm.Expr("request_count + 1"),
		}).Error; err != nil {
			return err
		}
		operation.BillingState = WildFlowBillingStateSettled
		operation.BillingUsageEventID = usageEvent.EventID
		operation.BillingSettledTime = now
		operation.UpdatedTime = now
		if _, err := ensureWildFlowCanonicalBillingLogTx(tx, operation, LogTypeConsume, "WildFlow job settled"); err != nil {
			return err
		}
		result = operation
		changed = true
		return nil
	})
	return result, changed, err
}

func RefundWildFlowOperationBilling(operationID string) (*WildFlowOperation, bool, error) {
	var result *WildFlowOperation
	var tokenKey string
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		locked, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		if locked.BillingState == WildFlowBillingStateRefunded {
			if _, err := ensureWildFlowCanonicalBillingLogTx(tx, locked, LogTypeRefund, "WildFlow job refunded"); err != nil {
				return err
			}
			result = locked
			return nil
		}
		if (locked.BillingState != WildFlowBillingStateReserved && locked.BillingState != WildFlowBillingStateRefunding) ||
			(locked.State != "failed" && locked.State != "cancelled") {
			result = locked
			return nil
		}
		if locked.BillingSource == WildFlowBillingSourceSubscription {
			if err := refundSubscriptionPreConsumeTx(tx, locked.OperationID); err != nil {
				return err
			}
		}
		if locked.BillingSource == WildFlowBillingSourceWallet {
			if err := tx.Model(&User{}).Where("id = ?", locked.UserID).
				Update("quota", gorm.Expr("quota + ?", locked.BillingQuota)).Error; err != nil {
				return err
			}
		}
		if locked.BillingTokenQuota > 0 && locked.TokenID > 0 {
			var token Token
			tokenErr := tx.Unscoped().Where("id = ?", locked.TokenID).First(&token).Error
			switch {
			case tokenErr == nil:
				tokenKey = token.Key
				if err := tx.Unscoped().Model(&Token{}).Where("id = ?", locked.TokenID).Updates(map[string]any{
					"remain_quota": gorm.Expr("remain_quota + ?", locked.BillingTokenQuota),
					"used_quota":   gorm.Expr("used_quota - ?", locked.BillingTokenQuota),
				}).Error; err != nil {
					return err
				}
			case errors.Is(tokenErr, gorm.ErrRecordNotFound):
				// A deleted API token no longer has a quota balance to restore. The
				// user's wallet/subscription refund must still complete exactly once.
			default:
				return tokenErr
			}
		}
		now := time.Now().Unix()
		if err := tx.Model(&WildFlowOperation{}).Where("id = ?", locked.ID).Updates(map[string]any{
			"billing_state":        WildFlowBillingStateRefunded,
			"billing_settled_time": now,
			"updated_time":         now,
		}).Error; err != nil {
			return err
		}
		locked.BillingState = WildFlowBillingStateRefunded
		locked.BillingSettledTime = now
		locked.UpdatedTime = now
		if _, err := ensureWildFlowCanonicalBillingLogTx(tx, locked, LogTypeRefund, "WildFlow job refunded"); err != nil {
			return err
		}
		result = locked
		changed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if changed {
		userDelta := 0
		if result.BillingSource == WildFlowBillingSourceWallet {
			userDelta = result.BillingQuota
		}
		syncWildFlowBillingQuotaCache(result.UserID, tokenKey, userDelta, result.BillingTokenQuota)
	}
	return result, changed, nil
}

func GetWildFlowOperationByID(operationID string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	err := DB.Where("operation_id = ?", operationID).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &operation, err
}

func syncWildFlowBillingQuotaCache(userID int, tokenKey string, userDelta int, tokenDelta int) {
	if userID > 0 && userDelta != 0 {
		if err := cacheIncrUserQuota(userID, int64(userDelta)); err != nil {
			common.SysLog("failed to sync WildFlow user billing cache: " + err.Error())
		}
	}
	if tokenKey != "" && tokenDelta != 0 && common.RedisEnabled {
		if err := cacheIncrTokenQuota(tokenKey, int64(tokenDelta)); err != nil {
			common.SysLog("failed to sync WildFlow token billing cache: " + err.Error())
		}
	}
}

func RecordWildFlowBillingLog(operation *WildFlowOperation, logType int, content string) error {
	if operation == nil {
		return fmt.Errorf("nil WildFlow operation")
	}
	if DB == nil {
		return fmt.Errorf("database is not initialized")
	}
	var entry *WildFlowBillingLogEntry
	if err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		entry, err = ensureWildFlowCanonicalBillingLogTx(tx, operation, logType, content)
		return err
	}); err != nil {
		return err
	}
	if entry.ProjectionState == WildFlowBillingProjectionNotRequired || entry.ProjectionState == WildFlowBillingProjectionProjected {
		return nil
	}
	return projectWildFlowBillingLog(operation.OperationID, logType)
}

func ReconcileWildFlowCanonicalBillingAudits(limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var candidates []*WildFlowOperation
	if err := DB.Table("wild_flow_operations AS operations").
		Select("operations.*").
		Joins(
			"LEFT JOIN wild_flow_billing_log_entries AS audits ON audits.operation_id = operations.operation_id "+
				"AND ((operations.billing_state = ? AND audits.log_type = ?) OR "+
				"(operations.billing_state = ? AND audits.log_type = ?))",
			WildFlowBillingStateSettled,
			LogTypeConsume,
			WildFlowBillingStateRefunded,
			LogTypeRefund,
		).
		Where("operations.billing_state IN ? AND audits.operation_id IS NULL", []string{
			WildFlowBillingStateSettled,
			WildFlowBillingStateRefunded,
		}).
		Order("operations.updated_time asc, operations.id asc").
		Limit(limit).
		Find(&candidates).Error; err != nil {
		return 0, err
	}

	processed := 0
	var reconcileErrors []error
	for _, candidate := range candidates {
		err := DB.Transaction(func(tx *gorm.DB) error {
			operation, err := loadWildFlowOperationForBilling(tx, candidate.OperationID)
			if err != nil {
				return err
			}
			logType := 0
			content := ""
			switch operation.BillingState {
			case WildFlowBillingStateSettled:
				logType = LogTypeConsume
				content = "WildFlow job settled"
			case WildFlowBillingStateRefunded:
				logType = LogTypeRefund
				content = "WildFlow job refunded"
			default:
				return nil
			}
			_, err = ensureWildFlowCanonicalBillingLogTx(tx, operation, logType, content)
			return err
		})
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("operation %s canonical billing audit: %w", candidate.OperationID, err))
			continue
		}
		processed++
	}
	return processed, errors.Join(reconcileErrors...)
}

func ensureWildFlowCanonicalBillingLogTx(
	tx *gorm.DB,
	operation *WildFlowOperation,
	logType int,
	content string,
) (*WildFlowBillingLogEntry, error) {
	if tx == nil || operation == nil || strings.TrimSpace(operation.OperationID) == "" ||
		(logType != LogTypeConsume && logType != LogTypeRefund) {
		return nil, errors.New("invalid WildFlow canonical billing audit")
	}
	projectionState := WildFlowBillingProjectionPending
	if logType == LogTypeConsume && !common.LogConsumeEnabled {
		projectionState = WildFlowBillingProjectionNotRequired
	}
	entry := &WildFlowBillingLogEntry{
		OperationID:         operation.OperationID,
		LogType:             logType,
		UsageEventID:        operation.BillingUsageEventID,
		BillingSource:       operation.BillingSource,
		BillingQuota:        operation.BillingQuota,
		BillingCurrency:     operation.BillingCurrency,
		BillingAmountMicros: operation.BillingAmountMicros,
		Content:             content,
		ProjectionState:     projectionState,
	}
	claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(entry)
	if claim.Error != nil {
		return nil, claim.Error
	}
	if err := lockForUpdate(tx).
		Where("operation_id = ? AND log_type = ?", operation.OperationID, logType).
		First(entry).Error; err != nil {
		return nil, err
	}
	if entry.UsageEventID != operation.BillingUsageEventID || entry.BillingSource != operation.BillingSource ||
		entry.BillingQuota != operation.BillingQuota || entry.BillingCurrency != operation.BillingCurrency ||
		entry.BillingAmountMicros != operation.BillingAmountMicros || entry.Content != content {
		return nil, ErrWildFlowBillingStateConflict
	}
	if entry.ProjectionState == "" {
		if err := tx.Model(&WildFlowBillingLogEntry{}).
			Where("operation_id = ? AND log_type = ? AND projection_state = ?", operation.OperationID, logType, "").
			Update("projection_state", projectionState).Error; err != nil {
			return nil, err
		}
		entry.ProjectionState = projectionState
	}
	return entry, nil
}

func projectWildFlowBillingLog(operationID string, logType int) error {
	entry, claimToken, acquired, err := claimWildFlowBillingProjection(operationID, logType)
	if err != nil || !acquired {
		return err
	}
	if LOG_DB == nil {
		projectionErr := errors.New("log database is not initialized")
		return errors.Join(projectionErr, markWildFlowBillingProjectionFailed(entry, claimToken, wildFlowBillingProjectionFailureCode(projectionErr)))
	}
	operation, err := GetWildFlowOperationByID(operationID)
	if err != nil || operation == nil {
		if err == nil {
			err = errors.New("WildFlow billing operation not found")
		}
		return errors.Join(err, markWildFlowBillingProjectionFailed(entry, claimToken, wildFlowBillingProjectionFailureCode(err)))
	}
	if err := createWildFlowGenericBillingLog(operation, entry); err != nil {
		return errors.Join(err, markWildFlowBillingProjectionFailed(entry, claimToken, wildFlowBillingProjectionFailureCode(err)))
	}
	return markWildFlowBillingProjectionProjected(entry, claimToken)
}

func claimWildFlowBillingProjection(operationID string, logType int) (*WildFlowBillingLogEntry, string, bool, error) {
	claimToken := uuid.NewString()
	now := time.Now().Unix()
	leaseExpiresAt := time.Now().Add(time.Minute).Unix()
	var result *WildFlowBillingLogEntry
	acquired := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var entry WildFlowBillingLogEntry
		if err := lockForUpdate(tx).
			Where("operation_id = ? AND log_type = ?", operationID, logType).
			First(&entry).Error; err != nil {
			return err
		}
		if entry.ProjectionState == WildFlowBillingProjectionProjected || entry.ProjectionState == WildFlowBillingProjectionNotRequired {
			result = &entry
			return nil
		}
		if entry.ProjectionState == WildFlowBillingProjectionUnsupported {
			return errWildFlowClickHouseProjectionUnsupported
		}
		if entry.ProjectionState == WildFlowBillingProjectionProjecting && entry.ProjectionLeaseExpiresAt > now {
			result = &entry
			return nil
		}
		update := tx.Model(&WildFlowBillingLogEntry{}).
			Where(
				"operation_id = ? AND log_type = ? AND (projection_state IN ? OR (projection_state = ? AND projection_lease_expires_at <= ?))",
				operationID,
				logType,
				[]string{"", WildFlowBillingProjectionPending, WildFlowBillingProjectionFailed},
				WildFlowBillingProjectionProjecting,
				now,
			).
			Updates(map[string]any{
				"projection_state":            WildFlowBillingProjectionProjecting,
				"projection_attempts":         gorm.Expr("projection_attempts + 1"),
				"projection_last_error":       "",
				"projection_claim_token":      claimToken,
				"projection_lease_expires_at": leaseExpiresAt,
			})
		if update.Error != nil {
			return update.Error
		}
		if err := tx.Where("operation_id = ? AND log_type = ?", operationID, logType).First(&entry).Error; err != nil {
			return err
		}
		result = &entry
		acquired = update.RowsAffected == 1 && entry.ProjectionClaimToken == claimToken
		return nil
	})
	return result, claimToken, acquired, err
}

func markWildFlowBillingProjectionFailed(entry *WildFlowBillingLogEntry, claimToken string, failureCode string) error {
	if entry == nil {
		return errors.New("nil WildFlow billing projection")
	}
	projectionState := WildFlowBillingProjectionFailed
	if failureCode == "log_projection_idempotency_unsupported" {
		projectionState = WildFlowBillingProjectionUnsupported
	}
	update := DB.Model(&WildFlowBillingLogEntry{}).
		Where(
			"operation_id = ? AND log_type = ? AND projection_state = ? AND projection_claim_token = ?",
			entry.OperationID,
			entry.LogType,
			WildFlowBillingProjectionProjecting,
			claimToken,
		).
		Updates(map[string]any{
			"projection_state":            projectionState,
			"projection_last_error":       failureCode,
			"projection_claim_token":      "",
			"projection_lease_expires_at": int64(0),
		})
	return update.Error
}

var errWildFlowClickHouseProjectionUnsupported = errors.New("ClickHouse does not provide the transactional idempotency required for WildFlow billing log projection")

func wildFlowBillingProjectionFailureCode(err error) string {
	if errors.Is(err, errWildFlowClickHouseProjectionUnsupported) {
		return "log_projection_idempotency_unsupported"
	}
	return "log_projection_failed"
}

func markWildFlowBillingProjectionProjected(entry *WildFlowBillingLogEntry, claimToken string) error {
	if entry == nil {
		return errors.New("nil WildFlow billing projection")
	}
	update := DB.Model(&WildFlowBillingLogEntry{}).
		Where(
			"operation_id = ? AND log_type = ? AND projection_state = ? AND projection_claim_token = ?",
			entry.OperationID,
			entry.LogType,
			WildFlowBillingProjectionProjecting,
			claimToken,
		).
		Updates(map[string]any{
			"projection_state":            WildFlowBillingProjectionProjected,
			"projection_last_error":       "",
			"projection_claim_token":      "",
			"projection_lease_expires_at": int64(0),
			"projected_time":              time.Now().Unix(),
		})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return errors.New("WildFlow billing projection claim lost")
	}
	return nil
}

func createWildFlowGenericBillingLog(operation *WildFlowOperation, entry *WildFlowBillingLogEntry) error {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return errWildFlowClickHouseProjectionUnsupported
	}
	username := ""
	group := ""
	var user User
	if err := DB.Select("username", "group").Where("id = ?", operation.UserID).First(&user).Error; err == nil {
		username = user.Username
		group = user.Group
	}
	tokenName := ""
	if operation.TokenID > 0 {
		var token Token
		if err := DB.Select("name").Where("id = ?", operation.TokenID).First(&token).Error; err == nil {
			tokenName = token.Name
		}
	}
	other := map[string]any{
		"operation_id":      operation.OperationID,
		"job_id":            operation.JobID,
		"usage_event_id":    entry.UsageEventID,
		"billing_source":    operation.BillingSource,
		"currency":          operation.BillingCurrency,
		"amount_micros":     operation.BillingAmountMicros,
		"unit":              operation.BillingUnit,
		"billable_units":    operation.BillingBillableUnits,
		"quota_per_unit":    operation.BillingQuotaPerUnit,
		"usd_exchange_rate": operation.BillingUSDExchangeRate,
		"price_version":     operation.BillingPriceVersion,
	}
	logEntry := &Log{
		UserId:    operation.UserID,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      entry.LogType,
		Content:   entry.Content,
		TokenName: tokenName,
		ModelName: operation.ProductModelRef,
		Quota:     operation.BillingQuota,
		TokenId:   operation.TokenID,
		Group:     group,
		RequestId: operation.OperationID,
		Other:     common.MapToJsonStr(other),
	}
	return LOG_DB.Transaction(func(tx *gorm.DB) error {
		receipt := &WildFlowBillingLogProjectionReceipt{
			OperationID: operation.OperationID,
			LogType:     entry.LogType,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(receipt).Error; err != nil {
			return err
		}
		if err := lockLogForUpdate(tx).
			Where("operation_id = ? AND log_type = ?", operation.OperationID, entry.LogType).
			First(receipt).Error; err != nil {
			return err
		}
		if receipt.Projected {
			return nil
		}
		var legacyProjectionCount int64
		if err := tx.Model(&Log{}).
			Where("request_id = ? AND type = ?", operation.OperationID, entry.LogType).
			Count(&legacyProjectionCount).Error; err != nil {
			return err
		}
		if legacyProjectionCount == 0 {
			if err := tx.Create(logEntry).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&WildFlowBillingLogProjectionReceipt{}).
			Where("operation_id = ? AND log_type = ? AND projected = ?", operation.OperationID, entry.LogType, false).
			Updates(map[string]any{
				"projected":      true,
				"projected_time": time.Now().Unix(),
			}).Error; err != nil {
			return err
		}
		var persisted WildFlowBillingLogProjectionReceipt
		if err := tx.Where("operation_id = ? AND log_type = ?", operation.OperationID, entry.LogType).
			First(&persisted).Error; err != nil {
			return err
		}
		if !persisted.Projected {
			return errors.New("WildFlow billing log projection receipt was not committed")
		}
		return nil
	})
}

func ReconcileWildFlowBillingLogProjections(limit int) (int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now := time.Now().Unix()
	var entries []*WildFlowBillingLogEntry
	if err := DB.Where(
		"projection_state IN ? OR (projection_state = ? AND projection_lease_expires_at <= ?)",
		[]string{"", WildFlowBillingProjectionPending, WildFlowBillingProjectionFailed},
		WildFlowBillingProjectionProjecting,
		now,
	).
		Order("created_time asc, operation_id asc, log_type asc").
		Limit(limit).
		Find(&entries).Error; err != nil {
		return 0, err
	}
	processed := 0
	var projectionErrors []error
	for _, entry := range entries {
		if err := projectWildFlowBillingLog(entry.OperationID, entry.LogType); err != nil {
			projectionErrors = append(projectionErrors, fmt.Errorf("operation %s log %d: %w", entry.OperationID, entry.LogType, err))
			continue
		}
		processed++
	}
	return processed, errors.Join(projectionErrors...)
}
