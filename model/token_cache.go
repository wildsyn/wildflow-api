package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/go-redis/redis/v8"
)

func tokenCacheKey(key string) string {
	return "token:" + common.GenerateHMAC(key)
}

// tokenCacheFillWG tracks in-flight asynchronous token cache fills so tests
// can drain them before restoring global Redis state; production callers
// never wait on it.
var tokenCacheFillWG sync.WaitGroup

// goTokenCacheFill schedules an asynchronous cache fill and tracks it until
// the fill body returns.
func goTokenCacheFill(fill func()) {
	tokenCacheFillWG.Add(1)
	gopool.Go(func() {
		defer tokenCacheFillWG.Done()
		fill()
	})
}

// waitTokenCacheFills blocks until every scheduled asynchronous token cache
// fill has finished. Test-only drain point.
func waitTokenCacheFills() {
	tokenCacheFillWG.Wait()
}

func cacheSetToken(token Token) error {
	err := common.RedisHSetObj(tokenCacheKey(token.Key), &token, time.Duration(common.RedisKeyCacheSeconds())*time.Second)
	if err != nil {
		return err
	}
	return nil
}

// cacheSetTokenRespectingRevocation writes the token hash only when no
// revocation fence exists for the key at write time. Used by cache fills: a
// fill racing a concurrent revoke on another node re-checks the fence after
// its snapshot was taken and drops its own write, so a deleted or disabled
// token never re-enters the cache behind the revoker's back.
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
//
// The read is fence-aware and fails closed: while a revocation fence exists
// for the key, any cached hash is treated as untrusted and refused, forcing
// the caller onto the database. This covers a committed revocation whose
// DEL/EXISTS phase failed against Redis — the leftover hash must not keep
// authorizing just because the fence could not delete it.
func cacheGetTokenByKey(key string) (*Token, error) {
	if !common.RedisEnabled {
		return nil, fmt.Errorf("redis is not enabled")
	}
	hmacKey := common.GenerateHMAC(key)
	ctx := context.Background()
	const script = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return {-1, ''}
end
local data = redis.call('HGETALL', KEYS[2])
local out = {'1'}
for i, v in ipairs(data) do
  out[#out + 1] = v
end
return out`
	raw, err := common.RDB.Eval(ctx, script,
		[]string{tokenRevokedFenceKeyOf(hmacKey), tokenCacheKeyOf(hmacKey)}).Result()
	if err != nil {
		return nil, err
	}
	values, ok := raw.([]interface{})
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("token cache read failed")
	}
	denied, err := strconv.Atoi(fmt.Sprint(values[0]))
	if err != nil {
		return nil, err
	}
	if denied == -1 {
		return nil, ErrTokenCacheRevocationPending
	}
	fields := make(map[string]string, (len(values)-1)/2)
	for i := 1; i+1 < len(values); i += 2 {
		fields[fmt.Sprint(values[i])] = fmt.Sprint(values[i+1])
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("key not found in Redis")
	}
	var token Token
	if err := common.RedisDecodeHash(fields, &token); err != nil {
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

// ErrTokenCacheRevocationPending reports that a revocation fence is active
// (or Redis failed mid-revocation) for a token key, so cached state for that
// key must not be trusted until the fence clears.
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

// tokenCacheKeyOf is the token hash key for an already-HMAC'd token key.
func tokenCacheKeyOf(hmacKey string) string {
	return "token:" + hmacKey
}

// AllowTokenCacheRefresh removes a revocation fence that was raised for an
// older generation, after the database re-enable commit, by comparing the
// fence epoch against observedRevocationEpoch read before the database
// mutation. A concurrent revoke that raised a NEWER fence wins: its epoch
// differs, so the fence stays and this re-enable does not release it.
func AllowTokenCacheRefresh(key string, observedRevocationEpoch int64) error {
	if !common.RedisEnabled {
		return nil
	}
	const script = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
if current ~= tonumber(ARGV[1]) then
  return 0
end
redis.call('DEL', KEYS[1])
return 1`
	result, err := common.RDB.Eval(context.Background(), script,
		[]string{tokenRevokedFenceKeyOf(common.GenerateHMAC(key))}, observedRevocationEpoch).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrTokenCacheRevocationPending
	}
	return nil
}

// readRevocationEpoch returns the current fence epoch for a token key (0 when
// no fence exists). Callers snapshot it before a database mutation and pass it
// to AllowTokenCacheRefresh afterwards, so a re-enable can only release the
// fence generation it actually observed — never one a concurrent revoke raised.
func readRevocationEpoch(key string) (int64, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	value, err := common.RDB.Get(context.Background(), tokenRevokedFenceKeyOf(common.GenerateHMAC(key))).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// invalidateTokenCacheNow synchronously deletes the cached token hash and
// proves the deletion, retrying bounded times. The fence is re-raised
// atomically INSIDE the same Lua script as the delete-and-verify, so no
// window exists where the hash is gone but the fence was also missing: an
// enable that raced between raise and release re-raises the fence and the
// delete-verify still proves the hash is gone. Errors are propagated and the
// fence stays raised on failure — a revocation must not return success while
// a stale grant could still authorize requests.
func invalidateTokenCacheNow(key string) error {
	if !common.RedisEnabled {
		return nil
	}
	hmacKey := common.GenerateHMAC(key)
	const script = `
redis.call('DEL', KEYS[1])
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
local next = tonumber(redis.call('GET', KEYS[2]) or '0') + 1
redis.call('SET', KEYS[2], next, 'EX', tonumber(ARGV[1]))
return 1`
	ttl := tokenRevocationTTLSeconds()
	var lastErr error
	for range tokenRevocationMaxAttempts {
		result, err := common.RDB.Eval(context.Background(), script,
			[]string{tokenCacheKeyOf(hmacKey), tokenRevokedFenceKeyOf(hmacKey)}, ttl).Int()
		if err == nil && result == 0 {
			err = fmt.Errorf("token cache still present after delete")
		}
		if err == nil {
			return nil
		}
		lastErr = err
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

// RevokeTokensCacheRetry is the idempotent retry path for a committed database
// revocation whose cache cleanup failed earlier (e.g. a batch delete retried
// after its rows are already soft-deleted). It re-raises the fences and
// re-proves the hash deletions; safe to repeat until Redis succeeds.
func RevokeTokensCacheRetry(keys []string) error {
	if !common.RedisEnabled || len(keys) == 0 {
		return nil
	}
	if err := raiseTokenRevocationFences(keys); err != nil {
		return err
	}
	return revokeTokensCacheCommitted(keys)
}
