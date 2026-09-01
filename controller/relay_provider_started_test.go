package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const normalDispatchTestModel = "provider-started-normal-test"

// TestRelayProviderStartedFailClosed sends a normal OpenAI Relay request
// through Controller.Relay. The local HTTP server is the real outbound
// boundary; a mark failure must leave its call counter at zero.
func TestRelayProviderStartedFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name         string
		install      func(t *testing.T, db *gorm.DB, requestID string)
		wantProvider int32
		wantStatus   int
	}{
		{
			name:         "successful marked dispatch reaches provider once",
			wantProvider: 1,
			wantStatus:   http.StatusOK,
		},
		{
			name: "missing reservation",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER relay_remove_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN DELETE FROM billing_reservation_records WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "settled terminal competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER relay_settle_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'settled' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "released CAS-zero competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER relay_release_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'released' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "mark database error",
			install: func(t *testing.T, db *gorm.DB, _ string) {
				t.Helper()
				var failMark atomic.Bool
				failMark.Store(true)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("normal_relay_provider_started_test_fail_mark", func(tx *gorm.DB) {
					if failMark.Load() && tx.Statement.Schema != nil && tx.Statement.Schema.Name == "BillingReservationRecord" {
						tx.AddError(errors.New("injected provider_started database error"))
					}
				}))
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupNormalRelayTestDB(t)
			requestID := fmt.Sprintf("normal-provider-started-%d", time.Now().UnixNano())
			if tc.install != nil {
				tc.install(t, db, requestID)
			}
			userID, tokenID, tokenKey := seedNormalRelayBilling(t, db)

			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"provider-response","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			t.Cleanup(provider.Close)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"provider-started-normal-test","messages":[{"role":"user","content":"hello"}]}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(ctx, common.RequestIdKey, requestID)
			common.SetContextKey(ctx, constant.ContextKeyUserId, userID)
			common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, normalDispatchTestModel)
			common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
			common.SetContextKey(ctx, constant.ContextKeyTokenKey, tokenKey)
			common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
			ctx.Set("channel_id", 1)
			ctx.Set("channel_type", constant.ChannelTypeOpenAI)
			ctx.Set("channel_name", "local-provider")
			ctx.Set("base_url", provider.URL)
			ctx.Set("channel_key", "test-provider-key")

			Relay(ctx, types.RelayFormatOpenAI)
			assert.Equal(t, tc.wantProvider, providerCalls.Load())
			assert.Equal(t, tc.wantStatus, recorder.Code)
		})
	}
}

func setupNormalRelayTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldLogConsume := common.RedisEnabled, common.LogConsumeEnabled
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	common.LogConsumeEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, common.LogConsumeEnabled = oldRedis, oldLogConsume
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.BillingReservationRecord{}))

	savedPrices := ratio_setting.ModelPrice2JSONString()
	var prices map[string]float64
	require.NoError(t, common.Unmarshal([]byte(savedPrices), &prices))
	prices[normalDispatchTestModel] = 0.00002
	priceBytes, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceBytes)))
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrices))
	})
	return db
}

func seedNormalRelayBilling(t *testing.T, db *gorm.DB) (int, int, string) {
	t.Helper()
	user := &model.User{Username: "normal-relay-user-" + t.Name(), Quota: 1_000, Group: "default", AffCode: "normal-relay-aff-" + t.Name()}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "sk-normal-relay-" + t.Name(), Name: "normal-relay-token", Status: common.TokenStatusEnabled, RemainQuota: 1_000}
	require.NoError(t, db.Create(token).Error)
	return user.Id, token.Id, token.Key
}
