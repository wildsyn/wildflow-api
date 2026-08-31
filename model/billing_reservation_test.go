package model

import (
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReservationFixture 建立用户 + Key + 预占表，返回各自 id。
func seedReservationFixture(t *testing.T, userQuota, tokenQuota int) (int, int, string) {
	t.Helper()
	user := &User{Username: fmt.Sprintf("reservation-user-%s", t.Name()), Quota: userQuota, Group: "default", AffCode: fmt.Sprintf("reservation-aff-%s", t.Name())}
	require.NoError(t, DB.Create(user).Error)
	token := &Token{
		UserId:      user.Id,
		Key:         fmt.Sprintf("sk-reservation-%s", t.Name()),
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

	// 追加 300：钱包无条件扣减（欠费合同），Key 条件扣减
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
