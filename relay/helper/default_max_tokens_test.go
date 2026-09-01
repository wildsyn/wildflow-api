package helper

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTextRequestOmittedMaxTokensUsesExecutableBillingLimit(t *testing.T) {
	c := defaultMaxTokensTestContext(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	request, err := GetAndValidateTextRequest(c, relayconstant.RelayModeChatCompletions)
	require.NoError(t, err)
	require.NotNil(t, request.MaxTokens)
	require.EqualValues(t, defaultStandardPreConsumeMaxTokens, *request.MaxTokens)
	require.Equal(t, defaultStandardPreConsumeMaxTokens, request.GetTokenCountMeta().MaxTokens)

	body, err := common.Marshal(request)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":8192}`, string(body))
}

func TestResponsesRequestOmittedMaxOutputTokensUsesExecutableBillingLimit(t *testing.T) {
	c := defaultMaxTokensTestContext(`{"model":"gpt-4o","input":"hi"}`)

	request, err := GetAndValidateResponsesRequest(c)
	require.NoError(t, err)
	require.NotNil(t, request.MaxOutputTokens)
	require.EqualValues(t, defaultStandardPreConsumeMaxTokens, *request.MaxOutputTokens)
	require.Equal(t, defaultStandardPreConsumeMaxTokens, request.GetTokenCountMeta().MaxTokens)

	body, err := common.Marshal(request)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-4o","input":"hi","max_output_tokens":8192}`, string(body))
}

func TestExplicitMaxTokensArePreserved(t *testing.T) {
	c := defaultMaxTokensTestContext(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096}`)

	request, err := GetAndValidateTextRequest(c, relayconstant.RelayModeChatCompletions)
	require.NoError(t, err)
	require.Nil(t, request.MaxTokens)
	require.NotNil(t, request.MaxCompletionTokens)
	require.EqualValues(t, 4096, *request.MaxCompletionTokens)
}

func defaultMaxTokensTestContext(body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}
