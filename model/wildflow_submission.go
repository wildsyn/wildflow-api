package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	WildFlowSubmissionPhasePrepared         = "prepared"
	WildFlowSubmissionPhaseSubmitting       = "submitting"
	WildFlowSubmissionPhaseRetryable        = "retryable"
	WildFlowSubmissionPhaseAccepted         = "accepted"
	WildFlowSubmissionPhaseFailed           = "failed"
	WildFlowSubmissionPhaseRecoveryRequired = "recovery_required"
)

var ErrWildFlowSubmissionLeaseLost = errors.New("WildFlow submission lease lost")

func ClaimWildFlowOperationSubmission(
	operationID string,
	owner string,
	leaseToken string,
	leaseExpiresAt int64,
) (*WildFlowOperation, bool, error) {
	now := time.Now().Unix()
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(owner) == "" || len(owner) > 128 ||
		strings.TrimSpace(leaseToken) == "" || len(leaseToken) > 64 || leaseExpiresAt <= now {
		return nil, false, errors.New("invalid WildFlow submission lease")
	}
	var result *WildFlowOperation
	acquired := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForSubmission(tx, operationID)
		if err != nil {
			return err
		}
		if operation.JobID != "" || operation.State != "submitting" ||
			(operation.SubmissionPhase != WildFlowSubmissionPhasePrepared && operation.SubmissionPhase != WildFlowSubmissionPhaseRetryable) {
			result = operation
			return nil
		}
		if operation.SubmissionRetryUntil > 0 && operation.SubmissionRetryUntil <= now {
			result = operation
			return nil
		}
		if operation.SubmissionLeaseToken != "" && operation.SubmissionLeaseExpiresAt > now {
			result = operation
			return nil
		}
		update := tx.Model(&WildFlowOperation{}).
			Where(
				"id = ? AND job_id = ? AND state = ? AND submission_phase IN ? AND (submission_lease_token = ? OR submission_lease_expires_at <= ?)",
				operation.ID,
				"",
				"submitting",
				[]string{WildFlowSubmissionPhasePrepared, WildFlowSubmissionPhaseRetryable},
				"",
				now,
			).
			Updates(map[string]any{
				"submission_phase":            WildFlowSubmissionPhasePrepared,
				"submission_owner":            owner,
				"submission_lease_token":      leaseToken,
				"submission_lease_expires_at": leaseExpiresAt,
				"updated_time":                now,
			})
		if update.Error != nil {
			return update.Error
		}
		if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
			return err
		}
		result = operation
		acquired = update.RowsAffected == 1 && operation.SubmissionOwner == owner && operation.SubmissionLeaseToken == leaseToken
		return nil
	})
	return result, acquired, err
}

func BeginWildFlowOperationSubmission(operationID string, owner string, leaseToken string) (*WildFlowOperation, error) {
	var result *WildFlowOperation
	now := time.Now().Unix()
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForSubmission(tx, operationID)
		if err != nil {
			return err
		}
		if operation.State != "submitting" || operation.JobID != "" ||
			operation.SubmissionPhase != WildFlowSubmissionPhasePrepared ||
			operation.SubmissionOwner != owner || operation.SubmissionLeaseToken != leaseToken ||
			operation.SubmissionLeaseExpiresAt <= now {
			return ErrWildFlowSubmissionLeaseLost
		}
		update := tx.Model(&WildFlowOperation{}).
			Where(
				"id = ? AND state = ? AND job_id = ? AND submission_phase = ? AND submission_owner = ? AND submission_lease_token = ? AND submission_lease_expires_at > ?",
				operation.ID,
				"submitting",
				"",
				WildFlowSubmissionPhasePrepared,
				owner,
				leaseToken,
				now,
			).
			Updates(map[string]any{
				"submission_phase":        WildFlowSubmissionPhaseSubmitting,
				"submission_attempt":      gorm.Expr("submission_attempt + 1"),
				"submission_started_time": now,
				"updated_time":            now,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrWildFlowSubmissionLeaseLost
		}
		if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
			return err
		}
		result = operation
		return nil
	})
	return result, err
}

func MarkWildFlowOperationSubmissionRetryable(
	operationID string,
	owner string,
	leaseToken string,
	errorCode string,
	retryUntil int64,
) (*WildFlowOperation, error) {
	if retryUntil <= time.Now().Unix() {
		return nil, errors.New("invalid WildFlow submission retry deadline")
	}
	return transitionWildFlowSubmissionLease(
		operationID,
		owner,
		leaseToken,
		[]string{WildFlowSubmissionPhasePrepared, WildFlowSubmissionPhaseSubmitting},
		map[string]any{
			"state":                       "submitting",
			"last_error_code":             errorCode,
			"submission_phase":            WildFlowSubmissionPhaseRetryable,
			"submission_owner":            "",
			"submission_lease_token":      "",
			"submission_lease_expires_at": int64(0),
			"submission_retry_until": gorm.Expr(
				"CASE WHEN submission_retry_until > 0 AND submission_retry_until < ? THEN submission_retry_until ELSE ? END",
				retryUntil,
				retryUntil,
			),
			"updated_time": time.Now().Unix(),
		},
	)
}

