package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestOIDCLogoutRouteRejectsCrossOriginWhenSecureCookiesEnabled(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() {
		common.SessionCookieSecure = previousSecure
	})
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	request := httptest.NewRequest(http.MethodGet, "/api/oauth/oidc/logout", nil)
	request.Header.Set("Referer", "https://attacker.example/")
	response := httptest.NewRecorder()

	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusForbidden, response.Code)
}
