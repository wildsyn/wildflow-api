package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

type WildFlowOperation struct {
	ID                     int64  `json:"-" gorm:"primaryKey"`
	OperationID            string `json:"id" gorm:"type:varchar(64);uniqueIndex"`
	UserID                 int    `json:"-" gorm:"uniqueIndex:idx_wildflow_operation_user_key,priority:1;index"`
	TokenID                int    `json:"-" gorm:"index"`
	IdempotencyKeyDigest   string `json:"-" gorm:"type:char(64);uniqueIndex:idx_wildflow_operation_user_key,priority:2"`
	RequestDigest          string `json:"-" gorm:"type:char(64)"`
	RequestID              string `json:"request_id" gorm:"type:varchar(64);index"`
	ProductModelRef        string `json:"model" gorm:"type:varchar(200);index"`
	ModelVersionRef        string `json:"model_version_ref" gorm:"type:varchar(200)"`
	JobID                  string `json:"job_id,omitempty" gorm:"type:varchar(200);index"`
	State                  string `json:"state" gorm:"type:varchar(32);index"`
	LastErrorCode          string `json:"error,omitempty" gorm:"type:varchar(64)"`
	BillingState           string `json:"-" gorm:"type:varchar(32);index"`
	BillingSource          string `json:"-" gorm:"type:varchar(32)"`
	BillingSubscriptionID  int    `json:"-" gorm:"index"`
	BillingQuota           int    `json:"-"`
	BillingTokenQuota      int    `json:"-"`
	BillingCurrency        string `json:"-" gorm:"type:varchar(8)"`
	BillingAmountMicros    int64  `json:"-" gorm:"bigint"`
	BillingUnit            string `json:"-" gorm:"type:varchar(32)"`
	BillingBillableUnits   int64  `json:"-" gorm:"bigint"`
	BillingQuotaPerUnit    string `json:"-" gorm:"type:varchar(64)"`
	BillingUSDExchangeRate string `json:"-" gorm:"type:varchar(64)"`
	BillingPriceVersion    string `json:"-" gorm:"type:varchar(64)"`
	BillingUsageEventID    string `json:"-" gorm:"type:varchar(128);index"`
	BillingSettledTime     int64  `json:"-" gorm:"bigint"`
	ResultJSON             string `json:"-" gorm:"type:text"`
	ResultValidatedTime    int64  `json:"-" gorm:"bigint"`
	ResultRetentionSeconds int64  `json:"-" gorm:"bigint"`
	ResultExpiresAt        int64  `json:"-" gorm:"bigint;index"`
	CreatedTime            int64  `json:"created_at" gorm:"bigint"`
	UpdatedTime            int64  `json:"updated_at" gorm:"bigint"`
}

func (operation *WildFlowOperation) BeforeCreate(_ *gorm.DB) error {
	now := time.Now().Unix()
	if operation.CreatedTime == 0 {
		operation.CreatedTime = now
	}
	if operation.BillingState == "" {
		operation.BillingState = WildFlowBillingStatePending
	}
	operation.UpdatedTime = now
	return nil
}

func GetWildFlowOperationByUserAndKey(userID int, keyDigest string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	err := DB.Where("user_id = ? AND idempotency_key_digest = ?", userID, keyDigest).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &operation, err
}

func GetWildFlowOperationForUser(userID int, operationID string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	err := DB.Where("user_id = ? AND operation_id = ?", userID, operationID).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &operation, err
}

func GetWildFlowOperationForUserAndJob(userID int, jobID string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	err := DB.Where("user_id = ? AND job_id = ?", userID, jobID).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &operation, err
}

func CreateWildFlowOperation(operation *WildFlowOperation) error {
	return DB.Create(operation).Error
}

func UpdateWildFlowOperationExecution(
	operationID string,
	jobID string,
	state string,
	errorCode string,
) error {
	return DB.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operationID).
		Updates(map[string]any{
			"job_id":          jobID,
			"state":           state,
			"last_error_code": errorCode,
			"updated_time":    time.Now().Unix(),
		}).Error
}

// StoreWildFlowOperationResult persists the first validated public result and
// never replaces it. This gives idempotent replays an immutable response even
// if inference storage changes later.
func StoreWildFlowOperationResult(operationID string, resultJSON string, expiresAt int64) (*WildFlowOperation, error) {
	if DB == nil || operationID == "" || resultJSON == "" || expiresAt <= time.Now().Unix() {
		return nil, errors.New("invalid WildFlow operation result")
	}
	var result *WildFlowOperation
	err := DB.Transaction(func(tx *gorm.DB) error {
		var operation WildFlowOperation
		if err := lockForUpdate(tx).Where("operation_id = ?", operationID).First(&operation).Error; err != nil {
			return err
		}
		if operation.State != "succeeded" {
			return errors.New("WildFlow operation result is not successful")
		}
		if operation.ResultJSON != "" {
			updates := map[string]any{}
			if operation.ResultRetentionSeconds == 0 {
				updates["result_retention_seconds"] = expiresAt - time.Now().Unix()
			}
			if operation.ResultExpiresAt == 0 {
				updates["result_expires_at"] = expiresAt
			}
			if operation.ResultValidatedTime == 0 {
				updates["result_validated_time"] = time.Now().Unix()
			}
			if len(updates) > 0 {
				if err := tx.Model(&WildFlowOperation{}).Where("id = ?", operation.ID).Updates(updates).Error; err != nil {
					return err
				}
				if err := tx.Where("id = ?", operation.ID).First(&operation).Error; err != nil {
					return err
				}
			}
			result = &operation
			return nil
		}
		now := time.Now().Unix()
		updates := map[string]any{
			"result_json":           resultJSON,
			"result_validated_time": now,
			"result_expires_at":     expiresAt,
			"updated_time":          now,
		}
		if operation.ResultRetentionSeconds == 0 {
			updates["result_retention_seconds"] = expiresAt - now
		}
		if err := tx.Model(&WildFlowOperation{}).
			Where("id = ? AND (result_json = ? OR result_json IS NULL)", operation.ID, "").
			Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", operation.ID).First(&operation).Error; err != nil {
			return err
		}
		result = &operation
		return nil
	})
	return result, err
}

func ListWildFlowOperationsForBillingReconciliation(limit int) ([]*WildFlowOperation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var operations []*WildFlowOperation
	err := DB.Where(
		"billing_state IN ? AND job_id <> ?",
		[]string{WildFlowBillingStateReserved, WildFlowBillingStateRefunding},
		"",
	).
		Order("updated_time asc, id asc").
		Limit(limit).
		Find(&operations).Error
	return operations, err
}
