package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupOIDCEnrollmentTest(t *testing.T) {
	t.Helper()
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousServerAddress := system_setting.ServerAddress
	settings := system_setting.GetOIDCSettings()
	previousSettings := *settings

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AuthFlow{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	system_setting.ServerAddress = "https://wildflow.cn"
	*settings = system_setting.OIDCSettings{
		Enabled:               true,
		ClientId:              "wildflow-main",
		ClientSecret:          "test-secret",
		AuthorizationEndpoint: "https://auth.wildflow.cn/application/o/authorize/",
		TokenEndpoint:         "https://auth.wildflow.cn/application/o/token/",
		UserInfoEndpoint:      "https://auth.wildflow.cn/application/o/userinfo/",
		EndSessionEndpoint:    "https://auth.wildflow.cn/application/o/wildflow/end-session/",
		EnrollmentEndpoint:    "https://auth.wildflow.cn/if/flow/wildflow-enrollment/",
	}
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		system_setting.ServerAddress = previousServerAddress
		*settings = previousSettings
	})
}

func TestOIDCEnrollmentLogsOutCentralSessionThenStartsOAuth(t *testing.T) {
	setupOIDCEnrollmentTest(t)
	router := gin.New()
	router.GET("/api/oauth/oidc/enroll", BeginOIDCEnrollment)
	router.GET("/api/oauth/oidc/enroll/start", ContinueOIDCEnrollment)

	beginResponse := httptest.NewRecorder()
	router.ServeHTTP(beginResponse, httptest.NewRequest(http.MethodGet, "/api/oauth/oidc/enroll", nil))
	require.Equal(t, http.StatusFound, beginResponse.Code)

	logoutURL, err := url.Parse(beginResponse.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "https://auth.wildflow.cn/if/flow/default-invalidation-flow/", logoutURL.Scheme+"://"+logoutURL.Host+logoutURL.Path)
	assert.Equal(t, "https://wildflow.cn/api/oauth/oidc/enroll/start", logoutURL.Query().Get("next"))
	assert.Empty(t, logoutURL.Query().Get("client_id"))
	assert.Empty(t, logoutURL.Query().Get("post_logout_redirect_uri"))

	var enrollmentCookie *http.Cookie
	for _, cookie := range beginResponse.Result().Cookies() {
		if cookie.Name == "wf_oidc_enroll_flow" {
			enrollmentCookie = cookie
			break
		}
	}
	require.NotNil(t, enrollmentCookie)
	assert.True(t, enrollmentCookie.HttpOnly)
	assert.True(t, enrollmentCookie.Secure)
	assert.Equal(t, http.SameSiteLaxMode, enrollmentCookie.SameSite)
	assert.Equal(t, "/api/oauth/oidc/enroll", enrollmentCookie.Path)
	_, err = model.GetAuthFlow(enrollmentCookie.Value, model.AuthFlowMatch{
		Purpose:  model.AuthFlowPurposeOAuth,
		Provider: "oidc",
		Intent:   model.AuthFlowIntentLogin,
	})
	require.NoError(t, err)

	continueRequest := httptest.NewRequest(http.MethodGet, "/api/oauth/oidc/enroll/start", nil)
	continueRequest.AddCookie(enrollmentCookie)
	continueResponse := httptest.NewRecorder()
	router.ServeHTTP(continueResponse, continueRequest)
	require.Equal(t, http.StatusFound, continueResponse.Code)

	enrollmentURL, err := url.Parse(continueResponse.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "https://auth.wildflow.cn/if/flow/wildflow-enrollment/", enrollmentURL.Scheme+"://"+enrollmentURL.Host+enrollmentURL.Path)
	authorizationURL, err := url.Parse(enrollmentURL.Query().Get("next"))
	require.NoError(t, err)
	assert.Equal(t, "https://auth.wildflow.cn/application/o/authorize/", authorizationURL.Scheme+"://"+authorizationURL.Host+authorizationURL.Path)
	assert.Equal(t, enrollmentCookie.Value, authorizationURL.Query().Get("state"))
	assert.Equal(t, "https://wildflow.cn/oauth/oidc", authorizationURL.Query().Get("redirect_uri"))
	assert.Equal(t, "openid email profile", authorizationURL.Query().Get("scope"))
	assert.Equal(t, "code", authorizationURL.Query().Get("response_type"))

	cleared := false
	for _, cookie := range continueResponse.Result().Cookies() {
		if cookie.Name == "wf_oidc_enroll_flow" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared)
}

func TestOIDCEnrollmentRejectsCrossOriginProviderEndpoints(t *testing.T) {
	setupOIDCEnrollmentTest(t)
	system_setting.GetOIDCSettings().EnrollmentEndpoint = "https://attacker.example/enroll"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/oauth/oidc/enroll", nil)

	BeginOIDCEnrollment(c)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Empty(t, recorder.Header().Get("Location"))
}
