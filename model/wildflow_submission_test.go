package model

import (
	"fmt"
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

// Stopping immediately after InsertTaskForOperation must leave both a durable
// result identity and final funding state. No service-layer Settle is required.
func TestTaskAcceptanceSettlesReservationInSameTransaction(t *testing.T) {
	for _, actual := range []int{0, 400, 500, 600} {
		t.Run(fmt.Sprint(actual), func(t *testing.T) {
			db := setupWildFlowBillingModelTest(t)
			require.NoError(t, db.AutoMigrate(&Task{}, &BillingReservationRecord{}))
			userID, tokenID, tokenKey := seedReservationFixture(t, 2000, 2000)
			reserved, err := ReserveWalletBillingQuota("image-billing", userID, tokenID, tokenKey, 500, false)
			require.NoError(t, err)
			require.True(t, reserved)
			require.NoError(t, MarkBillingReservationProviderStarted("image-billing"))
			op, created, err := ReserveTaskOperation(&WildFlowOperation{
				OperationID: "image-atomic", TaskID: "task_image-atomic", UserID: userID, TokenID: tokenID,
				RequestID: "image-billing", IdempotencyKeyDigest: strings.Repeat("a", 64),
				RequestDigest: strings.Repeat("b", 64), SubmissionLeaseExpiresAt: time.Now().Unix() + 60,
			})
			require.NoError(t, err)
			require.True(t, created)
			require.NoError(t, db.Create(&Task{ID: 91, TaskID: "existing", UserId: userID}).Error)
			task := &Task{ID: 91, TaskID: op.TaskID, UserId: userID, Quota: actual,
				PrivateData: TaskPrivateData{TokenId: tokenID}}
			require.Error(t, InsertTaskForOperation(op.OperationID, task))
			var reservation BillingReservationRecord
			require.NoError(t, db.Where("request_id = ?", op.RequestID).First(&reservation).Error)
			assert.Equal(t, BillingReservationStateProviderStarted, reservation.State)
			assert.Equal(t, 1500, reservationUserQuota(t, userID))
			remain, used := reservationTokenState(t, tokenID)
			assert.Equal(t, 1500, remain)
			assert.Equal(t, 500, used)
			task.ID = 0
			require.NoError(t, InsertTaskForOperation(op.OperationID, task))
			require.NoError(t, db.Where("request_id = ?", op.RequestID).First(&reservation).Error)
			assert.Equal(t, BillingReservationStateSettled, reservation.State)
			assert.Equal(t, 2000-actual, reservationUserQuota(t, userID))
			remain, used = reservationTokenState(t, tokenID)
			assert.Equal(t, 2000-actual, remain)
			assert.Equal(t, actual, used)
			require.ErrorIs(t, InsertTaskForOperation(op.OperationID, task), ErrTaskOperationState)
			// The normal request continuation and recovery replay are no-ops.
			require.NoError(t, SettleBillingReservation(op.RequestID, actual-500))
			assert.Equal(t, 2000-actual, reservationUserQuota(t, userID))
			assert.False(t, requireBillingReservationRelease(t, op.RequestID, tokenKey))
		})
	}
}

func TestTaskAcceptanceRejectsInvalidFundingWithoutPublishingTask(t *testing.T) {
	for _, scenario := range []string{"missing", "wrong_user", "wrong_token", "released", "settled", "insufficient_token"} {
		t.Run(scenario, func(t *testing.T) {
			db := setupWildFlowBillingModelTest(t)
			require.NoError(t, db.AutoMigrate(&Task{}, &BillingReservationRecord{}))
			userID, tokenID, tokenKey := seedReservationFixture(t, 2000, 700)
			reserved, err := ReserveWalletBillingQuota("image-invalid", userID, tokenID, tokenKey, 500, false)
			require.NoError(t, err)
			require.True(t, reserved)
			require.NoError(t, MarkBillingReservationProviderStarted("image-invalid"))
			op, _, err := ReserveTaskOperation(&WildFlowOperation{
				OperationID: "invalid-acceptance", TaskID: "task_invalid-acceptance", UserID: userID, TokenID: tokenID,
				RequestID: "image-invalid", IdempotencyKeyDigest: strings.Repeat("a", 64),
				RequestDigest: strings.Repeat("b", 64), SubmissionLeaseExpiresAt: time.Now().Unix() + 60,
			})
			require.NoError(t, err)
			task := &Task{TaskID: op.TaskID, UserId: userID, Quota: 500, PrivateData: TaskPrivateData{TokenId: tokenID}}
			record := db.Model(&BillingReservationRecord{}).Where("request_id = ?", op.RequestID)
			switch scenario {
			case "missing":
				require.NoError(t, record.Delete(&BillingReservationRecord{}).Error)
			case "wrong_user":
				require.NoError(t, record.Update("user_id", userID+1).Error)
			case "wrong_token":
				task.PrivateData.TokenId++
			case "released":
				require.True(t, requireBillingReservationRelease(t, op.RequestID, tokenKey))
			case "settled":
				require.NoError(t, SettleBillingReservation(op.RequestID, 0))
			case "insufficient_token":
				task.Quota = 800
			}
			before := reservationUserQuota(t, userID)
			beforeRemain, beforeUsed := reservationTokenState(t, tokenID)
			require.Error(t, InsertTaskForOperation(op.OperationID, task))
			assert.Equal(t, before, reservationUserQuota(t, userID))
			remain, used := reservationTokenState(t, tokenID)
			assert.Equal(t, beforeRemain, remain)
			assert.Equal(t, beforeUsed, used)
			var count int64
			require.NoError(t, db.Model(&Task{}).Count(&count).Error)
			assert.Zero(t, count)
			require.NoError(t, db.First(op, op.ID).Error)
			assert.Equal(t, TaskOperationSubmitting, op.State)
		})
	}
}
