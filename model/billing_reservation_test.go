package model

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReservationFixture 建立用户 + Key + 预占表，返回各自 id。
var reservationFixtureSequence atomic.Uint64

func seedReservationFixture(t *testing.T, userQuota, tokenQuota int) (int, int, string) {
	t.Helper()
	fixtureID := fmt.Sprintf("%s-%d", t.Name(), reservationFixtureSequence.Add(1))
	user := &User{Username: fmt.Sprintf("reservation-user-%s", fixtureID), Quota: userQuota, Group: "default", AffCode: fmt.Sprintf("reservation-aff-%s", fixtureID)}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{
		UserId:      user.Id,
		Key:         fmt.Sprintf("sk-reservation-%s", fixtureID),
		Name:        "reservation-token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: tokenQuota,
	}
	require.NoError(t, DB.Create(token).Error)
	return user.Id, token.Id, token.Key
}

func reservationUserQuota(t *testing.T, userId int) int {
	t.Helper()
	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userId).Select("quota").Scan(&quota).Error)
	return quota
}

func reservationTokenState(t *testing.T, tokenId int) (remain int, used int) {
	t.Helper()
	var token Token
	require.NoError(t, DB.Where("id = ?", tokenId).First(&token).Error)
	return token.RemainQuota, token.UsedQuota
}

func TestReserveWalletBillingQuotaAtomicBothBoundaries(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)

	// 预占 800：账户与 Key 同时扣减成功
	reserved, err := ReserveWalletBillingQuota("req-atomic-1", userId, tokenId, tokenKey, 800, false)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, 200, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 200, remain)
	assert.Equal(t, 800, used)

	// 再次预占 300：账户与 Key 都不足，整体失败、零账变
	_, err = ReserveWalletBillingQuota("req-atomic-2", userId, tokenId, tokenKey, 300, false)
	require.ErrorIs(t, err, ErrInsufficientUserQuota)
	assert.Equal(t, 200, reservationUserQuota(t, userId), "user quota must not change on failed reserve")
	remain, used = reservationTokenState(t, tokenId)
	assert.Equal(t, 200, remain, "token remain must not change on failed reserve")
	assert.Equal(t, 800, used)
}

func TestReserveWalletBillingQuotaTokenBoundaryRollsBackUser(t *testing.T) {
	// 账户充足但 Key 不足：事务回滚后账户余额零账变（双边界一致性）
	userId, tokenId, tokenKey := seedReservationFixture(t, 5_000, 100)

	_, err := ReserveWalletBillingQuota("req-token-boundary", userId, tokenId, tokenKey, 300, false)
	require.ErrorIs(t, err, ErrInsufficientTokenQuota)
	assert.Equal(t, 5_000, reservationUserQuota(t, userId), "user quota must roll back when token boundary fails")
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 100, remain)
	assert.Equal(t, 0, used)
}

func TestReserveWalletBillingQuotaIdempotentOnReplay(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)

	reserved, err := ReserveWalletBillingQuota("req-replay", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.True(t, reserved)

	// 同一 requestId 重放：不重复扣减
	reserved, err = ReserveWalletBillingQuota("req-replay", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.False(t, reserved, "replay must report already-reserved")
	assert.Equal(t, 600, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 600, remain)
	assert.Equal(t, 400, used)
}

func TestReserveWalletBillingQuotaUnlimitedTokenStillChargesUser(t *testing.T) {
	// unlimited Key 无上限但仍记账 remain/used；账户仍是硬边界
	userId, tokenId, tokenKey := seedReservationFixture(t, 500, 0)

	reserved, err := ReserveWalletBillingQuota("req-unlimited", userId, tokenId, tokenKey, 300, true)
	require.NoError(t, err)
	require.True(t, reserved)
	assert.Equal(t, 200, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, -300, remain)
	assert.Equal(t, 300, used)
}

func TestReserveWalletBillingQuotaInsufficientUserQuota(t *testing.T) {
	// Key 充足但账户不足
	userId, tokenId, tokenKey := seedReservationFixture(t, 100, 5_000)

	_, err := ReserveWalletBillingQuota("req-user-boundary", userId, tokenId, tokenKey, 300, false)
	require.ErrorIs(t, err, ErrInsufficientUserQuota)
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 5_000, remain)
	assert.Equal(t, 0, used)
}

