package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
)

func tokenCacheKey(key string) string {
	return "token:" + common.GenerateHMAC(key)
}

func cacheSetToken(token Token) error {
	err := common.RedisHSetObj(tokenCacheKey(token.Key), &token, time.Duration(common.RedisKeyCacheSeconds())*time.Second)
	if err != nil {
		return err
	}
	return nil
}

// cacheSetTokenRespectingRevocation writes the token hash only when no
// revocation fence exists for the key at write time. Used by asynchronous
// cache fills: a fill racing a concurrent revoke on another node re-checks the
// fence after its snapshot was taken and drops its own write, so a deleted or
// disabled token never re-enters the cache behind the revoker's back.
func cacheSetTokenRespectingRevocation(token Token) error {
	redisKey := tokenCacheKey(token.Key)
	fenceKey := tokenRevokedFenceKeyOf(common.GenerateHMAC(token.Key))
	token.Clean()
	ctx := context.Background()
	const script = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
local data = cjson.decode(ARGV[1])
for field, value in pairs(data) do
  redis.call('HSET', KEYS[2], field, value)
end
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[2]))
return 1`
	data, err := common.Marshal(common.RedisStructToHash(&token))
	if err != nil {
		return err
	}
	result, err := common.RDB.Eval(ctx, script,
		[]string{fenceKey, redisKey},
		string(data), common.RedisKeyCacheSeconds(),
	).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrTokenCacheRevocationPending
	}
	return nil
}

func cacheDeleteToken(key string) error {
	err := common.RedisDelKey(tokenCacheKey(key))
	if err != nil {
		return err
	}
	return nil
}

func cacheIncrTokenQuota(key string, increment int64) error {
	err := common.RedisHIncrBy(tokenCacheKey(key), constant.TokenFiledRemainQuota, increment)
	if err != nil {
		return err
	}
	return nil
}

func cacheDecrTokenQuota(key string, decrement int64) error {
	return cacheIncrTokenQuota(key, -decrement)
}

func cacheSetTokenField(key string, field string, value string) error {
	err := common.RedisHSetField(tokenCacheKey(key), field, value)
	if err != nil {
		return err
	}
	return nil
}

// CacheGetTokenByKey 从缓存中获取 token，如果缓存中不存在，则从数据库中获取
func cacheGetTokenByKey(key string) (*Token, error) {
	if !common.RedisEnabled {
		return nil, fmt.Errorf("redis is not enabled")
	}
	var token Token
	err := common.RedisHGetObj(tokenCacheKey(key), &token)
	if err != nil {
		return nil, err
	}
	token.Key = key
	return &token, nil
}

// tokenRevocationTTLSeconds bounds how long a revocation fence must outlive
// any token hash that could have been populated before the fence was raised.
// The token hash TTL is RedisKeyCacheSeconds(), so the fence needs to cover
// that plus margin for clock/latency skew between nodes.
func tokenRevocationTTLSeconds() int {
	cacheTTL := common.RedisKeyCacheSeconds()
	if cacheTTL <= 0 {
		cacheTTL = 60
	}
	extra := cacheTTL
	if extra < 60 {
		extra = 60
	}
	return cacheTTL + extra
}

const tokenRevocationMaxAttempts = 3

// ErrTokenCacheRevocationPending reports that Redis kept failing while the
// caller tried to make a revoked token stop authorizing. The revocation has
// been committed to the database, but the cache could not be proven clean, so
// the caller must not report success to the user until this clears.
var ErrTokenCacheRevocationPending = errors.New("token cache revocation is pending")

// raiseTokenRevocationFence atomically bumps the per-key revocation epoch and
// extends its TTL. The fence must be raised before the database mutation so
// any cache fill that observed a pre-revocation epoch cannot outlive it.
func raiseTokenRevocationFence(hmacKey string) error {
	const script = `
local next = tonumber(redis.call('GET', KEYS[1]) or '0') + 1
redis.call('SET', KEYS[1], next, 'EX', tonumber(ARGV[1]))
return next`
	ttl := tokenRevocationTTLSeconds()
	var lastErr error
	for range tokenRevocationMaxAttempts {
		lastErr = common.RDB.Eval(context.Background(), script,
			[]string{tokenRevokedFenceKeyOf(hmacKey)}, ttl).Err()
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// tokenRevokedFenceKeyOf is the fence key for an already-HMAC'd token key.
func tokenRevokedFenceKeyOf(hmacKey string) string {
	return "token:revoked:" + hmacKey
}

// clearTokenRevocationFence removes the fence so the key may be cached again.
// Only the re-enable path may call it: while a token stays revoked the fence
// outlives the whole token-hash TTL, so every fill that took its snapshot
// before the revocation also drops its write after the revoker finished.
func clearTokenRevocationFence(hmacKey string) error {
	return common.RedisDelKey(tokenRevokedFenceKeyOf(hmacKey))
}

// invalidateTokenCacheNow synchronously deletes the cached token hash and
// proves the deletion, retrying bounded times. Unlike the fire-and-forget
// invalidation used on cache refresh paths, a revocation must not return
// success while a stale grant could still authorize requests, so errors are
// propagated and the fence stays raised on failure.
func invalidateTokenCacheNow(key string) error {
	if !common.RedisEnabled {
		return nil
	}
	var lastErr error
	for range tokenRevocationMaxAttempts {
		lastErr = common.RedisDelKey(tokenCacheKey(key))
		if lastErr == nil {
			// Prove the deletion with a direct existence check instead of
			// trusting DEL's reply: clustered/proxy Redis clients may report
			// success while the key is still readable from another node view.
			exists, err := common.RDB.Exists(context.Background(), tokenCacheKey(key)).Result()
			if err != nil {
				lastErr = err
			} else if exists > 0 {
				lastErr = fmt.Errorf("token cache still present after delete")
			} else {
				return nil
			}
		}
	}
	return lastErr
}

// raiseTokenRevocationFences raises every per-key fence before the database
// mutation commits, so the deny epoch is authoritative from that point on.
// Any Redis failure is returned as ErrTokenCacheRevocationPending so callers
// fail closed instead of reporting success while a stale grant could still
// authorize requests.
func raiseTokenRevocationFences(keys []string) error {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := raiseTokenRevocationFence(common.GenerateHMAC(key)); err != nil {
			return fmt.Errorf("%w: raise fence failed: %v", ErrTokenCacheRevocationPending, err)
		}
	}
	return nil
}

// revokeTokensCacheCommitted proves every cached hash for the given keys is
// gone. Call it only after the database mutation is committed (or already
// known): on Redis failure the fences stay raised, so fills stay poisoned and
// the caller must surface ErrTokenCacheRevocationPending instead of success.
func revokeTokensCacheCommitted(keys []string) error {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := invalidateTokenCacheNow(key); err != nil {
			return fmt.Errorf("%w: delete failed: %v", ErrTokenCacheRevocationPending, err)
		}
	}
	return nil
}

// AllowTokenCacheRefresh clears the revocation fence when a token leaves a
// revoked state (re-enabled after disable). Until the fence expires or is
// explicitly cleared, cache fills for the key refuse to write, keeping the
// deny window deterministic instead of best-effort.
func AllowTokenCacheRefresh(key string) error {
	if !common.RedisEnabled {
		return nil
	}
	return clearTokenRevocationFence(common.GenerateHMAC(key))
}
