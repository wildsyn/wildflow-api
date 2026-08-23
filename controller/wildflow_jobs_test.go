package controller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWildFlowJobsControllerTest(t *testing.T, inference http.Handler) (*gin.Engine, *httptest.Server) {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	dsn := "file:wildflow-jobs-" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.SubscriptionPlan{},
		&model.UserSubscription{},
		&model.SubscriptionPreConsumeRecord{},
		&model.WildFlowOperation{},
		&model.WildFlowUsageEvent{},
		&model.WildFlowBillingLogEntry{},
	))
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.Create(&model.User{
		Id:       42,
		Username: "wildflow-billing-user",
		Quota:    1_000_000,
		Group:    "default",
	}).Error)
	require.NoError(t, db.Create(&model.Token{
		Id:          7,
		UserId:      42,
		Key:         "wildflow-test-token",
		Name:        "wildflow-test-token",
		RemainQuota: 1_000_000,
	}).Error)
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	server := httptest.NewServer(inference)
	t.Cleanup(server.Close)
	t.Setenv("WILDFLOW_INFERENCE_URL", server.URL)
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "internal-token")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		userID := 42
		if raw := c.GetHeader("X-Test-User"); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			require.NoError(t, parseErr)
			userID = parsed
		}
		c.Set("id", userID)
		c.Set("token_id", 7)
		c.Set(common.RequestIdKey, "request-public-1")
		if raw := c.GetHeader("X-Test-Model-Limits"); raw != "" {
			limits := make(map[string]bool)
			for _, modelName := range strings.Split(raw, ",") {
				limits[modelName] = true
			}
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, limits)
		}
		c.Next()
	})
	engine.POST("/v1/jobs", CreateWildFlowJob)
	engine.POST("/v1/input-artifacts", CreateWildFlowInputArtifact)
	engine.POST("/api/v1/audio/speech", CreateWildFlowLegacySpeechJob)
	engine.POST("/api/v1/images/generations", CreateWildFlowLegacyImageJob)
	engine.GET("/v1/jobs/:operation_id", GetWildFlowJob)
	engine.POST("/v1/jobs/:operation_id/cancel", CancelWildFlowJob)
	engine.GET("/v1/artifacts/:artifact_id", GetWildFlowArtifact)
	engine.GET("/v1/artifacts/:artifact_id/content", DownloadWildFlowArtifact)
	return engine, server
}

func TestCreateWildFlowInputArtifactAllowsStandardRegisteredUserTokenAndStreamsFLAC(t *testing.T) {
	requests := 0
	payload := []byte("fLaCcontrolled-audio")
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "/internal/v1/input-artifacts", r.URL.Path)
		require.Equal(t, "Bearer internal-token", r.Header.Get("Authorization"))
		require.Equal(t, "user:42", r.Header.Get("X-WildFlow-Tenant-Ref"))
		require.Equal(t, "audio/flac", r.Header.Get("Content-Type"))
		require.Equal(t, digest, r.Header.Get("X-WildFlow-Content-SHA256"))
		actual, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, actual)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"artifact":{"id":"input-1","media_type":"audio/flac","size_bytes":20,"sha256":"` + digest + `","retention_state":"active"}}`))
	}))

	response := performWildFlowBytesRequest(t, engine, http.MethodPost, "/v1/input-artifacts", payload, map[string]string{
		"Content-Type": "audio/flac", "X-WildFlow-Content-SHA256": digest,
	})
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	assert.Equal(t, 1, requests)
	assert.NotContains(t, response.Body.String(), "object_key")
}

func TestCreateWildFlowInputArtifactRejectsInvalidHeadersBeforeInference(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	headers := map[string]string{}

	wrongType := performWildFlowBytesRequest(t, engine, http.MethodPost, "/v1/input-artifacts", []byte("fLaCdata"), headers)
	require.Equal(t, http.StatusUnsupportedMediaType, wrongType.Code, wrongType.Body.String())

	headers["Content-Type"] = "audio/flac"
	headers["X-WildFlow-Content-SHA256"] = "not-a-digest"
	wrongDigest := performWildFlowBytesRequest(t, engine, http.MethodPost, "/v1/input-artifacts", []byte("fLaCdata"), headers)
	require.Equal(t, http.StatusBadRequest, wrongDigest.Code, wrongDigest.Body.String())
	assert.Zero(t, requests)
}

func TestCreateInternalExamDualASRJobAllowsStandardRegisteredUserTokenAndRemainsHiddenAndUnbilled(t *testing.T) {
	submissions := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/jobs", r.URL.Path)
		submissions++
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, service.WildFlowModelExamDualASR, body["product_model_ref"])
		assert.Equal(t, service.WildFlowModelExamDualASR, body["model_version_ref"])
		assert.Equal(t, []any{"input-1"}, body["input_artifact_ids"])
		deadline, err := time.Parse(time.RFC3339Nano, body["deadline_at"].(string))
		require.NoError(t, err)
		assert.GreaterOrEqual(t, time.Until(deadline), 5*time.Hour)
		assert.LessOrEqual(t, time.Until(deadline), 6*time.Hour+time.Minute)
		_, hasDescriptors := body["inputs"]
		assert.False(t, hasDescriptors)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-asr-1","state":"queued"}}`))
	}))
	body := `{"model":"wildflow/exam-replay-dual-asr-v1","input_artifact_ids":["input-1"],"parameters":{"language":"zh","context":"申论课程","hotwords":["青蜂六边形"]}}`

	response := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{
		"Idempotency-Key": "asr-standard-token",
	})
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.Equal(t, 1, submissions)
	var user model.User
	var token model.Token
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(&token, 7).Error)
	require.NoError(t, model.DB.Where("operation_id = ?", response.Header().Get("Location")[len("/v1/jobs/"):]).First(&operation).Error)
	assert.Equal(t, 1_000_000, user.Quota)
	assert.Equal(t, 1_000_000, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStatePending, operation.BillingState)
}

