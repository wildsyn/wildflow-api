package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTokenAuthTest(t *testing.T) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	model.InitCol()
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled = previousRedis
	})
}

func createTokenAuthUser(t *testing.T) *model.User {
	t.Helper()
	user := &model.User{
		Username: "token-auth-user", Password: "password-placeholder",
		Role: common.RoleCommonUser, Status: common.UserStatusEnabled,
		Group: "default", Quota: 1000000, AffCode: "token-auth-aff",
	}
	require.NoError(t, model.DB.Create(user).Error)
	return user
}

func createTokenAuthToken(t *testing.T, userID int, key string, mutate func(*model.Token)) *model.Token {
	t.Helper()
	allowIps := ""
	token := &model.Token{
		UserId: userID, Name: "auth-token-" + key, Key: key,
		Status: common.TokenStatusEnabled, CreatedTime: time.Now().Unix(),
		AccessedTime: time.Now().Unix(), ExpiredTime: -1,
		RemainQuota: 500000, UnlimitedQuota: false,
		AllowIps: &allowIps, Group: "default",
	}
	if mutate != nil {
		mutate(token)
	}
	require.NoError(t, model.DB.Create(token).Error)
	return token
}

type tokenAuthErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

func serveTokenAuthRequest(t *testing.T, key string) (*httptest.ResponseRecorder, tokenAuthErrorResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/protected", TokenAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.GetInt("id")})
	})
	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	var body tokenAuthErrorResponse
	if response.Body.Len() > 0 {
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	}
	return response, body
}

func TestTokenAuthRejectsUnknownKeyWithoutLeakingExistence(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	createTokenAuthToken(t, user.Id, "existingkey0001", nil)
	createTokenAuthToken(t, user.Id, "ordinaryinvalid1", func(token *model.Token) {
		token.Status = 99
	})

	unknownResponse, unknownBody := serveTokenAuthRequest(t, "missingkey00001")
	invalidResponse, invalidBody := serveTokenAuthRequest(t, "ordinaryinvalid1")
	assert.Equal(t, http.StatusUnauthorized, unknownResponse.Code)
	assert.Equal(t, http.StatusUnauthorized, invalidResponse.Code)
	assert.Equal(t, "token_invalid", unknownBody.Error.Code)
	assert.Equal(t, invalidBody.Error.Code, unknownBody.Error.Code)
	// A missing key and a real key in an ordinary invalid state must have an
	// indistinguishable complete response, including the machine code.
	assert.JSONEq(t, invalidResponse.Body.String(), unknownResponse.Body.String())
	assert.Contains(t, unknownBody.Error.Message, common.TranslateMessage(nil, "token.invalid"))
}

func TestTokenAuthDistinguishesExpiredDisabledAndExhaustedKeys(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)

	createTokenAuthToken(t, user.Id, "expiredkey0001", func(token *model.Token) {
		token.ExpiredTime = time.Now().Add(-time.Hour).Unix()
	})
	createTokenAuthToken(t, user.Id, "disabledkey01", func(token *model.Token) {
		token.Status = common.TokenStatusDisabled
	})
	createTokenAuthToken(t, user.Id, "exhaustedkey1", func(token *model.Token) {
		token.RemainQuota = 0
	})
	createTokenAuthToken(t, user.Id, "persistedstat1", func(token *model.Token) {
		token.Status = common.TokenStatusExhausted
	})

	tests := []struct {
		name        string
		key         string
		wantCode    string
		wantMessage string
	}{
		{name: "expired by time", key: "expiredkey0001", wantCode: "token_expired", wantMessage: common.TranslateMessage(nil, "token.expired")},
		{name: "manually disabled", key: "disabledkey01", wantCode: "token_disabled", wantMessage: common.TranslateMessage(nil, "token.disabled")},
		{name: "exhausted by remaining quota", key: "exhaustedkey1", wantCode: "token_quota_exhausted", wantMessage: common.TranslateMessage(nil, "token.exhausted")},
		{name: "persisted exhausted status", key: "persistedstat1", wantCode: "token_quota_exhausted", wantMessage: common.TranslateMessage(nil, "token.exhausted")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, body := serveTokenAuthRequest(t, test.key)
			assert.Equal(t, http.StatusUnauthorized, response.Code)
			assert.Equal(t, test.wantCode, body.Error.Code)
			// The response message is wrapped with a request id; compare the
			// translated message against the prefix.
			assert.Contains(t, body.Error.Message, test.wantMessage)
		})
	}
}

func TestTokenAuthValidKeyStillAuthorized(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	token := createTokenAuthToken(t, user.Id, "validkey00001", nil)

	response, _ := serveTokenAuthRequest(t, token.Key)
	assert.Equal(t, http.StatusOK, response.Code)
}

func TestTokenAuthEnforcesIPAllowList(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	allowIps := "10.0.0.0/8"
	token := createTokenAuthToken(t, user.Id, "ipallowkey001", func(token *model.Token) {
		token.AllowIps = &allowIps
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.SetTrustedProxies(nil)
	router.GET("/protected", TokenAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.GetInt("id")})
	})

	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+token.Key)
	request.RemoteAddr = "10.1.2.3:12345"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code)

	request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+token.Key)
	request.RemoteAddr = "203.0.113.7:12345"
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
	var body tokenAuthErrorResponse
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "access_denied", body.Error.Code)
}
