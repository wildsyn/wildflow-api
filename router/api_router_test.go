package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestTokenKeyRateLimitIsPerUserAndIndependentFromSharedIPAuthTraffic(t *testing.T) {
	previousDB := model.DB
	previousDatabaseType := common.MainDatabaseType()
	previousRedisEnabled := common.RedisEnabled
	previousRedisClient := common.RDB
	previousCriticalEnabled := common.CriticalRateLimitEnable
	previousCriticalNum := common.CriticalRateLimitNum
	previousCriticalDuration := common.CriticalRateLimitDuration
	previousGlobalEnabled := common.GlobalApiRateLimitEnable

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, redisClient.Ping(context.Background()).Err())
	common.RedisEnabled = true
	common.RDB = redisClient
	common.CriticalRateLimitEnable = true
	common.CriticalRateLimitNum = 1
	common.CriticalRateLimitDuration = 60
	common.GlobalApiRateLimitEnable = false

	t.Cleanup(func() {
		_ = redisClient.Close()
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
		model.DB = previousDB
		common.SetMainDatabaseType(previousDatabaseType)
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRedisClient
		common.CriticalRateLimitEnable = previousCriticalEnabled
		common.CriticalRateLimitNum = previousCriticalNum
		common.CriticalRateLimitDuration = previousCriticalDuration
		common.GlobalApiRateLimitEnable = previousGlobalEnabled
	})

	createUserAndToken := func(username string, pat string, key string) (*model.User, *model.Token) {
		user := &model.User{
			Username: username, Password: "password-placeholder", Role: common.RoleCommonUser,
			Status: common.UserStatusEnabled, Group: "default", AccessToken: &pat, AuthVersion: 1,
			AffCode: "router-aff-" + username,
		}
		require.NoError(t, model.DB.Create(user).Error)
		token := &model.Token{
			UserId: user.Id, Key: key, Name: "test-key", Status: common.TokenStatusEnabled,
			CreatedTime: 1, AccessedTime: 1, ExpiredTime: -1, UnlimitedQuota: true,
		}
		require.NoError(t, model.DB.Create(token).Error)
		return user, token
	}

	_, firstToken := createUserAndToken("rate-limit-user-1", "pat-user-1", "token-key-user-1")
	_, secondToken := createUserAndToken("rate-limit-user-2", "pat-user-2", "token-key-user-2")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	SetApiRouter(engine)
	sharedIP := "192.0.2.80:12345"

	request := httptest.NewRequest(http.MethodGet, "/api/ratio_config", nil)
	request.RemoteAddr = sharedIP
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	requestTokenKey := func(tokenID int, pat string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/token/"+strconv.Itoa(tokenID)+"/key", nil)
		request.RemoteAddr = sharedIP
		request.Header.Set("Authorization", "Bearer "+pat)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		return response
	}

	assert.Equal(t, http.StatusOK, requestTokenKey(firstToken.Id, "pat-user-1").Code)
	assert.Equal(t, http.StatusOK, requestTokenKey(secondToken.Id, "pat-user-2").Code)
	limitedResponse := requestTokenKey(firstToken.Id, "pat-user-1")
	assert.Equal(t, http.StatusTooManyRequests, limitedResponse.Code)
	assert.Equal(t, "60", limitedResponse.Header().Get("Retry-After"))
	assert.JSONEq(t, `{"success":false,"code":"rate_limited","message":"Too many requests. Please retry later.","retry_after":60}`, limitedResponse.Body.String())
}