func TestInternalExamDualASRJSONArtifactIsDownloadableWhileUnbilled(t *testing.T) {
	content := []byte(`{"schema_version":1}`)
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	metadata := fmt.Sprintf(`{"schema_version":1,"model_version_ref":%q,"model_revision":"d0c9efdb8d614685062c04425d91e01b6f37d944_edaa852ec7e145841d8ffdb056a99866b5f0a478","vibevoice_model_revision":"d0c9efdb8d614685062c04425d91e01b6f37d944","faster_whisper_model_revision":"edaa852ec7e145841d8ffdb056a99866b5f0a478","runtime_version_ref":"exam-dual-asr-runtime-v1-a09e48e-94da20d","duration_seconds":120,"source_artifact_id":"input-1"}`, service.WildFlowModelExamDualASR)
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/artifacts/artifact-asr":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"artifact":{"id":"artifact-asr","job_id":"job-asr","media_type":"application/json","size_bytes":%d,"sha256":%q,"metadata":%s}}`, len(content), digest, metadata)
		case "/internal/v1/artifacts/artifact-asr/content":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	require.NoError(t, model.DB.Create(&model.WildFlowOperation{
		OperationID: "op-asr-download", UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: "key-asr-download", RequestDigest: "request-asr-download",
		RequestID: "request-asr-download", ProductModelRef: service.WildFlowModelExamDualASR,
		ModelVersionRef: service.WildFlowModelExamDualASR, JobID: "job-asr", State: "succeeded",
		BillingState: model.WildFlowBillingStatePending,
	}).Error)

	response := performWildFlowRequest(t, engine, http.MethodGet, "/v1/artifacts/artifact-asr/content", "", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
	assert.Contains(t, response.Header().Get("Content-Disposition"), "artifact-asr.json")
	assert.Equal(t, content, response.Body.Bytes())
}

func TestInternalExamDualASROperationReadAllowsStandardRegisteredUserToken(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	require.NoError(t, model.DB.Create(&model.WildFlowOperation{
		OperationID: "op-asr-read", UserID: 42, TokenID: 7,
		IdempotencyKeyDigest: "key-asr-read", RequestDigest: "request-asr-read",
		RequestID: "request-asr-read", ProductModelRef: service.WildFlowModelExamDualASR,
		ModelVersionRef: service.WildFlowModelExamDualASR, State: "recovery_required",
		BillingState: model.WildFlowBillingStatePending,
	}).Error)

	response := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/op-asr-read", "", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Zero(t, requests)
}

func validVoxArtifactJSON(artifactID string, jobID string, characters int) string {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return fmt.Sprintf(
		`{"id":%q,"job_id":%q,"media_type":"audio/mpeg","size_bytes":12,"sha256":%q,"metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":%d,"completed_characters":%d,"segment_count":1,"completed_segment_count":1,"size_bytes":12,"sha256":%q,"voice":"default"}}`,
		artifactID,
		jobID,
		digest,
		characters,
		characters,
		digest,
	)
}

func TestCreateWildFlowJobPreConsumesRetailPriceExactlyOnce(t *testing.T) {
	submissions := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"job":{"id":"job-billed-image","state":"queued"}}`))
			return
		}
		submissions++
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-billed-image","state":"queued"}}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`

	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "billed-image"})
	replayed := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "billed-image"})

	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	require.Equal(t, http.StatusAccepted, replayed.Code, replayed.Body.String())
	assert.Equal(t, 1, submissions)
	var user model.User
	var token model.Token
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(&token, 7).Error)
	require.NoError(t, model.DB.Where("job_id = ?", "job-billed-image").First(&operation).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
	assert.Equal(t, 1_000_000-3_425, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, int64(50_000), operation.BillingAmountMicros)
}

func TestCreateWildFlowJobRejectsInsufficientQuotaBeforeInference(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 42).Update("quota", 1).Error)

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "insufficient-quota"},
	)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"insufficient_quota"`)
	assert.Equal(t, 0, requests)
}

func TestCreateWildFlowJobDoesNotReserveQuotaWhenInferenceIsNotConfigured(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Setenv("WILDFLOW_INFERENCE_URL", "")

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "inference-not-configured"},
	)

	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	var user model.User
	var token model.Token
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(&token, 7).Error)
	require.NoError(t, model.DB.Where("user_id = ?", 42).First(&operation).Error)
	assert.Equal(t, 1_000_000, user.Quota)
	assert.Equal(t, 1_000_000, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStatePending, operation.BillingState)
}

func createWildFlowControllerSubscription(t *testing.T, planID int, amountTotal int64, allowWalletOverflow bool) *model.UserSubscription {
	t.Helper()
	plan := &model.SubscriptionPlan{
		Id:                  planID,
		Title:               "WildFlow controller billing plan",
		Currency:            "CNY",
		DurationUnit:        model.SubscriptionDurationMonth,
		DurationValue:       1,
		Enabled:             true,
		AllowWalletOverflow: &allowWalletOverflow,
		TotalAmount:         amountTotal,
		QuotaResetPeriod:    model.SubscriptionResetNever,
	}
	require.NoError(t, model.DB.Create(plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	subscription := &model.UserSubscription{
		UserId:              42,
		PlanId:              plan.Id,
		AmountTotal:         amountTotal,
		StartTime:           time.Now().Add(-time.Hour).Unix(),
		EndTime:             time.Now().Add(time.Hour).Unix(),
		Status:              "active",
		AllowWalletOverflow: allowWalletOverflow,
	}
	require.NoError(t, model.DB.Create(subscription).Error)
	return subscription
}

func TestCreateWildFlowJobUsesActiveSubscriptionBeforeWallet(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-subscription-first","state":"queued"}}`))
	}))
	subscription := createWildFlowControllerSubscription(t, 810_001, 100_000, false)

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "subscription-first"},
	)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

	var user model.User
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(subscription, subscription.Id).Error)
	require.NoError(t, model.DB.Where("job_id = ?", "job-subscription-first").First(&operation).Error)
	assert.Equal(t, 1_000_000, user.Quota)
	assert.Equal(t, int64(3_425), subscription.AmountUsed)
	assert.Equal(t, model.WildFlowBillingSourceSubscription, operation.BillingSource)
}

