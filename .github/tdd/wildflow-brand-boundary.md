# WildFlow public brand boundary TDD evidence

## Red

- The focused status test initially returned the legacy system name, an empty
  logo, the upstream documentation URL, and enabled drawing/task defaults.
- The payment goods metadata also inherited the legacy public product name.

## Green

- `GOWORK=off go test ./controller -run 'TestGetStatus(NormalizesLegacyPublicBrandDefaults|PreservesExplicitPublicBrandOverrides)$'`:
  passed.
- `GOWORK=off go build ./...`: passed.
- `bash scripts/check-local.sh`: passed.
- `git diff --check`: passed.

## Known environment limits

- `make test` reached the full package suite, but tests that create local SMTP,
  HTTP, or mini-Redis listeners were blocked by the workspace sandbox with
  `bind: operation not permitted`. The focused brand tests do not require a
  listener and passed.
