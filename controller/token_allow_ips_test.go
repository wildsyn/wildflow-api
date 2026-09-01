package controller

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenAllowIpsBody struct {
	AllowIps string `json:"allow_ips"`
}

func newTokenPayload(id int, name string, allowIps string) map[string]any {
	return map[string]any{
		"id":                   id,
		"name":                 name,
		"expired_time":         -1,
		"remain_quota":         100,
		"unlimited_quota":      true,
		"model_limits_enabled": false,
		"model_limits":         "",
		"group":                "default",
		"cross_group_retry":    false,
		"allow_ips":            allowIps,
	}
}

func TestAddTokenAcceptsEmptyAndValidAllowIps(t *testing.T) {
	setupTokenControllerTestDB(t)

	nameCounter := 0
	tests := []struct {
		name     string
		allowIps string
	}{
		{name: "empty list means no restriction", allowIps: ""},
		{name: "single IPv4", allowIps: "192.168.1.1"},
		{name: "IPv4 CIDR", allowIps: "10.0.0.0/8"},
		{name: "IPv6 address", allowIps: "2001:db8::1"},
		{name: "IPv6 CIDR", allowIps: "fe80::/10"},
		{name: "mixed lines and commas", allowIps: "10.0.0.1,192.168.0.0/16\nfd00::/8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nameCounter++
			tokenName := "ips-" + strconv.Itoa(nameCounter)
			ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", newTokenPayload(0, tokenName, test.allowIps), 1)
			AddToken(ctx)
			response := decodeAPIResponse(t, recorder)
			require.True(t, response.Success, "message: %s", response.Message)

			var stored model.Token
			require.NoError(t, model.DB.First(&stored, "name = ?", tokenName).Error)
			assert.Equal(t, test.allowIps, *stored.AllowIps)
		})
	}
}

func TestAddTokenRejectsInvalidAllowIps(t *testing.T) {
	setupTokenControllerTestDB(t)

	tests := map[string]string{
		"plain garbage":    "not-an-ip",
		"truncated IPv4":   "192.168.1",
		"out of range cid": "10.0.0.0/33",
		"host with port":   "192.168.1.1:8080",
		"bad IPv6 mask":    "fe80::/200",
	}
	for name, allowIps := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", newTokenPayload(0, "reject-"+allowIps, allowIps), 1)
			AddToken(ctx)
			response := decodeAPIResponse(t, recorder)
			require.False(t, response.Success)
			assert.Contains(t, response.Message, allowIps)

			var count int64
			require.NoError(t, model.DB.Model(&model.Token{}).Where("name = ?", "reject-"+allowIps).Count(&count).Error)
			assert.Equal(t, int64(0), count, "invalid allow_ips must not create a token")
		})
	}
}

func TestUpdateTokenRejectsInvalidAllowIps(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "update-ips-token", "updateips00001")
	original := "10.0.0.0/8"
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("allow_ips", original).Error)

	body := newTokenPayload(token.Id, "update-ips-token", "not-an-ip")
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, 1)
	UpdateToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.False(t, response.Success)
	assert.Contains(t, response.Message, "not-an-ip")

	var stored model.Token
	require.NoError(t, db.First(&stored, "id = ?", token.Id).Error)
	assert.Equal(t, original, *stored.AllowIps, "existing allow_ips must be preserved when the update is rejected")
}

func TestUpdateTokenAcceptsValidAllowIpsChange(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "rotate-ips-token", "rotateips0001")

	body := newTokenPayload(token.Id, "rotate-ips-token", "2001:db8::/32")
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, 1)
	UpdateToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "message: %s", response.Message)

	var stored model.Token
	require.NoError(t, db.First(&stored, "id = ?", token.Id).Error)
	assert.Equal(t, "2001:db8::/32", *stored.AllowIps)
}

func TestUpdateTokenWithoutAllowIpsKeepsBackwardCompatibility(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "legacy-ips-token", "legacyips0001")

	// Legacy clients do not send allow_ips; the field must stay untouched.
	body := newTokenPayload(token.Id, "legacy-ips-token", "")
	delete(body, "allow_ips")
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, 1)
	UpdateToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "message: %s", response.Message)

	var stored model.Token
	require.NoError(t, db.First(&stored, "id = ?", token.Id).Error)
	if stored.AllowIps != nil {
		assert.Equal(t, "", *stored.AllowIps)
	}
}
