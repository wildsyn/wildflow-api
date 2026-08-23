package model

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
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
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &Log{}, &WildFlowOperation{}, &WildFlowUsageEvent{}, &WildFlowBillingLogEntry{}))
	DB = db
	previousLogDB := LOG_DB
	LOG_DB = db
	t.Cleanup(func() {
		DB = previousDB
		LOG_DB = previousLogDB
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestRefundSubscriptionPreConsumeRollsBackSubscriptionAndRecordTogether(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	require.NoError(t, db.AutoMigrate(&UserSubscription{}, &SubscriptionPreConsumeRecord{}))

	subscription := &UserSubscription{UserId: 91, PlanId: 7, AmountTotal: 100_000, AmountUsed: 3_425, Status: "active"}
	require.NoError(t, db.Create(subscription).Error)
	record := &SubscriptionPreConsumeRecord{
		RequestId:          "op-subscription-rollback",
		UserId:             subscription.UserId,
		UserSubscriptionId: subscription.Id,
		PreConsumed:        3_425,
		Status:             "consumed",
	}
	require.NoError(t, db.Create(record).Error)

	rollbackErr := errors.New("force outer transaction rollback")
	err := db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, refundSubscriptionPreConsumeTx(tx, record.RequestId))
		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)

	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(record, record.Id).Error)
	assert.Equal(t, int64(3_425), subscription.AmountUsed)
	assert.Equal(t, "consumed", record.Status)
}

