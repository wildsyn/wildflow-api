package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveTokenAuthRequestFrom exercises the full TokenAuth chain from a given
// client address, which is what an allow_ips list is enforced against.
func serveTokenAuthRequestFrom(t *testing.T, key string, remoteAddr string) (*httptest.ResponseRecorder, tokenAuthErrorResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.SetTrustedProxies(nil)
	router.GET("/protected", TokenAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.GetInt("id")})
	})
	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.RemoteAddr = remoteAddr
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	var body tokenAuthErrorResponse
	if response.Body.Len() > 0 {
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	}
	return response, body
}

// TestTokenAuthAllowsCommaSeparatedListEndToEnd pins the ZXDW-203 finding-1
// regression: a comma-separated allow_ips that passed write-time validation
// must be enforced with the same entries, not merged into a bogus address
// that silently rejects legitimate callers.
func TestTokenAuthAllowsCommaSeparatedListEndToEnd(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	allowIps := "10.0.0.1,192.168.0.0/16"
	token := createTokenAuthToken(t, user.Id, "commalistkey01", func(token *model.Token) {
		token.AllowIps = &allowIps
	})

	response, body := serveTokenAuthRequestFrom(t, token.Key, "10.0.0.1:12345")
	assert.Equal(t, http.StatusOK, response.Code, "first comma entry must be allowed, got: %s", body.Error.Message)

	response, _ = serveTokenAuthRequestFrom(t, token.Key, "192.168.44.5:12345")
	assert.Equal(t, http.StatusOK, response.Code, "comma-listed CIDR must be allowed")

	response, body = serveTokenAuthRequestFrom(t, token.Key, "203.0.113.7:12345")
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Equal(t, "access_denied", body.Error.Code)
}

func TestTokenAuthAllowsNewlineSeparatedListEndToEnd(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	allowIps := "10.0.0.1\n192.168.0.0/16\n2001:db8::/32"
	token := createTokenAuthToken(t, user.Id, "newlinelistkey", func(token *model.Token) {
		token.AllowIps = &allowIps
	})

	response, _ := serveTokenAuthRequestFrom(t, token.Key, "10.0.0.1:12345")
	assert.Equal(t, http.StatusOK, response.Code)

	response, _ = serveTokenAuthRequestFrom(t, token.Key, "192.168.9.9:12345")
	assert.Equal(t, http.StatusOK, response.Code)

	response, body := serveTokenAuthRequestFrom(t, token.Key, "203.0.113.7:12345")
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Equal(t, "access_denied", body.Error.Code)
}

// TestTokenAuthSkipsLegacyInvalidEntries covers rows written before write-time
// validation existed: invalid entries are skipped instead of being merged with
// neighbors, so valid entries in the same list keep working.
func TestTokenAuthSkipsLegacyInvalidEntries(t *testing.T) {
	setupTokenAuthTest(t)
	user := createTokenAuthUser(t)
	// "10.0.0.1,not-an-ip" merged under the old parser became 10.0.0.1not-an-ip.
	allowIps := "not-an-ip\n10.0.0.5"
	token := createTokenAuthToken(t, user.Id, "legacydirtykey", func(token *model.Token) {
		token.AllowIps = &allowIps
	})

	response, _ := serveTokenAuthRequestFrom(t, token.Key, "10.0.0.5:12345")
	assert.Equal(t, http.StatusOK, response.Code, "valid neighbor entry must still be enforced")

	response, body := serveTokenAuthRequestFrom(t, token.Key, "10.0.0.6:12345")
	assert.Equal(t, http.StatusForbidden, response.Code)
	assert.Equal(t, "access_denied", body.Error.Code)
}
