package model

import (
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
