package controller

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
	// The controller test process has no Redis fixture. Set these process-wide
	// test defaults before any Relay worker exists, then leave them unchanged:
	// detached metric workers may read them after a subtest returns.
	common.RedisEnabled = false

	cases := []struct {
		name          string
		install       func(t *testing.T, db *gorm.DB, requestID string)
		wantProvider  int32
		wantStatus    int
		refundQueries int32
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
			wantStatus:    http.StatusInternalServerError,
			refundQueries: 2, // mark's CAS miss lookup, then the asynchronous refund lookup
		},
		{
			name: "settled terminal competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER relay_settle_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'settled' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantStatus:    http.StatusInternalServerError,
			refundQueries: 2,
		},
		{
			name: "released CAS-zero competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER relay_release_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'released' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantStatus:    http.StatusInternalServerError,
			refundQueries: 2,
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
			wantStatus:    http.StatusInternalServerError,
			refundQueries: 1,
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
			// Keep the real billing path, but avoid an unrelated low-balance
			// notification worker. The fixture user starts with this quota too.
			common.SetContextKey(ctx, constant.ContextKeyUserQuota, 1_000)
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, normalDispatchTestModel)
			common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
			common.SetContextKey(ctx, constant.ContextKeyTokenKey, tokenKey)
			common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
			ctx.Set("channel_id", 1)
			ctx.Set("channel_type", constant.ChannelTypeOpenAI)
			ctx.Set("channel_name", "local-provider")
			ctx.Set("base_url", provider.URL)
			ctx.Set("channel_key", "test-provider-key")

			refundDone := waitForNormalRelayRefund(t, db, tc.refundQueries)

			Relay(ctx, types.RelayFormatOpenAI)
			if refundDone != nil {
				select {
				case <-refundDone:
				case <-time.After(3 * time.Second):
					t.Fatal("timed out waiting for the Relay refund worker")
				}
			}
			assert.Equal(t, tc.wantProvider, providerCalls.Load())
			assert.Equal(t, tc.wantStatus, recorder.Code)
		})
	}
}

// TestRelayUnknownProviderOutcomeKeepsReservationAndBlocksRequestReplay uses
// a real HTTP boundary that accepts the request then closes the connection
// without a response. The Provider may have acted, so neither the first
// failure nor a request-id replay may refund or dispatch again.
func TestRelayUnknownProviderOutcomeKeepsReservationAndBlocksRequestReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	db := setupNormalRelayTestDB(t)
	requestID := fmt.Sprintf("normal-provider-unknown-%d", time.Now().UnixNano())
	userID, tokenID, tokenKey := seedNormalRelayBilling(t, db)

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}))
	t.Cleanup(provider.Close)

	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"provider-started-normal-test","messages":[{"role":"user","content":"hello"}]}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		common.SetContextKey(ctx, common.RequestIdKey, requestID)
		common.SetContextKey(ctx, constant.ContextKeyUserId, userID)
		common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(ctx, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(ctx, constant.ContextKeyUserQuota, 1_000)
		common.SetContextKey(ctx, constant.ContextKeyOriginalModel, normalDispatchTestModel)
		common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
		common.SetContextKey(ctx, constant.ContextKeyTokenKey, tokenKey)
		common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
		ctx.Set("channel_id", 1)
		ctx.Set("channel_type", constant.ChannelTypeOpenAI)
		ctx.Set("channel_name", "local-provider")
		ctx.Set("base_url", provider.URL)
		ctx.Set("channel_key", "test-provider-key")
		return ctx, recorder
	}

	ctx, recorder := newContext()
	Relay(ctx, types.RelayFormatOpenAI)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, int32(1), providerCalls.Load())

	assertUnknownReservationAccounting(t, db, requestID, userID, tokenID)

	retryCtx, retryRecorder := newContext()
	Relay(retryCtx, types.RelayFormatOpenAI)
	require.Equal(t, http.StatusInternalServerError, retryRecorder.Code)
	assert.Equal(t, int32(1), providerCalls.Load(), "a request-id replay must fail closed before Provider dispatch")
	assertUnknownReservationAccounting(t, db, requestID, userID, tokenID)
}

