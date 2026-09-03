package model

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
	require.NoError(t, db.AutoMigrate(
		&User{}, &Token{}, &Log{}, &WildFlowOperation{}, &WildFlowUsageEvent{},
		&WildFlowBillingLogEntry{}, &WildFlowBillingLogProjectionReceipt{},
	))
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
	user := &User{Username: "billing-" + suffix, Quota: 100_000, Group: "default", AffCode: "aff-" + suffix}
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

func TestReconcileWildFlowCanonicalBillingAuditsBackfillsLegacyTerminalRows(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, settled := createWildFlowBillingFixture(t, db, "legacy-settled-audit")
	_, _, refunded := createWildFlowBillingFixture(t, db, "legacy-refunded-audit")
	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", settled.OperationID).
		Updates(map[string]any{
			"billing_state":          WildFlowBillingStateSettled,
			"billing_source":         WildFlowBillingSourceWallet,
			"billing_usage_event_id": "usage-legacy-settled-audit",
			"billing_quota":          testWildFlowBillingQuote().Quota,
			"billing_currency":       testWildFlowBillingQuote().Currency,
			"billing_amount_micros":  testWildFlowBillingQuote().AmountMicros,
		}).Error)
	require.NoError(t, db.Model(&WildFlowOperation{}).
		Where("operation_id = ?", refunded.OperationID).
		Updates(map[string]any{
			"billing_state":         WildFlowBillingStateRefunded,
			"billing_source":        WildFlowBillingSourceWallet,
			"billing_quota":         testWildFlowBillingQuote().Quota,
			"billing_currency":      testWildFlowBillingQuote().Currency,
			"billing_amount_micros": testWildFlowBillingQuote().AmountMicros,
		}).Error)

	processed, err := ReconcileWildFlowCanonicalBillingAudits(100)
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	processed, err = ReconcileWildFlowCanonicalBillingAudits(100)
	require.NoError(t, err)
	assert.Zero(t, processed)
	var consumeCount int64
	var refundCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", settled.OperationID, LogTypeConsume).
		Count(&consumeCount).Error)
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", refunded.OperationID, LogTypeRefund).
		Count(&refundCount).Error)
	assert.Equal(t, int64(1), consumeCount)
	assert.Equal(t, int64(1), refundCount)
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
		EventID: "usage-wallet-settle", PayloadDigest: strings.Repeat("a", 64),
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

func TestSettleDualASRBillingRefundsUnusedPreauthorization(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "dual-asr-settle")
	operation.ProductModelRef = "wildflow/dual-asr-v1"
	operation.ModelVersionRef = "wildflow/exam-replay-dual-asr-v1"
	require.NoError(t, db.Save(operation).Error)
	quote := WildFlowBillingQuote{
		Currency: "CNY", AmountMicros: 12_000_000, Unit: "audio_millisecond",
		BillableUnits: 7_200_000, Quota: 80_000, QuotaPerUnit: "500000",
		USDExchangeRate: "7.5", PriceVersion: "wildflow-retail-cny-v1",
	}
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-dual-asr", "succeeded", ""))
	_, err = StoreWildFlowOperationResult(operation.OperationID, `{"id":"op-dual-asr-settle","state":"succeeded"}`, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-dual-asr", PayloadDigest: strings.Repeat("a", 64),
		OperationID: operation.OperationID, JobID: "job-dual-asr",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "audio_duration", Quantity: 45_000, Unit: "millisecond",
	})
	require.NoError(t, err)
	assert.False(t, replayed)

	settled, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, WildFlowBillingStateSettled, settled.BillingState)
	assert.Equal(t, int64(45_000), settled.BillingBillableUnits)
	assert.Equal(t, int64(75_000), settled.BillingAmountMicros)
	assert.Equal(t, 500, settled.BillingQuota)
	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, 99_500, user.Quota)
	assert.Equal(t, 500, user.UsedQuota)
	assert.Equal(t, 99_500, token.RemainQuota)
	assert.Equal(t, 500, token.UsedQuota)
}

