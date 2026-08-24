package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestWildFlowMySQLJourneyReceiptMigrationAndConcurrency(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set TEST_MYSQL_DSN to run WildFlow MySQL compatibility tests")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(16)
	previousDB := DB
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	DB = database
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, previousLogDatabaseType)
	t.Cleanup(func() {
		DB = previousDB
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		_ = sqlDB.Close()
	})

	require.NoError(t, database.AutoMigrate(
		&WildFlowOperation{},
		&WildFlowUsageEvent{},
		&WildFlowArtifactDownloadReceipt{},
		&WildFlowPublicJourneyReceiptRecord{},
	))
	assert.True(t, database.Migrator().HasColumn(&WildFlowUsageEvent{}, "ingested_at"))
	assert.True(t, database.Migrator().HasIndex(
		&WildFlowArtifactDownloadReceipt{}, "idx_wildflow_download_operation_artifact",
	))
	assert.True(t, database.Migrator().HasIndex(
		&WildFlowPublicJourneyReceiptRecord{}, "idx_wildflow_public_journey_operation",
	))

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	operation := &WildFlowOperation{
		OperationID: "mysql-team-trial-" + suffix, UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: strings.Repeat("a", 56) + suffix,
		RequestDigest:        strings.Repeat("b", 64),
		RequestID:            "request-mysql-team-trial-" + suffix,
		ProductModelRef:      "wildflow/exam-replay-dual-asr-v1",
		ModelVersionRef:      "wildflow/exam-replay-dual-asr-v1",
		JobID:                "job-mysql-team-trial-" + suffix,
		State:                "succeeded",
		BillingState:         WildFlowBillingStatePending,
		BillingSource:        WildFlowBillingSourceTeamTrial,
	}
	require.NoError(t, database.Create(operation).Error)
	t.Cleanup(func() {
		_ = database.Where("operation_id = ?", operation.OperationID).Delete(&WildFlowPublicJourneyReceiptRecord{}).Error
		_ = database.Where("operation_id = ?", operation.OperationID).Delete(&WildFlowArtifactDownloadReceipt{}).Error
		_ = database.Where("operation_id = ?", operation.OperationID).Delete(&WildFlowUsageEvent{}).Error
		_ = database.Where("operation_id = ?", operation.OperationID).Delete(&WildFlowOperation{}).Error
	})

	const workers = 8
	usageErrors := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			startedAt := time.Now().UTC().Add(-time.Minute)
			_, usageErr := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
				EventID:       fmt.Sprintf("usage-mysql-team-%s-%d", suffix, index),
				PayloadDigest: strings.Repeat("c", 64), OperationID: operation.OperationID,
				JobID: operation.JobID, AttemptID: "attempt-mysql-team-" + suffix,
				ModelVersionRef: operation.ModelVersionRef, ChannelType: "gpu_agent",
				Kind: "audio_duration", Quantity: 1000, Unit: "millisecond",
				StartedAt: startedAt, EndedAt: startedAt.Add(time.Second),
				EvidenceRef: "artifact:artifact-mysql-team-" + suffix,
			})
			usageErrors <- usageErr
		}()
	}
	wait.Wait()
	close(usageErrors)
	usageSuccesses := 0
	for usageErr := range usageErrors {
		if usageErr == nil {
			usageSuccesses++
			continue
		}
		require.ErrorIs(t, usageErr, ErrWildFlowUsageEventConflict)
	}
	assert.Equal(t, 1, usageSuccesses)
	require.NoError(t, database.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.NotEmpty(t, operation.BillingUsageEventID)
	var usageCount int64
	require.NoError(t, database.Model(&WildFlowUsageEvent{}).
		Where("operation_id = ?", operation.OperationID).Count(&usageCount).Error)
	assert.Equal(t, int64(1), usageCount)

	artifactID := "artifact-mysql-team-" + suffix
	_, _, err = RecordWildFlowArtifactDownloadReceipt(&WildFlowArtifactDownloadReceipt{
		OperationID: operation.OperationID, JobID: operation.JobID, ArtifactID: artifactID,
		UserID: operation.UserID, TenantRefSHA256: strings.Repeat("d", 64),
		ArtifactMediaType: "application/json", ArtifactSizeBytes: 128,
		ArtifactSHA256: strings.Repeat("e", 64), CompletedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	receiptResults := make(chan *WildFlowPublicJourneyReceiptRecord, workers)
	receiptErrors := make(chan error, workers)
	wait = sync.WaitGroup{}
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			record, receiptErr := LoadOrCreateWildFlowPublicJourneyReceipt(
				context.Background(), operation.OperationID, operation.JobID, artifactID,
				func(*WildFlowJourneyEvidence) (*WildFlowJourneyReceiptMaterial, error) {
					payload := fmt.Sprintf("{\"candidate\":%d}\n", index)
					digest := sha256.Sum256([]byte(payload))
					return &WildFlowJourneyReceiptMaterial{
						ReceiptJSON: payload, ReceiptSHA256: fmt.Sprintf("%x", digest),
						ReceiptCreatedAt: time.Now().UTC(),
					}, nil
				},
			)
			receiptResults <- record
			receiptErrors <- receiptErr
		}()
	}
	wait.Wait()
	close(receiptResults)
	close(receiptErrors)
	for receiptErr := range receiptErrors {
		require.NoError(t, receiptErr)
	}
	winnerDigest := ""
	for record := range receiptResults {
		require.NotNil(t, record)
		if winnerDigest == "" {
			winnerDigest = record.ReceiptSHA256
		}
		assert.Equal(t, winnerDigest, record.ReceiptSHA256)
	}
	var receiptCount int64
	require.NoError(t, database.Model(&WildFlowPublicJourneyReceiptRecord{}).
		Where("operation_id = ?", operation.OperationID).Count(&receiptCount).Error)
	assert.Equal(t, int64(1), receiptCount)
	require.NoError(t, database.Model(&WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("state", "recovery_required").Error)
	replayedReceipt, err := LoadOrCreateWildFlowPublicJourneyReceipt(
		context.Background(), operation.OperationID, operation.JobID, artifactID,
		func(*WildFlowJourneyEvidence) (*WildFlowJourneyReceiptMaterial, error) {
			return nil, errors.New("durable receipt replay must not call the builder")
		},
	)
	require.NoError(t, err)
	assert.Equal(t, winnerDigest, replayedReceipt.ReceiptSHA256)
}
