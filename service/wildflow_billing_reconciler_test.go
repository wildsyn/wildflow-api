package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestStartWildFlowBillingReconcilerFailsClosedWithoutAuthorityOrConfiguration(t *testing.T) {
	previousMasterNode := common.IsMasterNode
	t.Cleanup(func() {
		common.IsMasterNode = previousMasterNode
		wildFlowBillingReconcilerOnce = sync.Once{}
	})
	wildFlowBillingReconcilerOnce = sync.Once{}
	common.IsMasterNode = false
	StartWildFlowBillingReconciler()
	assert.False(t, wildFlowBillingReconcilerRunning.Load())

	wildFlowBillingReconcilerOnce = sync.Once{}
	common.IsMasterNode = true
	t.Setenv("WILDFLOW_INFERENCE_URL", "")
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "")
	StartWildFlowBillingReconciler()
	assert.False(t, wildFlowBillingReconcilerRunning.Load())
}

func TestStartWildFlowBillingReconcilerProcessesReservedJobsWhenConfigured(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	operation := createReconcilerOperation(t, db, "startup", 111, 211, "job-startup")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":{"id":"job-startup","state":"succeeded","artifacts":[{"id":"artifact-startup","job_id":"job-startup","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
	}))
	t.Cleanup(server.Close)
	previousMasterNode := common.IsMasterNode
	t.Cleanup(func() {
		common.IsMasterNode = previousMasterNode
		wildFlowBillingReconcilerOnce = sync.Once{}
	})
	common.IsMasterNode = true
	wildFlowBillingReconcilerOnce = sync.Once{}
	t.Setenv("WILDFLOW_INFERENCE_URL", server.URL)
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "internal-token")
	t.Setenv("WILDFLOW_INFERENCE_ALLOW_INTERNAL_HTTP", "true")
	t.Setenv("WILDFLOW_BILLING_RECONCILE_SECONDS", "300")

	StartWildFlowBillingReconciler()
	require.Eventually(t, func() bool {
		if err := db.Where("operation_id = ?", operation.OperationID).First(operation).Error; err != nil {
			return false
		}
		return operation.BillingState == model.WildFlowBillingStateSettled
	}, time.Second, 10*time.Millisecond)
}

func setupWildFlowBillingReconcilerTest(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	db, err := gorm.Open(sqlite.Open("file:wildflow-reconciler-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{}, &model.Token{}, &model.Log{}, &model.WildFlowOperation{},
		&model.WildFlowUsageEvent{}, &model.WildFlowBillingLogEntry{},
	))
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func createReconcilerOperation(t *testing.T, db *gorm.DB, suffix string, userID int, tokenID int, jobID string) *model.WildFlowOperation {
	t.Helper()
	user := &model.User{Id: userID, Username: "reconciler-" + suffix, Quota: 100_000, Group: "default", AffCode: "aff-" + suffix}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{Id: tokenID, UserId: userID, Key: "token-" + suffix, RemainQuota: 100_000}
	require.NoError(t, db.Create(token).Error)
	operation := &model.WildFlowOperation{
		OperationID:          "op-" + suffix,
		UserID:               userID,
		TokenID:              tokenID,
		IdempotencyKeyDigest: "key-" + suffix,
		RequestDigest:        "request-" + suffix,
		RequestID:            "request-id-" + suffix,
		ProductModelRef:      WildFlowModelFlux2,
		ModelVersionRef:      "black-forest-labs/FLUX.2-klein-4B",
		JobID:                jobID,
		State:                "queued",
	}
	require.NoError(t, db.Create(operation).Error)
	quote, err := QuoteWildFlowBilling(WildFlowJobRequest{
		Model:      WildFlowModelFlux2,
		Parameters: map[string]any{"prompt": "一只熊猫"},
	})
	require.NoError(t, err)
	reserved, err := model.ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	_, err = model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-" + suffix, PayloadDigest: "usage-digest-" + suffix,
		OperationID: operation.OperationID, JobID: jobID,
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	require.NoError(t, err)
	return reserved
}

func createVoxReconcilerOperation(t *testing.T, db *gorm.DB, suffix string, userID int, tokenID int, jobID string, input string) *model.WildFlowOperation {
	t.Helper()
	user := &model.User{Id: userID, Username: "reconciler-" + suffix, Quota: 100_000, Group: "default", AffCode: "aff-" + suffix}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{Id: tokenID, UserId: userID, Key: "token-" + suffix, RemainQuota: 100_000}
	require.NoError(t, db.Create(token).Error)
	operation := &model.WildFlowOperation{
		OperationID:          "op-" + suffix,
		UserID:               userID,
		TokenID:              tokenID,
		IdempotencyKeyDigest: "key-" + suffix,
		RequestDigest:        "request-" + suffix,
		RequestID:            "request-id-" + suffix,
		ProductModelRef:      WildFlowModelVoxCPM2,
		ModelVersionRef:      "openbmb/VoxCPM2",
		JobID:                jobID,
		State:                "queued",
	}
	require.NoError(t, db.Create(operation).Error)
	quote, err := QuoteWildFlowBilling(WildFlowJobRequest{
		Model: WildFlowModelVoxCPM2,
		Parameters: map[string]any{
			"input": input,
			"voice": "default",
		},
	})
	require.NoError(t, err)
	reserved, err := model.ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	_, err = model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-" + suffix, PayloadDigest: "usage-digest-" + suffix,
		OperationID: operation.OperationID, JobID: jobID,
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "characters", Quantity: int64(len([]rune(input))), Unit: "character",
	})
	require.NoError(t, err)
	return reserved
}

