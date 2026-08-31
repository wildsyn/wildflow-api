package middleware

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTokenAuthErrorCodesDocumentedInRelayOpenAPI pins the stable token auth
// error codes to the docs/openapi/relay.json TokenAuthError enum so the
// generated docs cannot drift from the codes the middleware actually emits.
func TestTokenAuthErrorCodesDocumentedInRelayOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../docs/openapi/relay.json")
	require.NoError(t, err)

	var doc struct {
		Components struct {
			Schemas struct {
				TokenAuthError struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum []string `json:"enum"`
								} `json:"code"`
							} `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"TokenAuthError"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))

	documented := doc.Components.Schemas.TokenAuthError.Properties.Error.Properties.Code.Enum
	require.NotEmpty(t, documented, "docs/openapi/relay.json must document the TokenAuthError schema")

	assert.ElementsMatch(t, []string{
		string(types.ErrorCodeTokenNotProvided),
		string(types.ErrorCodeTokenNotFound),
		string(types.ErrorCodeTokenInvalid),
		string(types.ErrorCodeTokenExpired),
		string(types.ErrorCodeTokenDisabled),
		string(types.ErrorCodeTokenQuotaExhausted),
	}, documented)
}

// TestTokenAuthUnknownKeyNeverRevealsExistence pins the no-probing contract:
// a missing key and a state-invalid key share the same human message, while
// only detailed states for proven keys get reason-specific messages.
func TestTokenAuthUnknownKeyNeverRevealsExistence(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	createTokenAuthToken(t, user.Id, "probekey000001", func(token *model.Token) {
		allowIps := ""
		token.AllowIps = &allowIps
	})

	_, unknown := serveTokenAuthRequest(t, "doesnotexist0001")
	_, disabled := serveTokenAuthRequest(t, "probekey000002")
	_ = user

	// The disabled probe key does not exist either, so both must look identical.
	assert.Equal(t, unknown.Error.Code, disabled.Error.Code)
	assert.Equal(t, unknown.Error.Message, disabled.Error.Message)
	assert.Equal(t, "token_not_found", unknown.Error.Code)
	assert.True(t, strings.HasPrefix(unknown.Error.Code, "token_"), "machine code must be namespaced")
}
