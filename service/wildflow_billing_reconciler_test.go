package service

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func setupWildFlowBillingReconcilerTest(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	db, err := gorm.Open(sqlite.Open("file:wildflow-reconciler-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.WildFlowOperation{}))
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
	return reserved
}

func TestReconcileWildFlowBillingFinalizesJobsWithoutUserPolling(t *testing.T) {
	db := setupWildFlowBillingReconcilerTest(t)
	succeeded := createReconcilerOperation(t, db, "succeeded", 101, 201, "job-succeeded")
	failed := createReconcilerOperation(t, db, "failed", 102, 202, "job-failed")
	missingArtifact := createReconcilerOperation(t, db, "missing-artifact", 103, 203, "job-missing-artifact")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/v1/jobs/job-succeeded":
			_, _ = w.Write([]byte(`{"job":{"id":"job-succeeded","state":"succeeded","artifacts":[{"id":"artifact-1","job_id":"job-succeeded","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		case "/internal/v1/jobs/job-failed":
			_, _ = w.Write([]byte(`{"job":{"id":"job-failed","state":"failed","last_error":"provider failed"}}`))
		case "/internal/v1/jobs/job-missing-artifact":
			_, _ = w.Write([]byte(`{"job":{"id":"job-missing-artifact","state":"succeeded"}}`))
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
	assert.Equal(t, 3, processed)

	require.NoError(t, db.Where("operation_id = ?", succeeded.OperationID).First(succeeded).Error)
	require.NoError(t, db.Where("operation_id = ?", failed.OperationID).First(failed).Error)
	require.NoError(t, db.Where("operation_id = ?", missingArtifact.OperationID).First(missingArtifact).Error)
	assert.Equal(t, model.WildFlowBillingStateSettled, succeeded.BillingState)
	assert.Equal(t, model.WildFlowBillingStateRefunded, failed.BillingState)
	assert.Equal(t, "recovery_required", missingArtifact.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, missingArtifact.BillingState)

	var failedUser model.User
	require.NoError(t, db.First(&failedUser, 102).Error)
	assert.Equal(t, 100_000, failedUser.Quota)
}