func TestSettleDualASRSubscriptionBillingRefundsUnusedPreauthorization(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	require.NoError(t, db.AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	user, token, operation := createWildFlowBillingFixture(t, db, "dual-asr-subscription-settle")
	operation.ProductModelRef = "wildflow/dual-asr-v1"
	operation.ModelVersionRef = "wildflow/exam-replay-dual-asr-v1"
	require.NoError(t, db.Save(operation).Error)
	plan := &SubscriptionPlan{Title: "dual ASR", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100_000}
	require.NoError(t, db.Create(plan).Error)
	subscription := &UserSubscription{
		UserId: user.Id, PlanId: plan.Id, AmountTotal: 100_000,
		StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active",
	}
	require.NoError(t, db.Create(subscription).Error)
	quote := WildFlowBillingQuote{
		Currency: "CNY", AmountMicros: 12_000_000, Unit: "audio_millisecond",
		BillableUnits: 7_200_000, Quota: 80_000, QuotaPerUnit: "500000",
		USDExchangeRate: "7.5", PriceVersion: "wildflow-retail-cny-v1",
	}
	_, err := ReserveWildFlowSubscriptionBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-dual-asr-subscription", "succeeded", ""))
	_, err = StoreWildFlowOperationResult(operation.OperationID, `{"id":"op-dual-asr-subscription-settle","state":"succeeded"}`, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	_, err = RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-dual-asr-subscription", PayloadDigest: strings.Repeat("b", 64),
		OperationID: operation.OperationID, JobID: "job-dual-asr-subscription",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "audio_duration", Quantity: 45_000, Unit: "millisecond",
	})
	require.NoError(t, err)

	settled, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, WildFlowBillingSourceSubscription, settled.BillingSource)
	assert.Equal(t, 500, settled.BillingQuota)
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	assert.Equal(t, int64(500), subscription.AmountUsed)
	assert.Equal(t, 99_500, token.RemainQuota)
	assert.Equal(t, 500, token.UsedQuota)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, db.Where("request_id = ?", operation.OperationID).First(&record).Error)
	assert.Equal(t, int64(500), record.PreConsumed)
	assert.Equal(t, "consumed", record.Status)
}

func TestRecordWildFlowUsageEventRejectsBillingMismatchBeforePersistence(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "usage-mismatch")
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-usage-mismatch", "succeeded", ""))
	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-mismatch", PayloadDigest: strings.Repeat("a", 64),
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

func TestRecordWildFlowUsageEventRejectsNonCanonicalDigest(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "usage-invalid-digest")

	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-invalid-digest", PayloadDigest: "not-a-sha256",
		OperationID: operation.OperationID, JobID: operation.JobID,
		ModelVersionRef: operation.ModelVersionRef,
	})
	assert.False(t, replayed)
	require.ErrorIs(t, err, ErrWildFlowUsageEventInvalid)
	var count int64
	require.NoError(t, db.Model(&WildFlowUsageEvent{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestRecordWildFlowUsageEventReloadsIdentityWhenInsertReportsAffected(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "usage-found-rows")
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-usage-found-rows", "succeeded", ""))

	callbackName := "test:wildflow-usage-found-rows:" + uuid.NewString()
	var injected atomic.Bool
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName+":inject", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "wild_flow_usage_events" || !injected.CompareAndSwap(false, true) {
			return
		}
		tx.Session(&gorm.Session{NewDB: true}).Exec(
			"INSERT INTO wild_flow_usage_events (event_id, payload_digest, operation_id, job_id, model_version_ref, kind, quantity, unit, created_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			"usage-found-rows", strings.Repeat("b", 64), operation.OperationID, "job-usage-found-rows", operation.ModelVersionRef,
			"images", 1, "image", time.Now().Unix(),
		)
	}))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register(callbackName+":affected", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "wild_flow_usage_events" {
			tx.RowsAffected = 1 // emulate MySQL clientFoundRows=true on duplicate upsert
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName + ":inject")
		_ = db.Callback().Create().Remove(callbackName + ":affected")
	})

	replayed, err := RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-found-rows", PayloadDigest: strings.Repeat("a", 64),
		OperationID: operation.OperationID, JobID: "job-usage-found-rows",
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	assert.False(t, replayed)
	require.ErrorIs(t, err, ErrWildFlowUsageEventConflict)
}

