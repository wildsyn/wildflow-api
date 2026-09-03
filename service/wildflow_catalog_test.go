package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetWildFlowCatalogMergesOnlyCanonicalRuntimeAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/internal/v1/catalog", r.URL.Path)
		assert.Equal(t, "Bearer internal-test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"VoxCPM2","model_version_ref":"openbmb/VoxCPM2","callable":true},
			{"id":"FLUX.2 [klein] 4B","model_version_ref":"black-forest-labs/FLUX.2-klein-4B","callable":true},
			{"id":"ideogram-4-mixed-v3","model_version_ref":"ideogram-4-mixed-v3@bbee2ab2","callable":true},
			{"id":"qwen3.8-27b-fp8","model_version_ref":"Qwen/Qwen3.8-27B-FP8","callable":true},
			{"id":"exam-replay-dual-asr","model_version_ref":"wildflow/exam-replay-dual-asr-v1","callable":true},
			{"id":"untrusted-extra","model_version_ref":"other/model","callable":true}
		]}`))
	}))
	defer server.Close()
	t.Setenv("WILDFLOW_INFERENCE_URL", server.URL)
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "internal-test-token")

	catalog := GetWildFlowCatalog(context.Background())

	require.Len(t, catalog, 5)
	assert.Equal(t, []string{"VoxCPM2", "FLUX.2 [klein] 4B", WildFlowModelIdeogram4MixedV3, "Qwen/Qwen3.8-27B-FP8@gpu-4090-06", WildFlowModelExamDualASR}, []string{
		catalog[0].ID, catalog[1].ID, catalog[2].ID, catalog[3].ID, catalog[4].ID,
	})
	assert.True(t, catalog[0].Callable)
	assert.True(t, catalog[1].Callable)
	assert.True(t, catalog[2].Callable)
	assert.True(t, catalog[3].Callable)
	assert.True(t, catalog[4].Callable)
	assert.Equal(t, "openbmb/VoxCPM2", catalog[0].ModelVersionRef)
	assert.Equal(t, "¥0.8 / 万字符", catalog[0].Pricing.Display)
	assert.Equal(t, "ideogram-4-mixed-v3@bbee2ab2", catalog[2].ModelVersionRef)
	assert.Equal(t, "team_trial", catalog[2].Pricing.Unit)
	assert.Contains(t, catalog[2].Description, "非商业")
	assert.Equal(t, "chat", catalog[3].Kind)
	assert.Equal(t, "Qwen/Qwen3.8-27B-FP8", catalog[3].ModelVersionRef)
	assert.Equal(t, "¥4.38 / 百万输入或输出 Token", catalog[3].Pricing.Display)
	assert.Equal(t, "asr", catalog[4].Kind)
	assert.Equal(t, "团队内测 · 暂不扣零售余额", catalog[4].Pricing.Display)
}

func TestGetWildFlowCatalogFailsClosedButStillDisplaysCanonicalModels(t *testing.T) {
	t.Setenv("WILDFLOW_INFERENCE_URL", "")
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "")

	catalog := GetWildFlowCatalog(context.Background())

	require.Len(t, catalog, 5)
	for _, offering := range catalog {
		assert.False(t, offering.Callable)
		assert.Equal(t, "unavailable", offering.Status)
	}
}
