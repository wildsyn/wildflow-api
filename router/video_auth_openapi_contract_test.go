package router

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type videoContentAuthResponse struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func setupVideoContentContractDB(t *testing.T) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Task{}, &model.Channel{}))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	model.InitCol()
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedis
	})
}

func addVideoContentContractToken(t *testing.T) *model.Token {
	t.Helper()
	user := &model.User{
		Username: "video-contract-user", Password: "password-placeholder",
		Role: common.RoleCommonUser, Status: common.UserStatusEnabled,
		Group: "default", Quota: 1000000, AffCode: "video-contract-aff",
	}
	require.NoError(t, model.DB.Create(user).Error)
	allowIPs := ""
	token := &model.Token{
		UserId: user.Id, Name: "video-contract-token", Key: "videocontracttokenkey",
		Status: common.TokenStatusEnabled, CreatedTime: time.Now().Unix(),
		AccessedTime: time.Now().Unix(), ExpiredTime: -1, RemainQuota: 500000,
		AllowIps: &allowIPs, Group: "default",
	}
	require.NoError(t, model.DB.Create(token).Error)
	channel := &model.Channel{Name: "video-contract-channel", Key: "not-a-real-provider-key", Group: "default"}
	require.NoError(t, model.DB.Create(channel).Error)
	task := &model.Task{
		TaskID: "video-contract-task", UserId: user.Id, ChannelId: channel.Id,
		Group: "default", Status: model.TaskStatusSuccess,
		PrivateData: model.TaskPrivateData{ResultURL: "http://127.0.0.1:8080/video.mp4"},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return token
}

func serveVideoContentRequest(router *gin.Engine, authorization string) (*httptest.ResponseRecorder, videoContentAuthResponse) {
	request := httptest.NewRequest(http.MethodGet, "/v1/videos/video-contract-task/content", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var body videoContentAuthResponse
	_ = common.Unmarshal(response.Body.Bytes(), &body)
	return response, body
}

func expiredVideoContentDashboardToken(t *testing.T) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(common.SessionSecret))
	_, _ = mac.Write([]byte("new-api/auth/access/v1"))
	claims := jwt.MapClaims{
		"iss": "new-api", "aud": []string{"new-api-dashboard"}, "sub": "1",
		"token_use": "access", "sid": "video-contract-session", "uv": 1, "sv": 1,
		"exp": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-2 * time.Minute).Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(mac.Sum(nil))
	require.NoError(t, err)
	return token
}

func TestVideoContentRouteResponsesMatchOpenAPIAuthContract(t *testing.T) {
	setupVideoContentContractDB(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	SetVideoRouter(router)

	missingResponse, missingBody := serveVideoContentRequest(router, "")
	require.Equal(t, http.StatusUnauthorized, missingResponse.Code)
	assert.Equal(t, "token_not_provided", missingBody.Error.Code)
	assert.Equal(t, "new_api_error", missingBody.Error.Type)

	previousSecret := common.SessionSecret
	common.SessionSecret = "video-content-contract-session-secret"
	t.Cleanup(func() { common.SessionSecret = previousSecret })
	dashboardProof, _, err := service.IssueSecurityProof(service.AuthIdentity{
		UserID: 1, SessionID: "video-contract-session", UserAuthVersion: 1, SessionVersion: 1,
	}, "2fa", []string{"channel.key.read"})
	require.NoError(t, err)
	dashboardResponse, dashboardBody := serveVideoContentRequest(router, "Bearer "+dashboardProof)
	require.Equal(t, http.StatusUnauthorized, dashboardResponse.Code)
	assert.False(t, dashboardBody.Success)
	assert.Equal(t, "AUTH_UNAUTHORIZED", dashboardBody.Code)
	require.NotEmpty(t, dashboardBody.Message)

	expiredResponse, expiredBody := serveVideoContentRequest(router, "Bearer "+expiredVideoContentDashboardToken(t))
	require.Equal(t, http.StatusUnauthorized, expiredResponse.Code)
	assert.False(t, expiredBody.Success)
	assert.Equal(t, "AUTH_TOKEN_EXPIRED", expiredBody.Code)
	require.NotEmpty(t, expiredBody.Message)

	token := addVideoContentContractToken(t)
	ssrfResponse, ssrfBody := serveVideoContentRequest(router, "Bearer "+token.Key)
	require.Equal(t, http.StatusForbidden, ssrfResponse.Code)
	assert.Equal(t, "server_error", ssrfBody.Error.Type)
	assert.Contains(t, ssrfBody.Error.Message, "request blocked:")
}