func TestCreateWildFlowJobFallsBackToWalletWhenSubscriptionAllowsOverflow(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-wallet-overflow","state":"queued"}}`))
	}))
	subscription := createWildFlowControllerSubscription(t, 810_002, 1, true)

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "wallet-overflow"},
	)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())

	var user model.User
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(subscription, subscription.Id).Error)
	require.NoError(t, model.DB.Where("job_id = ?", "job-wallet-overflow").First(&operation).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, model.WildFlowBillingSourceWallet, operation.BillingSource)
}

func TestFailedWildFlowJobRefundsRetailPriceExactlyOnce(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-billed-failed","state":"queued"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"job":{"id":"job-billed-failed","state":"failed","last_error":"provider failed"}}`))
	}))
	created := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "billed-failed"},
	)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)

	first := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	second := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())

	var user model.User
	var token model.Token
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(&token, 7).Error)
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&operation).Error)
	assert.Equal(t, 1_000_000, user.Quota)
	assert.Equal(t, 1_000_000, token.RemainQuota)
	assert.Equal(t, model.WildFlowBillingStateRefunded, operation.BillingState)
}

func TestSucceededWildFlowJobSettlesRetailPriceExactlyOnce(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-billed-succeeded","state":"queued"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"job":{"id":"job-billed-succeeded","state":"succeeded","artifacts":[{"id":"artifact-billed","job_id":"job-billed-succeeded","media_type":"image/png","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
	}))
	created := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "billed-succeeded"},
	)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)
	replayed, err := model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-billed-succeeded", PayloadDigest: strings.Repeat("c", 64),
		OperationID: operationID, JobID: "job-billed-succeeded",
		ModelVersionRef: "black-forest-labs/FLUX.2-klein-4B",
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	require.NoError(t, err)
	assert.False(t, replayed)

	first := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	second := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())

	var user model.User
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&operation).Error)
	assert.Equal(t, 3_425, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, model.WildFlowBillingStateSettled, operation.BillingState)
	assert.Equal(t, "usage-billed-succeeded", operation.BillingUsageEventID)
	var consumeLogs int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).
		Where("user_id = ? AND type = ? AND request_id = ?", 42, model.LogTypeConsume, operationID).
		Count(&consumeLogs).Error)
	assert.Equal(t, int64(1), consumeLogs)
}

func TestRecoveryRequiredWildFlowJobKeepsReservation(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-billed-recovery","state":"queued"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"job":{"id":"job-billed-recovery","state":"recovery_required","last_error":"result ownership unknown"}}`))
	}))
	created := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "billed-recovery"},
	)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)

	status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())

	var user model.User
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&operation).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
}

func TestSucceededWildFlowJobWithoutArtifactEntersRecoveryRequired(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-missing-result","state":"queued"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"job":{"id":"job-missing-result","state":"succeeded"}}`))
	}))
	created := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`,
		map[string]string{"Idempotency-Key": "missing-result"},
	)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)

	status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, status.Code, status.Body.String())
	assert.Equal(t, "5", status.Header().Get("Retry-After"))
	assert.Contains(t, status.Body.String(), `"code":"recovery_required"`)

	var operation model.WildFlowOperation
	var user model.User
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&operation).Error)
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, "recovery_required", operation.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
}

func TestSucceededVoxCPM2JobRejectsIncompleteArtifactsAndKeepsReservation(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
	}{
		{
			name:     "WAV is not a public success artifact",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/wav","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		},
		{
			name:     "required MP3 metadata is missing",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		},
		{
			name:     "completed characters do not cover billed input",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":4,"segment_count":1,"completed_segment_count":1,"size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
		{
			name:     "completed segment count is inconsistent",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":5,"segment_count":2,"completed_segment_count":1,"size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
		{
			name:     "metadata size does not match artifact",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":5,"segment_count":1,"completed_segment_count":1,"size_bytes":11,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		},
		{
			name:     "metadata digest does not match artifact",
			artifact: `{"id":"artifact-invalid","job_id":"job-invalid-vox","media_type":"audio/mpeg","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","metadata":{"codec":"mp3","bitrate":96000,"sample_rate":48000,"channels":1,"duration_ms":1200,"input_characters":5,"completed_characters":5,"segment_count":1,"completed_segment_count":1,"size_bytes":12,"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"job":{"id":"job-invalid-vox","state":"queued"}}`))
					return
				}
				_, _ = fmt.Fprintf(w, `{"job":{"id":"job-invalid-vox","state":"succeeded","artifacts":[%s]}}`, test.artifact)
			}))
			created := performWildFlowRequest(
				t,
				engine,
				http.MethodPost,
				"/v1/jobs",
				`{"model":"VoxCPM2","parameters":{"input":"hello","voice":"default"}}`,
				map[string]string{"Idempotency-Key": "invalid-vox-artifact"},
			)
			require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
			var payload map[string]any
			require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))

			status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+payload["id"].(string), "", nil)

			require.Equal(t, http.StatusServiceUnavailable, status.Code, status.Body.String())
			assert.Contains(t, status.Body.String(), `"code":"recovery_required"`)
			var operation model.WildFlowOperation
			require.NoError(t, model.DB.Where("operation_id = ?", payload["id"].(string)).First(&operation).Error)
			assert.Equal(t, "recovery_required", operation.State)
			assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
			assert.Equal(t, int64(5), operation.BillingBillableUnits)
		})
	}
}

