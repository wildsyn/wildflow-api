package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWildFlowJourneyReceiptServiceTest(t *testing.T) (*gorm.DB, *model.WildFlowOperation, time.Time) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open("file:wildflow-journey-service-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.WildFlowOperation{},
		&model.WildFlowUsageEvent{},
		&model.WildFlowArtifactDownloadReceipt{},
		&model.WildFlowPublicJourneyReceiptRecord{},
	))
	previousDB := model.DB
	model.DB = database
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		model.DB = previousDB
		_ = sqlDB.Close()
	})

	operationCreatedAt := time.Date(2026, time.August, 25, 1, 0, 0, 0, time.UTC)
	usageStartedAt := operationCreatedAt.Add(time.Minute)
	usageEndedAt := usageStartedAt.Add(2 * time.Minute)
	usageIngestedAt := usageEndedAt.Add(time.Second)
	downloadedAt := usageIngestedAt.Add(time.Second)
	artifact := inferenceclient.Artifact{
		ID: "artifact-journey-1", JobID: "job-journey-1", MediaType: "application/json",
		SizeBytes: 128, SHA256: strings.Repeat("e", 64), Metadata: map[string]any{},
	}
	operation := &model.WildFlowOperation{
		OperationID: "op-journey-1", UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-journey-1", ProductModelRef: WildFlowModelExamDualASR,
		ModelVersionRef: wildFlowModelVersionDualASR, JobID: artifact.JobID, State: "succeeded",
		BillingState: model.WildFlowBillingStateSettled, BillingSource: model.WildFlowBillingSourceWallet,
		BillingQuota: 1096, BillingTokenQuota: 1096, BillingCurrency: "CNY",
		BillingAmountMicros: 200000, BillingUnit: "audio_millisecond", BillingBillableUnits: 120000,
		BillingQuotaPerUnit: "500000", BillingUSDExchangeRate: "7.3", BillingPriceVersion: wildFlowRetailPriceVersion,
		BillingUsageEventID: "usage-journey-1", BillingSettledTime: usageIngestedAt.Unix(),
		ResultValidatedTime: usageIngestedAt.Unix(),
		ResultExpiresAt:     downloadedAt.Add(time.Hour).Unix(), CreatedTime: operationCreatedAt.Unix(),
	}
	resultJSON, err := common.Marshal(WildFlowOperationResult(operation, []inferenceclient.Artifact{artifact}))
	require.NoError(t, err)
	operation.ResultJSON = string(resultJSON)
	require.NoError(t, database.Create(operation).Error)
	_, err = model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-journey-1", PayloadDigest: strings.Repeat("c", 64),
		OperationID: operation.OperationID, JobID: operation.JobID, AttemptID: "attempt-journey-1",
		ModelVersionRef: operation.ModelVersionRef, ChannelType: "gpu_agent", Kind: "audio_duration",
		Quantity: 120000, Unit: "millisecond", StartedAt: usageStartedAt, EndedAt: usageEndedAt,
		EvidenceRef: "artifact:" + artifact.ID, IngestedAt: usageIngestedAt,
	})
	require.NoError(t, err)
	tenantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("user:42")))
	require.NoError(t, database.Create(&model.WildFlowArtifactDownloadReceipt{
		OperationID: operation.OperationID, JobID: operation.JobID, ArtifactID: artifact.ID,
		UserID: operation.UserID, TenantRefSHA256: tenantDigest, ArtifactMediaType: artifact.MediaType,
		ArtifactSizeBytes: artifact.SizeBytes, ArtifactSHA256: artifact.SHA256, CompletedAt: downloadedAt,
	}).Error)
	return database, operation, downloadedAt.Add(time.Second)
}

