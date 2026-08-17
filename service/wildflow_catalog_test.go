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
			{"id":"untrusted-extra","model_version_ref":"other/model","callable":true}
		]}`))
	}))
	defer server.Close()
	t.Setenv("WILDFLOW_INFERENCE_URL", server.URL)
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "internal-test-token")

	catalog := GetWildFlowCatalog(context.Background())

	require.Len(t, catalog, 2)
	assert.Equal(t, []string{"VoxCPM2", "FLUX.2 [klein] 4B"}, []string{
		catalog[0].ID, catalog[1].ID,
	})
	assert.True(t, catalog[0].Callable)
	assert.True(t, catalog[1].Callable)
	assert.Equal(t, "openbmb/VoxCPM2", catalog[0].ModelVersionRef)
	assert.Equal(t, "¥0.8 / 万字符", catalog[0].Pricing.Display)
}

func TestGetWildFlowCatalogFailsClosedButStillDisplaysTwoModels(t *testing.T) {
	t.Setenv("WILDFLOW_INFERENCE_URL", "")
	t.Setenv("WILDFLOW_INTERNAL_TOKEN", "")

	catalog := GetWildFlowCatalog(context.Background())

	require.Len(t, catalog, 2)
	for _, offering := range catalog {
		assert.False(t, offering.Callable)
		assert.Equal(t, "unavailable", offering.Status)
	}
}