func TestReserveWalletBillingQuotaConcurrentDoesNotExceedCap(t *testing.T) {
	// 高并发竞态：Key 剩余 1000，20 个请求各预占 100 —— 恰好 10 个成功，
	// remain 不会为负；账户 1000 同步耗尽，不会透支。
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)

	const concurrency = 20
	const perRequest = 100
	var wg sync.WaitGroup
	results := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			requestId := fmt.Sprintf("req-concurrent-%d", i)
			_, err := ReserveWalletBillingQuota(requestId, userId, tokenId, tokenKey, perRequest, false)
			results[i] = err
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, err := range results {
		if err == nil {
			successCount++
		} else {
			require.ErrorIs(t, err, ErrInsufficientUserQuota)
		}
	}
	assert.Equal(t, 10, successCount, "exactly 10 requests must win the quota")
	assert.Equal(t, 0, reservationUserQuota(t, userId), "user balance must never go negative")
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 0, remain, "token remain must never go negative")
	assert.Equal(t, 1_000, used)
}

func TestSettleBillingReservationDeltaAndIdempotency(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-settle", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)

	// 实际用量 550：delta=+150 无条件补扣（取得资格后欠费有界）
	require.NoError(t, SettleBillingReservation("req-settle", 150))
	assert.Equal(t, 1_000-550, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000-550, remain)
	assert.Equal(t, 550, used)

	// 结算后记录为 settled：重复结算不再扣费
	require.NoError(t, SettleBillingReservation("req-settle", 150))
	assert.Equal(t, 1_000-550, reservationUserQuota(t, userId))

	// settled 记录不可释放：释放是幂等 no-op，不退任何已消费的额度
	require.NoError(t, ReleaseBillingReservation("req-settle", tokenKey))
	assert.Equal(t, 1_000-550, reservationUserQuota(t, userId))
	remain, used = reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000-550, remain)
	assert.Equal(t, 550, used)
}

func TestSettleBillingReservationRefundsOverestimate(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-settle-refund", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)

	// 实际用量 250：delta=-150 退还
	require.NoError(t, SettleBillingReservation("req-settle-refund", -150))
	assert.Equal(t, 1_000-250, reservationUserQuota(t, userId))
	remain, _ := reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000-250, remain)
}

func TestReleaseBillingReservationExactOnce(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-release", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.Equal(t, 600, reservationUserQuota(t, userId))

	require.NoError(t, ReleaseBillingReservation("req-release", tokenKey))
	assert.Equal(t, 1_000, reservationUserQuota(t, userId), "release must refund full reserve")
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000, remain)
	assert.Equal(t, 0, used)

	// 幂等：重复释放不重复退款
	require.NoError(t, ReleaseBillingReservation("req-release", tokenKey))
	assert.Equal(t, 1_000, reservationUserQuota(t, userId))
}

func TestAppendBillingReservationTopUpAtomically(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-append", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)

	// 追加 300：账户与 Key 同事务条件扣减
	require.NoError(t, AppendBillingReservation("req-append", 300))
	// 1000 - 400 - 300 = 300
	assert.Equal(t, 300, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 300, remain)
	assert.Equal(t, 700, used)

	// 追加额度计入预占总额：释放时全部退还
	require.NoError(t, ReleaseBillingReservation("req-append", tokenKey))
	assert.Equal(t, 1_000, reservationUserQuota(t, userId))
	remain, used = reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000, remain)
	assert.Equal(t, 0, used)
}

