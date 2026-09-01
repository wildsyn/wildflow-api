package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
//
// 预扣合同：调用 Provider 前，账户余额（钱包或订阅）与 Key 硬上限必须在
// model 层原子预占事务内同时取得资格；预占失败的请求零 Provider 调用、零账变。
// 预占额是消费上限：结算差额进入欠费时受"已取得资格"约束，余额不会无界为负。
// 退款与恢复以 requestId 为幂等键，恰好一次。
type BillingSession struct {
	relayInfo        *relaycommon.RelayInfo
	funding          FundingSource
	preConsumedQuota int  // 实际预扣额度（信任用户可能为 0）
	tokenConsumed    int  // 令牌额度实际扣减量
	extraReserved    int  // 发送前补充预扣的额度（订阅退款时需要单独回滚）
	alreadyReserved  bool // 预占记录在本次会话之前已存在（requestId 重放）
	fundingSettled   bool // funding.Settle 已成功，资金来源已提交
	settled          bool // Settle 全部完成（资金 + 令牌）
	refunded         bool // Refund 已调用
	mu               sync.Mutex
}

// Settle 根据实际消耗额度进行结算。
// 有预占记录时走 model 层幂等结算（delta 补扣/退还 + 状态推进）；
// 无预占记录的路径（异步任务等）沿用 funding.Settle + 令牌差额调整。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return nil
	}
	delta := actualQuota - s.preConsumedQuota
	if delta == 0 {
		// 无差额也要推进预占记录状态，防止恢复任务误退已消费的请求。
		if s.hasReservation() {
			if err := model.SettleBillingReservation(s.relayInfo.RequestId, 0); err != nil &&
				!errors.Is(err, model.ErrBillingReservationNotFound) {
				s.settled = true
				return err
			}
		}
		s.settled = true
		return nil
	}
	// 1) 调整资金来源 + Key 额度（model 层单事务幂等完成；结算失败时记录
	// 保持可恢复状态，调用方可重试——不会终结成"部分结算"）。
	if s.hasReservation() {
		if err := model.SettleBillingReservation(s.relayInfo.RequestId, delta); err != nil {
			if errors.Is(err, model.ErrBillingReservationNotFound) {
				// 记录不存在（requestId 为空的旧数据路径），回退到分步调整。
				return s.settleWithoutRecord(delta)
			}
			return err
		}
		if s.funding.Source() == BillingSourceSubscription {
			s.relayInfo.SubscriptionPostDelta += int64(delta)
		}
		s.settled = true
		return nil
	}
	return s.settleWithoutRecord(delta)
}

// hasReservation 报告本会话是否有对应的预占记录（含 requestId 重放）。
// 普通请求与异步任务（ForcePreConsume）现在共用同一预占记录：它同时是并发
// 资格账与崩溃恢复账本。preConsumedQuota=0 的免费/信任路径没有记录。
func (s *BillingSession) hasReservation() bool {
	return s.preConsumedQuota > 0
}

// MarkProviderStarted 在 Provider 请求即将发出前调用：把预占记录推进到
// provider_started（结果未知），此后恢复任务不得自动释放，防止把可能已被
// Provider 接受的请求退款。幂等；无记录（免费模型/信任为 0）时是 no-op。
// 标记失败必须阻止请求，避免 Provider 已收到请求但记录仍为 reserved，随后被
// 恢复任务错误退款。
func (s *BillingSession) MarkProviderStarted(c *gin.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled || s.refunded || !s.hasReservation() {
		return nil
	}
	if err := model.MarkBillingReservationProviderStarted(s.relayInfo.RequestId); err != nil {
		return fmt.Errorf("mark billing reservation provider_started (request=%s): %w", s.relayInfo.RequestId, err)
	}
	return nil
}

// settleWithoutRecord 无预占记录的差额结算：资金来源和令牌额度分两步提交，
// 若资金来源已提交但令牌调整失败，标记 fundingSettled 防止 Refund 重复退款。
func (s *BillingSession) settleWithoutRecord(delta int) error {
	if !s.fundingSettled {
		if err := s.funding.Settle(delta); err != nil {
			return err
		}
		s.fundingSettled = true
	}
	var tokenErr error
	if !s.relayInfo.IsPlayground {
		if delta > 0 {
			tokenErr = model.DecreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
		} else {
			tokenErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		}
		if tokenErr != nil {
			// 资金来源已提交，令牌调整失败只能记录日志；标记 settled 防止 Refund 误退资金
			common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
				s.relayInfo.UserId, s.relayInfo.TokenId, delta, tokenErr.Error()))
		}
	}
	if s.funding.Source() == BillingSourceSubscription {
		s.relayInfo.SubscriptionPostDelta += int64(delta)
	}
	s.settled = true
	return tokenErr
}

