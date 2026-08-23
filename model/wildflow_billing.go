package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
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
		result = operation
		changed = true
		return nil
	})
	return result, changed, err
}

func RefundWildFlowOperationBilling(operationID string) (*WildFlowOperation, bool, error) {
	operation, err := GetWildFlowOperationByID(operationID)
	if err != nil {
		return nil, false, err
	}
	if operation == nil || operation.BillingState == WildFlowBillingStateRefunded {
		return operation, false, nil
	}
	if (operation.BillingState != WildFlowBillingStateReserved && operation.BillingState != WildFlowBillingStateRefunding) ||
		(operation.State != "failed" && operation.State != "cancelled") {
		return operation, false, nil
	}
	if operation.BillingSource == WildFlowBillingSourceSubscription {
		operation, err = claimWildFlowSubscriptionRefund(operationID)
		if err != nil {
			return nil, false, err
		}
		if operation == nil || operation.BillingState == WildFlowBillingStateRefunded {
			return operation, false, nil
		}
		if err := RefundSubscriptionPreConsume(operation.OperationID); err != nil {
			return operation, false, err
		}
	}

	var result *WildFlowOperation
	var tokenKey string
	changed := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		locked, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		if locked.BillingState == WildFlowBillingStateRefunded {
			result = locked
			return nil
		}
		expectedBillingState := WildFlowBillingStateReserved
		if locked.BillingSource == WildFlowBillingSourceSubscription {
			expectedBillingState = WildFlowBillingStateRefunding
		}
		if locked.BillingState != expectedBillingState || (locked.State != "failed" && locked.State != "cancelled") {
			result = locked
			return nil
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

func claimWildFlowSubscriptionRefund(operationID string) (*WildFlowOperation, error) {
	var result *WildFlowOperation
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForBilling(tx, operationID)
		if err != nil {
			return err
		}
		if operation.BillingState == WildFlowBillingStateRefunded {
			result = operation
			return nil
		}
		if operation.BillingSource != WildFlowBillingSourceSubscription ||
			(operation.State != "failed" && operation.State != "cancelled") {
			result = operation
			return nil
		}
		switch operation.BillingState {
		case WildFlowBillingStateRefunding:
			result = operation
			return nil
		case WildFlowBillingStateReserved:
			now := time.Now().Unix()
			if err := tx.Model(&WildFlowOperation{}).Where("id = ?", operation.ID).Updates(map[string]any{
				"billing_state": WildFlowBillingStateRefunding,
				"updated_time":  now,
			}).Error; err != nil {
				return err
			}
			operation.BillingState = WildFlowBillingStateRefunding
			operation.UpdatedTime = now
			result = operation
			return nil
		default:
			result = operation
			return nil
		}
	})
	return result, err
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
	projectToGenericLog := logType != LogTypeConsume || common.LogConsumeEnabled
	if projectToGenericLog && LOG_DB == nil {
		return fmt.Errorf("log database is not initialized")
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
	}
	claim := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(entry)
	if claim.Error != nil {
		return claim.Error
	}
	if claim.RowsAffected == 0 {
		return nil
	}
	if !projectToGenericLog {
		return nil
	}
	// The marker is the concurrency primitive. This read only preserves legacy
	// projections created before the marker table existed.
	var legacyProjectionCount int64
	if err := LOG_DB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, logType).
		Count(&legacyProjectionCount).Error; err != nil {
		return err
	}
	if legacyProjectionCount > 0 {
		return nil
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
		"usage_event_id":    operation.BillingUsageEventID,
		"billing_source":    operation.BillingSource,
		"currency":          operation.BillingCurrency,
		"amount_micros":     operation.BillingAmountMicros,
		"unit":              operation.BillingUnit,
		"billable_units":    operation.BillingBillableUnits,
		"quota_per_unit":    operation.BillingQuotaPerUnit,
		"usd_exchange_rate": operation.BillingUSDExchangeRate,
		"price_version":     operation.BillingPriceVersion,
	}
	return createLog(&Log{
		UserId:    operation.UserID,
		Username:  username,
		CreatedAt: common.GetTimestamp(),
		Type:      logType,
		Content:   content,
		TokenName: tokenName,
		ModelName: operation.ProductModelRef,
		Quota:     operation.BillingQuota,
		TokenId:   operation.TokenID,
		Group:     group,
		RequestId: operation.OperationID,
		Other:     common.MapToJsonStr(other),
	})
}
