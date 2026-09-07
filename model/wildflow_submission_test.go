package model

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWildFlowSubmissionLeaseHasOneOwnerAndRecoversExpiredPreparedWork(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "submission-lease")
	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("billing_state", WildFlowBillingStateReserved).Error)
	now := time.Now().Unix()

	first, acquired, err := ClaimWildFlowOperationSubmission(
		operation.OperationID, "owner-a", "token-a", now+60,
	)
	require.NoError(t, err)
	assert.True(t, acquired)
	assert.Equal(t, WildFlowSubmissionPhasePrepared, first.SubmissionPhase)
	assert.Equal(t, "owner-a", first.SubmissionOwner)
	assert.Equal(t, "token-a", first.SubmissionLeaseToken)

	current, acquired, err := ClaimWildFlowOperationSubmission(
		operation.OperationID, "owner-b", "token-b", now+60,
	)
	require.NoError(t, err)
	assert.False(t, acquired)
	assert.Equal(t, "owner-a", current.SubmissionOwner)
	require.ErrorIs(t,
		func() error {
			_, beginErr := BeginWildFlowOperationSubmission(operation.OperationID, "owner-b", "token-b")
			return beginErr
		}(),
		ErrWildFlowSubmissionLeaseLost,
	)

	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("submission_lease_expires_at", now-1).Error)
	reclaimed, acquired, err := ClaimWildFlowOperationSubmission(
		operation.OperationID, "owner-b", "token-b", now+60,
	)
	require.NoError(t, err)
	assert.True(t, acquired)
	assert.Equal(t, "owner-b", reclaimed.SubmissionOwner)

	inFlight, err := BeginWildFlowOperationSubmission(operation.OperationID, "owner-b", "token-b")
	require.NoError(t, err)
	assert.Equal(t, WildFlowSubmissionPhaseSubmitting, inFlight.SubmissionPhase)
	assert.Equal(t, 1, inFlight.SubmissionAttempt)

	_, acquired, err = ClaimWildFlowOperationSubmission(
		operation.OperationID, "owner-c", "token-c", now+120,
	)
	require.NoError(t, err)
	assert.False(t, acquired, "an in-flight provider call cannot be taken over")
	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("submission_lease_expires_at", now-1).Error)
	processed, err := ReconcileExpiredWildFlowSubmissionLeases(now, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	var persisted WildFlowOperation
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(&persisted).Error)
	assert.Equal(t, "recovery_required", persisted.State)
	assert.Equal(t, "submission_lease_expired", persisted.LastErrorCode)
	assert.Equal(t, WildFlowSubmissionPhaseRecoveryRequired, persisted.SubmissionPhase)
	assert.Equal(t, WildFlowBillingStateReserved, persisted.BillingState, "unknown side effects keep the reservation")
}

func TestLegacySubmittingOperationWithoutLeaseBecomesStickyRecovery(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "legacy-submission")
	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Updates(map[string]any{
			"billing_state":    WildFlowBillingStateReserved,
			"submission_phase": "",
		}).Error)

	processed, err := ReconcileWildFlowSubmissionLease(operation.OperationID, time.Now().Unix())
	require.NoError(t, err)
	assert.True(t, processed)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, "recovery_required", operation.State)
	assert.Equal(t, "legacy_submission_state_unknown", operation.LastErrorCode)
	assert.Equal(t, WildFlowSubmissionPhaseRecoveryRequired, operation.SubmissionPhase)
	assert.Equal(t, WildFlowBillingStateReserved, operation.BillingState, "legacy provider side effects are unknown")
}

func TestTaskOperationReplayAndAtomicTaskAttachment(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	require.NoError(t, db.AutoMigrate(&Task{}))
	candidate := func(user int, id, key, request string) *WildFlowOperation {
		return &WildFlowOperation{OperationID: id, UserID: user, TokenID: 1, TaskID: "task_" + id, IdempotencyKeyDigest: strings.Repeat(key, 64), RequestDigest: strings.Repeat(request, 64), SubmissionLeaseExpiresAt: time.Now().Unix() + 60}
	}
	first, created, err := ReserveTaskOperation(candidate(7, "image-first", "a", "b"))
	require.NoError(t, err)
	require.True(t, created)
	replay, created, err := ReserveTaskOperation(candidate(7, "image-second", "a", "b"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.TaskID, replay.TaskID)
	_, _, err = ReserveTaskOperation(candidate(7, "image-different", "a", "c"))
	require.ErrorIs(t, err, ErrTaskOperationConflict)
	other, created, err := ReserveTaskOperation(candidate(8, "image-other", "a", "b"))
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, first.TaskID, other.TaskID)
	// Force the insert to fail after the operation update; both must roll back.
	require.NoError(t, db.Create(&Task{ID: 91, TaskID: "existing", UserId: 7}).Error)
	task := &Task{ID: 91, TaskID: first.TaskID, UserId: 7, Status: TaskStatusSubmitted}
	require.Error(t, InsertTaskForOperation(first.OperationID, task))
	fresh, err := GetWildFlowOperationByUserAndKey(7, strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, TaskOperationSubmitting, fresh.State)
	task.ID = 0
	require.NoError(t, InsertTaskForOperation(first.OperationID, task))
	require.ErrorIs(t, InsertTaskForOperation(first.OperationID, task), ErrTaskOperationState)
	replay, created, err = ReserveTaskOperation(candidate(7, "image-third", "a", "b"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, TaskOperationAccepted, replay.State)
	var count int64
	require.NoError(t, db.Model(&Task{}).Where("task_id = ?", first.TaskID).Count(&count).Error)
	require.EqualValues(t, 1, count)
	pending, err := ListWildFlowOperationsForBillingReconciliation(100)
	require.NoError(t, err)
	require.Empty(t, pending, "task billing must not also enter inference job settlement")
	// An expired uncertain submission stays recoverable, never a new winner.
	require.NoError(t, db.Model(other).Update("submission_lease_expires_at", time.Now().Unix()-1).Error)
	replay, created, err = ReserveTaskOperation(candidate(8, "image-fourth", "a", "b"))
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, "recovery_required", replay.State)
	// The original in-flight request can still attach a late successful result;
	// recovery forbids resubmission, not durable recording of known work.
	require.NoError(t, InsertTaskForOperation(other.OperationID, &Task{TaskID: other.TaskID, UserId: 8}))
}

func TestConcurrentTaskOperationReservationHasOneSubmitWinner(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	type reservation struct {
		operation *WildFlowOperation
		created   bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan reservation, 2)
	for _, id := range []string{"concurrent-a", "concurrent-b"} {
		go func(id string) {
			<-start
			operation, created, err := ReserveTaskOperation(&WildFlowOperation{OperationID: id, TaskID: "task_" + id, UserID: 7, IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), SubmissionLeaseExpiresAt: time.Now().Unix() + 60})
			results <- reservation{operation, created, err}
		}(id)
	}
	close(start)
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotEqual(t, first.created, second.created)
	require.Equal(t, first.operation.OperationID, second.operation.OperationID)
}
