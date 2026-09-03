package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPassThroughMaxTokensReachesProvider(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name      string
		body      string
		field     string
		value     *uint
		relayMode int
		expected  string
	}{
		{
			name:      "global chat pass-through injects max_tokens",
			body:      `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"vendor_option":true}`,
			field:     "max_tokens",
			value:     basecommon.GetPointer(uint(8192)),
			relayMode: relayconstant.RelayModeChatCompletions,
			expected:  `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"vendor_option":true,"max_tokens":8192}`,
		},
		{
			name:      "channel responses pass-through injects max_output_tokens",
			body:      `{"model":"gpt-4o","input":"hi","vendor_option":true}`,
			field:     "max_output_tokens",
			value:     basecommon.GetPointer(uint(8192)),
			relayMode: relayconstant.RelayModeResponses,
			expected:  `{"model":"gpt-4o","input":"hi","vendor_option":true,"max_output_tokens":8192}`,
		},
		{
			name:      "explicit zero is preserved",
			body:      `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
			field:     "max_tokens",
			value:     basecommon.GetPointer(uint(0)),
			relayMode: relayconstant.RelayModeChatCompletions,
			expected:  `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				assert.JSONEq(t, tt.expected, string(body))
				w.WriteHeader(http.StatusOK)
			}))
			defer provider.Close()

			storage, err := basecommon.CreateBodyStorage([]byte(tt.body))
			require.NoError(t, err)
			defer storage.Close()
			requestBody, closer, err := relaycommon.NewPassThroughJSONBody(storage, tt.field, tt.value)
			require.NoError(t, err)
			if closer != nil {
				defer closer.Close()
			}

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayMode:      tt.relayMode,
				RequestURLPath: "/v1/responses",
				ChannelMeta:    &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: provider.URL, ApiKey: "test-key"},
			}

			resp, err := (&Adaptor{}).DoRequest(ctx, info, requestBody)
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.NoError(t, resp.(*http.Response).Body.Close())
			assert.Equal(t, int32(1), providerCalls.Load())
		})
	}
}
