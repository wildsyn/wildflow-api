package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenAuthGuardedRelayPaths lists representative relay paths documented in
// relay.json that sit behind TokenAuth. Every one of them must answer an
// unauthenticated request with the documented 401 TokenAuthError shape.
var tokenAuthGuardedRelayPaths = []string{
	"/v1/models",
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/responses",
	"/v1/messages",
	"/v1/embeddings",
	"/v1/images/generations",
	"/v1/audio/speech",
	"/v1/audio/transcriptions",
	"/v1/moderations",
	"/v1/rerank",
	"/v1/video/generations",
	"/v1/videos",
	"/v1beta/models",
	"/kling/v1/videos/text2video",
	"/jimeng/",
}

// TestTokenAuthGuardedRelayRoutesReturnDocumented401 pins the per-route
// response contract required by ZXDW-210: every relay path behind TokenAuth
// answers a missing key with 401 and the exact TokenAuthError body shape
// (message/type/code) that relay.json documents.
func TestTokenAuthGuardedRelayRoutesReturnDocumented401(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	createTokenAuthToken(t, user.Id, "routecontractkey", nil)

	for _, path := range tokenAuthGuardedRelayPaths {
		t.Run(path, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { c.Next() })
			router.Use(TokenAuth())
			router.POST("/*any", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
			router.GET("/*any", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			// No Authorization header at all: the request must be rejected
			// before reaching any handler.
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusUnauthorized, response.Code, "path %s", path)
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, "new_api_error", body.Error.Type)
			assert.Equal(t, string(types.ErrorCodeTokenNotProvided), body.Error.Code)
			assert.Contains(t, body.Error.Message, common.TranslateMessage(nil, "token.not_provided"))
		})
	}
}

