# M1 inference client TDD evidence

## RED

`go test ./internal/inferenceclient` failed because the client contract types and
constructor did not exist. The reproducer was committed as `e36c939`.

## GREEN

The implementation adds a private HTTP client for `POST /internal/v1/jobs` with:

- fail-closed base URL, bearer token, and timeout validation;
- HTTPS outside loopback-only local development;
- exact Operation, tenant, model version, Artifact, deadline, and callback fields;
- typed `409`, `429`, and `503` errors with `Retry-After` preservation;
- no automatic redirect or retry of job submissions;
- bounded response bodies and no token or payload logging.

Local GREEN verification:

- `go test ./internal/inferenceclient`: 11 tests passed;
- `go vet ./...`: passed;
- `go test ./...`: 1161 tests passed in 88 packages;
- `GOWORK=off go test ./...` from `relaykit/`: passed;
- `bash scripts/check-local.sh`: passed.

Cross-repository local contract verification also passed: an opt-in Go
integration test submitted a real job to a local `wildflow-inference` FastAPI
process and received `202 Accepted` with a queued job identifier.