// TestRelayUnsentProviderSetupFailureReleasesReservation verifies that a
// malformed upstream URL fails while building the request, before any HTTP
// transport attempt. That is a provably unsent result and must release both
// account and Key reservations.
func TestRelayUnsentProviderSetupFailureReleasesReservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	db := setupNormalRelayTestDB(t)
	requestID := fmt.Sprintf("normal-provider-unsent-%d", time.Now().UnixNano())
	userID, tokenID, tokenKey := seedNormalRelayBilling(t, db)

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusOK)
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
	common.SetContextKey(ctx, constant.ContextKeyUserQuota, 1_000)
	common.SetContextKey(ctx, constant.ContextKeyOriginalModel, normalDispatchTestModel)
	common.SetContextKey(ctx, constant.ContextKeyTokenId, tokenID)
	common.SetContextKey(ctx, constant.ContextKeyTokenKey, tokenKey)
	common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	ctx.Set("channel_id", 1)
	ctx.Set("channel_type", constant.ChannelTypeOpenAI)
	ctx.Set("channel_name", "malformed-local-provider")
	ctx.Set("base_url", "://not-a-valid-upstream-url")
	ctx.Set("channel_key", "test-provider-key")

	Relay(ctx, types.RelayFormatOpenAI)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Equal(t, int32(0), providerCalls.Load())

	require.Eventually(t, func() bool {
		var reservation model.BillingReservationRecord
		if db.Where("request_id = ?", requestID).First(&reservation).Error != nil || reservation.State != model.BillingReservationStateReleased {
			return false
		}
		var user model.User
		var token model.Token
		return db.First(&user, userID).Error == nil && db.First(&token, tokenID).Error == nil &&
			user.Quota == 1_000 && token.RemainQuota == 1_000 && token.UsedQuota == 0
	}, time.Second, 10*time.Millisecond, "a locally rejected Provider setup must refund exactly once")
}

func TestIsProvablyUnsentRelayError(t *testing.T) {
	for _, code := range []types.ErrorCode{
		types.ErrorCodeInvalidApiType,
		types.ErrorCodeChannelModelMappedError,
		types.ErrorCodeChannelParamOverrideInvalid,
		types.ErrorCodeChannelHeaderOverrideInvalid,
		types.ErrorCodeConvertRequestFailed,
	} {
		assert.True(t, isProvablyUnsentRelayError(types.NewError(errors.New("local request setup failed"), code)), code)
	}
	assert.False(t, isProvablyUnsentRelayError(types.NewError(errors.New("transport failed"), types.ErrorCodeDoRequestFailed)))
}

func assertUnknownReservationAccounting(t *testing.T, db *gorm.DB, requestID string, userID, tokenID int) {
	t.Helper()
	var reservation model.BillingReservationRecord
	require.NoError(t, db.Where("request_id = ?", requestID).First(&reservation).Error)
	assert.Equal(t, model.BillingReservationStateProviderStarted, reservation.State)

	var user model.User
	require.NoError(t, db.First(&user, userID).Error)
	var token model.Token
	require.NoError(t, db.First(&token, tokenID).Error)
	assert.Less(t, user.Quota, 1_000, "unknown Provider result must retain the account reservation")
	assert.Equal(t, user.Quota, token.RemainQuota, "account and Key must retain the same reservation")
	assert.Equal(t, 1_000-user.Quota, token.UsedQuota, "Key usage must be charged exactly once")
}

func setupNormalRelayTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	oldDB, oldLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
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

// waitForNormalRelayRefund observes the reservation queries that occur after
// Relay has begun. Mark failures that reached the fallback lookup perform one
// query before Refund's worker performs its own lookup; a direct update error
// only has the latter. Waiting for that exact second query keeps global DB and
// cache flags alive until the asynchronous refund has finished its DB work.
func waitForNormalRelayRefund(t *testing.T, db *gorm.DB, queries int32) <-chan struct{} {
	t.Helper()
	if queries == 0 {
		return nil
	}
	done := make(chan struct{})
	var seen atomic.Int32
	var once sync.Once
	callbackName := "normal_relay_provider_started_test_wait_for_refund_" + t.Name()
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "BillingReservationRecord" {
			return
		}
		if seen.Add(1) == queries {
			once.Do(func() { close(done) })
		}
	}))
	return done
}

func seedNormalRelayBilling(t *testing.T, db *gorm.DB) (int, int, string) {
	t.Helper()
	user := &model.User{Username: "normal-relay-user-" + t.Name(), Quota: 1_000, Group: "default", AffCode: "normal-relay-aff-" + t.Name()}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "sk-normal-relay-" + t.Name(), Name: "normal-relay-token", Status: common.TokenStatusEnabled, RemainQuota: 1_000}
	require.NoError(t, db.Create(token).Error)
	return user.Id, token.Id, token.Key
}