func TestMaterializeWildFlowPublicJourneyReceiptBindsDurableRetailEvidence(t *testing.T) {
	_, operation, receiptCreatedAt := setupWildFlowJourneyReceiptServiceTest(t)

	envelope, err := MaterializeWildFlowPublicJourneyReceipt(
		context.Background(),
		operation.OperationID,
		operation.JobID,
		"artifact-journey-1",
		receiptCreatedAt,
	)
	require.NoError(t, err)
	receipt := envelope.Receipt
	assert.Equal(t, "public_journey_succeeded", receipt.State)
	assert.Equal(t, "retail_audio_duration", receipt.BillingMode)
	assert.Equal(t, model.WildFlowBillingStateSettled, receipt.BillingState)
	assert.Equal(t, WildFlowModelExamDualASR, receipt.PublicModelRef)
	assert.Equal(t, "exam-replay-dual-asr", receipt.RuntimeOfferingRef)
	assert.Equal(t, "usage-journey-1", receipt.UsageEventID)
	assert.Equal(t, strings.Repeat("c", 64), receipt.UsagePayloadDigest)
	assert.Equal(t, "artifact-journey-1", receipt.ArtifactID)
	assert.Equal(t, int64(128), receipt.ArtifactSizeBytes)
	assert.Equal(t, strings.Repeat("e", 64), receipt.ArtifactSHA256)
	assert.Equal(t, receiptCreatedAt.Format(time.RFC3339Nano), receipt.ReceiptCreatedAt)
	require.Len(t, envelope.ReceiptSHA256, 64)
	canonical, err := canonicalWildFlowPublicJourneyReceipt(receipt)
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), canonical[len(canonical)-1])
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(canonical)), envelope.ReceiptSHA256)
	var persisted model.WildFlowPublicJourneyReceiptRecord
	require.NoError(t, model.DB.Where("operation_id = ?", operation.OperationID).First(&persisted).Error)
	assert.Equal(t, string(canonical), persisted.ReceiptJSON)
	assert.Equal(t, envelope.ReceiptSHA256, persisted.ReceiptSHA256)

	require.NoError(t, model.DB.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("result_expires_at", receiptCreatedAt.Add(-time.Second).Unix()).Error)
	replayed, err := MaterializeWildFlowPublicJourneyReceipt(
		context.Background(), operation.OperationID, operation.JobID, "artifact-journey-1", receiptCreatedAt.Add(time.Hour),
	)
	require.NoError(t, err)
	assert.Equal(t, envelope, replayed)
}

func TestGetWildFlowPublicJourneyReceiptRejectsARehashedSemanticMutation(t *testing.T) {
	_, operation, receiptCreatedAt := setupWildFlowJourneyReceiptServiceTest(t)
	envelope, err := MaterializeWildFlowPublicJourneyReceipt(
		context.Background(), operation.OperationID, operation.JobID, "artifact-journey-1", receiptCreatedAt,
	)
	require.NoError(t, err)

	tampered := envelope.Receipt
	tampered.BillingMode = "team_trial_no_charge"
	canonical, err := canonicalWildFlowPublicJourneyReceipt(tampered)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.WildFlowPublicJourneyReceiptRecord{}).
		Where("operation_id = ?", operation.OperationID).
		Updates(map[string]any{
			"receipt_json":   string(canonical),
			"receipt_sha256": fmt.Sprintf("%x", sha256.Sum256(canonical)),
		}).Error)

	_, err = GetWildFlowPublicJourneyReceipt(context.Background(), operation.OperationID)
	require.ErrorIs(t, err, model.ErrWildFlowJourneyEvidenceConflict)
}

func TestMaterializeWildFlowPublicJourneyReceiptRejectsInvalidRetailOrConflictingEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gorm.DB, *model.WildFlowOperation, time.Time)
	}{
		{
			name: "unsettled billing state",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(operation).Update("billing_state", model.WildFlowBillingStatePending).Error)
			},
		},
		{
			name: "trial billing source",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(operation).
					Update("billing_source", model.WildFlowBillingSourceTeamTrial).Error)
			},
		},
		{
			name: "retail subscription",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(operation).Update("billing_subscription_id", 1).Error)
			},
		},
		{
			name: "missing retail amount",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(operation).Update("billing_amount_micros", 0).Error)
			},
		},
		{
			name: "retail pricing metadata",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(operation).Updates(map[string]any{
					"billing_currency":      "CNY",
					"billing_unit":          "minute",
					"billing_price_version": "retail-v1",
				}).Error)
			},
		},
		{
			name: "download tenant mismatch",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(&model.WildFlowArtifactDownloadReceipt{}).
					Where("operation_id = ?", operation.OperationID).
					Update("tenant_ref_sha256", strings.Repeat("f", 64)).Error)
			},
		},
		{
			name: "result expired",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, receiptCreatedAt time.Time) {
				require.NoError(t, database.Model(operation).Update("result_expires_at", receiptCreatedAt.Add(-time.Second).Unix()).Error)
			},
		},
		{
			name: "legacy usage without millisecond ingestion evidence",
			mutate: func(database *gorm.DB, operation *model.WildFlowOperation, _ time.Time) {
				require.NoError(t, database.Model(&model.WildFlowUsageEvent{}).
					Where("operation_id = ?", operation.OperationID).
					Update("ingested_at", time.Time{}).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, operation, receiptCreatedAt := setupWildFlowJourneyReceiptServiceTest(t)
			test.mutate(database, operation, receiptCreatedAt)
			_, err := MaterializeWildFlowPublicJourneyReceipt(
				context.Background(), operation.OperationID, operation.JobID, "artifact-journey-1", receiptCreatedAt,
			)
			require.ErrorIs(t, err, model.ErrWildFlowJourneyEvidenceConflict)
		})
	}
}
