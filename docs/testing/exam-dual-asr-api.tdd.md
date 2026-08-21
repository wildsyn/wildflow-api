# Exam dual-ASR internal API TDD evidence

## User journey

An explicitly allowlisted internal API token uploads one tenant-scoped FLAC Artifact, submits an idempotent
`wildflow/exam-replay-dual-asr-v1` Job, and reads the verified JSON result without exposing the workflow in the public
model catalog or charging retail quota.

## Checkpoints

| Stage | Commit | Evidence |
|---|---|---|
| Contract tests | `33c0bf07` | Added input, entitlement, idempotency, Artifact, and unbilled-internal-trial contract tests before the implementation commit. The original RED console output was not retained in this file. |
| Initial implementation | `bc3d4e7a` | Connected public API upload and Job submission to the private inference service. |
| Security RED | `f0874d2f` | `go test ./controller -run 'TestCreateInternalExamDualASRJob|TestInternalExamDualASR' -count=1` failed: the deadline was 30 minutes and non-allowlisted tokens could read the internal operation and Artifact. |
| Security GREEN | `a74ce865` | The same target passed after using a six-hour ASR deadline and enforcing current token model scope on operation and Artifact reads. |
| Provenance RED | `7133b161` | Artifact validation accepted a changed RuntimeVersion reference. |
| Provenance GREEN | `ee458254` | Artifact validation now requires the immutable dual-ASR RuntimeVersion attested by the Worker. |

## Guarantees

| # | Guarantee | Test scope | Result |
|---|---|---|---|
| 1 | Input upload requires API authentication, explicit model allowlisting, FLAC, bounded length, and SHA-256 | `controller`, `internal/inferenceclient`, `router` | PASS |
| 2 | Job submission is tenant scoped, idempotent, and forwards only input Artifact IDs | `controller`, `service`, `internal/inferenceclient` | PASS |
| 3 | The internal workflow remains absent from the public catalog and creates no retail reserve or settlement | `service`, `router` | PASS |
| 4 | Result download accepts only the exact dual-ASR model revisions and verified JSON Artifact metadata | `controller`, `service` | PASS |
| 5 | Current token model scope is enforced when reading internal operations and Artifacts | `controller` | PASS |
| 6 | The ASR deadline accommodates the documented two-hour input ceiling and long-running batch inference | `controller` | PASS |
| 7 | A succeeded ASR Artifact must carry the exact controller-attested RuntimeVersion reference | `controller`, `service` | PASS |

## Final verification

- `bash scripts/check-local.sh`: PASS, including attribution, repository split, Docker cross-build, and secret guard.
- `go test ./...`: PASS.
- `go test ./controller ./service ./internal/inferenceclient ./router -count=1`: PASS.
- `git diff --check`: PASS.

Production deployment, GPU Worker registration, real Artifact transfer, usage ingestion, and ordinary-user acceptance
remain separate evidence layers.