func MarkWildFlowOperationSubmissionRecoveryRequired(
	operationID string,
	owner string,
	leaseToken string,
	errorCode string,
) (*WildFlowOperation, error) {
	return transitionWildFlowSubmissionLease(
		operationID,
		owner,
		leaseToken,
		[]string{WildFlowSubmissionPhaseSubmitting},
		map[string]any{
			"state":                       "recovery_required",
			"last_error_code":             errorCode,
			"submission_phase":            WildFlowSubmissionPhaseRecoveryRequired,
			"submission_owner":            "",
			"submission_lease_token":      "",
			"submission_lease_expires_at": int64(0),
			"updated_time":                time.Now().Unix(),
		},
	)
}

func CompleteWildFlowOperationSubmission(
	operationID string,
	owner string,
	leaseToken string,
	jobID string,
	state string,
) (*WildFlowOperation, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(state) == "" {
		return nil, errors.New("invalid WildFlow submitted job")
	}
	return transitionWildFlowSubmissionLease(
		operationID,
		owner,
		leaseToken,
		[]string{WildFlowSubmissionPhaseSubmitting},
		map[string]any{
			"job_id":                      jobID,
			"state":                       state,
			"last_error_code":             "",
			"submission_phase":            WildFlowSubmissionPhaseAccepted,
			"submission_owner":            "",
			"submission_lease_token":      "",
			"submission_lease_expires_at": int64(0),
			"submission_retry_until":      int64(0),
			"updated_time":                time.Now().Unix(),
		},
	)
}

func transitionWildFlowSubmissionLease(
	operationID string,
	owner string,
	leaseToken string,
	allowedPhases []string,
	updates map[string]any,
) (*WildFlowOperation, error) {
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(leaseToken) == "" {
		return nil, ErrWildFlowSubmissionLeaseLost
	}
	var result *WildFlowOperation
	err := DB.Transaction(func(tx *gorm.DB) error {
		operation, err := loadWildFlowOperationForSubmission(tx, operationID)
		if err != nil {
			return err
		}
		matched := false
		for _, phase := range allowedPhases {
			if operation.SubmissionPhase == phase {
				matched = true
				break
			}
		}
		if !matched || operation.SubmissionOwner != owner || operation.SubmissionLeaseToken != leaseToken {
			return ErrWildFlowSubmissionLeaseLost
		}
		update := tx.Model(&WildFlowOperation{}).
			Where(
				"id = ? AND submission_phase IN ? AND submission_owner = ? AND submission_lease_token = ?",
				operation.ID,
				allowedPhases,
				owner,
				leaseToken,
			).
			Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrWildFlowSubmissionLeaseLost
		}
		if err := tx.Where("id = ?", operation.ID).First(operation).Error; err != nil {
			return err
		}
		result = operation
		return nil
	})
	return result, err
}

func loadWildFlowOperationForSubmission(tx *gorm.DB, operationID string) (*WildFlowOperation, error) {
	if tx == nil || strings.TrimSpace(operationID) == "" {
		return nil, errors.New("invalid WildFlow submission operation")
	}
	var operation WildFlowOperation
	if err := lockForUpdate(tx).Where("operation_id = ?", operationID).First(&operation).Error; err != nil {
		return nil, err
	}
	return &operation, nil
}