func TestCanonicalBillingLogReloadsIdentityWhenInsertReportsAffected(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "canonical-found-rows")
	operation.BillingState = WildFlowBillingStateSettled
	operation.BillingSource = WildFlowBillingSourceWallet
	operation.BillingUsageEventID = "usage-canonical-found-rows"

	callbackName := "test:wildflow-canonical-found-rows:" + uuid.NewString()
	var injected atomic.Bool
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName+":inject", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "wild_flow_billing_log_entries" || !injected.CompareAndSwap(false, true) {
			return
		}
		tx.Session(&gorm.Session{NewDB: true}).Exec(
			"INSERT INTO wild_flow_billing_log_entries (operation_id, log_type, usage_event_id, billing_source, content, projection_state, created_time) VALUES (?, ?, ?, ?, ?, ?, ?)",
			operation.OperationID, LogTypeConsume, operation.BillingUsageEventID, operation.BillingSource,
			"different persisted content", WildFlowBillingProjectionPending, time.Now().Unix(),
		)
	}))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register(callbackName+":affected", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "wild_flow_billing_log_entries" {
			tx.RowsAffected = 1
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName + ":inject")
		_ = db.Callback().Create().Remove(callbackName + ":affected")
	})

	err := db.Transaction(func(tx *gorm.DB) error {
		_, ensureErr := ensureWildFlowCanonicalBillingLogTx(tx, operation, LogTypeConsume, "expected content")
		return ensureErr
	})
	require.ErrorIs(t, err, ErrWildFlowBillingStateConflict)
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
		EventID: "usage-concurrent-settle", PayloadDigest: strings.Repeat("a", 64),
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

func failNextWildFlowCanonicalAuditCreate(t *testing.T, db *gorm.DB) error {
	t.Helper()
	forcedErr := errors.New("forced canonical audit insert failure")
	callbackName := "test:wildflow-canonical-audit-failure:" + uuid.NewString()
	var failed atomic.Bool
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "wild_flow_billing_log_entries" && failed.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callbackName) })
	return forcedErr
}

func useSeparateWildFlowLogDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousLogDB := LOG_DB
	logDB, err := gorm.Open(sqlite.Open("file:wildflow-log-projection-"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&Log{}, &WildFlowBillingLogProjectionReceipt{}))
	LOG_DB = logDB
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		sqlDB, sqlErr := logDB.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})
	return logDB
}

func prepareSuccessfulWildFlowBillingForSettlement(t *testing.T, db *gorm.DB, suffix string) (*User, *Token, *WildFlowOperation) {
	t.Helper()
	user, token, operation := createWildFlowBillingFixture(t, db, suffix)
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	jobID := "job-" + suffix
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, jobID, "succeeded", ""))
	_, err = StoreWildFlowOperationResult(operation.OperationID, `{"id":"`+operation.OperationID+`","state":"succeeded"}`, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	_, err = RecordWildFlowUsageEvent(&WildFlowUsageEvent{
		EventID: "usage-" + suffix, PayloadDigest: strings.Repeat("a", 64),
		OperationID: operation.OperationID, JobID: jobID,
		ModelVersionRef: operation.ModelVersionRef,
		Kind:            "images", Quantity: 1, Unit: "image",
	})
	require.NoError(t, err)
	return user, token, operation
}

func TestSettleWildFlowBillingRollsBackAllStateWhenCanonicalAuditInsertFails(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := prepareSuccessfulWildFlowBillingForSettlement(t, db, "settle-audit-rollback")
	forcedErr := failNextWildFlowCanonicalAuditCreate(t, db)

	_, changed, err := SettleWildFlowOperationBilling(operation.OperationID)
	require.ErrorIs(t, err, forcedErr)
	assert.False(t, changed)
	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, WildFlowBillingStateReserved, operation.BillingState)
	assert.Empty(t, operation.BillingUsageEventID)
	assert.Zero(t, user.UsedQuota)
	assert.Zero(t, user.RequestCount)
	assert.Equal(t, 100_000-testWildFlowBillingQuote().Quota, user.Quota)
	assert.Equal(t, 100_000-testWildFlowBillingQuote().Quota, token.RemainQuota)
	var canonicalCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).Count(&canonicalCount).Error)
	assert.Zero(t, canonicalCount)
}