func TestCreateWildFlowJobEnforcesTokenModelLimitsBeforeInference(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-model-limit","state":"queued"}}`))
	}))
	body := `{"model":"VoxCPM2","parameters":{"input":"hello","voice":"default"}}`

	forbidden := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		body,
		map[string]string{
			"Idempotency-Key":     "model-limit-forbidden",
			"X-Test-Model-Limits": "DeepSeek-V4-Pro",
		},
	)

	require.Equal(t, http.StatusForbidden, forbidden.Code, forbidden.Body.String())
	assert.Contains(t, forbidden.Body.String(), `"code":"model_forbidden"`)
	assert.Equal(t, 0, requests)

	allowed := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		body,
		map[string]string{
			"Idempotency-Key":     "model-limit-allowed",
			"X-Test-Model-Limits": "VoxCPM2",
		},
	)

	require.Equal(t, http.StatusAccepted, allowed.Code, allowed.Body.String())
	assert.Equal(t, 1, requests)
}

func TestLegacyWildCloudSpeechRequestMapsToCanonicalVoxCPM2Job(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "VoxCPM2", body["product_model_ref"])
		parameters := body["parameters"].(map[string]any)
		assert.Equal(t, "wangliqun", parameters["voice"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-legacy-tts","state":"queued"}}`))
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/api/v1/audio/speech",
		`{"model":"tts-voxcpm2","input":"兼容旧版野生云"}`,
		map[string]string{"X-Idempotency-Key": "legacy-tts"},
	)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"model":"VoxCPM2"`)
	assert.NotEmpty(t, response.Header().Get("Location"))
	assert.Equal(t, "job-legacy-tts", response.Header().Get("X-Job-ID"))
}

func TestLegacyWildCloudImageRequestDefaultsToCanonicalFluxModel(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "FLUX.2 [klein] 4B", body["product_model_ref"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-legacy-image","state":"queued"}}`))
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/api/v1/images/generations",
		`{"prompt":"一只熊猫","width":1024,"height":1024}`,
		map[string]string{"X-Idempotency-Key": "legacy-image"},
	)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"model":"FLUX.2 [klein] 4B"`)
}

func TestCompatibilityDecoderPassesUnrelatedModelsToNormalChannelRouting(t *testing.T) {
	_, recognized, err := decodeWildFlowLegacySpeechRequest(
		strings.NewReader(`{"model":"other-tts","input":"hello","instructions":"calm"}`),
		false,
	)
	require.NoError(t, err)
	assert.False(t, recognized)

	_, recognized, err = decodeWildFlowLegacyImageRequest(
		strings.NewReader(`{"model":"other-image","prompt":"panda","n":1}`),
		false,
	)
	require.NoError(t, err)
	assert.False(t, recognized)
}

func performWildFlowRequest(
	t *testing.T,
	engine *gin.Engine,
	method string,
	path string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func performWildFlowBytesRequest(
	t *testing.T,
	engine *gin.Engine,
	method string,
	path string,
	body []byte,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func TestCreateWildFlowJobSubmitsTTSOnceAndReplaysTheOperation(t *testing.T) {
	submissions := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer internal-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-tts-1" {
			_, _ = w.Write([]byte(`{"job":{"id":"job-tts-1","state":"queued"}}`))
			return
		}
		require.Equal(t, "/internal/v1/jobs", r.URL.Path)
		submissions++
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "VoxCPM2", body["product_model_ref"])
		assert.Equal(t, "openbmb/VoxCPM2", body["model_version_ref"])
		assert.Equal(t, "user:42", body["tenant_ref"])
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-tts-1","state":"queued"}}`))
	}))
	body := `{"model":"tts-standard","parameters":{"input":"你好，野生流动","voice":"default"}}`

	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "tts-1"})
	replayed := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "tts-1"})
	conflict := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", `{"model":"tts-standard","parameters":{"input":"different"}}`, map[string]string{"Idempotency-Key": "tts-1"})

	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	require.Equal(t, http.StatusAccepted, replayed.Code, replayed.Body.String())
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Equal(t, created.Header().Get("Location"), replayed.Header().Get("Location"))
	assert.Equal(t, "5", replayed.Header().Get("Retry-After"))
	assert.Equal(t, 1, submissions)
	var first, second map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &first))
	require.NoError(t, common.Unmarshal(replayed.Body.Bytes(), &second))
	assert.Equal(t, first["id"], second["id"])
	assert.Equal(t, second["id"], second["operation_id"])
	assert.Equal(t, "job-tts-1", first["job_id"])
	assert.Equal(t, "queued", first["state"])
}