func TestAppendBillingReservationTokenBoundaryRollsBackWallet(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 500)
	_, err := ReserveWalletBillingQuota("req-append-boundary", userId, tokenId, tokenKey, 300, false)
	require.NoError(t, err)

	// 追加 300 超出 Key 剩余（500-300=200）：整体回滚，钱包不扣
	require.ErrorIs(t, AppendBillingReservation("req-append-boundary", 300), ErrInsufficientTokenQuota)
	assert.Equal(t, 700, reservationUserQuota(t, userId), "wallet must not be charged when token top-up fails")
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 200, remain)
	assert.Equal(t, 300, used)
}

func TestAppendBillingReservationInsufficientAccountRollsBackToken(t *testing.T) {
	// 账户余额不足以追加：整体回滚，Key 零账变，请求不得继续发往 Provider
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 5_000)
	_, err := ReserveWalletBillingQuota("req-append-acct", userId, tokenId, tokenKey, 800, false)
	require.NoError(t, err)
	require.Equal(t, 200, reservationUserQuota(t, userId))

	require.ErrorIs(t, AppendBillingReservation("req-append-acct", 300), ErrInsufficientUserQuota)
	assert.Equal(t, 200, reservationUserQuota(t, userId), "account must not go negative on append")
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 5_000-800, remain, "token must roll back when account append fails")
	assert.Equal(t, 800, used)
}

func TestAppendBillingReservationAfterProviderStartedRejected(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-append-started", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.NoError(t, MarkBillingReservationProviderStarted("req-append-started"))

	// provider_started（Provider 可能已收到请求）不允许追加
	require.ErrorIs(t, AppendBillingReservation("req-append-started", 100), ErrBillingReservationStateConflict)
	assert.Equal(t, 600, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 600, remain)
	assert.Equal(t, 400, used)
}

func TestReleaseStaleSkipsProviderStartedRecords(t *testing.T) {
	// Provider 触达后崩溃：记录停在 provider_started，恢复任务不得退款
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-started-stale", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.NoError(t, MarkBillingReservationProviderStarted("req-started-stale"))
	require.NoError(t, DB.Model(&BillingReservationRecord{}).Where("request_id = ?", "req-started-stale").
		UpdateColumn("updated_at", GetDBTimestamp()-7200).Error)

	released, err := ReleaseStaleBillingReservations(60, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), released, "provider_started records must never be auto-released")
	assert.Equal(t, 600, reservationUserQuota(t, userId), "consumed-by-provider charge must not be refunded")

	// 活跃进程的失败路径（Provider 明确失败/取消）可以释放 provider_started 记录
	require.NoError(t, ReleaseBillingReservation("req-started-stale", tokenKey))
	assert.Equal(t, 1_000, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000, remain)
	assert.Equal(t, 0, used)
}

func TestMarkProviderStartedFailsClosedForMissingAndTerminalRecords(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-mark", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)

	require.NoError(t, MarkBillingReservationProviderStarted("req-mark"))
	_ = tokenKey
	// 幂等：重复标记 no-op
	require.NoError(t, MarkBillingReservationProviderStarted("req-mark"))

	require.NoError(t, SettleBillingReservation("req-mark", 0))
	// settled 或不存在记录都不能当作成功：发送入口必须停止 Provider 请求。
	require.ErrorIs(t, MarkBillingReservationProviderStarted("req-mark"), ErrBillingReservationStateConflict)
	require.ErrorIs(t, MarkBillingReservationProviderStarted("missing-request"), ErrBillingReservationNotFound)
	_, err = ReserveWalletBillingQuota("req-mark-released", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)
	require.NoError(t, ReleaseBillingReservation("req-mark-released", tokenKey))
	require.ErrorIs(t, MarkBillingReservationProviderStarted("req-mark-released"), ErrBillingReservationStateConflict)
	var rec BillingReservationRecord
	require.NoError(t, DB.Where("request_id = ?", "req-mark").First(&rec).Error)
	assert.Equal(t, BillingReservationStateSettled, rec.State)
}

