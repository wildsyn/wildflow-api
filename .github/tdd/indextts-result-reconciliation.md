# Free IndexTTS result reconciliation

A free IndexTTS operation could remain `recovery_required` even after inference recovered its saved audio. Include pending IndexTTS operations with a Job and no persisted result in the existing GET-only reconciliation loop. No inference submission, schema change or billing mutation is added.

## TDD evidence

- RED: `4f47670b`; the new recovery test expected an inference read and observed none.
- GREEN: `TestReconcileFreeIndexTTS` passes with race detection against SQLite 3.50.4, MySQL 8.4.11 and PostgreSQL 17.11.
- The test verifies missing audio stays in recovery, a recovered running Job continues reconciliation, valid WAV is persisted as succeeded, subsequent passes stop polling, and no charge is added. Other models, missing Jobs, terminal failure, settled operations and persisted results are excluded.

## Verification

- `go test ./service ./model ./controller ./internal/inferenceclient -count=1`: PASS.
- `go vet ./service ./model ./controller ./internal/inferenceclient`: PASS.
- `bash scripts/check-local.sh`: PASS.
- With local disposable MySQL and PostgreSQL DSNs, `go test -race ./service -run TestReconcileFreeIndexTTS -count=1 -v`: all three dialects PASS.
- Full service/model race testing finds existing video task and Redis cache races; reproduced on unchanged baseline `edb3a000`. The full race suite is not reported as passing.

Runtime durability and GET-only inference recovery are paired with wildflow-inference PR #116. Production journey evidence follows deployment and is separate from these local checks.
