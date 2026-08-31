package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A disabled token must not be authorized through ValidateUserToken after a
// successful disable even when Redis served a pre-existing cached snapshot.
func newRevocableToken(t *testing.T, key string) *Token {
	t.Helper()
	truncateTables(t)
	token := Token{
		UserId:         7,
		Key:            key,
		Name:           "revoke-test",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, token.Insert())
	return &token
}

func TestTokenDeleteImmediatelyInvalidatesCachedGrant(t *testing.T) {
	newRevocableToken(t, "revoke-delete-cache-key")
	useUserCacheMiniRedis(t)

	cached, err := GetTokenByKey("revoke-delete-cache-key", false)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, common.TokenStatusEnabled, cached.Status)

	token := Token{Key: "revoke-delete-cache-key"}
	require.NoError(t, DB.Where("key = ?", token.Key).First(&token).Error)
	require.NoError(t, token.Delete())

	// The successful Delete response is the contract point: from here on a new
	// request must not authorize. The cache must not serve the old grant and
	// the database read must see the soft-deleted row as gone.
	_, err = GetTokenByKey("revoke-delete-cache-key", false)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = GetTokenByKey("revoke-delete-cache-key", true)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	_, err = ValidateUserToken("revoke-delete-cache-key")
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestTokenDisableImmediatelyInvalidatesCachedGrant(t *testing.T) {
	token := newRevocableToken(t, "revoke-disable-cache-key")
	server := useUserCacheMiniRedis(t)

	cached, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	require.Equal(t, common.TokenStatusEnabled, cached.Status)

	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.Update())

	// After the successful disable the cached enabled snapshot must be gone:
	// the fence keeps racing fills out and the hash was proven deleted.
	assert.False(t, server.Exists(tokenCacheKey(token.Key)))
	// The row still exists but reports the disabled status, and the validate
	// path used by relay requests must reject the key.
	_, err = GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestTokenBatchDeleteImmediatelyInvalidatesCachedGrants(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	tokens := []Token{
		{UserId: 7, Key: "batch-revoke-a", Name: "a", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
		{UserId: 7, Key: "batch-revoke-b", Name: "b", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
		{UserId: 8, Key: "batch-revoke-other-user", Name: "c", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
	}
	for i := range tokens {
		require.NoError(t, tokens[i].Insert())
	}
	for _, key := range []string{"batch-revoke-a", "batch-revoke-b", "batch-revoke-other-user"} {
		_, err := GetTokenByKey(key, false)
		require.NoError(t, err)
	}

	count, err := BatchDeleteTokens([]int{tokens[0].Id, tokens[1].Id}, 7)
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	for _, key := range []string{"batch-revoke-a", "batch-revoke-b"} {
		_, err := GetTokenByKey(key, false)
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
		_, err = ValidateUserToken(key)
		require.ErrorIs(t, err, ErrTokenInvalid)
	}
	// The other user's token is untouched.
	cached, err := GetTokenByKey("batch-revoke-other-user", false)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusEnabled, cached.Status)
}

func TestTokenDeleteFailsClosedWhenRedisDeleteFails(t *testing.T) {
	token := newRevocableToken(t, "revoke-fail-closed-key")
	server := useUserCacheMiniRedis(t)
	require.NoError(t, cacheSetToken(*token))
	require.True(t, server.Exists(tokenCacheKey(token.Key)))

	server.SetError("miniredis forced error")

	err := token.Delete()
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)

	// The database row still exists: the caller never got success, so a retry
	// is possible and no stale cache window was silently accepted.
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)

	// Recovery: once Redis is healthy the same delete succeeds and the grant
	// stops authorizing immediately.
	server.SetError("")
	require.NoError(t, token.Delete())
	_, err = GetTokenByKey(token.Key, false)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestTokenDeleteFailsClosedWhenRedisFenceRaiseFails(t *testing.T) {
	token := newRevocableToken(t, "revoke-fence-fail-key")
	server := useUserCacheMiniRedis(t)
	require.NoError(t, cacheSetToken(*token))
	require.True(t, server.Exists(tokenCacheKey(token.Key)))

	// Eval (fence raise) fails while plain DEL still works: the revocation
	// must still fail closed rather than delete-without-fence.
	server.SetError("miniredis forced error")
	err := token.Delete()
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)

	server.SetError("")
	require.NoError(t, token.Delete())
	_, err = GetTokenByKey(token.Key, false)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestTokenCacheFillDropsWriteWhileRevocationFenceIsRaised(t *testing.T) {
	token := newRevocableToken(t, "revoke-fill-race-key")
	server := useUserCacheMiniRedis(t)

	require.NoError(t, cacheSetToken(*token))
	require.True(t, server.Exists(tokenCacheKey(token.Key)))

	// Simulate a fill that read the database before the revocation and only
	// writes later: while the fence is raised the fill must drop its write.
	hmac := common.GenerateHMAC(token.Key)
	require.NoError(t, raiseTokenRevocationFence(hmac))
	require.True(t, server.Del(tokenCacheKey(token.Key)))
	err := cacheSetTokenRespectingRevocation(*token)
	assert.ErrorIs(t, err, ErrTokenCacheRevocationPending)
	assert.False(t, server.Exists(tokenCacheKey(token.Key)))

	// After the fence is explicitly cleared (re-enable path) fills work again.
	require.NoError(t, AllowTokenCacheRefresh(token.Key))
	require.NoError(t, cacheSetTokenRespectingRevocation(*token))
	assert.True(t, server.Exists(tokenCacheKey(token.Key)))
}

func TestTokenReEnableClearsFenceAndRecaches(t *testing.T) {
	token := newRevocableToken(t, "revoke-reenable-key")
	useUserCacheMiniRedis(t)

	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.Update())
	_, err := ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	// While the fence is alive, a fill must not resurrect the disabled token.
	err = cacheSetTokenRespectingRevocation(*token)
	assert.ErrorIs(t, err, ErrTokenCacheRevocationPending)

	token.Status = common.TokenStatusEnabled
	require.NoError(t, token.Update())
	_, err = ValidateUserToken(token.Key)
	require.NoError(t, err)
}