// TestTokenAuthReadOnlyDocumentsStableCodes locks /api/usage/token/ and
// /api/log/token 401 semantics: stable machine codes with the same generic
// message for unknown keys, so read-only endpoints cannot probe key existence.
func TestTokenAuthReadOnlyDocumentsStableCodes(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	createTokenAuthToken(t, user.Id, "readonlykey0001", nil)
	disabledAllow := ""
	createTokenAuthToken(t, user.Id, "readonlykey0002", func(token *model.Token) {
		token.Status = common.TokenStatusDisabled
		token.AllowIps = &disabledAllow
	})

	tests := []struct {
		name     string
		auth     string
		wantCode string
		wantMsg  string
	}{
		{name: "missing header", auth: "", wantCode: "token_not_provided", wantMsg: common.TranslateMessage(nil, "token.not_provided")},
		{name: "unknown key", auth: "Bearer missingkey00001", wantCode: "token_invalid", wantMsg: common.TranslateMessage(nil, "token.invalid")},
		{name: "disabled key", auth: "Bearer readonlykey0002", wantCode: "token_disabled", wantMsg: common.TranslateMessage(nil, "token.disabled")},
		{name: "unknown key generic message identical to probe", auth: "Bearer readonlykey0009", wantCode: "token_invalid", wantMsg: common.TranslateMessage(nil, "token.invalid")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/api/usage/token/", TokenAuthReadOnly(), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})
			request := httptest.NewRequest(http.MethodGet, "/api/usage/token/", nil)
			if test.auth != "" {
				request.Header.Set("Authorization", test.auth)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, http.StatusUnauthorized, response.Code)
			var body struct {
				Success bool   `json:"success"`
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.False(t, body.Success)
			assert.Equal(t, test.wantCode, body.Code)
			assert.Contains(t, body.Message, test.wantMsg)
		})
	}
}

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
				TokenAuthForbiddenError struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum     []string `json:"enum"`
									Nullable bool     `json:"nullable"`
								} `json:"code"`
							} `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"TokenAuthForbiddenError"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))

	documented := doc.Components.Schemas.TokenAuthError.Properties.Error.Properties.Code.Enum
	require.NotEmpty(t, documented, "docs/openapi/relay.json must document the TokenAuthError schema")
	assert.ElementsMatch(t, []string{
		string(types.ErrorCodeTokenNotProvided),
		string(types.ErrorCodeTokenInvalid),
		string(types.ErrorCodeTokenExpired),
		string(types.ErrorCodeTokenDisabled),
		string(types.ErrorCodeTokenQuotaExhausted),
	}, documented)

	assert.True(t, doc.Components.Schemas.TokenAuthForbiddenError.Properties.Error.Properties.Code.Nullable)
	assert.ElementsMatch(t, []string{"", string(types.ErrorCodeAccessDenied)},
		doc.Components.Schemas.TokenAuthForbiddenError.Properties.Error.Properties.Code.Enum)

	// 401 (token auth failure) and 403 (TokenAuth authorization failure) must
	// be documented with their distinct schemas on EVERY operation relay.json
	// describes. Every path in relay.json sits behind TokenAuth (each op also
	// declares BearerAuth security), so iterate the document instead of a
	// hand-maintained list — a list can drift when new routes are added and
	// silently skip exactly the paths that need the contract.
	require.NotEmpty(t, doc.Paths, "docs/openapi/relay.json must document relay paths")
	for path, ops := range doc.Paths {
		require.NotEmpty(t, ops, "relay.json must document at least one operation for %s", path)
		for method, op := range ops {
			require.Contains(t, op.Responses, "401", "%s %s must document 401", method, path)
			require.Contains(t, op.Responses, "403", "%s %s must document 403", method, path)
			assert.Equal(t, "#/components/schemas/TokenAuthError",
				op.Responses["401"].Content["application/json"].Schema.Ref, "%s %s", method, path)
			assert.Equal(t, "#/components/schemas/TokenAuthForbiddenError",
				op.Responses["403"].Content["application/json"].Schema.Ref, "%s %s", method, path)
		}
	}
}

// TestTokenAuthForbiddenResponsesMatchRelayOpenAPI verifies real 403 bodies
// against the distinct schema's code contract. TokenAuth has both the
// allow_ips-specific access_denied response and legacy authorization branches
// whose code is intentionally empty.
func TestTokenAuthForbiddenResponsesMatchRelayOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../docs/openapi/relay.json")
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas struct {
				TokenAuthForbiddenError struct {
					Properties struct {
						Error struct {
							Properties struct {
								Type struct {
									Enum []string `json:"enum"`
								} `json:"type"`
								Code struct {
									Enum []string `json:"enum"`
								} `json:"code"`
							} `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"TokenAuthForbiddenError"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	schema := doc.Components.Schemas.TokenAuthForbiddenError.Properties.Error.Properties

	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	allowIps := "10.0.0.0/8"
	ipToken := createTokenAuthToken(t, user.Id, "forbiddenschemaip", func(token *model.Token) {
		token.AllowIps = &allowIps
	})
	userToken := createTokenAuthToken(t, user.Id, "forbiddenschemauser", nil)

	ipResponse, ipBody := serveTokenAuthRequestFrom(t, ipToken.Key, "203.0.113.7:12345")
	require.Equal(t, http.StatusForbidden, ipResponse.Code)

	require.NoError(t, model.DB.Model(user).Update("status", common.UserStatusDisabled).Error)
	userResponse, userBody := serveTokenAuthRequest(t, userToken.Key)
	require.Equal(t, http.StatusForbidden, userResponse.Code)

	for name, body := range map[string]tokenAuthErrorResponse{
		"allow_ips":     ipBody,
		"disabled_user": userBody,
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, body.Error.Message)
			assert.Contains(t, schema.Type.Enum, body.Error.Type)
			assert.Contains(t, schema.Code.Enum, body.Error.Code)
		})
	}
	assert.Equal(t, string(types.ErrorCodeAccessDenied), ipBody.Error.Code)
	assert.Equal(t, "", userBody.Error.Code)
}

// TestTokenAuthUnknownKeyNeverRevealsExistence pins the no-probing contract:
// a missing key and a state-invalid key share the same human message, while
// only detailed states for proven keys get reason-specific messages.
func TestTokenAuthUnknownKeyNeverRevealsExistence(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	createTokenAuthToken(t, user.Id, "ordinaryinvalid2", func(token *model.Token) {
		token.Status = 99
	})

	unknownResponse, unknown := serveTokenAuthRequest(t, "doesnotexist0001")
	invalidResponse, invalid := serveTokenAuthRequest(t, "ordinaryinvalid2")

	assert.JSONEq(t, invalidResponse.Body.String(), unknownResponse.Body.String())
	assert.Equal(t, unknown.Error.Code, invalid.Error.Code)
	assert.Equal(t, unknown.Error.Message, invalid.Error.Message)
	assert.Equal(t, "token_invalid", unknown.Error.Code)
	assert.True(t, strings.HasPrefix(unknown.Error.Code, "token_"), "machine code must be namespaced")
}

var _ = model.Token{}
