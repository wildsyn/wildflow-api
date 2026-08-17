package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCProvider_GetName(t *testing.T) {
	settings := system_setting.GetOIDCSettings()
	originalDisplayName := settings.DisplayName
	defer func() { settings.DisplayName = originalDisplayName }()

	p := &OIDCProvider{}

	settings.DisplayName = ""
	assert.Equal(t, "OIDC", p.GetName())

	settings.DisplayName = "  Acme SSO  "
	assert.Equal(t, "Acme SSO", p.GetName())
}

func TestOIDCProviderRequiresVerifiedEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"authentik-user","email":"user@example.com","email_verified":false}`))
	}))
	defer server.Close()

	settings := system_setting.GetOIDCSettings()
	original := *settings
	settings.UserInfoEndpoint = server.URL
	t.Cleanup(func() { *settings = original })

	_, err := (&OIDCProvider{}).GetUserInfo(context.Background(), &OAuthToken{AccessToken: "access-token"})
	require.Error(t, err)
}

func TestOIDCProviderCarriesTrustedLegacyUsername(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"authentik-user","email":"USER@example.com","email_verified":true,"preferred_username":"profile-name","legacy_username":"legacy-owner"}`))
	}))
	defer server.Close()

	settings := system_setting.GetOIDCSettings()
	original := *settings
	settings.UserInfoEndpoint = server.URL
	t.Cleanup(func() { *settings = original })

	user, err := (&OIDCProvider{}).GetUserInfo(context.Background(), &OAuthToken{AccessToken: "access-token"})
	require.NoError(t, err)
	assert.Equal(t, "user@example.com", user.Email)
	assert.Equal(t, "profile-name", user.Username)
	assert.Equal(t, "legacy-owner", user.Extra["legacy_username"])
}