func TestProviderStartedConcurrentMarkSettleReleaseAndRecovery(t *testing.T) {
	// All contenders cross the same barrier: this deliberately does not wait for
	// mark to finish before settle, explicit release, and stale recovery start.
	// SQLite serializes the resulting writes, while the state machine decides
	// which terminal transition wins.
	userId, tokenId, tokenKey := seedReservationFixture(t, 2_000, 2_000)
	const requestID = "req-provider-started-concurrent"
	_, err := ReserveWalletBillingQuota(requestID, userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&BillingReservationRecord{}).Where("request_id = ?", requestID).
		UpdateColumn("updated_at", GetDBTimestamp()-7200).Error)

	const markers = 8
	start := make(chan struct{})
	markErrs := make(chan error, markers)
	settleErrs := make(chan error, 1)
	releaseErrs := make(chan error, 1)
	type recoveryResult struct {
		released int64
		err      error
	}
	recoveryResults := make(chan recoveryResult, 1)
	var ready sync.WaitGroup
	var wg sync.WaitGroup
	for range markers {
		ready.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			markErrs <- MarkBillingReservationProviderStarted(requestID)
		}()
	}
	ready.Add(3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		ready.Done()
		<-start
		settleErrs <- SettleBillingReservation(requestID, 0)
	}()
	go func() {
		defer wg.Done()
		ready.Done()
		<-start
		releaseErrs <- ReleaseBillingReservation(requestID, tokenKey)
	}()
	go func() {
		defer wg.Done()
		ready.Done()
		<-start
		released, recoveryErr := ReleaseStaleBillingReservations(60, 100)
		recoveryResults <- recoveryResult{released: released, err: recoveryErr}
	}()

	ready.Wait()
	close(start)
	wg.Wait()
	close(markErrs)
	for markErr := range markErrs {
		if markErr != nil {
			require.ErrorIs(t, markErr, ErrBillingReservationStateConflict)
		}
	}
	require.NoError(t, <-settleErrs)
	require.NoError(t, <-releaseErrs)
	recovery := <-recoveryResults
	require.NoError(t, recovery.err)
	assert.LessOrEqual(t, recovery.released, int64(1), "one stale scan must never report more than one release")

	var record BillingReservationRecord
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&record).Error)
	switch record.State {
	case BillingReservationStateSettled:
		assert.Equal(t, 1_600, reservationUserQuota(t, userId), "settle retains the one reservation charge")
		remain, used := reservationTokenState(t, tokenId)
		assert.Equal(t, 1_600, remain)
		assert.Equal(t, 400, used)
	case BillingReservationStateReleased:
		assert.Equal(t, 2_000, reservationUserQuota(t, userId), "a release refunds exactly once")
		remain, used := reservationTokenState(t, tokenId)
		assert.Equal(t, 2_000, remain)
		assert.Equal(t, 0, used)
	default:
		t.Fatalf("concurrent terminal operations left non-terminal state %q", record.State)
	}

	// A late sender must fail closed after either terminal outcome. Replays of
	// settle/release are no-ops: neither may revive the terminal state or alter
	// its quota outcome.
	require.ErrorIs(t, MarkBillingReservationProviderStarted(requestID), ErrBillingReservationStateConflict)
	require.NoError(t, SettleBillingReservation(requestID, 0))
	require.NoError(t, ReleaseBillingReservation(requestID, tokenKey))
	var terminal BillingReservationRecord
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&terminal).Error)
	assert.Equal(t, record.State, terminal.State)
}

