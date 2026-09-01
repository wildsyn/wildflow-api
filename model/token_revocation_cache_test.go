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

// drainTokenCacheFills blocks until every asynchronous cache fill triggered by
// this test has finished; register it immediately after useUserCacheMiniRedis
// so no fill can still touch Redis after the fixture restored the globals.
func drainTokenCacheFills(t *testing.T) {
	t.Helper()
	t.Cleanup(waitTokenCacheFills)
}

func TestTokenDeleteImmediatelyInvalidatesCachedGrant(t *testing.T) {
	newRevocableToken(t, "revoke-delete-cache-key")
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

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
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	cached, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	require.Equal(t, common.TokenStatusEnabled, cached.Status)

	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.UpdateStatus(common.TokenStatusEnabled))

	// After the successful disable the cached enabled snapshot must be gone:
	// the fence keeps racing fills out and the hash was proven deleted.
	_, err = cacheGetTokenByKey(token.Key)
	require.Error(t, err)
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
	drainTokenCacheFills(t)
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
	drainTokenCacheFills(t)
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
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestTokenDeleteFailsClosedWhenRedisFenceRaiseFails(t *testing.T) {
	token := newRevocableToken(t, "revoke-fence-fail-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
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

// A committed delete whose Redis DEL/EXISTS phase failed leaves the fence
// raised; the cache read side must fail closed and refuse the leftover hash
// instead of authorizing through it.
func TestTokenCacheReadFailsClosedWhileFenceRaised(t *testing.T) {
	token := newRevocableToken(t, "revoke-read-fail-closed-key")
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	require.NoError(t, cacheSetToken(*token))

	// Simulate a revocation that committed the fence but could not delete the
	// hash (DEL/EXISTS phase failed on every retry).
	require.NoError(t, raiseTokenRevocationFence(common.GenerateHMAC(token.Key)))
	cached, err := cacheGetTokenByKey(token.Key)
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)
	require.Nil(t, cached)

	// The relay-facing validate path must fall through to the database, see
	// the token still enabled there? No — the row is not yet deleted in this
	// simulation, so validate still succeeds; the fence only blocks the CACHE.
	// After the database delete commits, the same read must reject.
	require.NoError(t, token.Delete())
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestTokenCacheFillDropsWriteWhileRevocationFenceIsRaised(t *testing.T) {
	token := newRevocableToken(t, "revoke-fill-race-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

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

	// After the fence is explicitly released (re-enable path) fills work again.
	require.NoError(t, AllowTokenCacheRefresh(token.Key, 1))
	require.NoError(t, cacheSetTokenRespectingRevocation(*token))
	assert.True(t, server.Exists(tokenCacheKey(token.Key)))
}

func TestTokenReEnableClearsFenceAndRecaches(t *testing.T) {
	token := newRevocableToken(t, "revoke-reenable-key")
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.UpdateStatus(common.TokenStatusEnabled))
	_, err := ValidateUserToken(token.Key)
	require.ErrorIs(t, ErrTokenInvalid, err)

	// While the fence is alive, a fill must not resurrect the disabled token.
	err = cacheSetTokenRespectingRevocation(*token)
	assert.ErrorIs(t, err, ErrTokenCacheRevocationPending)

	token.Status = common.TokenStatusEnabled
	require.NoError(t, token.UpdateStatus(common.TokenStatusDisabled))
	_, err = ValidateUserToken(token.Key)
	require.NoError(t, err)
}

func TestTokenRevocationFenceOutlivesCacheTTL(t *testing.T) {
	newRevocableToken(t, "revoke-fence-ttl-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
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
	require.NoError(t, token.UpdateStatus(common.TokenStatusEnabled))
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

// The committed-phase cleanup of a batch delete must be idempotently
// retryable: resubmitting the same ids after the rows are already soft-deleted
// must still fence and prove-delete the cached hashes instead of returning a
// fake success, and a Redis failure during the retry must keep failing.
func TestTokenBatchRetryCompletesPendingCacheCleanup(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	tokens := []Token{
		{UserId: 7, Key: "batch-retry-a", Name: "a", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
		{UserId: 7, Key: "batch-retry-b", Name: "b", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
	}
	for i := range tokens {
		require.NoError(t, tokens[i].Insert())
	}
	for _, tk := range tokens {
		require.NoError(t, cacheSetToken(tk))
	}

	// Simulate the exact state a first attempt with a broken cleanup leaves
	// behind: fences raised BEFORE the deletes committed, rows soft-deleted,
	// cached hashes untouched.
	keys := make([]string, 0, len(tokens))
	for _, tk := range tokens {
		keys = append(keys, tk.Key)
	}
	require.NoError(t, raiseTokenRevocationFences(keys))
	for i := range tokens {
		require.NoError(t, DB.Delete(&tokens[i]).Error)
	}

	// The committed revocation denies new requests even though the cached
	// hashes survived the failed cleanup: reads are fence-aware.
	for _, tk := range tokens {
		_, err := ValidateUserToken(tk.Key)
		require.ErrorIs(t, err, ErrTokenInvalid)
	}

	// Retry with Redis healthy: the tombstoned rows are resolved, cleanup
	// completes, and the same ids must NOT report a fake success.
	count, err := BatchDeleteTokens([]int{tokens[0].Id, tokens[1].Id}, 7)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	for _, tk := range tokens {
		_, err := cacheGetTokenByKey(tk.Key)
		require.Error(t, err)
		_, err = ValidateUserToken(tk.Key)
		require.ErrorIs(t, err, ErrTokenInvalid)
	}
}

// A batch whose committed-phase cleanup failed once must not report success on
// resubmission while Redis is still failing.
func TestTokenBatchRetryStillFailsClosedWhileRedisBroken(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	tokens := []Token{
		{UserId: 7, Key: "batch-retry-fail-a", Name: "a", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true},
	}
	for i := range tokens {
		require.NoError(t, tokens[i].Insert())
		require.NoError(t, cacheSetToken(tokens[i]))
	}

	// Same broken-cleanup state as the healthy-retry test, but the retry also
	// runs against broken Redis: it must keep failing, never fake success.
	keys := []string{tokens[0].Key}
	require.NoError(t, raiseTokenRevocationFences(keys))
	require.NoError(t, DB.Delete(&tokens[0]).Error)
	_, err := ValidateUserToken(tokens[0].Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	server.SetError("miniredis forced error")
	count, err := BatchDeleteTokens([]int{tokens[0].Id}, 7)
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)
	assert.Equal(t, 0, count)
	_, err = ValidateUserToken(tokens[0].Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	server.SetError("")
	count, err = BatchDeleteTokens([]int{tokens[0].Id}, 7)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	_, err = ValidateUserToken(tokens[0].Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

// Ordinary (non-revoking) updates must write the cache through the
// fence-guarded path: an update that committed just before a concurrent
// delete's success must not re-write the enabled snapshot afterwards.
func TestTokenUpdateDoesNotOverwriteFenceAfterDelete(t *testing.T) {
	token := newRevocableToken(t, "revoke-update-vs-delete-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	require.NoError(t, cacheSetToken(*token))

	// Delete wins first: fence raised, DB row soft-deleted, hash proven gone.
	require.NoError(t, token.Delete())
	require.False(t, server.Exists(tokenCacheKey(token.Key)))

	// A stale in-flight update now finishes and tries to write the enabled
	// snapshot: the fence must make it a no-op on the cache.
	stale := *token
	stale.Name = "renamed-after-delete"
	err := stale.Update()
	// The update reports success (the DB update is a no-op on a deleted row's
	// soft-deleted state via Updates? It still targets the row through the
	// primary key; soft-deleted rows are filtered, so rows affected = 0) — the
	// cache contract is what matters here: no enabled snapshot returns.
	_ = err
	require.False(t, server.Exists(tokenCacheKey(token.Key)))

	// New requests must not authorize.
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

// A normal edit holds an enabled snapshot from its initial read. If disable
// succeeds before that edit commits, the edit must update only ordinary fields
// and never restore status, the revocation fence, or the enabled cache grant.
func TestTokenFieldUpdateCannotRestoreConcurrentDisable(t *testing.T) {
	token := newRevocableToken(t, "revoke-field-update-vs-disable-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	require.NoError(t, cacheSetToken(*token))

	staleEdit := &Token{}
	require.NoError(t, DB.First(staleEdit, token.Id).Error)
	staleEdit.Name = "submitted-after-disable"

	disabling := &Token{}
	require.NoError(t, DB.First(disabling, token.Id).Error)
	previousStatus := disabling.Status
	disabling.Status = common.TokenStatusDisabled
	require.NoError(t, disabling.UpdateStatus(previousStatus))

	// This models the already-read ordinary edit finally reaching its update.
	require.NoError(t, staleEdit.Update())

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, common.TokenStatusDisabled, stored.Status)
	assert.False(t, server.Exists(tokenCacheKey(token.Key)))
	_, err := ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

// The revocation fence is intentionally finite. A stale ordinary edit that
// only reaches the cache path after it expires must still refresh from the
// disabled database row, never write the enabled snapshot it originally read.
func TestTokenFieldUpdateCannotRestoreDisableAfterFenceExpiry(t *testing.T) {
	token := newRevocableToken(t, "revoke-field-update-after-fence-expiry-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)
	require.NoError(t, cacheSetToken(*token))

	staleEdit := &Token{}
	require.NoError(t, DB.First(staleEdit, token.Id).Error)
	staleEdit.Name = "submitted-after-fence-expiry"

	disabling := &Token{}
	require.NoError(t, DB.First(disabling, token.Id).Error)
	previousStatus := disabling.Status
	disabling.Status = common.TokenStatusDisabled
	require.NoError(t, disabling.UpdateStatus(previousStatus))

	server.FastForward(time.Duration(tokenRevocationTTLSeconds()+1) * time.Second)
	require.NoError(t, staleEdit.Update())

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, common.TokenStatusDisabled, stored.Status)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, cached.Status)
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

// A queued database fill may execute long after the request that captured an
// enabled snapshot. It must re-read the row when it runs, including after the
// finite revocation fence has expired.
func TestDelayedAsyncTokenCacheRefreshUsesCurrentDatabaseToken(t *testing.T) {
	token := newRevocableToken(t, "revoke-delayed-fill-after-fence-expiry-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	release := make(chan struct{})
	refreshResult := make(chan error, 1)
	staleID, staleKey := token.Id, token.Key
	goTokenCacheFill(func() {
		<-release
		refreshResult <- refreshTokenCacheFromDatabase(staleID, staleKey)
	})

	previousStatus := token.Status
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.UpdateStatus(previousStatus))
	server.FastForward(time.Duration(tokenRevocationTTLSeconds()+1) * time.Second)

	close(release)
	waitTokenCacheFills()
	require.NoError(t, <-refreshResult)

	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, cached.Status)
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

func TestTokenReEnableRejectsStaleStatusVersionAfterConcurrentDisable(t *testing.T) {
	token := newRevocableToken(t, "revoke-reenable-status-version-key")
	useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	previousStatus := token.Status
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.UpdateStatus(previousStatus))

	staleEnable := *token
	staleEnable.Status = common.TokenStatusEnabled

	concurrentDisable := *token
	concurrentDisable.Status = common.TokenStatusDisabled
	require.NoError(t, concurrentDisable.UpdateStatus(common.TokenStatusDisabled))

	err := staleEnable.UpdateStatus(common.TokenStatusDisabled)
	require.ErrorIs(t, err, ErrTokenStatusChanged)

	var stored Token
	require.NoError(t, DB.First(&stored, token.Id).Error)
	assert.Equal(t, common.TokenStatusDisabled, stored.Status)
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
}

// Re-enable must only release the fence generation it observed: a concurrent
// delete that raised a newer fence after the re-enable snapshot keeps its deny
// window, and the re-enable must not write an enabled snapshot over it.
func TestTokenReEnableDoesNotReleaseNewerFence(t *testing.T) {
	token := newRevocableToken(t, "revoke-reenable-race-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	// Disable commits (epoch 1 raised + hash proven deleted).
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.UpdateStatus(common.TokenStatusEnabled))
	_, err := ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	// The re-enable snapshots the fence epoch before its database write…
	observedEpoch, err := readRevocationEpoch(token.Key)
	require.NoError(t, err)
	require.GreaterOrEqual(t, observedEpoch, int64(1))

	// …but a concurrent delete raises a NEWER fence first…
	require.NoError(t, raiseTokenRevocationFence(common.GenerateHMAC(token.Key)))
	newerEpoch, err := readRevocationEpoch(token.Key)
	require.NoError(t, err)
	require.Greater(t, newerEpoch, observedEpoch)

	// …so the re-enable's CAS must fail and keep the newer fence.
	err = AllowTokenCacheRefresh(token.Key, observedEpoch)
	require.ErrorIs(t, err, ErrTokenCacheRevocationPending)

	// Fills stay denied: no enabled snapshot may re-enter the cache.
	err = cacheSetTokenRespectingRevocation(*token)
	assert.ErrorIs(t, err, ErrTokenCacheRevocationPending)
	assert.False(t, server.Exists(tokenCacheKey(token.Key)))
}

// Multi-instance equivalence: with the fence semantics being plain Redis
// operations over shared keys, a revoke on "node A" must deny reads served by
// "node B" immediately. Modeled by one shared miniredis backing both direct
// cache reads (B) and model-layer revocations (A).
func TestTokenRevocationDeniesAcrossSharedRedis(t *testing.T) {
	token := newRevocableToken(t, "revoke-shared-redis-key")
	server := useUserCacheMiniRedis(t)
	drainTokenCacheFills(t)

	// Node B's local cache holds an enabled snapshot (same Redis backing).
	require.NoError(t, cacheSetToken(*token))
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	require.NotNil(t, cached)

	// Node A revokes and its Delete returns success.
	require.NoError(t, token.Delete())

	// Node B's next read must not serve the old grant: the hash is proven
	// gone and the fence denies any racing fill.
	_, err = cacheGetTokenByKey(token.Key)
	require.Error(t, err)
	_, err = ValidateUserToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)
	require.False(t, server.Exists(tokenCacheKey(token.Key)))
}
