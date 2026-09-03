# API key reveal rate-limit TDD evidence

## Source and user journeys

The journeys were derived from the request to stop a shared company NAT from
blocking authenticated users who reveal their own API keys.

1. As an authenticated user, I can reveal my API key even when another user on
   the same public IP has used authentication or key-management endpoints.
2. As an operator, repeated reveal attempts by the same user remain limited in
   one action-specific bucket.
3. As a client, a 429 response includes a stable error code, `Retry-After`, and
   a numeric `retry_after` value without exposing key material.

## RED and GREEN evidence

- RED: `go test ./middleware ./router -run
  'TestRedis(IPRateLimiterThresholdTTLAndNamespace|UserRateLimiterSeparatesUsersAndScopesBehindSharedIP)|TestTokenKeyRateLimitIsPerUserAndIndependentFromSharedIPAuthTraffic'
  -count=1` failed because 429 responses had no JSON body and both users on one
  IP received 429 after unrelated critical traffic. Checkpoint: `bfdcee79`.
- GREEN: the same command passed after the reveal routes moved to
  `UserCriticalRateLimit("token-key-read")` and 429 responses gained the
  structured retry contract. Checkpoint: `dc3d8e7c`.

## Test specification

| Guarantee | Test | Type | Result |
|---|---|---|---|
| Same-IP users receive independent reveal quotas | `router/api_router_test.go:TestTokenKeyRateLimitIsPerUserAndIndependentFromSharedIPAuthTraffic` | integration | PASS |
| Authentication traffic does not consume the reveal quota | same integration test | regression | PASS |
| One user is still limited after exhausting the reveal quota | same integration test | security contract | PASS |
| User and action scopes produce distinct Redis keys | `middleware/rate_limit_test.go:TestRedisUserRateLimiterSeparatesUsersAndScopesBehindSharedIP` | unit/integration | PASS |
| 429 includes a stable body and retry delay | `middleware/rate_limit_test.go:TestRedisIPRateLimiterThresholdTTLAndNamespace` | contract | PASS |

## Final verification

- `go test ./...`: PASS.
- Focused race run for middleware and router regressions: PASS.
- `go test ./middleware ./router -coverprofile=... -count=1`: PASS;
  router statement coverage was 91.1%. The combined package percentage was
  48.3% because the middleware/router packages contain unrelated routes and
  authentication behavior outside this change.
- `git diff --check`: PASS.

## Known boundary

The implementation, full Go suite, focused race test, and local SQLite/Redis
integration behavior were verified. No PR was pushed or merged, and no
production deployment or authenticated production journey was performed.
