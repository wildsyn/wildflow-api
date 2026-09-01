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
	taskdto "github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestMidjourneyProviderStartedFailClosed covers both independently billed
// Midjourney send entry points.  Each case uses a local HTTP server as the
// provider boundary, so a zero counter proves the production handler stopped
// before any outbound request was attempted.
func TestMidjourneyProviderStartedFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	entries := []struct {
		name  string
		call  func(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.MidjourneyResponse
		body  string
		model string
	}{
		{
			name:  "submit",
			call:  RelayMidjourneySubmit,
			body:  `{"prompt":"draw a test image"}`,
			model: "mj_imagine",
		},
		{
			name:  "swap face",
			call:  RelaySwapFace,
			body:  `{"sourceBase64":"source","targetBase64":"target"}`,
			model: "swap_face",
		},
	}

	faults := []struct {
		name         string
		install      func(t *testing.T, db *gorm.DB, requestID string)
		wantProvider int32
		wantSuccess  bool
	}{
		{
			name:         "successful marked dispatch",
			wantProvider: 1,
			wantSuccess:  true,
		},
		{
			name: "missing record",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER mj_remove_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN DELETE FROM billing_reservation_records WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantSuccess: false,
		},
		{
			name: "settled terminal competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER mj_settle_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'settled' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantSuccess: false,
		},
		{
			name: "released CAS-zero competition",
			install: func(t *testing.T, db *gorm.DB, requestID string) {
				t.Helper()
				require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER mj_release_reservation AFTER INSERT ON billing_reservation_records WHEN NEW.request_id = '%s' BEGIN UPDATE billing_reservation_records SET state = 'released' WHERE request_id = NEW.request_id; END`, requestID)).Error)
			},
			wantSuccess: false,
		},
		{
			name: "mark database error",
			install: func(t *testing.T, db *gorm.DB, _ string) {
				t.Helper()
				var failMark atomic.Bool
				failMark.Store(true)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("mj_provider_started_test_fail_mark", func(tx *gorm.DB) {
					if failMark.Load() && tx.Statement.Schema != nil && tx.Statement.Schema.Name == "BillingReservationRecord" {
						tx.AddError(errors.New("injected provider_started database error"))
					}
				}))
			},
			wantSuccess: false,
		},
	}

	for _, entry := range entries {
		for _, fault := range faults {
			t.Run(entry.name+"/"+fault.name, func(t *testing.T) {
				db := setupTaskDispatchTestDB(t)
				requestID := fmt.Sprintf("mj-provider-started-%d", time.Now().UnixNano())
				if fault.install != nil {
					fault.install(t, db, requestID)
				}

				var providerCalls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					providerCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":1,"result":"provider-task"}`))
				}))
				t.Cleanup(provider.Close)

				userID, tokenID, tokenKey := seedTaskDispatchBilling(t, db)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/mj/submit", bytes.NewBufferString(entry.body))
				ctx.Request.Header.Set("Content-Type", "application/json")
				ctx.Set("base_url", provider.URL)
				ctx.Set("channel_id", 1)
				common.SetContextKey(ctx, constant.ContextKeyChannelBaseUrl, provider.URL)
				common.SetContextKey(ctx, constant.ContextKeyChannelId, 1)
				common.SetContextKey(ctx, constant.ContextKeyChannelKey, "test-provider-key")

				info := &relaycommon.RelayInfo{
					RequestId:       requestID,
					UserId:          userID,
					TokenId:         tokenID,
					TokenKey:        tokenKey,
					UserGroup:       "default",
					UsingGroup:      "default",
					OriginModelName: entry.model,
					StartTime:       time.Now(),
					RelayMode:       relayconstant.RelayModeMidjourneyImagine,
					TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
				}
				if entry.name == "swap face" {
					info.RelayMode = relayconstant.RelayModeSwapFace
				}

				response := entry.call(ctx, info)
				if fault.wantSuccess {
					require.Nil(t, response)
				} else {
					require.NotNil(t, response)
					assert.Equal(t, 4, response.Code)
				}
				assert.Equal(t, fault.wantProvider, providerCalls.Load())
			})
		}
	}
}
