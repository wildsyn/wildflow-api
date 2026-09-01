package relay

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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const taskDispatchTestModel = "52_textgenerate"

// TestRelayTaskSubmitProviderStartedFailClosed exercises the real first-submit
// path with Vidu's production adaptor and a local HTTP server.  The server is
// the observable provider boundary: any request received there means billing
// allowed dispatch to proceed.
func TestRelayTaskSubmitProviderStartedFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name          string
		prepare       func(t *testing.T, db *gorm.DB, requestID string)
		wantProvider  int32
		wantTaskError bool
	}{
		{
			name:         "successful force-pre-consume dispatch reaches provider once",
			wantProvider: 1,
		},
		{
			name: "missing reservation fails closed",
			prepare: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER remove_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN DELETE FROM billing_reservation_records WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantTaskError: true,
		},
		{
			name: "settled terminal competition fails closed",
			prepare: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER settle_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'settled' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantTaskError: true,
		},
		{
			name: "released CAS-zero competition fails closed",
			prepare: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER release_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'released' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantTaskError: true,
		},
		{
			name: "mark database error fails closed",
			prepare: func(t *testing.T, db *gorm.DB, _ string) {
				t.Helper()
				var failMark atomic.Bool
				failMark.Store(true)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("task_provider_started_test_fail_mark", func(tx *gorm.DB) {
					if failMark.Load() && tx.Statement.Schema != nil && tx.Statement.Schema.Name == "BillingReservationRecord" {
						tx.AddError(errors.New("injected provider_started database error"))
					}
				}))
			},
			wantTaskError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTaskDispatchTestDB(t)
			requestID := fmt.Sprintf("task-provider-started-%d", time.Now().UnixNano())
			if tc.prepare != nil {
				tc.prepare(t, db, requestID)
			}

			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task_id":"provider-task","state":"created"}`))
			}))
			t.Cleanup(provider.Close)

			userID, tokenID, tokenKey := seedTaskDispatchBilling(t, db)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewBufferString(`{"prompt":"make a short test video"}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("platform", fmt.Sprintf("%d", constant.ChannelTypeVidu))
			ctx.Set("group", "default")
			common.SetContextKey(ctx, constant.ContextKeyChannelType, constant.ChannelTypeVidu)
			common.SetContextKey(ctx, constant.ContextKeyChannelId, 1)
			common.SetContextKey(ctx, constant.ContextKeyChannelBaseUrl, provider.URL)
			common.SetContextKey(ctx, constant.ContextKeyChannelKey, "test-provider-key")

			info := &relaycommon.RelayInfo{
				RequestId:     requestID,
				UserId:        userID,
				TokenId:       tokenID,
				TokenKey:      tokenKey,
				UserGroup:     "default",
				UsingGroup:    "default",
				StartTime:     time.Now(),
				UserSetting:   dto.UserSetting{BillingPreference: "wallet_only"},
				TaskRelayInfo: &relaycommon.TaskRelayInfo{},
			}

			_, taskErr := RelayTaskSubmit(ctx, info)
			if tc.wantTaskError {
				require.NotNil(t, taskErr)
			} else {
				require.Nil(t, taskErr)
				require.True(t, info.ForcePreConsume)
			}
			assert.Equal(t, tc.wantProvider, providerCalls.Load())
		})
	}
}

func setupTaskDispatchTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis := common.RedisEnabled
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled = oldRedis
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.Channel{},
		&model.Log{},
		&model.BillingReservationRecord{},
		&model.Midjourney{},
	))

	savedPrices := ratio_setting.ModelPrice2JSONString()
	var prices map[string]float64
	require.NoError(t, common.Unmarshal([]byte(savedPrices), &prices))
	prices[taskDispatchTestModel] = 0.00002
	prices["mj_imagine"] = 0.00002
	prices["swap_face"] = 0.00002
	priceBytes, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceBytes)))
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrices))
	})
	return db
}

func seedTaskDispatchBilling(t *testing.T, db *gorm.DB) (int, int, string) {
	t.Helper()
	user := &model.User{Username: "task-dispatch-user-" + t.Name(), Quota: 1_000, Group: "default", AffCode: "task-dispatch-aff-" + t.Name()}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "sk-task-dispatch-" + t.Name(), Name: "task-dispatch-token", Status: common.TokenStatusEnabled, RemainQuota: 1_000}
	require.NoError(t, db.Create(token).Error)
	return user.Id, token.Id, token.Key
}
