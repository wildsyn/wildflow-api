package model

import (
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedValidateTokenFixture(t *testing.T, key string, mutate func(*Token)) *Token {
	t.Helper()
	allowIps := ""
	token := &Token{
		UserId: 77, Name: "validate-" + key, Key: key,
		Status: common.TokenStatusEnabled, CreatedTime: time.Now().Unix(),
		AccessedTime: time.Now().Unix(), ExpiredTime: -1,
		RemainQuota: 100000, AllowIps: &allowIps, Group: "default",
	}
	if mutate != nil {
		mutate(token)
	}
	require.NoError(t, DB.Create(token).Error)
	return token
}

func TestValidateUserTokenDistinguishesRejectionReasons(t *testing.T) {
	truncateTables(t)

	seedValidateTokenFixture(t, "validkey0000001", nil)
	seedValidateTokenFixture(t, "expiredkey000001", func(token *Token) {
		token.ExpiredTime = time.Now().Add(-time.Hour).Unix()
	})
	seedValidateTokenFixture(t, "disabledkey00001", func(token *Token) {
		token.Status = common.TokenStatusDisabled
	})
	seedValidateTokenFixture(t, "exhaustedkey0001", func(token *Token) {
		token.RemainQuota = 0
	})
	seedValidateTokenFixture(t, "persistedexp0001", func(token *Token) {
		token.Status = common.TokenStatusExpired
		token.ExpiredTime = time.Now().Add(-time.Hour).Unix()
	})
	seedValidateTokenFixture(t, "persistedexh0001", func(token *Token) {
		token.Status = common.TokenStatusExhausted
		token.RemainQuota = 0
	})

	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		{name: "missing key", key: "", wantErr: ErrTokenNotProvided},
		{name: "unknown key", key: "unknownkey00001", wantErr: ErrTokenNotFound},
		{name: "expired by time", key: "expiredkey000001", wantErr: ErrTokenExpired},
		{name: "persisted expired status", key: "persistedexp0001", wantErr: ErrTokenExpired},
		{name: "manually disabled", key: "disabledkey00001", wantErr: ErrTokenDisabled},
		{name: "exhausted by remaining quota", key: "exhaustedkey0001", wantErr: ErrTokenQuotaExhausted},
		{name: "persisted exhausted status", key: "persistedexh0001", wantErr: ErrTokenQuotaExhausted},
		{name: "valid key", key: "validkey0000001", wantErr: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, err := ValidateUserToken(test.key)
			if test.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, "validkey0000001", token.Key)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, test.wantErr), "expected %v, got %v", test.wantErr, err)
		})
	}
}

func TestValidateUserTokenUnknownKeyIsNotAStateError(t *testing.T) {
	truncateTables(t)

	_, err := ValidateUserToken("nosuchkey00001")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrTokenExpired))
	assert.False(t, errors.Is(err, ErrTokenDisabled))
	assert.False(t, errors.Is(err, ErrTokenQuotaExhausted))
	assert.ErrorIs(t, err, ErrTokenNotFound)
}
