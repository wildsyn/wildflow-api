package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddUpdateThenTokenAuthCommaListRoundTrip is the ZXDW-210 end-to-end
// regression: a comma-separated IPv4/IPv6/CIDR allow_ips accepted by
// AddToken/UpdateToken must be enforced by TokenAuth with exactly the
// written entries — no merging, no silent rejections.
func TestAddUpdateThenTokenAuthCommaListRoundTrip(t *testing.T) {
	setupTokenControllerTestDB(t)
	// TokenAuth reads the user cache; keep redis off and DB-backed users.
	require.NoError(t, model.DB.AutoMigrate(&model.User{}))
	user := &model.User{
		Username: "roundtrip-user", Password: "password-placeholder",
		Role: common.RoleCommonUser, Status: common.UserStatusEnabled,
		Group: "default", Quota: 1000000, AffCode: "roundtrip-aff",
	}
	require.NoError(t, model.DB.Create(user).Error)

	allowIps := "10.0.0.1,192.168.0.0/16,2001:db8::1"
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", newTokenPayload(0, "roundtrip-token", allowIps), user.Id)
	AddToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "AddToken must accept a comma-separated list: %s", response.Message)

	var stored model.Token
	require.NoError(t, model.DB.First(&stored, "user_id = ? AND name = ?", user.Id, "roundtrip-token").Error)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.SetTrustedProxies(nil)
	router.GET("/protected", middleware.TokenAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	serve := func(remoteAddr string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/protected", nil)
		request.Header.Set("Authorization", "Bearer "+stored.Key)
		request.RemoteAddr = remoteAddr
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	assert.Equal(t, http.StatusOK, serve("10.0.0.1:1000").Code, "first comma entry must be allowed after AddToken")
	assert.Equal(t, http.StatusOK, serve("192.168.77.3:1000").Code, "comma-listed CIDR must be allowed after AddToken")
	assert.Equal(t, http.StatusOK, serve("[2001:db8::1]:1000").Code, "comma-listed IPv6 must be allowed after AddToken")

	denied := serve("203.0.113.7:1000")
	assert.Equal(t, http.StatusForbidden, denied.Code, "non-listed IP must be rejected after AddToken")

	// Update path: rotate to a different comma list.
	updated := newTokenPayload(stored.Id, "roundtrip-token", "10.9.0.0/16,fd00::/8")
	updateCtx, updateRecorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", updated, user.Id)
	UpdateToken(updateCtx)
	updatedResponse := decodeAPIResponse(t, updateRecorder)
	require.True(t, updatedResponse.Success, "UpdateToken must accept a comma-separated list: %s", updatedResponse.Message)

	require.NoError(t, model.DB.First(&stored, "id = ?", stored.Id).Error)
	assert.Equal(t, http.StatusOK, serve("10.9.3.4:1000").Code, "updated list entry must be allowed")
	assert.Equal(t, http.StatusForbidden, serve("10.0.0.1:1000").Code, "removed list entry must now be rejected")
}
