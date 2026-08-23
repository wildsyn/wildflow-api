package model

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestWildFlowPostgresMigrationConcurrencyAndFaultRecovery(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run WildFlow PostgreSQL compatibility tests")
	}

	mainDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	logDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	mainSQL, err := mainDB.DB()
	require.NoError(t, err)
	logSQL, err := logDB.DB()
	require.NoError(t, err)
	mainSQL.SetMaxOpenConns(16)
	logSQL.SetMaxOpenConns(16)

	previousDB := DB
	previousLogDB := LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousLogConsumeEnabled := common.LogConsumeEnabled
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	DB = mainDB
	LOG_DB = logDB
	common.RedisEnabled = false
	common.LogConsumeEnabled = true
	common.SetDatabaseTypes(common.DatabaseTypePostgreSQL, common.DatabaseTypePostgreSQL)
	t.Cleanup(func() {
		DB = previousDB
		LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.LogConsumeEnabled = previousLogConsumeEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		_ = mainSQL.Close()
		_ = logSQL.Close()
	})

	require.NoError(t, mainDB.AutoMigrate(
		&User{},
		&Token{},
		&Log{},
		&SubscriptionPlan{},
		&UserSubscription{},
		&SubscriptionPreConsumeRecord{},
		&WildFlowOperation{},
		&WildFlowUsageEvent{},
		&WildFlowBillingLogEntry{},
		&WildFlowBillingLogProjectionReceipt{},
	))
	require.NoError(t, logDB.AutoMigrate(&Log{}, &WildFlowBillingLogProjectionReceipt{}))
	for _, column := range []string{
		"submission_phase",
		"submission_owner",
		"submission_lease_token",
		"submission_lease_expires_at",
		"submission_retry_until",
		"submission_attempt",
	} {
		assert.True(t, mainDB.Migrator().HasColumn(&WildFlowOperation{}, column), column)
	}
	assert.True(t, logDB.Migrator().HasTable(&WildFlowBillingLogProjectionReceipt{}))
	for _, column := range []string{
		"projection_state",
		"projection_attempts",
		"projection_claim_token",
		"projection_lease_expires_at",
	} {
		assert.True(t, mainDB.Migrator().HasColumn(&WildFlowBillingLogEntry{}, column), column)
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	user, _, operation := prepareSuccessfulWildFlowBillingForSettlement(t, mainDB, "pgs-"+suffix)
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
	require.NoError(t, mainDB.First(user, user.Id).Error)
	assert.Equal(t, testWildFlowBillingQuote().Quota, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	var canonicalCount int64
	require.NoError(t, mainDB.Model(&WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume).
		Count(&canonicalCount).Error)
	assert.Equal(t, int64(1), canonicalCount)

	var settledOperation WildFlowOperation
	require.NoError(t, mainDB.Where("operation_id = ?", operation.OperationID).First(&settledOperation).Error)
	projectionErr := errors.New("forced PostgreSQL log projection failure")
	projectionCallback := "test:wildflow-postgres-log-failure:" + uuid.NewString()
	var projectionFailed atomic.Bool
	require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register(projectionCallback, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "logs" && projectionFailed.CompareAndSwap(false, true) {
			tx.AddError(projectionErr)
		}
	}))
	t.Cleanup(func() { _ = logDB.Callback().Create().Remove(projectionCallback) })
	require.ErrorIs(t, RecordWildFlowBillingLog(&settledOperation, LogTypeConsume, "WildFlow job settled"), projectionErr)

	errorsByWorker = make(chan error, workers)
	wait = sync.WaitGroup{}
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByWorker <- RecordWildFlowBillingLog(&settledOperation, LogTypeConsume, "WildFlow job settled")
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for recordErr := range errorsByWorker {
		require.NoError(t, recordErr)
	}
	var logCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", operation.OperationID, LogTypeConsume).
		Count(&logCount).Error)
	assert.Equal(t, int64(1), logCount)
	var canonical WildFlowBillingLogEntry
	require.NoError(t, mainDB.Where(
		"operation_id = ? AND log_type = ?", operation.OperationID, LogTypeConsume,
	).First(&canonical).Error)
	assert.Equal(t, WildFlowBillingProjectionProjected, canonical.ProjectionState)

	_, _, takeoverOperation := createWildFlowBillingFixture(t, mainDB, "pg-takeover-"+suffix)
	takeoverOperation.BillingState = WildFlowBillingStateSettled
	takeoverOperation.BillingSource = WildFlowBillingSourceWallet
	takeoverOperation.BillingUsageEventID = "usage-pg-takeover-" + suffix
	require.NoError(t, mainDB.Transaction(func(tx *gorm.DB) error {
		_, ensureErr := ensureWildFlowCanonicalBillingLogTx(tx, takeoverOperation, LogTypeConsume, "WildFlow takeover projection")
		return ensureErr
	}))
	firstPaused := make(chan struct{})
	releaseFirst := make(chan struct{})
	pauseCallback := "test:wildflow-postgres-log-pause:" + uuid.NewString()
	var paused atomic.Bool
	require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register(pauseCallback, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "logs" && paused.CompareAndSwap(false, true) {
			close(firstPaused)
			<-releaseFirst
		}
	}))
	t.Cleanup(func() { _ = logDB.Callback().Create().Remove(pauseCallback) })
	projectionResults := make(chan error, 2)
	go func() {
		projectionResults <- RecordWildFlowBillingLog(takeoverOperation, LogTypeConsume, "WildFlow takeover projection")
	}()
	select {
	case <-firstPaused:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "first PostgreSQL projection did not pause before external insert")
	}
	require.NoError(t, mainDB.Model(&WildFlowBillingLogEntry{}).
		Where("operation_id = ? AND log_type = ?", takeoverOperation.OperationID, LogTypeConsume).
		Update("projection_lease_expires_at", time.Now().Add(-time.Minute).Unix()).Error)
	go func() {
		projectionResults <- RecordWildFlowBillingLog(takeoverOperation, LogTypeConsume, "WildFlow takeover projection")
	}()
	require.Eventually(t, func() bool {
		var entry WildFlowBillingLogEntry
		if err := mainDB.Where("operation_id = ? AND log_type = ?", takeoverOperation.OperationID, LogTypeConsume).First(&entry).Error; err != nil {
			return false
		}
		return entry.ProjectionAttempts >= 2
	}, 5*time.Second, 10*time.Millisecond, "expired lease must permit a takeover claim")
	close(releaseFirst)
	var projectionErrors []error
	for index := 0; index < 2; index++ {
		projectionErrors = append(projectionErrors, <-projectionResults)
	}
	assert.True(t, projectionErrors[0] == nil || projectionErrors[1] == nil, "takeover owner must complete projection")
	var takeoverLogCount int64
	require.NoError(t, logDB.Model(&Log{}).
		Where("request_id = ? AND type = ?", takeoverOperation.OperationID, LogTypeConsume).
		Count(&takeoverLogCount).Error)
	assert.Equal(t, int64(1), takeoverLogCount, "lease takeover must not duplicate an already in-flight external insert")
	canonical = WildFlowBillingLogEntry{}
	require.NoError(t, mainDB.Where(
		"operation_id = ? AND log_type = ?", takeoverOperation.OperationID, LogTypeConsume,
	).First(&canonical).Error)
	assert.Equal(t, WildFlowBillingProjectionProjected, canonical.ProjectionState)
	reconciledAudits, err := ReconcileWildFlowCanonicalBillingAudits(100)
	require.NoError(t, err)
	assert.Zero(t, reconciledAudits)

	_, _, faultSettle := prepareSuccessfulWildFlowBillingForSettlement(t, mainDB, "pgsf-"+suffix)
	forcedSettleErr := failNextWildFlowCanonicalAuditCreate(t, mainDB)
	_, changed, err := SettleWildFlowOperationBilling(faultSettle.OperationID)
	require.ErrorIs(t, err, forcedSettleErr)
	assert.False(t, changed)
	require.NoError(t, mainDB.Where("operation_id = ?", faultSettle.OperationID).First(faultSettle).Error)
	assert.Equal(t, WildFlowBillingStateReserved, faultSettle.BillingState)
	var faultSettleUser User
	require.NoError(t, mainDB.First(&faultSettleUser, faultSettle.UserID).Error)
	assert.Zero(t, faultSettleUser.UsedQuota)
	assert.Zero(t, faultSettleUser.RequestCount)

	walletUser, walletToken, faultWallet := createWildFlowBillingFixture(t, mainDB, "pgwf-"+suffix)
	_, err = ReserveWildFlowWalletBilling(faultWallet.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(faultWallet.OperationID, "job-pg-wallet-fault", "failed", "execution_failed"))
	forcedWalletErr := failNextWildFlowCanonicalAuditCreate(t, mainDB)
	_, changed, err = RefundWildFlowOperationBilling(faultWallet.OperationID)
	require.ErrorIs(t, err, forcedWalletErr)
	assert.False(t, changed)
	require.NoError(t, mainDB.First(walletUser, walletUser.Id).Error)
	require.NoError(t, mainDB.First(walletToken, walletToken.Id).Error)
	assert.Equal(t, 100_000-testWildFlowBillingQuote().Quota, walletUser.Quota)
	assert.Equal(t, 100_000-testWildFlowBillingQuote().Quota, walletToken.RemainQuota)
	require.NoError(t, mainDB.Where("operation_id = ?", faultWallet.OperationID).First(faultWallet).Error)
	assert.Equal(t, WildFlowBillingStateReserved, faultWallet.BillingState)

	subscriptionUser, subscriptionToken, faultSubscription := createWildFlowBillingFixture(t, mainDB, "pgsub-"+suffix)
	plan := &SubscriptionPlan{
		Title: "PostgreSQL refund fault", DurationUnit: SubscriptionDurationMonth,
		DurationValue: 1, TotalAmount: 100_000,
	}
	require.NoError(t, mainDB.Create(plan).Error)
	subscription := &UserSubscription{
		UserId: subscriptionUser.Id, PlanId: plan.Id, AmountTotal: 100_000,
		StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active",
	}
	require.NoError(t, mainDB.Create(subscription).Error)
	_, err = ReserveWildFlowSubscriptionBilling(faultSubscription.OperationID, testWildFlowBillingQuote())
	require.NoError(t, err)
	require.NoError(t, UpdateWildFlowOperationExecution(
		faultSubscription.OperationID, "job-pg-subscription-fault", "failed", "execution_failed",
	))
	forcedSubscriptionErr := failNextWildFlowCanonicalAuditCreate(t, mainDB)
	_, changed, err = RefundWildFlowOperationBilling(faultSubscription.OperationID)
	require.ErrorIs(t, err, forcedSubscriptionErr)
	assert.False(t, changed)
	require.NoError(t, mainDB.First(subscription, subscription.Id).Error)
	require.NoError(t, mainDB.First(subscriptionToken, subscriptionToken.Id).Error)
	assert.Equal(t, int64(testWildFlowBillingQuote().Quota), subscription.AmountUsed)
	assert.Equal(t, 100_000-testWildFlowBillingQuote().Quota, subscriptionToken.RemainQuota)
	var preConsume SubscriptionPreConsumeRecord
	require.NoError(t, mainDB.Where("request_id = ?", faultSubscription.OperationID).First(&preConsume).Error)
	assert.Equal(t, "consumed", preConsume.Status)

	_, _, leaseOperation := createWildFlowBillingFixture(t, mainDB, "pgl-"+suffix)
	require.NoError(t, mainDB.Model(&WildFlowOperation{}).
		Where("operation_id = ?", leaseOperation.OperationID).
		Updates(map[string]any{
			"billing_state":          WildFlowBillingStateReserved,
			"submission_retry_until": time.Now().Add(time.Minute).Unix(),
		}).Error)
	owners := make(chan string, workers)
	errorsByWorker = make(chan error, workers)
	wait = sync.WaitGroup{}
	for index := 0; index < workers; index++ {
		owner := "pg-owner-" + strings.ReplaceAll(uuid.NewString(), "-", "")
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, acquired, claimErr := ClaimWildFlowOperationSubmission(
				leaseOperation.OperationID, owner, uuid.NewString(), time.Now().Add(time.Minute).Unix(),
			)
			if acquired {
				owners <- owner
			}
			errorsByWorker <- claimErr
		}()
	}
	wait.Wait()
	close(owners)
	close(errorsByWorker)
	for claimErr := range errorsByWorker {
		require.NoError(t, claimErr)
	}
	ownerCount := 0
	for range owners {
		ownerCount++
	}
	assert.Equal(t, 1, ownerCount)
}