func TestSettleBillingReservationFailureStaysRecoverable(t *testing.T) {
	// 结算差额调整失败（订阅溢出）：整体回滚，记录保持 provider_started 可重试，
	// 不得终结成"部分结算"
	require.NoError(t, DB.AutoMigrate(&SubscriptionPreConsumeRecord{}))
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	plan := &SubscriptionPlan{Title: "settle-fail", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100}
	require.NoError(t, DB.Create(plan).Error)
	sub := &UserSubscription{
		UserId:      userId,
		PlanId:      plan.Id,
		AmountTotal: 100,
		AmountUsed:  90,
		Status:      "active",
		StartTime:   time.Now().Unix(),
		EndTime:     time.Now().Add(24 * time.Hour).Unix(),
	}
	require.NoError(t, DB.Create(sub).Error)

	_, reserved, err := ReserveSubscriptionBillingQuota("req-settle-fail", userId, tokenId, tokenKey, 10, false)
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, MarkBillingReservationProviderStarted("req-settle-fail"))

	// 实际用量超过订阅剩余（100-90=10，实际 20）：delta=+10 溢出 → 失败
	settleErr := SettleBillingReservation("req-settle-fail", 10)
	require.Error(t, settleErr, "overflowing subscription delta must fail settlement")

	// 记录保持 provider_started（可恢复状态）：账变未收口
	var rec BillingReservationRecord
	require.NoError(t, DB.Where("request_id = ?", "req-settle-fail").First(&rec).Error)
	assert.Equal(t, BillingReservationStateProviderStarted, rec.State)
	assert.Equal(t, 1_000, reservationUserQuota(t, userId), "wallet untouched by subscription settle failure")

	// 重试结算合法差额（实际用量等于预占）：成功收口
	require.NoError(t, SettleBillingReservation("req-settle-fail", 0))
	require.NoError(t, DB.Where("request_id = ?", "req-settle-fail").First(&rec).Error)
	assert.Equal(t, BillingReservationStateSettled, rec.State)
}

func TestReleaseStaleBillingReservationsRecoversAfterCrash(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-stale", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)

	// 窗口下限保护：近期记录不被恢复任务误释放
	released, err := ReleaseStaleBillingReservations(60, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), released, "recent reservations must not be released")
	assert.Equal(t, 600, reservationUserQuota(t, userId))

	// 把记录回拨到陈旧窗口之前（模拟进程崩溃/重启后遗留的 reserved 记录）
	require.NoError(t, DB.Model(&BillingReservationRecord{}).Where("request_id = ?", "req-stale").
		UpdateColumn("updated_at", GetDBTimestamp()-3600).Error)

	// 超过陈旧窗口：释放并退款
	released, err = ReleaseStaleBillingReservations(60, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), released)
	assert.Equal(t, 1_000, reservationUserQuota(t, userId))
	remain, used := reservationTokenState(t, tokenId)
	assert.Equal(t, 1_000, remain)
	assert.Equal(t, 0, used)

	// 恢复幂等：再次执行不重复退款
	released, err = ReleaseStaleBillingReservations(60, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), released)
	assert.Equal(t, 1_000, reservationUserQuota(t, userId))
}

func TestCleanupBillingReservationRecordsRemovesOnlyTerminal(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)
	_, err := ReserveWalletBillingQuota("req-cleanup-settled", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)
	_, err = ReserveWalletBillingQuota("req-cleanup-released", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)
	require.NoError(t, SettleBillingReservation("req-cleanup-settled", 0))
	require.NoError(t, ReleaseBillingReservation("req-cleanup-released", tokenKey))

	// 把本测试产生的已终结记录回拨到默认清理窗口之前
	// （UpdateColumn 绕过 UpdatedAt 自动填充，保证回拨生效）
	require.NoError(t, DB.Model(&BillingReservationRecord{}).
		Where("state IN ? AND request_id IN ?", []string{BillingReservationStateSettled, BillingReservationStateReleased},
			[]string{"req-cleanup-settled", "req-cleanup-released"}).
		UpdateColumn("updated_at", GetDBTimestamp()-7*24*3600-60).Error)

	deleted, err := CleanupBillingReservationRecords(0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(2), "both terminal records must be cleaned")

	var terminalCount int64
	require.NoError(t, DB.Model(&BillingReservationRecord{}).
		Where("request_id IN ?", []string{"req-cleanup-settled", "req-cleanup-released"}).
		Count(&terminalCount).Error)
	assert.Equal(t, int64(0), terminalCount, "cleaned records must be gone")
}

func TestReserveWalletBillingQuotaRejectsInvalidArgs(t *testing.T) {
	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)

	_, err := ReserveWalletBillingQuota("", userId, tokenId, tokenKey, 100, false)
	require.ErrorIs(t, err, ErrBillingReservationNotFound)

	_, err = ReserveWalletBillingQuota("req-invalid-user", 0, tokenId, tokenKey, 100, false)
	require.Error(t, err)

	_, err = ReserveWalletBillingQuota("req-negative", userId, tokenId, tokenKey, -1, false)
	require.Error(t, err)
}