func ReconcileExpiredWildFlowSubmissionLeases(now int64, limit int) (int, error) {
	if now <= 0 {
		now = time.Now().Unix()
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var candidates []*WildFlowOperation
	if err := DB.Where(
		"job_id = ? AND ((state = ? AND (submission_phase = ? OR submission_phase IS NULL)) OR "+
			"(state = ? AND submission_phase = ? AND submission_lease_expires_at > 0 AND submission_lease_expires_at <= ?) OR "+
			"(state = ? AND submission_phase IN ? AND submission_retry_until > 0 AND submission_retry_until <= ?) OR "+
			"(state = ? AND submission_phase = ? AND billing_state IN ?))",
		"",
		"submitting",
		"",
		"submitting",
		WildFlowSubmissionPhaseSubmitting,
		now,
		"submitting",
		[]string{WildFlowSubmissionPhasePrepared, WildFlowSubmissionPhaseRetryable},
		now,
		"failed",
		WildFlowSubmissionPhaseFailed,
		[]string{WildFlowBillingStateReserved, WildFlowBillingStateRefunding},
	).
		Order("updated_time asc, id asc").
		Limit(limit).
		Find(&candidates).Error; err != nil {
		return 0, err
	}
	return reconcileWildFlowSubmissionCandidates(candidates, now)
}

func ReconcileWildFlowSubmissionLease(operationID string, now int64) (bool, error) {
	if strings.TrimSpace(operationID) == "" {
		return false, errors.New("invalid WildFlow submission operation")
	}
	if now <= 0 {
		now = time.Now().Unix()
	}
	var candidate WildFlowOperation
	err := DB.Where("operation_id = ?", operationID).First(&candidate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	processed, err := reconcileWildFlowSubmissionCandidates([]*WildFlowOperation{&candidate}, now)
	return processed == 1, err
}

func reconcileWildFlowSubmissionCandidates(candidates []*WildFlowOperation, now int64) (int, error) {
	processed := 0
	var reconcileErrors []error
	for _, candidate := range candidates {
		shouldRefund := false
		transitioned := false
		err := DB.Transaction(func(tx *gorm.DB) error {
			operation, err := loadWildFlowOperationForSubmission(tx, candidate.OperationID)
			if err != nil {
				return err
			}
			switch {
			case operation.State == "submitting" && operation.SubmissionPhase == "":
				if err := tx.Model(&WildFlowOperation{}).Where(
					"id = ? AND state = ? AND (submission_phase = ? OR submission_phase IS NULL)",
					operation.ID,
					"submitting",
					"",
				).Updates(map[string]any{
					"state":                       "recovery_required",
					"last_error_code":             "legacy_submission_state_unknown",
					"submission_phase":            WildFlowSubmissionPhaseRecoveryRequired,
					"submission_owner":            "",
					"submission_lease_token":      "",
					"submission_lease_expires_at": int64(0),
					"updated_time":                now,
				}).Error; err != nil {
					return err
				}
				transitioned = true
			case operation.State == "submitting" &&
				operation.SubmissionPhase == WildFlowSubmissionPhaseSubmitting &&
				operation.SubmissionLeaseExpiresAt > 0 && operation.SubmissionLeaseExpiresAt <= now:
				if err := tx.Model(&WildFlowOperation{}).Where(
					"id = ? AND state = ? AND submission_phase = ? AND submission_lease_token = ? AND submission_lease_expires_at <= ?",
					operation.ID,
					"submitting",
					WildFlowSubmissionPhaseSubmitting,
					operation.SubmissionLeaseToken,
					now,
				).Updates(map[string]any{
					"state":                       "recovery_required",
					"last_error_code":             "submission_lease_expired",
					"submission_phase":            WildFlowSubmissionPhaseRecoveryRequired,
					"submission_owner":            "",
					"submission_lease_token":      "",
					"submission_lease_expires_at": int64(0),
					"updated_time":                now,
				}).Error; err != nil {
					return err
				}
				transitioned = true
			case operation.State == "submitting" &&
				(operation.SubmissionPhase == WildFlowSubmissionPhasePrepared || operation.SubmissionPhase == WildFlowSubmissionPhaseRetryable) &&
				operation.SubmissionRetryUntil > 0 && operation.SubmissionRetryUntil <= now:
				if err := tx.Model(&WildFlowOperation{}).Where("id = ? AND state = ?", operation.ID, "submitting").Updates(map[string]any{
					"state":                       "failed",
					"last_error_code":             "submission_retry_expired",
					"submission_phase":            WildFlowSubmissionPhaseFailed,
					"submission_owner":            "",
					"submission_lease_token":      "",
					"submission_lease_expires_at": int64(0),
					"updated_time":                now,
				}).Error; err != nil {
					return err
				}
				shouldRefund = operation.BillingState == WildFlowBillingStateReserved || operation.BillingState == WildFlowBillingStateRefunding
				transitioned = true
			case operation.State == "failed" && operation.SubmissionPhase == WildFlowSubmissionPhaseFailed:
				shouldRefund = operation.BillingState == WildFlowBillingStateReserved || operation.BillingState == WildFlowBillingStateRefunding
				transitioned = shouldRefund
			}
			return nil
		})
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("operation %s submission reconciliation: %w", candidate.OperationID, err))
			continue
		}
		if !transitioned {
			continue
		}
		processed++
		if shouldRefund {
			refunded, _, refundErr := RefundWildFlowOperationBilling(candidate.OperationID)
			if refundErr != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("operation %s submission refund: %w", candidate.OperationID, refundErr))
				continue
			}
			if refunded != nil && refunded.BillingState == WildFlowBillingStateRefunded {
				if projectionErr := RecordWildFlowBillingLog(refunded, LogTypeRefund, "WildFlow job refunded"); projectionErr != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("operation %s refund projection: %w", candidate.OperationID, projectionErr))
				}
			}
		}
	}
	return processed, errors.Join(reconcileErrors...)
}