func TestConcurrentSameKeySubmitsAndReservesExactlyOnce(t *testing.T) {
	var submissions atomic.Int32
	submitted := make(chan struct{})
	release := make(chan struct{})
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if submissions.Add(1) == 1 {
			close(submitted)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-concurrent-key","state":"queued"}}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	firstResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstResponse <- performWildFlowRequest(
			t, engine, http.MethodPost, "/v1/jobs", body,
			map[string]string{"Idempotency-Key": "concurrent-key"},
		)
	}()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		require.FailNow(t, "first provider submission did not start")
	}

	replay := performWildFlowRequest(
		t, engine, http.MethodPost, "/v1/jobs", body,
		map[string]string{"Idempotency-Key": "concurrent-key"},
	)
	close(release)
	created := <-firstResponse
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	require.Equal(t, http.StatusAccepted, replay.Code, replay.Body.String())
	assert.Equal(t, "5", replay.Header().Get("Retry-After"))
	assert.Equal(t, int32(1), submissions.Load())
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
}

func TestCreateWildFlowJobMapsFLUXToTheExactModelVersion(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "FLUX.2 [klein] 4B", body["product_model_ref"])
		assert.Equal(t, "black-forest-labs/FLUX.2-klein-4B", body["model_version_ref"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-image-1","state":"queued"}}`))
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"flux2-klein-4b","parameters":{"prompt":"一只熊猫","width":1024,"height":1024}}`,
		map[string]string{"Idempotency-Key": "image-1"},
	)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
}

func TestCreateWildFlowJobRejectsUnknownTopLevelFieldsBeforeInference(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"tts-standard","parameters":{"input":"hello"},"callback_url":"https://example.com"}`,
		map[string]string{"Idempotency-Key": "unknown-field"},
	)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, 0, requests)
}

func TestCreateWildFlowJobRequiresVoiceForTheCanonicalVoxCPM2Model(t *testing.T) {
	requests := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"VoxCPM2","parameters":{"input":"hello"}}`,
		map[string]string{"Idempotency-Key": "missing-voice"},
	)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, 0, requests)
}

func TestCreateWildFlowJobPreservesRetryAfterFromInference(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"temporarily unavailable"}`))
	}))

	response := performWildFlowRequest(
		t,
		engine,
		http.MethodPost,
		"/v1/jobs",
		`{"model":"tts-standard","parameters":{"input":"hello"}}`,
		map[string]string{"Idempotency-Key": "retry-after"},
	)

	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "17", response.Header().Get("Retry-After"))
}

func TestCreateWildFlowJobRecoversSameKeyAfterInferenceConfigurationAppears(t *testing.T) {
	var submissions atomic.Int32
	engine, server := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		submissions.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-config-recovered","state":"queued"}}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	headers := map[string]string{"Idempotency-Key": "config-recovers"}
	t.Setenv("WILDFLOW_INFERENCE_URL", "")

	missingConfig := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	require.Equal(t, http.StatusServiceUnavailable, missingConfig.Code, missingConfig.Body.String())
	assert.Equal(t, int32(0), submissions.Load())
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000, user.Quota)

	t.Setenv("WILDFLOW_INFERENCE_URL", server.URL)
	recovered := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	replayed := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)

	require.Equal(t, http.StatusAccepted, recovered.Code, recovered.Body.String())
	require.Equal(t, http.StatusAccepted, replayed.Code, replayed.Body.String())
	assert.Equal(t, recovered.Header().Get("Location"), replayed.Header().Get("Location"))
	assert.Equal(t, int32(1), submissions.Load())
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
}

func TestRetryableInferenceSubmissionReusesOperationWithoutDoubleReserve(t *testing.T) {
	var submissions atomic.Int32
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := submissions.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"detail":"admission unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-after-retryable","state":"queued"}}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	headers := map[string]string{"Idempotency-Key": "retryable-reserve-once"}

	first := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	recovered := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)

	require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())
	assert.Equal(t, "9", first.Header().Get("Retry-After"))
	require.Equal(t, http.StatusAccepted, recovered.Code, recovered.Body.String())
	assert.Equal(t, int32(2), submissions.Load())
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
	var operation model.WildFlowOperation
	require.NoError(t, model.DB.Where("idempotency_key_digest <> ?", "").First(&operation).Error)
	assert.Equal(t, "job-after-retryable", operation.JobID)
	assert.Equal(t, 2, operation.SubmissionAttempt)
}