// Refund 退还所有预扣费，幂等安全，异步执行。
// 有预占记录时由 model.ReleaseBillingReservation 在单个事务内原子退还
// 资金来源 + Key 额度并推进状态（恰好一次）；无预占记录的路径沿用
// funding.Refund + IncreaseTokenQuota 的分步退还。
func (s *BillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	if s.settled || s.refunded || !s.needsRefundLocked() {
		s.mu.Unlock()
		return
	}
	s.refunded = true
	requestId := s.relayInfo.RequestId
	tokenKey := s.relayInfo.TokenKey
	hasRecord := s.hasReservation() || s.alreadyReserved
	s.mu.Unlock()

	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费（token_quota=%s, funding=%s）",
		s.relayInfo.UserId,
		logger.FormatQuota(s.tokenConsumed),
		s.funding.Source(),
	))

	// 复制需要的值到闭包中
	tokenId := s.relayInfo.TokenId
	isPlayground := s.relayInfo.IsPlayground
	tokenConsumed := s.tokenConsumed
	extraReserved := s.extraReserved
	subscriptionId := s.relayInfo.SubscriptionId
	funding := s.funding

	gopool.Go(func() {
		if hasRecord {
			if _, err := model.ReleaseBillingReservation(requestId, tokenKey); err != nil &&
				!errors.Is(err, model.ErrBillingReservationNotFound) {
				common.SysLog("error releasing billing reservation: " + err.Error())
			}
			return
		}
		// 无预占记录的分步退还
		// 1) 退还资金来源
		if err := funding.Refund(); err != nil {
			common.SysLog("error refunding billing source: " + err.Error())
		}
		if extraReserved > 0 && funding.Source() == BillingSourceSubscription && subscriptionId > 0 {
			if err := model.PostConsumeUserSubscriptionDelta(subscriptionId, -int64(extraReserved)); err != nil {
				common.SysLog("error refunding subscription extra reserved quota: " + err.Error())
			}
		}
		// 2) 退还令牌额度
		if tokenConsumed > 0 && !isPlayground {
			if err := model.IncreaseTokenQuota(tokenId, tokenKey, tokenConsumed); err != nil {
				common.SysLog("error refunding token quota: " + err.Error())
			}
		}
	})
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.refunded || s.fundingSettled {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	if s.tokenConsumed > 0 {
		return true
	}
	// 订阅可能在 tokenConsumed=0 时仍预扣了额度
	if sub, ok := s.funding.(*SubscriptionFunding); ok && sub.preConsumed > 0 {
		return true
	}
	return false
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	return s.preConsumedQuota
}

func (s *BillingSession) Reserve(targetQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.settled || s.refunded || targetQuota <= s.preConsumedQuota {
		return nil
	}

	delta := targetQuota - s.preConsumedQuota
	if delta <= 0 {
		return nil
	}

	// 有预占记录时在 model 层单事务内追加（资金 + Key 原子，任一失败整体回滚）。
	// 记录不存在（requestId 为空/异步任务）时回退到分步追加。
	if err := model.AppendBillingReservation(s.relayInfo.RequestId, delta); err != nil {
		if !errors.Is(err, model.ErrBillingReservationNotFound) {
			return err
		}
		if err := s.reserveWithoutRecord(delta); err != nil {
			return err
		}
	}
	s.preConsumedQuota += delta
	s.tokenConsumed += delta
	s.extraReserved += delta
	s.syncRelayInfo()
	return nil
}