func TestReconcileWildFlowBillingFinalizesJobsWithoutUserPolling(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	succeeded := createReconcilerOperation(t, db, "succeeded", 101, 201, "job-succeeded")
	failed := createReconcilerOperation(t, db, "failed", 102, 202, "job-failed")
	missingArtifact := createReconcilerOperation(t, db, "missing-artifact", 103, 203, "job-missing-artifact")
	billingConflict := createReconcilerOperation(t, db, "billing-conflict", 105, 205, "job-billing-conflict")
	require.NoError(t, db.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", billingConflict.OperationID).
		Updates(map[string]any{
			"billing_state":  model.WildFlowBillingStateRefunding,
			"billing_source": model.WildFlowBillingSourceSubscription,
		}).Error)
	billingConflict.BillingState = model.WildFlowBillingStateRefunding
	billingConflict.BillingSource = model.WildFlowBillingSourceSubscription

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/jobs/job-succeeded":
			_, _ = w.Write([]byte(`{"job":{"id":"job-succeeded","state":"succeeded","artifacts":[{"id":"artifact-1","job_id":"job-succeeded","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		case "/internal/v1/jobs/job-failed":
			_, _ = w.Write([]byte(`{"job":{"id":"job-failed","state":"failed","last_error":"provider failed"}}`))
		case "/internal/v1/jobs/job-missing-artifact":
			_, _ = w.Write([]byte(`{"job":{"id":"job-missing-artifact","state":"succeeded"}}`))
		case "/internal/v1/jobs/job-billing-conflict":
			_, _ = w.Write([]byte(`{"job":{"id":"job-billing-conflict","state":"succeeded","artifacts":[{"id":"artifact-conflict","job_id":"job-billing-conflict","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL:           server.URL,
		Token:             "internal-token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	require.NoError(t, err)

	processed, err := ReconcileWildFlowBillingOnce(context.Background(), client, 100)
	require.NoError(t, err)
	assert.Equal(t, 4, processed)

	require.NoError(t, db.Where("operation_id = ?", succeeded.OperationID).First(succeeded).Error)
	require.NoError(t, db.Where("operation_id = ?", failed.OperationID).First(failed).Error)
	require.NoError(t, db.Where("operation_id = ?", missingArtifact.OperationID).First(missingArtifact).Error)
	require.NoError(t, db.Where("operation_id = ?", billingConflict.OperationID).First(billingConflict).Error)
	assert.Equal(t, model.WildFlowBillingStateSettled, succeeded.BillingState)
	assert.Equal(t, model.WildFlowBillingStateRefunded, failed.BillingState)
	assert.Equal(t, "recovery_required", missingArtifact.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, missingArtifact.BillingState)
	assert.Equal(t, "recovery_required", billingConflict.State)
	assert.Equal(t, model.WildFlowBillingStateRefunding, billingConflict.BillingState)

	var failedUser model.User
	require.NoError(t, db.First(&failedUser, 102).Error)
	assert.Equal(t, 100_000, failedUser.Quota)
}

func TestReconcileWildFlowBillingKeepsReservationOnTransientInferenceFailure(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	operation := createReconcilerOperation(t, db, "transient", 104, 204, "job-transient")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL:           server.URL,
		Token:             "internal-token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	require.NoError(t, err)

	processed, err := ReconcileWildFlowBillingOnce(context.Background(), client, 100)
	require.Error(t, err)
	assert.Zero(t, processed)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, "queued", operation.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
}

func TestReconcileWildFlowBillingNeverCommitsTerminalBillingWithoutCanonicalAudit(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	succeeded := createReconcilerOperation(t, db, "audit-failure-succeeded", 121, 221, "job-audit-failure-succeeded")
	failed := createReconcilerOperation(t, db, "audit-failure-failed", 122, 222, "job-audit-failure-failed")
	forcedErr := errors.New("forced canonical audit insert failure")
	callbackName := "test:wildflow-reconciler-canonical-failure:" + uuid.NewString()
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "wild_flow_billing_log_entries" {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/jobs/job-audit-failure-succeeded":
			_, _ = w.Write([]byte(`{"job":{"id":"job-audit-failure-succeeded","state":"succeeded","artifacts":[{"id":"artifact-audit-failure","job_id":"job-audit-failure-succeeded","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		case "/internal/v1/jobs/job-audit-failure-failed":
			_, _ = w.Write([]byte(`{"job":{"id":"job-audit-failure-failed","state":"failed","last_error":"provider failed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL: server.URL, Token: "internal-token", Timeout: time.Second, AllowInternalHTTP: true,
	})
	require.NoError(t, err)

	processed, err := ReconcileWildFlowBillingOnce(context.Background(), client, 100)
	require.Error(t, err)
	assert.Equal(t, 2, processed)
	require.NoError(t, db.Where("operation_id = ?", succeeded.OperationID).First(succeeded).Error)
	require.NoError(t, db.Where("operation_id = ?", failed.OperationID).First(failed).Error)
	assert.Equal(t, model.WildFlowBillingStateReserved, succeeded.BillingState)
	assert.Equal(t, model.WildFlowBillingStateReserved, failed.BillingState)
	var succeededUser model.User
	var failedUser model.User
	require.NoError(t, db.First(&succeededUser, succeeded.UserID).Error)
	require.NoError(t, db.First(&failedUser, failed.UserID).Error)
	assert.Zero(t, succeededUser.UsedQuota)
	assert.Zero(t, succeededUser.RequestCount)
	assert.Equal(t, 100_000-3_425, failedUser.Quota)
	var canonicalCount int64
	require.NoError(t, db.Model(&model.WildFlowBillingLogEntry{}).Count(&canonicalCount).Error)
	assert.Zero(t, canonicalCount)
}

func TestReconcileWildFlowBillingRequiresCompleteVoxMP3Artifact(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	operation := createVoxReconcilerOperation(t, db, "invalid-vox", 106, 206, "job-invalid-vox", "hello")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":{"id":"job-invalid-vox","state":"succeeded","artifacts":[{"id":"artifact-invalid-vox","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":4,"segment_count":1,"completed_segment_count":1,"size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}]}}`))
	}))
	t.Cleanup(server.Close)
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL:           server.URL,
		Token:             "internal-token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	require.NoError(t, err)

	processed, err := ReconcileWildFlowBillingOnce(context.Background(), client, 100)

	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, "recovery_required", operation.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, int64(5), operation.BillingBillableUnits)
}

func TestReconcileWildFlowBillingRevisitsReservedRecoveryOperations(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	repaired := createVoxReconcilerOperation(t, db, "repaired-vox", 107, 207, "job-repaired-vox", "hello")
	failed := createReconcilerOperation(t, db, "recovery-failed", 108, 208, "job-recovery-failed")
	for _, operation := range []*model.WildFlowOperation{repaired, failed} {
		require.NoError(t, model.UpdateWildFlowOperationExecution(operation.OperationID, operation.JobID, "recovery_required", "invalid_artifact"))
		operation.State = "recovery_required"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/jobs/job-repaired-vox":
			const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			_, _ = fmt.Fprintf(w, `{"job":{"id":"job-repaired-vox","state":"succeeded","artifacts":[{"id":"artifact-repaired-vox","job_id":"job-repaired-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":%q,"metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":5,"segment_count":1,"completed_segment_count":1,"size_bytes":12,"sha256":%q}}]}}`, digest, digest)
		case "/internal/v1/jobs/job-recovery-failed":
			_, _ = w.Write([]byte(`{"job":{"id":"job-recovery-failed","state":"failed","last_error":"provider failed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := inferenceclient.New(inferenceclient.Config{
		BaseURL:           server.URL,
		Token:             "internal-token",
		Timeout:           time.Second,
		AllowInternalHTTP: true,
	})
	require.NoError(t, err)

	processed, err := ReconcileWildFlowBillingOnce(context.Background(), client, 100)

	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	require.NoError(t, db.Where("operation_id = ?", repaired.OperationID).First(repaired).Error)
	require.NoError(t, db.Where("operation_id = ?", failed.OperationID).First(failed).Error)
	assert.Equal(t, "succeeded", repaired.State)
	assert.Equal(t, model.WildFlowBillingStateSettled, repaired.BillingState)
	assert.Equal(t, "failed", failed.State)
	assert.Equal(t, model.WildFlowBillingStateRefunded, failed.BillingState)
	var failedUser model.User
	require.NoError(t, db.First(&failedUser, failed.UserID).Error)
	assert.Equal(t, 100_000, failedUser.Quota)
}

func TestReconcileWildFlowBillingRejectsNilClient(t *testing.T) {
	processed, err := ReconcileWildFlowBillingOnce(context.Background(), nil, 100)
	require.Error(t, err)
	assert.Zero(t, processed)
}
