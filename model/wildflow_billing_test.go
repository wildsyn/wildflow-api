package model

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWildFlowBillingModelTest(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := DB
	db, err := gorm.Open(sqlite.Open("file:wildflow-billing-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &WildFlowOperation{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func createWildFlowBillingFixture(t *testing.T, db *gorm.DB, suffix string) (*User, *Token, *WildFlowOperation) {
	t.Helper()
	user := &User{Username: "billing-" + suffix, Quota: 100_000, Group: "default"}
	require.NoError(t, db.Create(user).Error)
	token := &Token{
		UserId:      user.Id,
		Key:         "token-" + suffix,
		Name:        "billing-token",
		RemainQuota: 100_000,
	}
	require.NoError(t, db.Create(token).Error)
	operation := &WildFlowOperation{
		OperationID:          "op-" + suffix,
		UserID:               user.Id,
		TokenID:              token.Id,
		IdempotencyKeyDigest: "key-" + suffix,
		RequestDigest:        "request-" + suffix,
		RequestID:            "request-id-" + suffix,
		ProductModelRef:      "FLUX.2 [klein] 4B",
		ModelVersionRef:      "black-forest-labs/FLUX.2-klein-4B",
		State:                "submitting",
		BillingState:         WildFlowBillingStatePending,
	}
	require.NoError(t, db.Create(operation).Error)
	return user, token, operation
}

func testWildFlowBillingQuote() WildFlowBillingQuote {
	return WildFlowBillingQuote{
		Currency:        "CNY",
		AmountMicros:    50_000,
		Unit:            "image",
		BillableUnits:   1,
		Quota:           3_425,
		QuotaPerUnit:    "500000",
		USDExchangeRate: "7.3",
		PriceVersion:    "wildflow-retail-cny-v1",
	}
}

func TestReserveWildFlowWalletBillingIsIdempotent(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "wallet-reserve")
	quote := testWildFlowBillingQuote()

	first, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	second, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)

	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, 100_000-quote.Quota, user.Quota)
	assert.Equal(t, 100_000-quote.Quota, token.RemainQuota)
	assert.Equal(t, quote.Quota, token.UsedQuota)
	assert.Equal(t, WildFlowBillingStateReserved, first.BillingState)
	assert.Equal(t, WildFlowBillingStateReserved, second.BillingState)
	assert.Equal(t, WildFlowBillingSourceWallet, second.BillingSource)
}

func TestRefundWildFlowBillingRestoresWalletAndTokenExactlyOnce(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "wallet-refund")
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-refund", "failed", "execution_failed"))

	first, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.True(t, changed)
	second, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.False(t, changed)

	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, 100_000, user.Quota)
	assert.Equal(t, 100_000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.Equal(t, WildFlowBillingStateRefunded, first.BillingState)
	assert.Equal(t, WildFlowBillingStateRefunded, second.BillingState)
}

func TestSettleWildFlowBillingIsIdempotentAndDoesNotMoveReservedQuota(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "wallet-settle")
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-settle", "succeeded", ""))

	first, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.True(t, changed)
	second, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.False(t, changed)

	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, 100_000-quote.Quota, user.Quota)
	assert.Equal(t, 100_000-quote.Quota, token.RemainQuota)
	assert.Equal(t, quote.Quota, token.UsedQuota)
	assert.Equal(t, WildFlowBillingStateSettled, first.BillingState)
	assert.Equal(t, WildFlowBillingStateSettled, second.BillingState)
}

func TestRefundWildFlowBillingDoesNotReleaseRecoveryHold(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "wallet-recovery")
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-recovery", "recovery_required", "submission_unknown"))

	current, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.False(t, changed)

	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, 100_000-quote.Quota, user.Quota)
	assert.Equal(t, 100_000-quote.Quota, token.RemainQuota)
	assert.Equal(t, WildFlowBillingStateReserved, current.BillingState)
}