func TestRetryableSubmissionWithoutClientReplayExpiresAndRefundsReservation(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"admission unavailable"}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	response := performWildFlowRequest(
		t, engine, http.MethodPost, "/v1/jobs", body,
		map[string]string{"Idempotency-Key": "retry-window-refund"},
	)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

	var operation model.WildFlowOperation
	require.NoError(t, model.DB.Where("idempotency_key_digest <> ?", "").First(&operation).Error)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, model.WildFlowSubmissionPhaseRetryable, operation.SubmissionPhase)
	assert.Greater(t, operation.SubmissionRetryUntil, time.Now().Unix())
	require.NoError(t, model.DB.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", operation.OperationID).
		Update("submission_retry_until", time.Now().Add(-time.Minute).Unix()).Error)

	processed, err := service.ReconcileWildFlowSubmissionLeasesOnce(time.Now().Unix(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	require.NoError(t, model.DB.Where("operation_id = ?", operation.OperationID).First(&operation).Error)
	assert.Equal(t, "failed", operation.State)
	assert.Equal(t, "submission_retry_expired", operation.LastErrorCode)
	assert.Equal(t, model.WildFlowSubmissionPhaseFailed, operation.SubmissionPhase)
	assert.Equal(t, model.WildFlowBillingStateRefunded, operation.BillingState)
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000, user.Quota)
	var canonicalCount int64
	require.NoError(t, model.DB.Model(&model.WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", operation.OperationID, model.LogTypeRefund).
		Count(&canonicalCount).Error)
	assert.Equal(t, int64(1), canonicalCount)
}

func TestConcurrentRetryableReplaysHaveOneSubmissionLeaseOwner(t *testing.T) {
	var submissions atomic.Int32
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := submissions.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if attempt == 2 {
			close(secondStarted)
			<-releaseSecond
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"job-single-lease-owner","state":"queued"}}`))
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	headers := map[string]string{"Idempotency-Key": "single-lease-owner"}
	first := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())

	recoveredResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recoveredResponse <- performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "retryable submission owner did not start")
	}

	const replayWorkers = 6
	var wait sync.WaitGroup
	replays := make(chan *httptest.ResponseRecorder, replayWorkers)
	for index := 0; index < replayWorkers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			replays <- performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
		}()
	}
	wait.Wait()
	close(replays)
	for replay := range replays {
		require.Equal(t, http.StatusAccepted, replay.Code, replay.Body.String())
		assert.Equal(t, "5", replay.Header().Get("Retry-After"))
	}
	close(releaseSecond)
	recovered := <-recoveredResponse
	require.Equal(t, http.StatusAccepted, recovered.Code, recovered.Body.String())
	assert.Equal(t, int32(2), submissions.Load())
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota)
}

func TestLegacySucceededGETUsesStickyResultUnavailableRecovery(t *testing.T) {
	var submissions atomic.Int32
	var jobReads atomic.Int32
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			submissions.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-legacy-get","state":"queued"}}`))
			return
		}
		jobReads.Add(1)
		http.NotFound(w, r)
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	headers := map[string]string{"Idempotency-Key": "legacy-get-recovery"}
	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)
	require.NoError(t, model.UpdateWildFlowOperationExecution(operationID, "job-legacy-get", "succeeded", ""))

	firstGET := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	secondGET := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	replayPOST := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, headers)
	hidden := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", map[string]string{"X-Test-User": "43"})

	for _, response := range []*httptest.ResponseRecorder{firstGET, secondGET, replayPOST} {
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"state":"recovery_required"`)
		assert.Contains(t, response.Body.String(), `"error":"result_unavailable"`)
	}
	require.Equal(t, http.StatusNotFound, hidden.Code, hidden.Body.String())
	assert.Equal(t, int32(1), submissions.Load())
	assert.Equal(t, int32(1), jobReads.Load(), "sticky recovery must not read inference twice")
}

func TestLegacySubmittingGETReconcilesUnknownSubmissionBeforeResponse(t *testing.T) {
	var inferenceRequests atomic.Int32
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		inferenceRequests.Add(1)
	}))
	operation := &model.WildFlowOperation{
		OperationID:          "op-legacy-submitting-get",
		UserID:               42,
		TokenID:              7,
		IdempotencyKeyDigest: "legacy-submitting-key",
		RequestDigest:        "legacy-submitting-request",
		RequestID:            "legacy-submitting-request-id",
		ProductModelRef:      service.WildFlowModelFlux2,
		ModelVersionRef:      "black-forest-labs/FLUX.2-klein-4B",
		State:                "submitting",
		BillingState:         model.WildFlowBillingStateReserved,
		BillingSource:        model.WildFlowBillingSourceWallet,
		BillingQuota:         3_425,
	}
	require.NoError(t, model.DB.Create(operation).Error)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 42).Update("quota", 1_000_000-3_425).Error)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 7).Update("remain_quota", 1_000_000-3_425).Error)

	hidden := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operation.OperationID, "", map[string]string{"X-Test-User": "43"})
	require.Equal(t, http.StatusNotFound, hidden.Code, hidden.Body.String())
	response := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operation.OperationID, "", nil)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"state":"recovery_required"`)
	assert.Contains(t, response.Body.String(), `"error":"legacy_submission_state_unknown"`)
	assert.Zero(t, inferenceRequests.Load(), "blank-job local recovery must not contact inference")
	require.NoError(t, model.DB.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, "recovery_required", operation.State)
	assert.Equal(t, model.WildFlowBillingStateReserved, operation.BillingState)
	var user model.User
	var token model.Token
	require.NoError(t, model.DB.First(&user, 42).Error)
	require.NoError(t, model.DB.First(&token, 7).Error)
	assert.Equal(t, 1_000_000-3_425, user.Quota, "unknown provider side effects must keep the reservation")
	assert.Equal(t, 1_000_000-3_425, token.RemainQuota)
}

