package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWildFlowCatalogIsPublicAndDisplaysPricedAndTeamTrialModels(t *testing.T) {
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
			},
			{
				"id":"Ideogram 4 mixed-v3",
				"display_name":"Ideogram 4 mixed-v3",
				"kind":"image",
				"vendor":"Ideogram",
				"model_version_ref":"ideogram-4-mixed-v3@bbee2ab2",
				"description":"Ideogram 4 图片生成，仅限团队内部非商业评测使用，不提供商用承诺。",
				"required_parameters":["prompt","width","height","seed","steps"],
				"pricing":{"currency":"CNY","amount":0,"unit":"team_trial","display":"团队内部非商业评测 · 暂不扣零售余额"},
				"callable":false,
				"status":"unavailable"
			},
			{
				"id":"Qwen/Qwen3.8-27B-FP8@gpu-4090-06",
				"display_name":"Qwen3.8-27B (FP8) 对话",
				"kind":"chat",
				"vendor":"Qwen",
				"model_version_ref":"Qwen/Qwen3.8-27B-FP8",
				"description":"通义千问 27B FP8 对话模型，支持中文问答、代码与思考模式，当前由 WildFlow 四卡 4090 节点提供。",
				"pricing":{"currency":"CNY","amount":4.38,"unit":"million_tokens","display":"¥4.38 / 百万输入或输出 Token"},
				"callable":false,
				"status":"unavailable"
			},
			{
				"id":"wildflow/dual-asr-v1",
				"display_name":"双引擎语音识别",
				"kind":"asr",
				"vendor":"WildFlow",
				"model_version_ref":"wildflow/exam-replay-dual-asr-v1",
				"description":"同时输出分段转写与逐词时间戳，适用于通用音频转写、字幕整理和检索。",
				"required_parameters":["input_artifact_ids"],
				"pricing":{"currency":"CNY","amount":0.05,"unit":"audio_minute","display":"¥0.05 / 音频分钟"},
					"callable":false,
					"status":"unavailable"
				},
				{
					"id":"IndexTTS-2.5",
					"display_name":"IndexTTS-2.5",
					"kind":"tts",
					"vendor":"IndexTeam",
					"model_version_ref":"indextts-2.5@0b328234",
					"description":"固定服务端参考音频的 IndexTTS-2.5 语音合成，仅供团队内部使用。",
					"required_parameters":["text"],
					"pricing":{"currency":"CNY","amount":0,"unit":"team_trial","display":"团队内部使用 · 暂不扣零售余额"},
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
