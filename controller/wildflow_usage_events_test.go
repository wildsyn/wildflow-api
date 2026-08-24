package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestValidUsageEventAcceptsPositiveAudioDuration(t *testing.T) {
	startedAt := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	event := wildFlowUsageEventEnvelope{
		EventID:       "usage-asr-1",
		AggregateType: "job",
		AggregateID:   "job-asr-1",
		EventType:     "usage.recorded.v1",
		Payload: wildFlowUsageEventPayload{
			UsageEventID:    "usage-asr-1",
			OperationID:     "operation-asr-1",
			JobID:           "job-asr-1",
			AttemptID:       "attempt-asr-1",
			ModelVersionRef: "wildflow/exam-replay-dual-asr-v1",
			ChannelType:     "gpu_agent",
			Kind:            "audio_duration",
			Quantity:        7_376,
			Unit:            "millisecond",
			StartedAt:       startedAt,
			EndedAt:         startedAt.Add(90 * time.Second),
			EvidenceRef:     "artifact:artifact-asr-1",
		},
	}

	assert.True(t, validUsageEvent(event))
	event.Payload.Quantity = 0
	assert.False(t, validUsageEvent(event))
	event.Payload.Quantity = 7_376
	event.Payload.Unit = "second"
	assert.False(t, validUsageEvent(event))
}

func TestReceiveWildFlowUsageEventPersistsGoCanonicalDigestGolden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open("file:wildflow-usage-golden?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.WildFlowOperation{},
		&model.WildFlowUsageEvent{},
		&model.WildFlowBillingLogEntry{},
	))
	previousDB := model.DB
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })

	modelVersionRef := "vendor/模型:<alpha>&\"quoted\"\\v1\u2028tail"
	require.NoError(t, database.Create(&model.WildFlowOperation{
		OperationID:          "op-golden-20260825",
		UserID:               1,
		IdempotencyKeyDigest: strings.Repeat("a", 64),
		RequestDigest:        strings.Repeat("b", 64),
		RequestID:            "request-golden-20260825",
		ProductModelRef:      "golden-model",
		ModelVersionRef:      modelVersionRef,
		JobID:                "job-golden-20260825",
		State:                "succeeded",
		BillingState:         model.WildFlowBillingStatePending,
	}).Error)

	t.Setenv("WILDFLOW_USAGE_EVENT_TOKEN", strings.Repeat("t", 40))
	engine := gin.New()
	engine.POST("/internal/v1/usage-events", ReceiveWildFlowUsageEvent)
	body := `{
		"payload":{
			"evidence_ref":"artifact:音频/<take>&\"quote\"\\path\u2029end",
			"ended_at":"2026-08-25T09:10:12.123456789+08:00",
			"started_at":"2026-08-25T09:10:11.12+08:00",
			"unit":"millisecond","quantity":12345,"kind":"audio_duration",
			"channel_type":"gpu_agent",
			"model_version_ref":"vendor/模型:<alpha>&\"quoted\"\\v1\u2028tail",
			"attempt_id":"attempt-golden-1","job_id":"job-golden-20260825",
			"operation_id":"op-golden-20260825","usage_event_id":"usage-golden-20260825"
		},
		"event_type":"usage.recorded.v1","aggregate_id":"job-golden-20260825",
		"aggregate_type":"job","event_id":"usage-golden-20260825"
	}`
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/usage-events", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 40))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "usage-golden-20260825")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

	var persisted model.WildFlowUsageEvent
	require.NoError(t, database.Where("event_id = ?", "usage-golden-20260825").First(&persisted).Error)
	assert.Equal(t, modelVersionRef, persisted.ModelVersionRef)
	assert.Equal(
		t,
		"ef75c4055f64819a146c360e219e175f543b2fad9b00f26a830790f6b7366787",
		persisted.PayloadDigest,
	)
}

func TestReceiveWildFlowUsageEventIsAuthenticatedImmutableAndIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open("file:wildflow-usage-events?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(
		&model.User{}, &model.Token{}, &model.Log{}, &model.WildFlowOperation{},
		&model.WildFlowUsageEvent{}, &model.WildFlowBillingLogEntry{}, &model.WildFlowBillingLogProjectionReceipt{},
	))
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	model.DB = database
	model.LOG_DB = database
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
	})
	require.NoError(t, database.Create(&model.User{Id: 1, Username: "usage-event-user", Quota: 10_000}).Error)
	require.NoError(t, database.Create(&model.WildFlowOperation{
		OperationID: "operation-1", UserID: 1, TokenID: 1,
		IdempotencyKeyDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64),
		RequestID: "request-1", ProductModelRef: "VoxCPM2", ModelVersionRef: "openbmb/VoxCPM2",
		JobID: "job-1", State: "succeeded", ResultJSON: `{"id":"operation-1","state":"succeeded"}`,
		ResultValidatedTime: time.Now().Unix(), ResultExpiresAt: time.Now().Add(time.Hour).Unix(),
		BillingState: model.WildFlowBillingStateReserved, BillingSource: model.WildFlowBillingSourceWallet,
		BillingQuota: 100, BillingCurrency: "CNY", BillingAmountMicros: 160,
		BillingUnit: "10k_characters", BillingBillableUnits: 2,
		BillingQuotaPerUnit: "500000", BillingUSDExchangeRate: "7.3", BillingPriceVersion: "test-v1",
	}).Error)

	t.Setenv("WILDFLOW_USAGE_EVENT_TOKEN", strings.Repeat("t", 40))
	engine := gin.New()
	engine.POST("/internal/v1/usage-events", ReceiveWildFlowUsageEvent)
	body := `{
		"event_id":"usage-1","aggregate_type":"job","aggregate_id":"job-1",
		"event_type":"usage.recorded.v1","payload":{
			"usage_event_id":"usage-1","operation_id":"operation-1","job_id":"job-1",
			"attempt_id":"attempt-1","model_version_ref":"openbmb/VoxCPM2",
			"channel_type":"provider_connector","kind":"characters","quantity":2,"unit":"character",
			"started_at":"2026-08-19T00:00:00Z","ended_at":"2026-08-19T00:00:01Z",
			"evidence_ref":"artifact:artifact-1"
		}
	}`

	perform := func(payload string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/usage-events", bytes.NewBufferString(payload))
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 40))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "usage-1")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}

	first := perform(body)
	assert.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	replay := perform(body)
	assert.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	assert.Equal(t, "true", replay.Header().Get("X-WildFlow-Event-Replayed"))

	conflictBody := strings.Replace(body, `"quantity":2`, `"quantity":3`, 1)
	conflict := perform(conflictBody)
	assert.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	var count int64
	require.NoError(t, database.Model(&model.WildFlowUsageEvent{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
	var operation model.WildFlowOperation
	require.NoError(t, database.Where("operation_id = ?", "operation-1").First(&operation).Error)
	assert.Equal(t, model.WildFlowBillingStateSettled, operation.BillingState)
	assert.Equal(t, "usage-1", operation.BillingUsageEventID)
	var user model.User
	require.NoError(t, database.First(&user, 1).Error)
	assert.Equal(t, 100, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
}

func TestReceiveWildFlowUsageEventRejectsMissingIdentityBeforeDatabaseAccess(t *testing.T) {
	previousDB := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = previousDB })
	t.Setenv("WILDFLOW_USAGE_EVENT_TOKEN", strings.Repeat("t", 40))
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodPost, "/internal/v1/usage-events", bytes.NewBufferString(`{}`))
	ReceiveWildFlowUsageEvent(context)
	assert.Equal(t, http.StatusUnauthorized, context.Writer.Status())
}