func TestWildFlowJobStatusAndArtifactDownloadRemainUserScoped(t *testing.T) {
	jobReads := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "user:42", r.Header.Get("X-WildFlow-Tenant-Ref"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/jobs":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"queued"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-1":
			jobReads++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"job":{"id":"job-1","state":"succeeded","artifacts":[%s]}}`, validVoxArtifactJSON("artifact-1", "job-1", 5))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"artifact":%s}`, validVoxArtifactJSON("artifact-1", "job-1", 5))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-1/content":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Disposition", `attachment; filename="artifact-1.mp3"`)
			_, _ = io.Copy(w, bytes.NewBufferString("audio-result"))
		default:
			http.NotFound(w, r)
		}
	}))
	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", `{"model":"tts-standard","parameters":{"input":"hello"}}`, map[string]string{"Idempotency-Key": "result-1"})
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var operation map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &operation))
	operationID := operation["id"].(string)
	_, err := model.RecordWildFlowUsageEvent(&model.WildFlowUsageEvent{
		EventID: "usage-result-1", PayloadDigest: "usage-result-1-digest",
		OperationID: operationID, JobID: "job-1", ModelVersionRef: "openbmb/VoxCPM2",
		Kind: "characters", Quantity: 5, Unit: "character",
	})
	require.NoError(t, err)

	statusResponse := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	hiddenOperation := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", map[string]string{"X-Test-User": "43"})
	replayed := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", `{"model":"tts-standard","parameters":{"input":"hello"}}`, map[string]string{"Idempotency-Key": "result-1"})
	artifactResponse := performWildFlowRequest(t, engine, http.MethodGet, "/v1/artifacts/artifact-1", "", nil)
	downloadResponse := performWildFlowRequest(t, engine, http.MethodGet, "/v1/artifacts/artifact-1/content", "", nil)

	require.Equal(t, http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	require.Equal(t, http.StatusNotFound, hiddenOperation.Code, hiddenOperation.Body.String())
	assert.Contains(t, statusResponse.Body.String(), `"state":"succeeded"`)
	assert.Contains(t, statusResponse.Body.String(), `"download":"/v1/artifacts/artifact-1/content"`)
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	assert.Equal(t, "true", replayed.Header().Get("X-Idempotent-Replay"))
	assert.Equal(t, statusResponse.Body.String(), replayed.Body.String())
	assert.Equal(t, 1, jobReads, "successful replay must use the persisted immutable result")
	assert.Contains(t, replayed.Body.String(), `"download":"/v1/artifacts/artifact-1/content"`)
	require.Equal(t, http.StatusOK, artifactResponse.Code, artifactResponse.Body.String())
	assert.NotContains(t, artifactResponse.Body.String(), "storage_ref")
	assert.Contains(t, artifactResponse.Body.String(), `"codec":"mp3"`)
	assert.Contains(t, artifactResponse.Body.String(), `"input_characters":5`)
	assert.Contains(t, artifactResponse.Body.String(), `"completed_characters":5`)
	assert.Contains(t, artifactResponse.Body.String(), `"segment_count":1`)
	assert.Contains(t, artifactResponse.Body.String(), `"completed_segment_count":1`)
	require.Equal(t, http.StatusOK, downloadResponse.Code, downloadResponse.Body.String())
	assert.Equal(t, "audio/mpeg", downloadResponse.Header().Get("Content-Type"))
	assert.Contains(t, downloadResponse.Header().Get("Content-Disposition"), ".mp3")
	assert.Equal(t, "audio-result", downloadResponse.Body.String())
}

func TestSucceededWildFlowJobReplayReturnsGoneAfterResultRetentionExpires(t *testing.T) {
	t.Setenv("WILDFLOW_OPERATION_RESULT_RETENTION_SECONDS", "3600")
	jobReads := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-expired-result","state":"queued"}}`))
			return
		}
		jobReads++
		_, _ = fmt.Fprintf(w, `{"job":{"id":"job-expired-result","state":"succeeded","artifacts":[%s]}}`, validVoxArtifactJSON("artifact-expired-result", "job-expired-result", 5))
	}))
	body := `{"model":"tts-standard","parameters":{"input":"hello"}}`
	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "expired-result"})
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)
	status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())
	var persisted model.WildFlowOperation
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&persisted).Error)
	assert.Equal(t, int64(3600), persisted.ResultRetentionSeconds)
	assert.WithinDuration(t, time.Now().Add(time.Hour), time.Unix(persisted.ResultExpiresAt, 0), 5*time.Second)
	require.NoError(t, model.DB.Model(&model.WildFlowOperation{}).
		Where("operation_id = ?", operationID).
		Update("result_expires_at", time.Now().Add(-time.Minute).Unix()).Error)

	replay := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "expired-result"})
	require.Equal(t, http.StatusGone, replay.Code, replay.Body.String())
	assert.Contains(t, replay.Body.String(), `"code":"result_expired"`)
	assert.Equal(t, 1, jobReads)
}

func TestLegacySucceededOperationWithoutRecoverableResultEntersManualRecovery(t *testing.T) {
	submissions := 0
	jobReads := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			submissions++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-legacy-result","state":"queued"}}`))
			return
		}
		jobReads++
		http.NotFound(w, r)
	}))
	body := `{"model":"FLUX.2 [klein] 4B","parameters":{"prompt":"一只熊猫"}}`
	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "legacy-result"})
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var payload map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &payload))
	operationID := payload["id"].(string)
	require.NoError(t, model.UpdateWildFlowOperationExecution(operationID, "job-legacy-result", "succeeded", ""))

	replay := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", body, map[string]string{"Idempotency-Key": "legacy-result"})
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	assert.Contains(t, replay.Body.String(), `"state":"recovery_required"`)
	assert.Contains(t, replay.Body.String(), `"error":"result_unavailable"`)
	assert.Empty(t, replay.Header().Get("Retry-After"))
	assert.Equal(t, 1, submissions)
	assert.Equal(t, 1, jobReads)
}