func TestRefundWildFlowWalletBillingRollsBackAllStateWhenCanonicalAuditInsertFails(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	user, token, operation := createWildFlowBillingFixture(t, db, "refund-wallet-audit-rollback")
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowWalletBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-refund-wallet-audit", "failed", "execution_failed"))
	forcedErr := failNextWildFlowCanonicalAuditCreate(t, db)

	_, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.ErrorIs(t, err, forcedErr)
	assert.False(t, changed)
	require.NoError(t, db.First(user, user.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, 100_000-quote.Quota, user.Quota)
	assert.Equal(t, 100_000-quote.Quota, token.RemainQuota)
	assert.Equal(t, quote.Quota, token.UsedQuota)
	var canonicalCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).Count(&canonicalCount).Error)
	assert.Zero(t, canonicalCount)
}

func TestRefundWildFlowSubscriptionBillingRollsBackAllStateWhenCanonicalAuditInsertFails(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	require.NoError(t, db.AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))
	user, token, operation := createWildFlowBillingFixture(t, db, "refund-subscription-audit-rollback")
	plan := &SubscriptionPlan{Title: "refund audit", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100_000}
	require.NoError(t, db.Create(plan).Error)
	subscription := &UserSubscription{
		UserId: user.Id, PlanId: plan.Id, AmountTotal: 100_000,
		StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active",
	}
	require.NoError(t, db.Create(subscription).Error)
	quote := testWildFlowBillingQuote()
	_, err := ReserveWildFlowSubscriptionBilling(operation.OperationID, quote)
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(operation.OperationID, "job-refund-subscription-audit", "failed", "execution_failed"))
	forcedErr := failNextWildFlowCanonicalAuditCreate(t, db)

	_, changed, err := RefundWildFlowOperationBilling(operation.OperationID)
	require.ErrorIs(t, err, forcedErr)
	assert.False(t, changed)
	require.NoError(t, db.First(subscription, subscription.Id).Error)
	require.NoError(t, db.First(token, token.Id).Error)
	require.NoError(t, db.Where("operation_id = ?", operation.OperationID).First(operation).Error)
	assert.Equal(t, WildFlowBillingStateReserved, operation.BillingState)
	assert.Equal(t, int64(quote.Quota), subscription.AmountUsed)
	assert.Equal(t, 100_000-quote.Quota, token.RemainQuota)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, db.Where("request_id = ?", operation.OperationID).First(&record).Error)
	assert.Equal(t, "consumed", record.Status)
	var canonicalCount int64
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).Count(&canonicalCount).Error)
	assert.Zero(t, canonicalCount)
}

func TestWildFlowGenericLogProjectionRetriesAfterIndependentLogDBFailure(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	logDB := useSeparateWildFlowLogDB(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "projection-retry")
	operation.BillingState = WildFlowBillingStateSettled
	operation.BillingUsageEventID = "usage-projection-retry"
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	forcedErr := errors.New("forced independent log database failure")
	callbackName := "test:wildflow-log-first-failure:" + uuid.NewString()
	var failed atomic.Bool
	require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "logs" && failed.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() { _ = logDB.Callback().Create().Remove(callbackName) })

	require.ErrorIs(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"), forcedErr)
	var entry WildFlowBillingLogEntry
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, WildFlowBillingProjectionFailed, entry.ProjectionState)
	assert.Equal(t, 1, entry.ProjectionAttempts)
	assert.Equal(t, "log_projection_failed", entry.ProjectionLastError)

	require.NoError(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"))
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, WildFlowBillingProjectionProjected, entry.ProjectionState)
	assert.GreaterOrEqual(t, entry.ProjectionAttempts, 2)
	assert.Greater(t, entry.ProjectedTime, int64(0))
	var logCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
}

