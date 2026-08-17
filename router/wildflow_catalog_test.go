package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWildFlowCatalogIsPublicAndDisplaysThreeOfferings(t *testing.T) {
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
				"id":"tts-standard",
				"display_name":"VoxCPM2 标准语音合成",
				"kind":"tts",
				"vendor":"OpenBMB",
				"model_version_ref":"openbmb/VoxCPM2",
				"profile":"standard",
				"description":"通用音色、声音设计与声音克隆。",
				"callable":false,
				"status":"unavailable"
			},
			{
				"id":"tts-premium",
				"display_name":"VoxCPM2 王立群精品语音",
				"kind":"tts",
				"vendor":"OpenBMB",
				"model_version_ref":"openbmb/VoxCPM2",
				"profile":"wangliqun-premium",
				"description":"基于同一 VoxCPM2 底模的王立群精品音色产品。",
				"callable":false,
				"status":"unavailable"
			},
			{
				"id":"flux2-klein-4b",
				"display_name":"FLUX.2 [klein] 4B 图片生成",
				"kind":"image",
				"vendor":"Black Forest Labs",
				"model_version_ref":"black-forest-labs/FLUX.2-klein-4B",
				"profile":"default",
				"description":"支持中英文提示词的开源图片生成模型。",
				"callable":false,
				"status":"unavailable"
			}
		]
	}`, response.Body.String())
}
