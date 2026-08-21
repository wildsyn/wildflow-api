package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const testManifestJSON = `{
  "migration_id":"wildcloud-balance-copy-2026-08-21-v1",
  "quota_per_unit":500000,
  "usd_to_cny_cents":730,
  "expected_account_count":1,
  "expected_source_balance_cents":730,
  "accounts":[{
    "subject":"authentik-user",
    "preferred_username":"user",
    "display_name":"User",
    "email":"user@example.com",
    "source_balance_cents":730
  }]
}`

func TestManifestDigestIsStableAcrossJSONWhitespace(t *testing.T) {
	first, err := decodeManifest([]byte(testManifestJSON))
	require.NoError(t, err)
	second, err := decodeManifest([]byte(`{"migration_id":"wildcloud-balance-copy-2026-08-21-v1","quota_per_unit":500000,"usd_to_cny_cents":730,"expected_account_count":1,"expected_source_balance_cents":730,"accounts":[{"subject":"authentik-user","preferred_username":"user","display_name":"User","email":"user@example.com","source_balance_cents":730}]}`))
	require.NoError(t, err)

	firstDigest, err := manifestDigest(first)
	require.NoError(t, err)
	secondDigest, err := manifestDigest(second)
	require.NoError(t, err)
	assert.Equal(t, firstDigest, secondDigest)
}

func TestRunApplyRequiresDigestAndExactConfirmationBeforeDatabaseInit(t *testing.T) {
	initCalled := false
	initialize := func() error {
		initCalled = true
		return errors.New("must not initialize")
	}

	err := run(
		[]string{"apply", "--expected-account-count", "1", "--expected-balance-cents", "730"},
		bytes.NewBufferString(testManifestJSON),
		&bytes.Buffer{},
		initialize,
	)
	assert.ErrorIs(t, err, errApplyConfirmationRequired)
	assert.False(t, initCalled)

	manifest, err := decodeManifest([]byte(testManifestJSON))
	require.NoError(t, err)
	digest, err := manifestDigest(manifest)
	require.NoError(t, err)
	err = run(
		[]string{
			"apply",
			"--expected-account-count", "1",
			"--expected-balance-cents", "730",
			"--expected-manifest-sha256", digest,
			"--confirm", "wrong phrase",
		},
		bytes.NewBufferString(testManifestJSON),
		&bytes.Buffer{},
		initialize,
	)
	assert.ErrorIs(t, err, errApplyConfirmationRequired)
	assert.False(t, initCalled)
}

func TestValidateRuntimeMatchesManifestCurrencyContract(t *testing.T) {
	originalQuotaPerUnit := common.QuotaPerUnit
	originalRate := operation_setting.USDExchangeRate
	originalDisplay := operation_setting.GetGeneralSetting().QuotaDisplayType
	t.Cleanup(func() {
		common.QuotaPerUnit = originalQuotaPerUnit
		operation_setting.USDExchangeRate = originalRate
		operation_setting.GetGeneralSetting().QuotaDisplayType = originalDisplay
	})
	manifest, err := decodeManifest([]byte(testManifestJSON))
	require.NoError(t, err)

	common.QuotaPerUnit = 500_000
	operation_setting.USDExchangeRate = 7.3
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeCNY
	require.NoError(t, validateRuntime(manifest))

	operation_setting.USDExchangeRate = 7.31
	assert.ErrorIs(t, validateRuntime(manifest), errRuntimeDrift)
	operation_setting.USDExchangeRate = 7.3
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
	assert.ErrorIs(t, validateRuntime(manifest), errRuntimeDrift)
}

func TestApplyConfirmationPhraseIncludesFrozenScope(t *testing.T) {
	manifest := model.UnifiedAccountMigrationManifest{
		MigrationID:                "wildcloud-balance-copy-2026-08-21-v1",
		ExpectedAccountCount:       19,
		ExpectedSourceBalanceCents: 1_120_099_455,
	}
	assert.Equal(
		t,
		"APPLY wildcloud-balance-copy-2026-08-21-v1 19 1120099455",
		applyConfirmationPhrase(manifest),
	)
}

func setupCommandTestDatabase(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.QuotaForNewUser = 0
	common.QuotaPerUnit = 500_000
	operation_setting.USDExchangeRate = 7.3
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeCNY
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.UserSession{},
		&model.ExternalIdentityClaim{},
		&model.Token{},
		&model.UnifiedAccountMigrationRecord{},
	))
}

func TestRunPlanApplyReplayAndRollback(t *testing.T) {
	setupCommandTestDatabase(t)
	initialize := func() error { return nil }

	var planOutput bytes.Buffer
	require.NoError(t, run(
		[]string{"plan"}, bytes.NewBufferString(testManifestJSON), &planOutput, initialize,
	))
	var planned commandOutput
	require.NoError(t, common.Unmarshal(planOutput.Bytes(), &planned))
	require.NotNil(t, planned.Plan)
	assert.Equal(t, 1, planned.Plan.CreateCount)
	assert.Len(t, planned.ManifestSHA256, 64)

	manifest, err := decodeManifest([]byte(testManifestJSON))
	require.NoError(t, err)
	confirmation := applyConfirmationPhrase(manifest)
	applyArgs := []string{
		"apply",
		"--expected-account-count", "1",
		"--expected-balance-cents", "730",
		"--expected-manifest-sha256", planned.ManifestSHA256,
		"--confirm", confirmation,
	}
	var applyOutput bytes.Buffer
	require.NoError(t, run(applyArgs, bytes.NewBufferString(testManifestJSON), &applyOutput, initialize))
	var applied commandOutput
	require.NoError(t, common.Unmarshal(applyOutput.Bytes(), &applied))
	require.NotNil(t, applied.Apply)
	assert.Equal(t, 1, applied.Apply.CreatedCount)

	var replayOutput bytes.Buffer
	require.NoError(t, run(applyArgs, bytes.NewBufferString(testManifestJSON), &replayOutput, initialize))
	var replayed commandOutput
	require.NoError(t, common.Unmarshal(replayOutput.Bytes(), &replayed))
	require.NotNil(t, replayed.Apply)
	assert.Equal(t, 1, replayed.Apply.IdempotentCount)

	var rollbackOutput bytes.Buffer
	require.NoError(t, run(
		[]string{"rollback", "--migration-id", manifest.MigrationID, "--confirm", "ROLLBACK " + manifest.MigrationID},
		bytes.NewBuffer(nil), &rollbackOutput, initialize,
	))
	var rolledBack commandOutput
	require.NoError(t, common.Unmarshal(rollbackOutput.Bytes(), &rolledBack))
	require.NotNil(t, rolledBack.Rollback)
	assert.Equal(t, 1, rolledBack.Rollback.RolledBackCount)
}

func TestDecodeManifestRejectsUnknownFields(t *testing.T) {
	_, err := decodeManifest([]byte(`{"migration_id":"id","unexpected":true}`))
	assert.ErrorIs(t, err, errUnexpectedManifestField)
	_, err = decodeManifest([]byte(`{"migration_id":"id","accounts":[{"subject":"sub","unexpected":true}]}`))
	assert.ErrorIs(t, err, errUnexpectedManifestField)
}