func TestWildFlowGenericLogProjectionConcurrentRecoveryIsExactlyOnce(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	logDB := useSeparateWildFlowLogDB(t)
	mainSQL, err := db.DB()
	require.NoError(t, err)
	mainSQL.SetMaxOpenConns(1)
	logSQL, err := logDB.DB()
	require.NoError(t, err)
	logSQL.SetMaxOpenConns(1)
	_, _, operation := createWildFlowBillingFixture(t, db, "projection-concurrent-retry")
	operation.BillingState = WildFlowBillingStateSettled
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	forcedErr := errors.New("forced first projection failure")
	callbackName := "test:wildflow-log-concurrent-first-failure:" + uuid.NewString()
	var failed atomic.Bool
	require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "logs" && failed.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() { _ = logDB.Callback().Create().Remove(callbackName) })
	require.ErrorIs(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"), forcedErr)

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
	for projectionErr := range errorsByWorker {
		require.NoError(t, projectionErr)
	}
	var logCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
	var entry WildFlowBillingLogEntry
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, WildFlowBillingProjectionProjected, entry.ProjectionState)
}

func TestWildFlowGenericLogProjectionAdoptsWriteAfterStatusCrash(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	logDB := useSeparateWildFlowLogDB(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "projection-status-crash")
	operation.BillingState = WildFlowBillingStateSettled
	previousLogConsumeEnabled := common.LogConsumeEnabled
	common.LogConsumeEnabled = true
	t.Cleanup(func() { common.LogConsumeEnabled = previousLogConsumeEnabled })

	var logWritten atomic.Bool
	logCallback := "test:wildflow-log-written:" + uuid.NewString()
	require.NoError(t, logDB.Callback().Create().After("gorm:create").Register(logCallback, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "logs" && tx.Error == nil {
			logWritten.Store(true)
		}
	}))
	t.Cleanup(func() { _ = logDB.Callback().Create().Remove(logCallback) })
	forcedErr := errors.New("forced projection status persistence failure")
	mainCallback := "test:wildflow-projection-status-failure:" + uuid.NewString()
	var failed atomic.Bool
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(mainCallback, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "wild_flow_billing_log_entries" &&
			logWritten.Load() && failed.CompareAndSwap(false, true) {
			tx.AddError(forcedErr)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(mainCallback) })

	require.ErrorIs(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"), forcedErr)
	var logCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
	require.NoError(t, db.Model(&WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).
		Update("projection_lease_expires_at", time.Now().Add(-time.Minute).Unix()).Error)

	require.NoError(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"))
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount, "retry must adopt the existing legacy projection")
	var entry WildFlowBillingLogEntry
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, WildFlowBillingProjectionProjected, entry.ProjectionState)
}

func TestWildFlowGenericLogProjectionFailsClosedForClickHouse(t *testing.T) {
	db := setupWildFlowBillingModelTest(t)
	logDB := useSeparateWildFlowLogDB(t)
	_, _, operation := createWildFlowBillingFixture(t, db, "projection-clickhouse")
	operation.BillingState = WildFlowBillingStateSettled
	previousLogConsumeEnabled := common.LogConsumeEnabled
	previousLogDatabaseType := common.LogDatabaseType()
	common.LogConsumeEnabled = true
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	t.Cleanup(func() {
		common.LogConsumeEnabled = previousLogConsumeEnabled
		common.SetLogDatabaseType(previousLogDatabaseType)
	})

	err := RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClickHouse")
	var logCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Zero(t, logCount)
	var entry WildFlowBillingLogEntry
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, WildFlowBillingProjectionUnsupported, entry.ProjectionState)
	assert.Equal(t, "log_projection_idempotency_unsupported", entry.ProjectionLastError)
	firstAttempts := entry.ProjectionAttempts
	require.Error(t, RecordWildFlowBillingLog(operation, LogTypeConsume, "WildFlow job settled"))
	require.NoError(t, db.Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).First(&entry).Error)
	assert.Equal(t, firstAttempts, entry.ProjectionAttempts, "unsupported ClickHouse projection must remain a terminal observable state")
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