func TestReserveWildFlowSubscriptionBillingRollsBackPreConsumeWhenTokenReserveFails(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	require.NoError(t, db.AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	user, token, operation := createWildFlowBillingFixture(t, db, "subscription-token-rollback")
	require.NoError(t, db.Model(token).Update("remain_quota", 1).Error)

	plan := &SubscriptionPlan{Title: "billing test", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100_000}
	require.NoError(t, db.Create(plan).Error)
	subscription := &UserSubscription{
		UserId:      user.Id,
		PlanId:      plan.Id,
		AmountTotal: 100_000,
		StartTime:   time.Now().Add(-time.Hour).Unix(),
		EndTime:     time.Now().Add(time.Hour).Unix(),
		Status:      "active",
	}
	require.NoError(t, db.Create(subscription).Error)

	_, err := ReserveWildFlowSubscriptionBilling(operation.OperationID, testWildFlowBillingQuote())
	require.ErrorIs(t, err, ErrWildFlowInsufficientTokenQuota)

	require.NoError(t, db.First(subscription, subscription.Id).Error)
	assert.Zero(t, subscription.AmountUsed)
	var recordCount int64
	require.NoError(t, db.Model(&SubscriptionPreConsumeRecord{}).
		Where("request_id = ?", operation.OperationID).
		Count(&recordCount).Error)
	assert.Zero(t, recordCount)
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

func TestWildFlowBillingQuoteValidationRejectsIncompleteSnapshots(t *testing.T) {
	tests := []WildFlowBillingQuote{
		{},
		{Currency: "CNY", AmountMicros: 50_000, Unit: "image", BillableUnits: 1, Quota: 3_425},
	}
	for _, quote := range tests {
		require.Error(t, quote.Validate())
	}
}

func TestWildFlowBillingHelpersRejectInvalidReferences(t *testing.T) {
	_, err := ReserveWildFlowSubscriptionBilling("missing-operation", WildFlowBillingQuote{})
	require.Error(t, err)
	_, err = ReserveWildFlowSubscriptionBilling("missing-operation", testWildFlowBillingQuote())
	require.Error(t, err)
	require.Error(t, RecordWildFlowBillingLog(nil, LogTypeConsume, "invalid"))
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

func TestReserveWildFlowWalletBillingRejectsChangedPriceSnapshot(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "price-conflict")
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	quote.AmountMicros++

	_, err = ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.ErrorIs(t, err, ErrWildFlowBillingStateConflict)
}

func TestReserveWildFlowWalletBillingRejectsInsufficientTokenQuota(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "token-insufficient")
	require.NoError(t, db.Model(token).Update("remain_quota", 1).Error)

	_, err := ReserveWildFlowWalletBilling(operation.OperationID, testWildFlowBillingQuote())
	require.ErrorIs(t, err, ErrWildFlowInsufficientTokenQuota)
	require.NoError(t, db.First(user, user.Id).Error)
	assert.Equal(t, 100_000, user.Quota)
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
	_, err = StoreWildFlowOperationResult(operation.OperationID, `{"id":"op-wallet-settle","state":"succeeded"}`, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)

	first, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.False(t, changed, "a succeeded Job without a recorded usage event must not capture")
	assert.Equal(t, WildFlowBillingStateReserved, first.BillingState)
	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-wallet-settle", PayloadDigest: "usage-wallet-settle-digest",
		OperationID: operation.OperationID, JobID: "job-settle",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	first, changed, err = SettleWildFlowOperationBilling(operation.OperationID)
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
	assert.Equal(t, "usage-wallet-settle", second.BillingUsageEventID)
}

func TestRecordWildFlowUsageEventRejectsBillingMismatchBeforePersistence(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "usage-mismatch")
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-usage-mismatch", "succeeded", ""))
	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-mismatch", PayloadDigest: "usage-mismatch-digest",
		OperationID: operation.OperationID, JobID: "job-usage-mismatch",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 2, Unit: "image",
	})
	assert.False(t, replayed)
	require.ErrorIs(t, err, ErrWildFlowUsageEventConflict)
	var count int64
	require.NoError(t, db.Model(&WildFlowUsageEvent{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestSettleWildFlowBillingIsConcurrentAndUsageEventDeduplicated(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	user, _, operation := createWildFlowBillingFixture(t, db, "concurrent-settle")
	quote := testWildFlowBillingQuote()
	_, err = ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-concurrent-settle", "succeeded", ""))
	_, err = StoreWildFlowOperationResult(operation.OperationID, `{"id":"op-concurrent-settle","state":"succeeded"}`, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	_, err = RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-concurrent-settle", PayloadDigest: "usage-concurrent-settle-digest",
		OperationID: operation.OperationID, JobID: "job-concurrent-settle",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	require.NoError(t, err)

	const workers = 8
	var wait sync.WaitGroup
	errorsByWorker := make(chan error, workers)
	changedByWorker := make(chan bool, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, changed, settleErr := SettleWildFlowOperationBilling(operation.OperationID)
			errorsByWorker <- settleErr
			changedByWorker <- changed
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	close(changedByWorker)
	for settleErr := range errorsByWorker {
		require.NoError(t, settleErr)
	}
	changedCount := 0
	for changed := range changedByWorker {
		if changed {
			changedCount++
		}
	}
	assert.Equal(t, 1, changedCount)
	require.NoError(t, db.First(user, user.Id).Error)
	assert.Equal(t, quote.Quota, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
}

func TestRecordWildFlowBillingLogHasConcurrentUniqueConsumeEntry(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	_, _, operation := createWildFlowBillingFixture(t, db, "concurrent-log")
	operation.BillingState = WildFlowBillingStateSettled
	operation.BillingUsageEventID = "usage-concurrent-log"

	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })
	const workers = 8
	var wait sync.WaitGroup
	errorsByWorker := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByWorker <- RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled")
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for recordErr := range errorsByWorker {
		require.NoError(t, recordErr)
	}
	var auditCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).Count(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount)
	var logCount int64
	require.NoError(t, db.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
}

func TestRecordWildFlowBillingLogAdoptsLegacyProjectionWithoutDuplicatingIt(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "legacy-log")
	require.NoError(t, db.Create(&Log{
		UserId: operation.UserID, Type: LogTypeConsume, RequestId: operation.OperationID,
	}).Error)
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	require.NoError(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"))
	var auditCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).Count(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount)
	var logCount int64
	require.NoError(t, db.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
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

func TestRefundWildFlowBillingRestoresFundingWhenTokenWasDeleted(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "deleted-token-refund")
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	require.NoError(t, db.Unscoped().Delete(token).Error)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-deleted-token", "failed", "execution_failed"))

	refunded, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.True(t, changed)
	require.NoError(t, db.First(user, user.Id).Error)
	assert.Equal(t, 100_000, user.Quota)
	assert.Equal(t, WildFlowBillingStateRefunded, refunded.BillingState)
}

func TestGetWildFlowOperationByIDReturnsNilForUnknownOperation(t *testing.T) {
	setupWildFlowBillingModelTest(t)
	operation, err := GetWildFlowOperationByID("op-does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, operation)
	_, err = ReserveWildFlowWalletBilling("op-does-not-exist", testWildFlowBillingQuote())
	require.Error(t, err)
	_, err = ReserveWildFlowSubscriptionBilling("op-does-not-exist", testWildFlowBillingQuote())
	require.Error(t, err)
}
