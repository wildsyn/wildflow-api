# Exam dual-ASR public journey and receipt TDD evidence

## User journey

A registered WildFlow user with a standard valid API token selects the public team-trial catalog entry
`wildflow/exam-replay-dual-asr-v1`, uploads one tenant-scoped FLAC Artifact, submits an idempotent Job, reads the
verified JSON result, and downloads its exact bytes without a separate entitlement or retail charge. The API persists
the public model identity while submitting the distinct Runtime offering `exam-replay-dual-asr` to inference. Tokens
that the user deliberately restricts to selected models continue to honor that scope.

After one exact team-trial Usage Event has been ingested and the API response writer has completed an Artifact
download whose actual byte count and SHA-256 match the attested metadata, a dedicated internal endpoint may
materialize one immutable public-journey receipt. That receipt is a rollback-proof input; it does not by itself prove
that the remote client durably saved the response, so the release coordinator must hash its independently received
bytes.

## Checkpoints

| Stage | Commit | Evidence |
|---|---|---|
| Contract tests | `33c0bf07` | Added input, entitlement, idempotency, Artifact, and unbilled-internal-trial contract tests before the implementation commit. The original RED console output was not retained in this file. |
| Initial implementation | `bc3d4e7a` | Connected public API upload and Job submission to the private inference service. |
| Security RED | `f0874d2f` | `go test ./controller -run 'TestCreateInternalExamDualASRJob|TestInternalExamDualASR' -count=1` failed: the deadline was 30 minutes and non-allowlisted tokens could read the internal operation and Artifact. |
| Security GREEN | `a74ce865` | The same target passed after using a six-hour ASR deadline and enforcing current token model scope on operation and Artifact reads. |
| Provenance RED | `7133b161` | Artifact validation accepted a changed RuntimeVersion reference. |
| Provenance GREEN | `ee458254` | Artifact validation now requires the immutable dual-ASR RuntimeVersion attested by the Worker. |
| Registered-user access RED | `95e9d904` | The upload, submit, read, and download controller tests all returned `403 model_forbidden` for a standard registered-user token. |
| Registered-user access GREEN | `b39d4a92` | Removed the dual-ASR-only denial for unrestricted standard tokens; the same four paths pass while normal token scope enforcement remains intact. |
| Public/runtime identity regression | current branch | Added a real controller submission assertion and service mapping test. The historical RED console output was not retained. |
| Actual-download integrity regression | current branch | Added success, same-length digest mismatch, short stream, immutable completion time, and no-false-receipt tests. The historical RED console output was not retained. |
| Usage uniqueness and canonical digest regression | current branch | Added atomic team-trial binding, millisecond ingestion time, and an actual Go receiver digest golden. The historical RED console output was not retained. |
| Durable public receipt regression | current branch | Added immutable materialize/replay, semantic-tamper rejection, dedicated authentication, exact envelope, and Go/Python-compatible digest golden tests. The historical RED console output was not retained. |

## Guarantees

| # | Guarantee | Test scope | Result |
|---|---|---|---|
| 1 | Input upload requires API authentication, FLAC, bounded length, and SHA-256; no separate dual-ASR entitlement is required for a standard token | `controller`, `internal/inferenceclient`, `router` | PASS |
| 2 | Job submission is tenant scoped, idempotent, and forwards only input Artifact IDs | `controller`, `service`, `internal/inferenceclient` | PASS |
| 3 | The public catalog exposes the team trial while Operation rows keep the public identity and inference receives the separate Runtime offering identity | `controller`, `service` | PASS |
| 4 | Result download accepts only exact dual-ASR revisions and records completion only after the actual bytes match Artifact size and SHA-256 | `controller`, `service`, `model` | PASS |
| 5 | Standard tokens can read owned internal operations and Artifacts; deliberately model-scoped tokens still honor their configured scope | `controller` | PASS |
| 6 | The ASR deadline accommodates the documented two-hour input ceiling and long-running batch inference | `controller` | PASS |
| 7 | A succeeded ASR Artifact must carry the exact controller-attested RuntimeVersion reference | `controller`, `service` | PASS |
| 8 | A team-trial Operation atomically binds one Usage Event ID and stores a UTC millisecond ingestion time; another Event ID conflicts | `model`, `controller` | PASS on SQLite, PostgreSQL 16, and MySQL 8.4 |
| 9 | A final receipt is created from one repeatable-read transaction, persisted once per Operation, digest-verified, semantically revalidated on replay, and unchanged after live Operation expiry/state drift | `model`, `service`, `controller` | PASS on SQLite, PostgreSQL 16, and MySQL 8.4 |
| 10 | Receipt materialization and retrieval use a dedicated minimum-32-byte Bearer token, authenticate before input/DB work, expose no list/search endpoint, and emit `Cache-Control: no-store` | `controller`, `router` | PASS |
| 11 | Go canonical Usage and public-receipt bytes are pinned by literal SHA-256 goldens, including fractional timestamps, offsets, Unicode, HTML characters, quotes, backslashes, and U+2028/U+2029 where applicable | `controller`, `service` | PASS |
| 12 | Team-trial receipt creation requires pending billing plus zero/empty subscription, quota, amount, unit, rate, currency, price-version, and settlement fields | `service` | PASS |

Legacy Usage rows created before the `ingested_at` column have no trustworthy millisecond ingestion evidence and are
therefore rejected for receipt creation. G3 acceptance must use a fresh post-deploy journey; this migration does not
invent or backfill historical timestamps.

## Final verification

- `go test ./...`: PASS.
- Focused `go test -race ./model ./service ./controller ... -count=1`: PASS.
- New SQLite journey fixtures and concurrency tests with `-count=20`: PASS.
- `TEST_POSTGRES_DSN=... go test ./model -run TestWildFlowPostgresMigrationConcurrencyAndFaultRecovery -count=1`: PASS against an isolated PostgreSQL 16 container, including migrations, unique indexes, eight-way Usage binding, eight-way receipt creation, and durable replay.
- `TEST_MYSQL_DSN=... go test ./model -run TestWildFlowMySQLJourneyReceiptMigrationAndConcurrency -count=1`: PASS against an isolated MySQL 8.4 container with the same journey migration/concurrency/replay boundaries.
- Python-compatible public-receipt canonical SHA-256 golden: PASS (`ed72db823ed847e16d1e8cff84e923853e2fe9548314f52c9f835159675a2600`).
- `bash scripts/check-local.sh`: PASS, including attribution, repository split, Docker cross-build contract, and secret guard.
- `git diff --check`: PASS.

The PostgreSQL and MySQL results prove compatibility in isolated empty databases; they do not prove a production
migration, deployed revision, production data compatibility, or an ordinary-user journey.

This document does not prove PR, merge, production deployment revision, GPU Worker registration, real Artifact
transfer, production Usage ingestion, ordinary-user acceptance, rollback completion, or business `Done`; those remain
separate evidence layers.
