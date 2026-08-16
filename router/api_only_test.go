package router

import (
	"embed"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetRouterDefaultsToAPIOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("FRONTEND_BASE_URL", "")
	t.Setenv("SERVE_EMBEDDED_FRONTEND", "")

	engine := gin.New()
	require.NotPanics(t, func() {
		SetRouter(engine, WebAssets{BuildFS: embed.FS{}})
	})

	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.JSONEq(t, `{"error":"not found","hint":"frontend is deployed separately as wildflow-web"}`, response.Body.String())
}