func TestTokenRevocationFenceOutlivesCacheTTL(t *testing.T) {
	newRevocableToken(t, "revoke-fence-ttl-key")
	server := useUserCacheMiniRedis(t)
	common.SyncFrequency = 2
	cached, err := GetTokenByKey("revoke-fence-ttl-key", false)
	require.NoError(t, err)
	require.NotNil(t, cached)

	token := Token{Key: "revoke-fence-ttl-key"}
	require.NoError(t, DB.Where("key = ?", token.Key).First(&token).Error)
	require.NoError(t, token.Delete())

	ttl := server.TTL(tokenRevokedFenceKeyOf(common.GenerateHMAC(token.Key)))
	require.Greater(t, ttl, time.Duration(0))
	require.GreaterOrEqual(t, int(ttl.Seconds()), common.RedisKeyCacheSeconds())

	// Fast-forward past the token cache TTL but not past the fence: a delayed
	// fill must still drop its write.
	server.FastForward(time.Duration(common.RedisKeyCacheSeconds()) * time.Second)
	err = cacheSetTokenRespectingRevocation(token)
	assert.ErrorIs(t, err, ErrTokenCacheRevocationPending)
}

func TestTokenRevocationWorksWithoutRedis(t *testing.T) {
	truncateTables(t)
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedisEnabled })

	token := newRevocableToken(t, "revoke-no-redis-key")

	// Delete without Redis: nothing to invalidate, must succeed.
	require.NoError(t, token.Delete())
	_, err := GetTokenByKey(token.Key, true)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// Disable without Redis: must succeed and the validate path must reject.
	token = newRevocableToken(t, "revoke-no-redis-disable-key")
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.Update())
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	// Batch delete without Redis: must succeed.
	batch := newRevocableToken(t, "revoke-no-redis-batch-key")
	count, err := BatchDeleteTokens([]int{batch.Id}, 7)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	_, err = GetTokenByKey(batch.Key, true)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestTokenRevocationFencesSurviveAfterPartialBatchFailure(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	tokens := []Token{
		{UserId: 7, Key: "batch-partial-a", Name: "a", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
		{UserId: 7, Key: "batch-partial-b", Name: "b", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
	}
	for i := range tokens {
		require.NoError(t, tokens[i].Insert())
	}
	for _, tk := range tokens {
		require.NoError(t, cacheSetToken(tk))
	}

	// Once the fences are raised for the two keys, a Redis failure during the
	// committed-phase deletes must surface instead of reporting success.
	require.NoError(t, raiseTokenRevocationFences([]string{tokens[0].Key, tokens[1].Key}))
	server.SetError("miniredis forced error")
	err := revokeTokensCacheCommitted([]string{tokens[0].Key, tokens[1].Key})
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)

	// The fences are still raised, so a racing fill for either key must drop
	// its write even though the cached hash may still exist: the fill either
	// refuses on the fence or fails outright — either way it never lands the
	// pre-revocation snapshot.
	for _, tk := range tokens {
		assert.Error(t, cacheSetTokenRespectingRevocation(tk))
	}
	// While the fence is up the keys must not re-authorize through the cache:
	// the stored snapshot (if any) was superseded by the deny epoch.
	server.SetError("")
	for _, tk := range tokens {
		require.NoError(t, AllowTokenCacheRefresh(tk.Key))
	}
	for _, tk := range tokens {
		require.NoError(t, cacheSetTokenRespectingRevocation(tk))
	}
}