// TestBillingReservationCacheDeltaSigns 验证缓存同步的 HINCRBY 符号合同：
// 数据库扣减 N（预占/补扣）→ 缓存余额必须减少 N（HINCRBY -N）；
// 数据库退还 N（释放/退差）→ 缓存余额必须增加 N（HINCRBY +N）。
// 符号反了会造成错误余额展示、错误前置拒绝或持续回源。
func TestBillingReservationCacheDeltaSigns(t *testing.T) {
	server := miniredis.RunT(t)
	oldRedisEnabled, oldRDB := common.RedisEnabled, common.RDB
	common.RedisEnabled = true
	common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = common.RDB.Close()
		common.RedisEnabled, common.RDB = oldRedisEnabled, oldRDB
	})

	userId, tokenId, tokenKey := seedReservationFixture(t, 1_000, 1_000)

	// 预置缓存：用户 Quota=1000，Key RemainQuota=1000（带 TTL，否则 HINCRBY 跳过）
	userKey := fmt.Sprintf("user:%d", userId)
	tokenCacheKey := fmt.Sprintf("token:%s", common.GenerateHMAC(tokenKey))
	require.NoError(t, common.RDB.HSet(context.Background(), userKey, "Quota", 1000).Err())
	require.NoError(t, common.RDB.Expire(context.Background(), userKey, time.Minute).Err())
	require.NoError(t, common.RDB.HSet(context.Background(), tokenCacheKey, "RemainQuota", 1000).Err())
	require.NoError(t, common.RDB.Expire(context.Background(), tokenCacheKey, time.Minute).Err())

	// 预占 400：DB 扣 400，缓存必须 -400
	_, err := ReserveWalletBillingQuota("req-cache-1", userId, tokenId, tokenKey, 400, false)
	require.NoError(t, err)
	userQuota, err := common.RDB.HGet(context.Background(), userKey, "Quota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(600), userQuota, "reserve must decrement cached user quota by 400")
	tokenQuota, err := common.RDB.HGet(context.Background(), tokenCacheKey, "RemainQuota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(600), tokenQuota, "reserve must decrement cached token quota by 400")

	// 结算补扣 150：DB 扣 150，缓存必须 -150
	require.NoError(t, SettleBillingReservation("req-cache-1", 150))
	userQuota, err = common.RDB.HGet(context.Background(), userKey, "Quota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), userQuota, "settle top-up must decrement cached user quota by 150")
	tokenQuota, err = common.RDB.HGet(context.Background(), tokenCacheKey, "RemainQuota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), tokenQuota, "settle top-up must decrement cached token quota by 150")

	// 结算退差 100：DB 退 100，缓存必须 +100
	_, err = ReserveWalletBillingQuota("req-cache-2", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)
	require.NoError(t, SettleBillingReservation("req-cache-2", -100))
	userQuota, err = common.RDB.HGet(context.Background(), userKey, "Quota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), userQuota, "refund must restore cached user quota")
	tokenQuota, err = common.RDB.HGet(context.Background(), tokenCacheKey, "RemainQuota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), tokenQuota, "refund must restore cached token quota")

	// 释放（Provider 明确失败，未结算）：DB 退 100，缓存必须 +100
	_, err = ReserveWalletBillingQuota("req-cache-3", userId, tokenId, tokenKey, 100, false)
	require.NoError(t, err)
	require.NoError(t, MarkBillingReservationProviderStarted("req-cache-3"))
	require.NoError(t, ReleaseBillingReservation("req-cache-3", tokenKey))
	userQuota, err = common.RDB.HGet(context.Background(), userKey, "Quota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), userQuota, "reserve+release net zero on cache")
	tokenQuota, err = common.RDB.HGet(context.Background(), tokenCacheKey, "RemainQuota").Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(450), tokenQuota, "reserve+release net zero on cache")
}
