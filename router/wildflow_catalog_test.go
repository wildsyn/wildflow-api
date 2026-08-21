package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWildFlowCatalogIsPublicAndDisplaysTwoPricedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("WILDFLOW_INFERENCE_URL", "")
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "")
	engine := gin.New()
	SetApiRouter(engine)

	request := httptest.NewRequest(http.MethodGet, "/api/wildflow/catalog", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{
		"success": true,
		"data": [
			{
				"id":"VoxCPM2",
				"display_name":"VoxCPM2",
				"kind":"tts",
				"vendor":"OpenBMB",
				"model_version_ref":"openbmb/VoxCPM2",
				"description":"支持四个内置音色与王立群克隆音色的语音合成模型。",
				"required_parameters":["voice"],
				"voices":[
					{"id":"shuoshuren","name":"说书人","category":"official"},
					{"id":"dabin","name":"大斌","category":"official"},
					{"id":"tingting","name":"婷婷","category":"official"},
					{"id":"default","name":"默认","category":"official"},
					{"id":"wangliqun","name":"王立群","category":"custom"}
				],
				"pricing":{"currency":"CNY","amount":0.8,"unit":"10k_characters","display":"¥0.8 / 万字符"},
				"callable":false,
				"status":"unavailable"
			},
			{
				"id":"FLUX.2 [klein] 4B",
				"display_name":"FLUX.2 [klein] 4B 图片生成",
				"kind":"image",
				"vendor":"Black Forest Labs",
				"model_version_ref":"black-forest-labs/FLUX.2-klein-4B",
				"description":"支持中英文提示词的开源图片生成模型。",
				"pricing":{"currency":"CNY","amount":0.05,"unit":"image","display":"¥0.05 / 张"},
				"callable":false,
				"status":"unavailable"
			}
		]
	}`, response.Body.String())
}

func TestWildFlowDurableJobRoutesRequireAnAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetRelayRouter(engine)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/jobs", nil),
		httptest.NewRequest(http.MethodPost, "/v1/input-artifacts", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/audio/speech", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/images/generations", nil),
		httptest.NewRequest(http.MethodGet, "/v1/jobs/op-1", nil),
		httptest.NewRequest(http.MethodGet, "/v1/artifacts/artifact-1/content", nil),
	} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusUnauthorized, response.Code, request.URL.Path)
	}
}
