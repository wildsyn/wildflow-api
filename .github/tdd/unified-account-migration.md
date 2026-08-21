# Unified account and balance migration TDD evidence

## User journeys

1. An active human identity from the unified login can enter WildFlow even when
   public registration is disabled.
2. A pre-existing WildFlow OIDC binding is reused instead of creating a
   duplicate user.
3. A migrated user receives a copy of the source CNY balance, converted to
   WildFlow quota with integer half-up rounding; the source balance is not
   mutated by this tool.
4. Replaying the same migration does not create another user or credit quota a
   second time.
5. Rollback subtracts only the copied quota, refuses an unsafe rollback after
   that quota has been spent, and disables only users created by the migration.
6. Operators cannot apply or roll back without an exact frozen manifest digest
   and confirmation phrase.

## RED and GREEN evidence

- RED, migration contract: focused model tests failed to compile because the
  manifest, plan, apply, and rollback APIs did not exist.
- GREEN, migration contract: the model tests passed after adding the durable
  migration ledger, identity collision checks, transactional account creation,
  quota credit, idempotent replay, and guarded rollback.
- RED, replay drift: the new regression test failed because an already-applied
  subject could be rebound without detection.
- GREEN, replay drift: plan and apply now fail closed when the ledger, OIDC
  binding, user, or credited amount differs from the frozen manifest.
- RED, command safety: focused command tests failed to compile because the
  fail-closed runtime initializer and safety gates did not exist.
- GREEN, command safety: the command now rejects unknown JSON fields, runtime
  currency drift, incorrect manifest hashes, stale counts/totals, and incorrect
  apply or rollback confirmation phrases.
- RED, OIDC identity without email:
  `go test ./model -run TestPlanUnifiedAccountMigrationAllowsOIDCSubjectWithoutEmail -count=1`
  failed because the manifest validator required an email address.
- GREEN, OIDC identity without email: the same test passed after making email
  optional for subject-bound OIDC provisioning while retaining validation and
  collision checks for every non-empty email.

## Test specification

| Guarantee | Scope | Result |
|---|---|---|
| Large CNY balances convert without floating-point loss | `model/unified_account_migration_test.go` | PASS |
| Existing OIDC users are reused; new users are OIDC-only | `model/unified_account_migration_test.go` | PASS |
| A stable OIDC subject can be provisioned without inventing an email | `model/unified_account_migration_test.go` | PASS |
| Duplicate subjects, emails, and identity conflicts fail closed | `model/unified_account_migration_test.go` | PASS |
| Apply and rollback are transactional and idempotent | `model/unified_account_migration_test.go` | PASS |
| Unsafe rollback and replay drift are rejected | `model/unified_account_migration_test.go` | PASS |
| Manifest digest and confirmation phrases freeze scope | `cmd/unified-account-migration/main_test.go` | PASS |
| Runtime quota/currency drift blocks execution | `cmd/unified-account-migration/main_test.go` | PASS |
| CLI initialization avoids normal service bootstrap side effects | `cmd/unified-account-migration/main_test.go` | PASS |

## Final verification

- `go test ./model ./cmd/unified-account-migration -count=1 -timeout=180s`:
  PASS.
- `go vet ./model ./cmd/unified-account-migration`: PASS.
- `git diff --check`: PASS.
- Dedicated model migration file coverage: **81.3% (200/246 statements)**.
- Migration command coverage: **83.9%**.

## Verification boundary

The focused SQLite-backed model integration tests, command tests, static checks,
and coverage gates were verified locally. Production manifest planning,
production database mutation, real unified-login journeys, and public
registration closure remain separate production acceptance steps.
