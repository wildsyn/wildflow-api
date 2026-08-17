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

func TestSetRouterExposesPublicServiceInfoAtRoot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("FRONTEND_BASE_URL", "")

	engine := gin.New()
	require.NotPanics(t, func() {
		SetRouter(engine, WebAssets{BuildFS: embed.FS{}})
	})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "application/json; charset=utf-8", response.Header().Get("Content-Type"))
	assert.JSONEq(t, `{
		"service": "WildFlow API",
		"api_base": "/v1",
		"api_version": "v1",
		"documentation": "https://github.com/wildsyn/wildflow/tree/main/docs",
		"website": "https://wildflow.cn"
	}`, response.Body.String())
}

func TestSetRouterRootDoesNotBypassExistingAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("FRONTEND_BASE_URL", "")

	engine := gin.New()
	require.NotPanics(t, func() {
		SetRouter(engine, WebAssets{BuildFS: embed.FS{}})
	})

	for _, path := range []string{"/v1/models", "/api/user/self"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			assert.Equal(t, http.StatusUnauthorized, response.Code)
		})
	}
}
