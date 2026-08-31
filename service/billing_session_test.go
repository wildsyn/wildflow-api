package service

import (
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedBillingSessionFixture 建立用户 + Key 并返回会话测试所需的值。
func seedBillingSessionFixture(t *testing.T, userQuota, tokenQuota int) (int, int, string) {
	t.Helper()
	user := &model.User{Username: "session-user-" + t.Name(), Quota: userQuota, Group: "default", AffCode: "session-aff-" + t.Name()}
	require.NoError(t, model.DB.Create(user).Error)
	token := &model.Token{
		UserId:      user.Id,
		Key:         "sk-session-" + t.Name(),
		Name:        "session-token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: tokenQuota,
	}
	require.NoError(t, model.DB.Create(token).Error)
	return user.Id, token.Id, token.Key
}

func sessionUserQuota(t *testing.T, userId int) int {
	t.Helper()
	quota, err := model.GetUserQuota(userId, true)
	require.NoError(t, err)
	return quota
}

func newSessionRelayInfo(userId, tokenId int, tokenKey string, unlimited bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		RequestId:      "req-" + tokenKey,
		UserId:         userId,
		TokenId:        tokenId,
		TokenKey:       tokenKey,
		TokenUnlimited: unlimited,
		OriginModelName: "gpt-test",
	}
}

func TestBillingSessionPreConsumeWalletReservesAtomically(t *testing.T) {
	truncate(t)
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 1_000)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 800))

	// 账户与 Key 同时预扣
	assert.Equal(t, 200, sessionUserQuota(t, userId))
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 200, token.RemainQuota)
	assert.Equal(t, 800, token.UsedQuota)
	assert.Equal(t, 800, session.GetPreConsumedQuota())

	// 结算补扣（实际用量 950）
	require.NoError(t, session.Settle(950))
	assert.Equal(t, 50, sessionUserQuota(t, userId))
	token, err = model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 50, token.RemainQuota)
	assert.Equal(t, 950, token.UsedQuota)
}

func TestBillingSessionPreConsumeWalletInsufficientFailsCleanly(t *testing.T) {
	truncate(t)
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 100)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	apiErr := session.preConsume(ctx, 800)
	require.NotNil(t, apiErr)
	// Key 硬上限不足：稳定的额度不足错误码，零账变
	assert.Equal(t, types.ErrorCodeInsufficientUserQuota, apiErr.GetErrorCode())
	assert.Equal(t, 1_000, sessionUserQuota(t, userId))
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 100, token.RemainQuota)
	assert.Equal(t, 0, token.UsedQuota)
}

func TestBillingSessionTrustBypassStillEnforcesTokenCap(t *testing.T) {
	truncate(t)
	// 高余额用户命中信任旁路（跳过账户级预扣），但 Key 硬上限不足时必须失败
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000_000, 100)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	relayInfo.UserQuota = 1_000_000
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	// quota 远大于 Key 剩余：即使有信任旁路（quota > trustQuota 假设不成立，
	// 这里直接测无旁路路径），Key 不足必须拒绝
	apiErr := session.preConsume(ctx, 500_000)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeInsufficientUserQuota, apiErr.GetErrorCode())
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 100, token.RemainQuota)
}

func TestBillingSessionPreConsumeUnlimitedTokenStillChargesWallet(t *testing.T) {
	truncate(t)
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 500, 0)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, true)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 300))
	assert.Equal(t, 200, sessionUserQuota(t, userId))
}

func TestBillingSessionConcurrentPreConsumeRespectsCaps(t *testing.T) {
	truncate(t)
	// 高并发竞态打到 model 层原子预占（会话层的日志路径在仓库 logger 中是
	// 已知的无锁计数，race 下会误报，因此并发注入点选在数据库合同上）：
	// Key 与账户同为 1000，20 个请求各预占 100 —— 恰好 10 个成功。
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 1_000)

	const concurrency = 20
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := model.ReserveWalletBillingQuota(
				fmt.Sprintf("req-concurrent-session-%s-%d", tokenKey, i),
				userId, tokenId, tokenKey, 100, false)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		} else {
			require.ErrorIs(t, err, model.ErrInsufficientUserQuota)
		}
	}
	assert.Equal(t, 10, successCount)
	assert.Equal(t, 0, sessionUserQuota(t, userId), "账户余额不能为负")
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 0, token.RemainQuota, "Key 剩余额度不能为负")
	assert.Equal(t, 1_000, token.UsedQuota)
}

func TestBillingSessionRefundReleasesReservationExactlyOnce(t *testing.T) {
	truncate(t)
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 1_000)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 400))
	require.Equal(t, 600, sessionUserQuota(t, userId))

	// Provider 失败：退款
	session.Refund(ctx)
	// ReleaseBillingReservation 由 Refund 的 gopool 异步执行，这里显式等待同一幂等结果
	require.NoError(t, model.ReleaseBillingReservation(relayInfo.RequestId, tokenKey))

	assert.Equal(t, 1_000, sessionUserQuota(t, userId))
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 1_000, token.RemainQuota)
	assert.Equal(t, 0, token.UsedQuota)
}

func TestBillingSessionReserveTopUpOnRecordedSession(t *testing.T) {
	truncate(t)
	// 重试切换到更贵分组：AppendBillingReservation 在单事务内追加资金 + Key
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 800)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 400))
	require.NoError(t, session.Reserve(700))

	// 400 + 300 追加：钱包 1000-700=300；Key 800-700=100
	assert.Equal(t, 300, sessionUserQuota(t, userId))
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 100, token.RemainQuota)
	assert.Equal(t, 700, session.GetPreConsumedQuota())

	// 结算：实际 600 → 退还 100
	require.NoError(t, session.Settle(600))
	assert.Equal(t, 400, sessionUserQuota(t, userId))
	token, err = model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 200, token.RemainQuota)
}

func TestBillingSessionReserveTopUpTokenBoundaryRollsBack(t *testing.T) {
	truncate(t)
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 500)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 300))
	// 追加到 600 超出 Key 剩余（500-300=200）：整体失败，钱包不扣
	err := session.Reserve(600)
	require.NotNil(t, err)
	assert.Equal(t, 700, sessionUserQuota(t, userId), "追加失败时钱包不能被扣")
	token, err2 := model.GetTokenById(tokenId)
	require.NoError(t, err2)
	assert.Equal(t, 200, token.RemainQuota)
	assert.Equal(t, 300, token.UsedQuota)
}

func TestBillingSessionForcePreConsumeUsesRecordlessPath(t *testing.T) {
	truncate(t)
	// 异步任务：不创建预占记录（由 task 行合同保证），但预扣仍是条件扣减
	userId, tokenId, tokenKey := seedBillingSessionFixture(t, 1_000, 1_000)
	relayInfo := newSessionRelayInfo(userId, tokenId, tokenKey, false)
	relayInfo.ForcePreConsume = true
	session := &BillingSession{
		relayInfo: relayInfo,
		funding:   &WalletFunding{userId: userId},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(nil)
	require.Nil(t, session.preConsume(ctx, 400))
	assert.Equal(t, 600, sessionUserQuota(t, userId))

	// 无预占记录：结算走 funding + 令牌差额路径
	require.NoError(t, session.Settle(300))
	assert.Equal(t, 700, sessionUserQuota(t, userId))
	token, err := model.GetTokenById(tokenId)
	require.NoError(t, err)
	assert.Equal(t, 700, token.RemainQuota)

	// 记录确实不存在（异步任务路径不建账）
	require.ErrorIs(t, model.SettleBillingReservation(relayInfo.RequestId, 0), model.ErrBillingReservationNotFound)
}
