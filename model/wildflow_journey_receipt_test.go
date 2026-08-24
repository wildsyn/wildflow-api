package model

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWildFlowJourneyReceiptModelTest(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(sqlite.Open("file:wildflow-journey-receipt-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&WildFlowOperation{},
		&WildFlowUsageEvent{},
		&WildFlowArtifactDownloadReceipt{},
		&WildFlowPublicJourneyReceiptRecord{},
	))
	previousDB := DB
	DB = database
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		DB = previousDB
		_ = sqlDB.Close()
	})
	return database
}

func TestRecordWildFlowArtifactDownloadReceiptIsImmutableAndIdempotent(t *testing.T) {
	database := setupWildFlowJourneyReceiptModelTest(t)
	completedAt := time.Date(2026, time.August, 25, 1, 2, 3, 456789000, time.UTC)
	receipt := &WildFlowArtifactDownloadReceipt{
		OperationID: "op-download-1", JobID: "job-download-1", ArtifactID: "artifact-download-1",
		UserID: 42, TenantRefSHA256: strings.Repeat("a", 64), ArtifactMediaType: "application/json",
		ArtifactSizeBytes: 128, ArtifactSHA256: strings.Repeat("b", 64), CompletedAt: completedAt,
	}

	created, replayed, err := RecordWildFlowArtifactDownloadReceipt(receipt)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Equal(t, completedAt.Truncate(time.Millisecond), created.CompletedAt)

	later := *receipt
	later.CompletedAt = completedAt.Add(time.Hour)
	persisted, replayed, err := RecordWildFlowArtifactDownloadReceipt(&later)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, completedAt.Truncate(time.Millisecond), persisted.CompletedAt)

	conflict := *receipt
	conflict.ArtifactSHA256 = strings.Repeat("c", 64)
	_, _, err = RecordWildFlowArtifactDownloadReceipt(&conflict)
	require.ErrorIs(t, err, ErrWildFlowJourneyEvidenceConflict)
	var count int64
	require.NoError(t, database.Model(&WildFlowArtifactDownloadReceipt{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestLoadWildFlowJourneyEvidenceUsesOneExactSnapshotAndRejectsAmbiguity(t *testing.T) {
	database := setupWildFlowJourneyReceiptModelTest(t)
	operation := &WildFlowOperation{
		OperationID: "op-evidence-1", UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-evidence-1", ProductModelRef: "wildflow/exam-replay-dual-asr-v1",
		ModelVersionRef: "wildflow/exam-replay-dual-asr-v1", JobID: "job-evidence-1", State: "succeeded",
		BillingState: WildFlowBillingStatePending, BillingSource: WildFlowBillingSourceTeamTrial,
		BillingUsageEventID: "usage-evidence-1", ResultJSON: `{"artifacts":[{"id":"artifact-evidence-1"}]}`,
		ResultValidatedTime: time.Now().Unix(), ResultExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, database.Create(operation).Error)
	require.NoError(t, database.Create(&WildFlowUsageEvent{
		EventID: "usage-evidence-1", PayloadDigest: strings.Repeat("c", 64), OperationID: operation.OperationID,
		JobID: operation.JobID, AttemptID: "attempt-evidence-1", ModelVersionRef: operation.ModelVersionRef,
		ChannelType: "gpu_agent", Kind: "audio_duration", Quantity: 1000, Unit: "millisecond",
		StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now().Add(-30 * time.Second),
		EvidenceRef: "artifact:artifact-evidence-1", IngestedAt: time.Now().UTC(),
	}).Error)
	require.NoError(t, database.Create(&WildFlowArtifactDownloadReceipt{
		OperationID: operation.OperationID, JobID: operation.JobID, ArtifactID: "artifact-evidence-1",
		UserID: 42, TenantRefSHA256: strings.Repeat("d", 64), ArtifactMediaType: "application/json",
		ArtifactSizeBytes: 128, ArtifactSHA256: strings.Repeat("e", 64), CompletedAt: time.Now().UTC(),
	}).Error)

	evidence, err := LoadWildFlowJourneyEvidence(
		context.Background(), operation.OperationID, operation.JobID, "artifact-evidence-1",
	)
	require.NoError(t, err)
	assert.Equal(t, operation.OperationID, evidence.Operation.OperationID)
	assert.Equal(t, "usage-evidence-1", evidence.UsageEvent.EventID)
	assert.Equal(t, "artifact-evidence-1", evidence.DownloadReceipt.ArtifactID)

	const workers = 8
	results := make(chan *WildFlowPublicJourneyReceiptRecord, workers)
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			record, receiptErr := LoadOrCreateWildFlowPublicJourneyReceipt(
				context.Background(), operation.OperationID, operation.JobID, "artifact-evidence-1",
				func(*WildFlowJourneyEvidence) (*WildFlowJourneyReceiptMaterial, error) {
					payload := fmt.Sprintf("{\"candidate\":%d}\n", index)
					digest := sha256.Sum256([]byte(payload))
					return &WildFlowJourneyReceiptMaterial{
						ReceiptJSON: payload, ReceiptSHA256: fmt.Sprintf("%x", digest),
						ReceiptCreatedAt: time.Now().UTC(),
					}, nil
				},
			)
			results <- record
			errorsByWorker <- receiptErr
		}()
	}
	wait.Wait()
	close(results)
	close(errorsByWorker)
	for receiptErr := range errorsByWorker {
		require.NoError(t, receiptErr)
	}
	winnerDigest := ""
	for record := range results {
		require.NotNil(t, record)
		if winnerDigest == "" {
			winnerDigest = record.ReceiptSHA256
		}
		assert.Equal(t, winnerDigest, record.ReceiptSHA256)
	}
	var receiptCount int64
	require.NoError(t, database.Model(&WildFlowPublicJourneyReceiptRecord{}).Count(&receiptCount).Error)
	assert.Equal(t, int64(1), receiptCount)

	require.NoError(t, database.Create(&WildFlowUsageEvent{
		EventID: "usage-evidence-2", PayloadDigest: strings.Repeat("f", 64), OperationID: operation.OperationID,
		JobID: operation.JobID, AttemptID: "attempt-evidence-1", ModelVersionRef: operation.ModelVersionRef,
		ChannelType: "gpu_agent", Kind: "audio_duration", Quantity: 1000, Unit: "millisecond",
		StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now().Add(-30 * time.Second),
		EvidenceRef: "artifact:artifact-evidence-1", IngestedAt: time.Now().UTC(),
	}).Error)
	_, err = LoadWildFlowJourneyEvidence(
		context.Background(), operation.OperationID, operation.JobID, "artifact-evidence-1",
	)
	require.ErrorIs(t, err, ErrWildFlowJourneyEvidenceConflict)
}

func TestRecordWildFlowUsageEventBindsExactlyOneTeamTrialEvent(t *testing.T) {
	database := setupWildFlowJourneyReceiptModelTest(t)
	operation := &WildFlowOperation{
		OperationID: "op-team-usage-1", UserID: 42,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-team-usage-1", ProductModelRef: "wildflow/exam-replay-dual-asr-v1",
		ModelVersionRef: "wildflow/exam-replay-dual-asr-v1", JobID: "job-team-usage-1", State: "succeeded",
		BillingState: WildFlowBillingStatePending, BillingSource: WildFlowBillingSourceTeamTrial,
	}
	require.NoError(t, database.Create(operation).Error)
	startedAt := time.Now().UTC().Add(-time.Minute)
	event := &WildFlowUsageEvent{
		EventID: "usage-team-1", PayloadDigest: strings.Repeat("c", 64), OperationID: operation.OperationID,
		JobID: operation.JobID, AttemptID: "attempt-team-1", ModelVersionRef: operation.ModelVersionRef,
		ChannelType: "gpu_agent", Kind: "audio_duration", Quantity: 1000, Unit: "millisecond",
		StartedAt: startedAt, EndedAt: startedAt.Add(30 * time.Second), EvidenceRef: "artifact:artifact-team-1",
	}
	replayed, err := RecordWildFlowUsageEvent(event)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.False(t, event.IngestedAt.IsZero())
	assert.Equal(t, event.IngestedAt.Truncate(time.Millisecond), event.IngestedAt)
	require.NoError(t, database.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, event.EventID, operation.BillingUsageEventID)

	conflict := *event
	conflict.EventID = "usage-team-2"
	conflict.PayloadDigest = strings.Repeat("d", 64)
	_, err = RecordWildFlowUsageEvent(&conflict)
	require.ErrorIs(t, err, ErrWildFlowUsageEventConflict)
	var count int64
	require.NoError(t, database.Model(&WildFlowUsageEvent{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestRecordWildFlowUsageEventConcurrentTeamTrialBindingKeepsOneEvent(t *testing.T) {
	database := setupWildFlowJourneyReceiptModelTest(t)
	operation := &WildFlowOperation{
		OperationID: "op-team-usage-concurrent", UserID: 42,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-team-usage-concurrent", ProductModelRef: "wildflow/exam-replay-dual-asr-v1",
		ModelVersionRef: "wildflow/exam-replay-dual-asr-v1", JobID: "job-team-usage-concurrent",
		State: "succeeded", BillingState: WildFlowBillingStatePending,
		BillingSource: WildFlowBillingSourceTeamTrial,
	}
	require.NoError(t, database.Create(operation).Error)
	const workers = 8
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			startedAt := time.Now().UTC().Add(-time.Minute)
			_, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
				EventID:       fmt.Sprintf("usage-team-concurrent-%d", index),
				PayloadDigest: strings.Repeat("c", 64), OperationID: operation.OperationID,
				JobID: operation.JobID, AttemptID: "attempt-team-concurrent",
				ModelVersionRef: operation.ModelVersionRef, ChannelType: "gpu_agent",
				Kind: "audio_duration", Quantity: 1000, Unit: "millisecond",
				StartedAt: startedAt, EndedAt: startedAt.Add(time.Second),
				EvidenceRef: "artifact:artifact-team-concurrent",
			})
			errorsByWorker <- err
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	successes := 0
	for err := range errorsByWorker {
		if err == nil {
			successes++
			continue
		}
		require.ErrorIs(t, err, ErrWildFlowUsageEventConflict)
	}
	assert.Equal(t, 1, successes)
	require.NoError(t, database.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.NotEmpty(t, operation.BillingUsageEventID)
	var count int64
	require.NoError(t, database.Model(&WildFlowUsageEvent{}).
		Where("operation_id = ?", operation.OperationID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
