package model

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Task-host operations share user-scoped idempotency identity, while quota
// reservation/settlement stays in the existing Task billing lifecycle.
const TaskOperationBillingState = "managed_by_task"
const TaskOperationSubmitting = "task_submitting"
const TaskOperationAccepted = "task_accepted"

var ErrTaskOperationConflict = errors.New("idempotency key was used for another request")
var ErrTaskOperationState = errors.New("task operation is not available for attachment")

// ReserveTaskOperation returns created=true only for the insert winner. Existing
// operations are never taken over for another upstream submission, even if their
// submission deadline has expired. Keys and requests arrive as SHA-256 digests.
func ReserveTaskOperation(candidate *WildFlowOperation) (*WildFlowOperation, bool, error) {
	if candidate == nil || candidate.UserID <= 0 || candidate.OperationID == "" || !strings.HasPrefix(candidate.TaskID, "task_") ||
		len(candidate.IdempotencyKeyDigest) != 64 || len(candidate.RequestDigest) != 64 || candidate.SubmissionLeaseExpiresAt <= time.Now().Unix() {
		return nil, false, errors.New("invalid task operation reservation")
	}
	existing, err := GetWildFlowOperationByUserAndKey(candidate.UserID, candidate.IdempotencyKeyDigest)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		candidate.State = TaskOperationSubmitting
		candidate.BillingState = TaskOperationBillingState
		candidate.BillingSource = "task_host"
		candidate.SubmissionPhase = WildFlowSubmissionPhaseSubmitting
		candidate.SubmissionAttempt = 1
		if err := DB.Create(candidate).Error; err == nil {
			return candidate, true, nil
		} else {
			// A concurrent insert may have won the unique (user,key) constraint.
			existing, lookupErr := GetWildFlowOperationByUserAndKey(candidate.UserID, candidate.IdempotencyKeyDigest)
			if lookupErr != nil {
				return nil, false, lookupErr
			}
			if existing == nil {
				return nil, false, err
			}
			return resolveTaskOperationReplay(existing, candidate.RequestDigest)
		}
	}
	return resolveTaskOperationReplay(existing, candidate.RequestDigest)
}

func resolveTaskOperationReplay(operation *WildFlowOperation, digest string) (*WildFlowOperation, bool, error) {
	if operation.RequestDigest != digest || operation.BillingState != TaskOperationBillingState || operation.TaskID == "" {
		return nil, false, ErrTaskOperationConflict
	}
	if operation.State == TaskOperationSubmitting && operation.SubmissionLeaseExpiresAt <= time.Now().Unix() {
		result := DB.Model(&WildFlowOperation{}).Where("id = ? AND state = ?", operation.ID, TaskOperationSubmitting).
			Updates(map[string]any{"state": "recovery_required", "submission_phase": WildFlowSubmissionPhaseRecoveryRequired, "last_error_code": "task_submission_outcome_unknown", "updated_time": time.Now().Unix()})
		if result.Error != nil {
			return nil, false, result.Error
		}
		if err := DB.First(operation, operation.ID).Error; err != nil {
			return nil, false, err
		}
	}
	return operation, false, nil
}

// InsertTaskForOperation atomically links a successfully submitted task to its
// reserved operation. A lost response after commit can be recovered by key.
func InsertTaskForOperation(operationID string, task *Task) error {
	if task == nil || task.UserId <= 0 {
		return ErrTaskOperationState
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var operation WildFlowOperation
		if err := lockForUpdate(tx).Where("operation_id = ? AND user_id = ?", operationID, task.UserId).First(&operation).Error; err != nil {
			return err
		}
		canAttach := operation.State == TaskOperationSubmitting || (operation.State == "recovery_required" && operation.LastErrorCode == "task_submission_outcome_unknown")
		if operation.TaskID != task.TaskID || !canAttach || operation.BillingState != TaskOperationBillingState {
			return ErrTaskOperationState
		}
		update := tx.Model(&WildFlowOperation{}).Where("id = ? AND state = ?", operation.ID, operation.State).
			Updates(map[string]any{"state": TaskOperationAccepted, "submission_phase": WildFlowSubmissionPhaseAccepted, "last_error_code": "", "updated_time": time.Now().Unix()})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrTaskOperationState
		}
		return tx.Create(task).Error
	})
}

// GetTaskOperationForUserAndTask also makes an expired unknown submission
// visible as recovery_required when the caller only uses the polling URL.
func GetTaskOperationForUserAndTask(userID int, taskID string) (*WildFlowOperation, error) {
	var operation WildFlowOperation
	err := DB.Where("user_id = ? AND task_id = ? AND billing_state = ?", userID, taskID, TaskOperationBillingState).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, _, err := resolveTaskOperationReplay(&operation, operation.RequestDigest)
	return current, err
}

// RecordTaskOperationSubmissionFailure never overwrites an attached task. It
// does not touch quota: the existing reservation lifecycle owns that decision.
func RecordTaskOperationSubmissionFailure(operationID string, uncertain bool) (bool, error) {
	state, phase, code := "task_failed", WildFlowSubmissionPhaseFailed, "task_submission_rejected"
	if uncertain {
		state = "recovery_required"
		phase = WildFlowSubmissionPhaseRecoveryRequired
		code = "task_submission_outcome_unknown"
	}
	result := DB.Model(&WildFlowOperation{}).Where("operation_id = ? AND state = ? AND billing_state = ?", operationID, TaskOperationSubmitting, TaskOperationBillingState).
		Updates(map[string]any{"state": state, "submission_phase": phase, "last_error_code": code, "updated_time": time.Now().Unix()})
	return result.RowsAffected == 1, result.Error
}
