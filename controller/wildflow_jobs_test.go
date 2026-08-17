package controller

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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
	engine.POST("/api/v1/audio/speech", CreateWildFlowLegacySpeechJob)
	engine.POST("/api/v1/images/generations", CreateWildFlowLegacyImageJob)
	engine.GET("/v1/jobs/:operation_id", GetWildFlowJob)
	engine.POST("/v1/jobs/:operation_id/cancel", CancelWildFlowJob)
	engine.GET("/v1/artifacts/:artifact_id", GetWildFlowArtifact)
	engine.GET("/v1/artifacts/:artifact_id/content", DownloadWildFlowArtifact)
	return engine, server
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
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
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
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	assert.Equal(t, 1, submissions)
	var first, second map[string]any
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &first))
	require.NoError(t, common.Unmarshal(replayed.Body.Bytes(), &second))
	assert.Equal(t, first["id"], second["id"])
	assert.Equal(t, "job-tts-1", first["job_id"])
	assert.Equal(t, "queued", first["state"])
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

func TestWildFlowJobStatusAndArtifactDownloadRemainUserScoped(t *testing.T) {
	engine, _ := setupWildFlowJobsControllerTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "user:42", r.Header.Get("X-WildFlow-Tenant-Ref"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/v1/jobs":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"queued"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/jobs/job-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"job":{"id":"job-1","state":"succeeded","artifacts":[{"id":"artifact-1","job_id":"job-1","media_type":"audio/wav","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"artifact":{"id":"artifact-1","job_id":"job-1","media_type":"audio/wav","size_bytes":12,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/internal/v1/artifacts/artifact-1/content":
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Content-Disposition", `attachment; filename="artifact-1.wav"`)
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
	assert.Contains(t, replayed.Body.String(), `"download":"/v1/artifacts/artifact-1/content"`)
	require.Equal(t, http.StatusOK, artifactResponse.Code, artifactResponse.Body.String())
	assert.NotContains(t, artifactResponse.Body.String(), "storage_ref")
	require.Equal(t, http.StatusOK, downloadResponse.Code, downloadResponse.Body.String())
	assert.Equal(t, "audio/wav", downloadResponse.Header().Get("Content-Type"))
	assert.Equal(t, "audio-result", downloadResponse.Body.String())
}
