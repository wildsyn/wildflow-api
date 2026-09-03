package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/inferenceclient"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWildFlowJourneyReceiptControllerTest(t *testing.T) (*gin.Engine, *model.WildFlowOperation) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open("file:wildflow-journey-controller-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
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

	now := time.Now().UTC().Truncate(time.Millisecond)
	artifact := inferenceclient.Artifact{
		ID: "artifact-controller-1", JobID: "job-controller-1", MediaType: "application/json",
		SizeBytes: 128, SHA256: strings.Repeat("e", 64),
	}
	operation := &model.WildFlowOperation{
		OperationID: "op-controller-1", UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-controller-1", ProductModelRef: service.WildFlowModelExamDualASR,
		ModelVersionRef: "wildflow/exam-replay-dual-asr-v1", JobID: artifact.JobID, State: "succeeded",
		BillingState: model.WildFlowBillingStateSettled, BillingSource: model.WildFlowBillingSourceWallet,
		BillingQuota: 10, BillingTokenQuota: 10, BillingCurrency: "CNY", BillingAmountMicros: 1667,
		BillingUnit: "audio_millisecond", BillingBillableUnits: 1000, BillingQuotaPerUnit: "500000",
		BillingUSDExchangeRate: "7.3", BillingPriceVersion: "wildflow-retail-cny-v1",
		BillingUsageEventID: "usage-controller-1", BillingSettledTime: now.Add(-2 * time.Minute).Unix(),
		ResultValidatedTime: now.Add(-2 * time.Minute).Unix(), ResultExpiresAt: now.Add(time.Hour).Unix(),
		CreatedTime: now.Add(-5 * time.Minute).Unix(),
	}
	resultJSON, err := common.Marshal(service.WildFlowOperationResult(operation, []inferenceclient.Artifact{artifact}))
	require.NoError(t, err)
	operation.ResultJSON = string(resultJSON)
	require.NoError(t, database.Create(operation).Error)
	_, err = model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-controller-1", PayloadDigest: strings.Repeat("c", 64),
		OperationID: operation.OperationID, JobID: operation.JobID, AttemptID: "attempt-controller-1",
		ModelVersionRef: operation.ModelVersionRef, ChannelType: "gpu_agent", Kind: "audio_duration",
		Quantity: 1000, Unit: "millisecond", StartedAt: now.Add(-4 * time.Minute),
		EndedAt: now.Add(-3 * time.Minute), IngestedAt: now.Add(-2 * time.Minute),
		EvidenceRef: "artifact:" + artifact.ID,
	})
	require.NoError(t, err)
	_, _, err = model.RecordWildFlowArtifactDownloadReceipt(&model.WildFlowArtifactDownloadReceipt{
		OperationID: operation.OperationID, JobID: operation.JobID, ArtifactID: artifact.ID,
		UserID:            operation.UserID,
		TenantRefSHA256:   "ea3fd43be1e57d62e163dae19fc740bd6d660eec497235fd0ef859e2bd9fa328",
		ArtifactMediaType: artifact.MediaType, ArtifactSizeBytes: artifact.SizeBytes,
		ArtifactSHA256: artifact.SHA256, CompletedAt: now.Add(-time.Minute),
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/internal/v1/journey-receipts", middleware.DisableCache(), MaterializeWildFlowJourneyReceipt)
	engine.GET("/internal/v1/journey-receipts/:operation_id", middleware.DisableCache(), GetWildFlowJourneyReceipt)
	return engine, operation
}

func TestWildFlowJourneyReceiptInternalEndpointsAreDedicatedAuthenticatedAndDurable(t *testing.T) {
	engine, operation := setupWildFlowJourneyReceiptControllerTest(t)
	t.Setenv("WILDFLOW_JOURNEY_RECEIPT_TOKEN", strings.Repeat("r", 40))
	body := `{"operation_id":"op-controller-1","job_id":"job-controller-1","artifact_id":"artifact-controller-1"}`

	unauthenticated := httptest.NewRecorder()
	unauthenticatedRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/journey-receipts", bytes.NewBufferString(body))
	unauthenticatedRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(unauthenticated, unauthenticatedRequest)
	assert.Equal(t, http.StatusUnauthorized, unauthenticated.Code)

	wrongToken := httptest.NewRecorder()
	wrongTokenRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/journey-receipts", bytes.NewBufferString(body))
	wrongTokenRequest.Header.Set("Authorization", "Bearer "+strings.Repeat("w", 40))
	wrongTokenRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(wrongToken, wrongTokenRequest)
	assert.Equal(t, http.StatusUnauthorized, wrongToken.Code)

	request := httptest.NewRequest(http.MethodPost, "/internal/v1/journey-receipts", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	assert.Contains(t, response.Body.String(), `"public_journey_receipt"`)
	assert.Contains(t, response.Body.String(), `"public_journey_receipt_sha256"`)
	assert.Contains(t, response.Body.String(), `"billing_mode":"retail_audio_duration"`)
	assert.Contains(t, response.Body.String(), `"billing_state":"settled"`)
	assert.NotContains(t, response.Body.String(), "user:42")
	assert.NotContains(t, response.Body.String(), "user_id")

	require.NoError(t, model.DB.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Updates(map[string]any{"state": "recovery_required", "result_expires_at": time.Now().Add(-time.Hour).Unix()}).Error)
	readRequest := httptest.NewRequest(http.MethodGet, "/internal/v1/journey-receipts/"+operation.OperationID, nil)
	readRequest.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	readResponse := httptest.NewRecorder()
	engine.ServeHTTP(readResponse, readRequest)
	require.Equal(t, http.StatusOK, readResponse.Code, readResponse.Body.String())
	assert.Equal(t, response.Body.String(), readResponse.Body.String())

	invalidRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/journey-receipts", bytes.NewBufferString(strings.TrimSuffix(body, "}")+`,"receipt_created_at":"forged"}`))
	invalidRequest.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	engine.ServeHTTP(invalidResponse, invalidRequest)
	assert.Equal(t, http.StatusBadRequest, invalidResponse.Code)
}

func TestWildFlowJourneyReceiptEndpointFailsClosedWhenTokenIsUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/internal/v1/journey-receipts/:operation_id", GetWildFlowJourneyReceipt)
	t.Setenv("WILDFLOW_JOURNEY_RECEIPT_TOKEN", "")
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/journey-receipts/op-controller-1", nil)
	engine.ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
}