func TestDownloadVoxCPM2ArtifactFailsClosedOnInternalContentMismatch(t *testing.T) {
	tests := []struct {
		name          string
		mediaType     string
		contentLength string
	}{
		{name: "media type", mediaType: "audio/wav", contentLength: "12"},
		{name: "content length", mediaType: "audio/mpeg", contentLength: "11"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jobReads := 0
			engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/jobs":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"job":{"id":"job-download-mismatch","state":"queued"}}`))
				case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-download-mismatch":
					jobReads++
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"job":{"id":"job-download-mismatch","state":"succeeded","artifacts":[%s]}}`, validVoxArtifactJSON("artifact-download-mismatch", "job-download-mismatch", 5))
				case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-download-mismatch":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"artifact":%s}`, validVoxArtifactJSON("artifact-download-mismatch", "job-download-mismatch", 5))
				case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-download-mismatch/content":
					w.Header().Set("Content-Type", test.mediaType)
					w.Header().Set("Content-Length", test.contentLength)
				default:
					http.NotFound(w, r)
				}
			}))
			created := performWildFlowRequest(
				t,
				engine,
				http.MethodPost,
				"/v1/jobs",
				`{"model":"VoxCPM2","parameters":{"input":"hello","voice":"default"}}`,
				map[string]string{"Idempotency-Key": "download-mismatch"},
			)
			require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
			var operation map[string]any
			require.NoError(t, common.Unmarshal(created.Body.Bytes(), &operation))
			status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operation["id"].(string), "", nil)
			require.Equal(t, http.StatusOK, status.Code, status.Body.String())
			assert.Equal(t, 1, jobReads)

			download := performWildFlowRequest(t, engine, http.MethodGet, "/v1/artifacts/artifact-download-mismatch/content", "", nil)
			recoveryStatus := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operation["id"].(string), "", nil)
			replay := performWildFlowRequest(
				t,
				engine,
				http.MethodPost,
				"/v1/jobs",
				`{"model":"VoxCPM2","parameters":{"input":"hello","voice":"default"}}`,
				map[string]string{"Idempotency-Key": "download-mismatch"},
			)

			require.Equal(t, http.StatusServiceUnavailable, download.Code, download.Body.String())
			assert.Contains(t, download.Body.String(), `"code":"artifact_integrity_error"`)
			assert.NotEqual(t, "audio/wav", download.Header().Get("Content-Type"))
			require.Equal(t, http.StatusOK, recoveryStatus.Code, recoveryStatus.Body.String())
			assert.Contains(t, recoveryStatus.Body.String(), `"state":"recovery_required"`)
			assert.NotContains(t, recoveryStatus.Body.String(), `"artifacts"`)
			require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
			assert.Contains(t, replay.Body.String(), `"state":"recovery_required"`)
			assert.Equal(t, 1, jobReads, "recovery_required must be sticky for public polling and replay")
			var persisted model.WildFlowOperation
			require.NoError(t, model.DB.Where("operation_id = ?", operation["id"].(string)).First(&persisted).Error)
			assert.Equal(t, "recovery_required", persisted.State)
		})
	}
}

func TestDownloadVoxCPM2ArtifactPersistsRecoveryAfterStreamFailure(t *testing.T) {
	jobReads := 0
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/jobs":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-stream-failure","state":"queued"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-stream-failure":
			jobReads++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"job":{"id":"job-stream-failure","state":"succeeded","artifacts":[%s]}}`, validVoxArtifactJSON("artifact-stream-failure", "job-stream-failure", 5))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-stream-failure":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"artifact":%s}`, validVoxArtifactJSON("artifact-stream-failure", "job-stream-failure", 5))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-stream-failure/content":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Length", "12")
			_, _ = w.Write([]byte("short"))
		default:
			http.NotFound(w, r)
		}
	}))
	requestBody := `{"model":"VoxCPM2","parameters":{"input":"hello","voice":"default"}}`
	requestHeaders := map[string]string{"Idempotency-Key": "stream-failure"}
	created := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", requestBody, requestHeaders)
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var operation map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &operation))
	operationID := operation["id"].(string)
	status := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())
	assert.Equal(t, 1, jobReads)

	download := performWildFlowRequest(t, engine, http.MethodGet, "/v1/artifacts/artifact-stream-failure/content", "", nil)
	recoveryStatus := performWildFlowRequest(t, engine, http.MethodGet, "/v1/jobs/"+operationID, "", nil)
	replay := performWildFlowRequest(t, engine, http.MethodPost, "/v1/jobs", requestBody, requestHeaders)

	require.Equal(t, http.StatusOK, download.Code, "the public stream had already started before unexpected EOF")
	assert.Equal(t, "short", download.Body.String())
	require.Equal(t, http.StatusOK, recoveryStatus.Code, recoveryStatus.Body.String())
	assert.Contains(t, recoveryStatus.Body.String(), `"state":"recovery_required"`)
	assert.Contains(t, recoveryStatus.Body.String(), `"error":"artifact_stream_error"`)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	assert.Contains(t, replay.Body.String(), `"state":"recovery_required"`)
	assert.Equal(t, 1, jobReads)
	var persisted model.WildFlowOperation
	require.NoError(t, model.DB.Where("operation_id = ?", operationID).First(&persisted).Error)
	assert.Equal(t, "recovery_required", persisted.State)
	assert.Equal(t, "artifact_stream_error", persisted.LastErrorCode)
}
