package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

const maxManifestBytes = 1 << 20

var (
	errApplyConfirmationRequired    = errors.New("exact apply confirmation is required")
	errRollbackConfirmationRequired = errors.New("exact rollback confirmation is required")
	errRuntimeDrift                 = errors.New("runtime quota conversion differs from the migration manifest")
	errUnexpectedManifestField      = errors.New("migration manifest contains an unexpected field")
	sha256Pattern                   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type commandOutput struct {
	Mode           string                                       `json:"mode"`
	ManifestSHA256 string                                       `json:"manifest_sha256,omitempty"`
	Plan           *model.UnifiedAccountMigrationPlan           `json:"plan,omitempty"`
	Apply          *model.UnifiedAccountMigrationResult         `json:"apply,omitempty"`
	Rollback       *model.UnifiedAccountMigrationRollbackResult `json:"rollback,omitempty"`
}

func rejectUnexpectedFields(raw []byte, allowed map[string]struct{}) error {
	var object map[string]json.RawMessage
	if err := common.Unmarshal(raw, &object); err != nil {
		return err
	}
	for key := range object {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: %s", errUnexpectedManifestField, key)
		}
	}
	return nil
}

func decodeManifest(raw []byte) (model.UnifiedAccountMigrationManifest, error) {
	var manifest model.UnifiedAccountMigrationManifest
	if len(raw) == 0 || len(raw) > maxManifestBytes {
		return manifest, model.ErrUnifiedAccountMigrationInvalidManifest
	}
	if err := rejectUnexpectedFields(raw, map[string]struct{}{
		"migration_id": {}, "quota_per_unit": {}, "usd_to_cny_cents": {},
		"expected_account_count": {}, "expected_source_balance_cents": {}, "accounts": {},
	}); err != nil {
		return manifest, err
	}
	var envelope struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := common.Unmarshal(raw, &envelope); err != nil {
		return manifest, err
	}
	for _, account := range envelope.Accounts {
		if err := rejectUnexpectedFields(account, map[string]struct{}{
			"subject": {}, "preferred_username": {}, "display_name": {},
			"email": {}, "source_balance_cents": {},
		}); err != nil {
			return manifest, err
		}
	}
	if err := common.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func manifestDigest(manifest model.UnifiedAccountMigrationManifest) (string, error) {
	canonical, err := common.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

func applyConfirmationPhrase(manifest model.UnifiedAccountMigrationManifest) string {
	return fmt.Sprintf(
		"APPLY %s %d %d",
		manifest.MigrationID,
		manifest.ExpectedAccountCount,
		manifest.ExpectedSourceBalanceCents,
	)
}

func validateRuntime(manifest model.UnifiedAccountMigrationManifest) error {
	if operation_setting.GetQuotaDisplayType() != operation_setting.QuotaDisplayTypeCNY ||
		math.Round(common.QuotaPerUnit) != float64(manifest.QuotaPerUnit) ||
		math.Round(operation_setting.USDExchangeRate*100) != float64(manifest.USDToCNYCents) {
		return errRuntimeDrift
	}
	return nil
}

func initializeRuntimeWith(
	initEnv func(),
	initDB func() error,
	initOptions func(),
	initRedis func() error,
) error {
	initEnv()
	// A one-off migration command must never run the service startup migration
	// set or create bootstrap users. Its own ledger is migrated only by apply.
	common.IsMasterNode = false
	if err := initDB(); err != nil {
		return err
	}
	initOptions()
	return initRedis()
}

func initializeRuntime() error {
	return initializeRuntimeWith(
		common.InitEnv,
		model.InitDB,
		model.InitOptionMap,
		common.InitRedisClient,
	)
}

func writeOutput(output io.Writer, value commandOutput) error {
	payload, err := common.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(payload))
	return err
}

func run(args []string, input io.Reader, output io.Writer, initialize func() error) error {
	if len(args) == 0 {
		return errors.New("mode must be plan, apply, or rollback")
	}
	switch args[0] {
	case "plan", "apply":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		expectedCount := flags.Int("expected-account-count", 0, "frozen eligible account count")
		expectedBalance := flags.Int64("expected-balance-cents", -1, "frozen WildCloud balance total in cents")
		expectedDigest := flags.String("expected-manifest-sha256", "", "approved canonical manifest digest")
		confirmation := flags.String("confirm", "", "exact apply confirmation phrase")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return model.ErrUnifiedAccountMigrationInvalidManifest
		}
		raw, err := io.ReadAll(io.LimitReader(input, maxManifestBytes+1))
		if err != nil {
			return err
		}
		manifest, err := decodeManifest(raw)
		if err != nil {
			return err
		}
		digest, err := manifestDigest(manifest)
		if err != nil {
			return err
		}
		if args[0] == "apply" {
			if *expectedCount != manifest.ExpectedAccountCount ||
				*expectedBalance != manifest.ExpectedSourceBalanceCents ||
				!sha256Pattern.MatchString(*expectedDigest) || !strings.EqualFold(*expectedDigest, digest) ||
				*confirmation != applyConfirmationPhrase(manifest) {
				return errApplyConfirmationRequired
			}
		}
		if err := initialize(); err != nil {
			return err
		}
		if err := validateRuntime(manifest); err != nil {
			return err
		}
		if args[0] == "plan" {
			plan, err := model.PlanUnifiedAccountMigration(manifest)
			if err != nil {
				return err
			}
			return writeOutput(output, commandOutput{Mode: "plan", ManifestSHA256: digest, Plan: plan})
		}
		result, err := model.ApplyUnifiedAccountMigration(manifest)
		if err != nil {
			return err
		}
		return writeOutput(output, commandOutput{Mode: "apply", ManifestSHA256: digest, Apply: result})

	case "rollback":
		flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		migrationID := flags.String("migration-id", "", "migration identifier")
		confirmation := flags.String("confirm", "", "exact rollback confirmation phrase")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 ||
			*confirmation != "ROLLBACK "+*migrationID {
			return errRollbackConfirmationRequired
		}
		if err := initialize(); err != nil {
			return err
		}
		result, err := model.RollbackUnifiedAccountMigration(*migrationID)
		if err != nil {
			return err
		}
		return writeOutput(output, commandOutput{Mode: "rollback", Rollback: result})
	default:
		return errors.New("mode must be plan, apply, or rollback")
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, initializeRuntime); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
