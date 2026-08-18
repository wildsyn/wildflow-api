# WildFlow first-party billing TDD evidence

## Source and user journeys

The journeys were derived from the request to make the first-party VoxCPM2 and
FLUX.2 offerings charge the configured retail prices in `wildflow-api`.

1. As an API user, I am charged exactly once for an idempotent job submission:
   CNY 0.80 per 10,000 Unicode characters for VoxCPM2 and CNY 0.05 per FLUX
   image.
2. As an API user, my configured wallet/subscription preference is respected,
   and insufficient quota rejects the request before inference submission.
3. As an API user, a successful job is settled only after a durable artifact
   exists; failed or cancelled jobs refund exactly once.
4. As an operator, ambiguous results retain their reservation in
   `recovery_required`, and a background reconciler completes billing even if
   the user stops polling.

## RED and GREEN evidence

- RED, billing primitives: `go test ./service ./model -run
  'Test(QuoteWildFlowBilling|ReserveWildFlowWalletBilling|RefundWildFlowBilling|SettleWildFlowBilling)'
  -count=1` failed to compile because the quote, state, reserve, settle, and
  refund contracts did not exist.
- GREEN, billing primitives: the same command passed for both packages.
- RED, HTTP lifecycle: `go test ./controller -run
  'Test(CreateWildFlowJobPreConsumesRetailPriceExactlyOnce|FailedWildFlowJobRefundsRetailPriceExactlyOnce|SucceededWildFlowJobSettlesRetailPriceExactlyOnce|RecoveryRequiredWildFlowJobKeepsReservation)'
  -count=1` ran the new tests and failed because balances and billing state did
  not change.
- GREEN, HTTP lifecycle: the same command passed after wiring reservation and
  terminal settlement into `/v1/jobs`.
- RED, asynchronous reconciliation: `go test ./service -run
  TestReconcileWildFlowBillingFinalizesJobsWithoutUserPolling -count=1` failed
  to compile because the reconciler did not exist.
- GREEN, asynchronous reconciliation: the same command passed after the
  master-node reconciler was added.
- RED, missing artifact: `go test ./controller -run
  TestSucceededWildFlowJobWithoutArtifactEntersRecoveryRequired -count=1`
  returned HTTP 500 instead of a durable `recovery_required` hold.
- GREEN, missing artifact: the same command passed with HTTP 503,
  `Retry-After`, and a retained reservation.
- RED, deleted token refund: `go test ./model -run
  TestRefundWildFlowBillingRestoresFundingWhenTokenWasDeleted -count=1` failed
  with `record not found`.
- GREEN, deleted token refund: the same command passed after making the
  funding refund independent of a deleted token record.
- RED, subscription refund recovery: `go test ./service -run
  TestReserveWildFlowOperationBillingUsesSubscriptionPreferenceDurably
  -count=1` failed to compile because a durable `refunding` claim did not
  exist.
- GREEN, subscription refund recovery: the same command passed after refund
  ownership was serialized and made resumable by the background reconciler.
- RED, atomic subscription reservation: `go test ./model -run
  TestReserveWildFlowSubscriptionBillingRollsBackPreConsumeWhenTokenReserveFails
  -count=1` failed because no single-transaction reservation contract existed.
- GREEN, atomic subscription reservation: the same command passed after
  subscription pre-consume, token reserve, and operation snapshot persistence
  were moved into one database transaction.
- RED, atomic subscription refund: `go test ./model -run
  TestRefundSubscriptionPreConsumeRollsBackSubscriptionAndRecordTogether
  -count=1` failed because the refund path opened a nested independent
  transaction.
- GREEN, atomic subscription refund: the same command passed after the quota
  update and refund record transition were bound to the caller transaction.
- RED, unavailable inference configuration: `go test ./controller -run
  TestCreateWildFlowJobDoesNotReserveQuotaWhenInferenceIsNotConfigured
  -count=1` showed wallet and token quota were reserved before the local client
  configuration failed.
- GREEN, unavailable inference configuration: the same command passed after
  client configuration validation was moved before every billing side effect.

## Test specification

| Guarantee | Test scope | Type | Result |
|---|---|---|---|
| VoxCPM2 and FLUX quotes use the configured CNY prices and safe quota conversion | `service/wildflow_billing_test.go` | unit | PASS |
| Replaying one idempotency key does not double charge wallet, token, or subscription | `model/wildflow_billing_test.go`, `controller/wildflow_jobs_test.go` | integration | PASS |
| Wallet/subscription preference and wallet-overflow fallback are preserved | `service/wildflow_billing_test.go`, `controller/wildflow_jobs_test.go` | integration | PASS |
| Insufficient quota stops before inference | `controller/wildflow_jobs_test.go` | contract | PASS |
| Success settles once; failure/cancellation refund once | `model/wildflow_billing_test.go`, `controller/wildflow_jobs_test.go` | integration | PASS |
| Unknown or missing results retain funds for recovery | `controller/wildflow_jobs_test.go`, `service/wildflow_billing_reconciler_test.go` | integration | PASS |
| Background reconciliation works without client polling and preserves funds on transient inference failure | `service/wildflow_billing_reconciler_test.go` | integration | PASS |
| Deleting an API token cannot block the user's funding refund | `model/wildflow_billing_test.go` | regression | PASS |
| Subscription reserve failure cannot leave consumed subscription quota behind | `model/wildflow_billing_test.go` | regression | PASS |
| Subscription quota and refund record roll back together | `model/wildflow_billing_test.go` | regression | PASS |
| Invalid inference configuration fails before wallet, token, or subscription reservation | `controller/wildflow_jobs_test.go` | regression | PASS |

## Final verification

- `go test ./... -count=1`: PASS (1241 tests across 88 packages).
- `go vet ./...`: PASS.
- `GOWORK=off go test ./... -count=1` from `relaykit/`: PASS.
- `go test -race ./model ./service ./controller -run
  'WildFlow|SubscriptionPreConsume' -count=1`: PASS (58 tests).
- Cross-package coverage command:
  `go test ./model ./service ./controller
  -coverpkg=./model,./service,./controller
  -coverprofile=/tmp/wildflow-billing-cross-cover.out -count=1`: PASS.
- Deduplicated statement coverage for the three new dedicated billing files:
  **80.89% (326/403)**. Deduplication takes the maximum hit count for identical
  source ranges because `go test` emits one profile section per tested package
  when `-coverpkg` is shared.

## Known verification boundary

SQLite integration, the complete Go suite, relaykit isolation, vet, and a
focused race run were verified locally. Live MySQL/PostgreSQL migration,
production deployment revision, production balance changes, and an
authenticated production job journey were not performed in this change.