// reserveWithoutRecord 无预占记录的追加预占：先资金后 Key，Key 失败回滚资金。
func (s *BillingSession) reserveWithoutRecord(delta int) error {
	if err := s.reserveFunding(delta); err != nil {
		return err
	}
	if err := s.reserveToken(delta); err != nil {
		s.rollbackFundingReserve(delta)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口
// ---------------------------------------------------------------------------

// preConsume 执行原子预占：账户余额（钱包或订阅）与 Key 硬上限必须在 model 层
// 同一事务内同时取得资格，之后才允许调用 Provider。并发的请求合计不能突破
// 账户余额或单 Key 消费上限；预占是请求获得消费资格的唯一途径，跳过扣减
// 会让估算误差与并发叠加成无界欠费。
func (s *BillingSession) preConsume(c *gin.Context, quota int) *types.NewAPIError {
	effectiveQuota := quota

	if effectiveQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s (funding=%s)", s.relayInfo.UserId, logger.FormatQuota(effectiveQuota), s.funding.Source()))
	}

	// ---- 1) 原子预占：账户余额 + Key 硬上限在同一事务内取得资格 ----
	// 预占失败的请求零 Provider 调用、零账变；并发/多实例下任何一方不足都
	// 会整体失败，不会把余额或 Key 剩余额度扣成负数。普通请求与异步任务
	// （ForcePreConsume）使用同一预占记录：它既是并发硬上限的资格账，也是
	// 崩溃恢复账本（触达 Provider 前崩溃可自动退款，触达后进入
	// provider_started 不被自动误退）。
	if effectiveQuota > 0 {
		switch funding := s.funding.(type) {
		case *WalletFunding:
			// 预占事务即资金扣减本身；成功后同步 funding.consumed 维持退款合同。
			reserved, err := model.ReserveWalletBillingQuota(s.relayInfo.RequestId, s.relayInfo.UserId,
				s.relayInfo.TokenId, s.relayInfo.TokenKey, effectiveQuota, s.relayInfo.TokenUnlimited)
			if err != nil {
				return s.preConsumeFailure(c, err)
			}
			funding.consumed = effectiveQuota
			s.tokenConsumed = effectiveQuota
			if !reserved {
				// requestId 重放（同一请求的重复预占）：账已预扣，只同步会话状态。
				s.alreadyReserved = true
			}
		case *SubscriptionFunding:
			_, reserved, err := model.ReserveSubscriptionBillingQuota(s.relayInfo.RequestId, s.relayInfo.UserId,
				s.relayInfo.TokenId, s.relayInfo.TokenKey, int64(effectiveQuota), s.relayInfo.TokenUnlimited)
			if err != nil {
				return s.preConsumeFailure(c, err)
			}
			// 订阅资金与 Key 硬上限已原子预占；直接标记 consumed 防止兜底路径重复扣减。
			s.tokenConsumed = effectiveQuota
			funding.preConsumed = int64(effectiveQuota)
			if !reserved {
				s.alreadyReserved = true
			}
		default:
			return types.NewError(fmt.Errorf("unsupported funding source: %s", s.funding.Source()), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
	}

	s.preConsumedQuota = effectiveQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return nil
}

// preConsumeFailure 把预扣失败映射为稳定的 API 错误：额度不足类返回 403 且不
// 记错误日志（属正常业务拒绝），其余按数据错误处理。
func (s *BillingSession) preConsumeFailure(c *gin.Context, err error) *types.NewAPIError {
	if errors.Is(err, model.ErrInsufficientUserQuota) ||
		errors.Is(err, model.ErrInsufficientTokenQuota) ||
		isSubscriptionQuotaError(err) {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
}

// isSubscriptionQuotaError 判断资金来源错误是否属于额度不足类（用户可理解、需回退），
// 与 model 层哨兵错误和既有字符串合同保持一致。
func isSubscriptionQuotaError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, model.ErrNoActiveSubscription) || errors.Is(err, model.ErrSubscriptionQuotaInsufficient) {
		return true
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "no active subscription") || strings.Contains(errMsg, "subscription quota insufficient")
}

func (s *BillingSession) reserveFunding(delta int) error {
	switch funding := s.funding.(type) {
	case *WalletFunding:
		// 与结算补扣（SettleBilling 正差额 → WalletFunding.Settle）语义一致：
		// 全额无条件扣减，余额不足的部分记为欠费（余额可为负），不中断请求，
		// 保证日志记录的预扣额度与用户余额的实际变动始终对账一致。
		// DecreaseUserQuota 仅在数据库错误时失败。
		if err := model.DecreaseUserQuota(funding.userId, delta, false); err != nil {
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		funding.consumed += delta
		return nil
	case *SubscriptionFunding:
		if err := model.PostConsumeUserSubscriptionDelta(funding.subscriptionId, int64(delta)); err != nil {
			return types.NewErrorWithStatusCode(
				fmt.Errorf("订阅额度不足或未配置订阅: %s", err.Error()),
				types.ErrorCodeInsufficientUserQuota,
				http.StatusForbidden,
				types.ErrOptionWithSkipRetry(),
				types.ErrOptionWithNoRecordErrorLog(),
			)
		}
		return nil
	default:
		return types.NewError(fmt.Errorf("unsupported funding source: %s", s.funding.Source()), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}
}

func (s *BillingSession) rollbackFundingReserve(delta int) {
	switch funding := s.funding.(type) {
	case *WalletFunding:
		if err := model.IncreaseUserQuota(funding.userId, delta, false); err != nil {
			common.SysLog("error rolling back wallet funding reserve: " + err.Error())
		} else {
			funding.consumed -= delta
		}
	case *SubscriptionFunding:
		if err := model.PostConsumeUserSubscriptionDelta(funding.subscriptionId, -int64(delta)); err != nil {
			common.SysLog("error rolling back subscription funding reserve: " + err.Error())
		}
	}
}

func (s *BillingSession) reserveToken(delta int) error {
	if delta <= 0 || s.relayInfo.IsPlayground {
		return nil
	}
	if err := PreConsumeTokenQuota(s.relayInfo, delta); err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return nil
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的兼容字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()

	if sub, ok := s.funding.(*SubscriptionFunding); ok {
		info.SubscriptionId = sub.subscriptionId
		info.SubscriptionPreConsumed = sub.preConsumed + int64(s.extraReserved)
		info.SubscriptionPostDelta = 0
		info.SubscriptionAmountTotal = sub.AmountTotal
		info.SubscriptionAmountUsedAfterPreConsume = sub.AmountUsedAfter + int64(s.extraReserved)
		info.SubscriptionPlanId = sub.PlanId
		info.SubscriptionPlanTitle = sub.PlanTitle
	} else {
		info.SubscriptionId = 0
		info.SubscriptionPreConsumed = 0
	}
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 根据计费偏好创建会话并处理回退
// ---------------------------------------------------------------------------

// NewBillingSession 根据用户计费偏好创建 BillingSession，处理 subscription_first / wallet_first 的回退。
//
// 原子预占：钱包/订阅路径在 model 层事务内同时校验账户余额（或订阅额度）与
// Key 硬上限（ReserveWalletBillingQuota / ReserveSubscriptionBillingQuota），
// 预占失败即返回额度不足，零账变、零 Provider 调用。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	if relayInfo == nil {
		return nil, types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	pref := common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference)

	// 钱包路径需要先检查用户额度
	tryWallet := func() (*BillingSession, *types.NewAPIError) {
		userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if userQuota <= 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if userQuota-preConsumedQuota < 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		relayInfo.UserQuota = userQuota

		session := &BillingSession{
			relayInfo: relayInfo,
			funding:   &WalletFunding{userId: relayInfo.UserId},
		}
		if apiErr := session.preConsume(c, preConsumedQuota); apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	trySubscription := func() (*BillingSession, *types.NewAPIError) {
		subConsume := int64(preConsumedQuota)
		if subConsume <= 0 {
			subConsume = 1
		}
		session := &BillingSession{
			relayInfo: relayInfo,
			funding: &SubscriptionFunding{
				requestId: relayInfo.RequestId,
				userId:    relayInfo.UserId,
				modelName: relayInfo.OriginModelName,
				amount:    subConsume,
			},
		}
		// 必须传 subConsume 而非 preConsumedQuota，保证 SubscriptionFunding.amount、
		// preConsume 参数和 FinalPreConsumedQuota 三者一致，避免订阅多扣费。
		if apiErr := session.preConsume(c, int(subConsume)); apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	switch pref {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		session, err := tryWallet()
		if err != nil {
			if err.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				return trySubscription()
			}
			return nil, err
		}
		return session, nil
	case "subscription_first":
		fallthrough
	default:
		hasSub, subCheckErr := model.HasActiveUserSubscription(relayInfo.UserId)
		if subCheckErr != nil {
			return nil, types.NewError(subCheckErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !hasSub {
			return tryWallet()
		}
		session, apiErr := trySubscription()
		if apiErr != nil {
			if apiErr.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				// 仅当用户的活跃订阅允许钱包回退时才回退到钱包，否则返回订阅额度不足错误
				allowOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
				if overflowErr != nil {
					return nil, types.NewError(overflowErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
				}
				if allowOverflow {
					return tryWallet()
				}
				return nil, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}
}
