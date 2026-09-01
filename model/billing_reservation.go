package model

import (
	"errors"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 原子预占合同（API Key 额度 + 账户可用余额）
//
// API Key 额度是单 Key 的硬消费上限，不是独立钱包；账户余额仍是账户级上限，
// 两者必须同时满足才允许调用 Provider。资格取得通过同一个数据库事务内的
// 条件 UPDATE（WHERE quota >= amount）完成：并发、多实例下都不会把余额或
// Key 剩余额度扣成负数；任一边界不足时整体回滚，零账变、零 Provider 调用。
// 追加（重试切换更贵分组）与初次预占使用同一合同：账户与 Key 在同一事务内
// 条件扣减，任一不足整体回滚。
//
// 生命周期（以 requestId 为幂等键，状态单向推进）：
//   reserved -> provider_started   Provider 请求即将发出前由调用方标记，
//                                  之后结果未知；恢复任务不得自动释放该状态。
//   provider_started -> settled    结算：delta = 实际用量 - 预占；delta>0
//                                  补扣、delta<0 退还。资金或 Key 任一差额
//                                  调整失败时结算整体回滚（记录保持
//                                  provider_started），调用方可重试或由对账
//                                  收口；不会终结成"部分结算"。
//   reserved -> released           释放：可证明未发送（Provider 未触达）时
//                                  退还全部预占（含追加部分），恰好一次。
//   provider_started -> released   进程存活且已知本次结果的处理路径
//                                  （Provider 明确失败/取消/请求未成功）
//                                  释放；自动恢复任务不触碰该状态，结果
//                                  未知的记录由人工对账收口。
//
// 异步任务（ForcePreConsume）与普通请求使用同一预占记录：Provider 提交前
// 同样原子取得双边界资格，Provider 触达前崩溃由恢复任务自动退款，触达后
// 崩溃进入 provider_started 不可自动退款；task 行的 RefundTaskQuota/
// RecalculateTaskQuota 继续负责任务结果侧的结算与退款。
//
// 本表只是请求级预占/幂等记录，与 SubscriptionPreConsumeRecord 同一可靠性
// 合同；账户余额与 Key 额度仍是唯一账本，这里不复制任何余额。

const (
	BillingReservationStateReserved = "reserved"
	// BillingReservationStateProviderStarted 表示 Provider 请求可能已发出：
	// 结果未知。恢复任务不得自动释放该状态的记录。
	BillingReservationStateProviderStarted = "provider_started"
	BillingReservationStateSettled         = "settled"
	BillingReservationStateReleased        = "released"

	BillingReservationSourceWallet       = "wallet"
	BillingReservationSourceSubscription = "subscription"

	// billingReservationStaleFloor 限制恢复窗口下限，防止误释放仍在进行的请求。
	billingReservationStaleFloorSeconds int64 = 600
)

var (
	ErrInsufficientUserQuota  = errors.New("账户可用余额不足")
	ErrInsufficientTokenQuota = errors.New("令牌可用额度不足")
	// ErrNoActiveSubscription / ErrSubscriptionQuotaInsufficient 是订阅预占的
	// 哨兵错误：调用方据此返回稳定的额度不足错误码并触发计费偏好回退。
	ErrNoActiveSubscription          = errors.New("no active subscription")
	ErrSubscriptionQuotaInsufficient = errors.New("subscription quota insufficient")
	// ErrBillingReservationNotFound 表示该请求没有预占记录（异步任务路径或
	// 旧数据）；调用方据此回退到无记录的账务路径。
	ErrBillingReservationNotFound = errors.New("billing reservation not found")
	// ErrBillingReservationStateConflict 表示记录状态与操作不兼容（如对
	// released 记录结算）。调用方不得重试同操作。
	ErrBillingReservationStateConflict = errors.New("billing reservation state conflict")
)

type BillingReservationRecord struct {
	Id                 int    `json:"id"`
	RequestId          string `json:"request_id" gorm:"type:varchar(64);uniqueIndex"`
	UserId             int    `json:"user_id" gorm:"index"`
	TokenId            int    `json:"token_id"`
	UnlimitedToken     bool   `json:"unlimited_token"`
	Source             string `json:"source" gorm:"type:varchar(32)"`
	UserSubscriptionId int    `json:"user_subscription_id"`
	Amount             int64  `json:"amount" gorm:"not null;default:0"`        // 预占总额（含追加部分）
	TopUpAmount        int64  `json:"top_up_amount" gorm:"not null;default:0"` // 发送前追加预占的部分
	State              string `json:"state" gorm:"type:varchar(32);index"`
	CreatedAt          int64  `json:"created_at" gorm:"bigint"`
	UpdatedAt          int64  `json:"updated_at" gorm:"bigint;index"`
}

func (r *BillingReservationRecord) BeforeCreate(tx *gorm.DB) error {
	now := common.GetTimestamp()
	r.CreatedAt = now
	r.UpdatedAt = now
	return nil
}

func (r *BillingReservationRecord) BeforeUpdate(tx *gorm.DB) error {
	r.UpdatedAt = common.GetTimestamp()
	return nil
}

func validateBillingReservationArgs(requestId string, userId int, amount int64) error {
	if requestId == "" {
		// 无 requestId 的会话没有可幂等的预占记录，调用方按"记录不存在"回退。
		return ErrBillingReservationNotFound
	}
	if userId <= 0 {
		return errors.New("invalid userId")
	}
	if amount < 0 {
		return errors.New("amount must be >= 0")
	}
	return nil
}

// transactionWithBillingReservationRetry absorbs SQLite's transient table
// locks when multiple connections race for a reservation terminal CAS. Other
// dialects rely on their row locks and return their errors immediately.
func transactionWithBillingReservationRetry(operation func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = operation()
		if err == nil || !common.UsingMainDatabase(common.DatabaseTypeSQLite) || !strings.Contains(err.Error(), "database table is locked") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return err
}

// claimBillingReservationTx 用 requestId 唯一键占位。claimed=false 表示该请求
// 已有预占记录（重放/重复提交），调用方必须跳过所有扣减与缓存同步。
func claimBillingReservationTx(tx *gorm.DB, requestId string, userId, tokenId int, unlimitedToken bool, source string, amount int64) (bool, error) {
	record := &BillingReservationRecord{
		RequestId:      requestId,
		UserId:         userId,
		TokenId:        tokenId,
		UnlimitedToken: unlimitedToken,
		Source:         source,
		Amount:         amount,
		State:          BillingReservationStateReserved,
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(record)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// reserveUserQuotaTx 条件扣减账户余额。RowsAffected=0 表示余额不足（或账户
// 不存在，按额度不足处理），事务回滚后不产生任何账变。
func reserveUserQuotaTx(tx *gorm.DB, userId int, amount int64) error {
	res := tx.Model(&User{}).Where("id = ? AND quota >= ?", userId, amount).
		Update("quota", gorm.Expr("quota - ?", amount))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInsufficientUserQuota
	}
	return nil
}

// reserveTokenQuotaTx 条件扣减 Key 剩余额度（硬上限）。unlimited 的 Key 不检查
// 上限但仍记账 remain/used 以维持统计。tokenId<=0（如 playground）跳过。
func reserveTokenQuotaTx(tx *gorm.DB, tokenId int, amount int64, unlimitedToken bool) error {
	if tokenId <= 0 || amount == 0 {
		return nil
	}
	query := tx.Model(&Token{}).Where("id = ?", tokenId)
	if !unlimitedToken {
		query = query.Where("remain_quota >= ?", amount)
	}
	res := query.Updates(map[string]any{
		"remain_quota":  gorm.Expr("remain_quota - ?", amount),
		"used_quota":    gorm.Expr("used_quota + ?", amount),
		"accessed_time": common.GetTimestamp(),
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInsufficientTokenQuota
	}
	return nil
}

// adjustTokenQuotaDeltaTx 结算差额调整 Key 额度：delta>0 补扣（允许把 remain
// 扣为负，上限已在预占时守住），delta<0 退还。Key 已删除时静默跳过。
func adjustTokenQuotaDeltaTx(tx *gorm.DB, tokenId int, delta int64) error {
	if tokenId <= 0 || delta == 0 {
		return nil
	}
	if delta > 0 {
		return tx.Model(&Token{}).Where("id = ?", tokenId).Updates(map[string]any{
			"remain_quota": gorm.Expr("remain_quota - ?", delta),
			"used_quota":   gorm.Expr("used_quota + ?", delta),
		}).Error
	}
	return tx.Model(&Token{}).Where("id = ?", tokenId).Updates(map[string]any{
		"remain_quota": gorm.Expr("remain_quota + ?", -delta),
		"used_quota":   gorm.Expr("used_quota - ?", -delta),
	}).Error
}

// adjustUserQuotaDeltaTx 结算差额调整账户余额：delta>0 无条件补扣（该请求已
// 通过预占取得资格，欠费有界），delta<0 退还。仅用于结算/对账；预占与追加
// 一律走条件扣减。
func adjustUserQuotaDeltaTx(tx *gorm.DB, userId int, delta int64) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		return tx.Model(&User{}).Where("id = ?", userId).
			Update("quota", gorm.Expr("quota - ?", delta)).Error
	}
	return tx.Model(&User{}).Where("id = ?", userId).
		Update("quota", gorm.Expr("quota + ?", -delta)).Error
}

func refundTokenQuotaTx(tx *gorm.DB, tokenId int, amount int64) error {
	if tokenId <= 0 || amount == 0 {
		return nil
	}
	// Key 可能已被删除：删除的 Key 没有可恢复的额度，资金侧退款仍必须完成。
	return tx.Model(&Token{}).Where("id = ?", tokenId).Updates(map[string]any{
		"remain_quota": gorm.Expr("remain_quota + ?", amount),
		"used_quota":   gorm.Expr("used_quota - ?", amount),
	}).Error
}

// syncBillingReservationCache 把数据库账变同步到 Redis 缓存。
// HINCRBY 语义：数据库扣减 N → 缓存传 -N；数据库退还 N → 缓存传 +N。
func syncBillingReservationCache(userId int, tokenKey string, userDelta int, tokenDelta int) {
	if userId > 0 && userDelta != 0 {
		if err := cacheIncrUserQuota(userId, int64(userDelta)); err != nil {
			common.SysLog("failed to sync billing reservation user cache: " + err.Error())
		}
	}
	if tokenKey != "" && tokenDelta != 0 && common.RedisEnabled {
		if err := cacheIncrTokenQuota(tokenKey, int64(tokenDelta)); err != nil {
			common.SysLog("failed to sync billing reservation token cache: " + err.Error())
		}
	}
}

func tokenKeyForCacheSync(tokenId int, tokenKey string) string {
	if tokenId <= 0 {
		return ""
	}
	return tokenKey
}

// ReserveWalletBillingQuota 在同一个事务内原子预占账户余额与 Key 硬上限。
// requestId 幂等：重复调用不会重复扣减（reserved=false）。任一边界不足时
// 整体失败、零账变。
func ReserveWalletBillingQuota(requestId string, userId, tokenId int, tokenKey string, amount int, unlimitedToken bool) (bool, error) {
	if err := validateBillingReservationArgs(requestId, userId, int64(amount)); err != nil {
		return false, err
	}
	if amount == 0 {
		return false, nil
	}
	reserved := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		claimed, err := claimBillingReservationTx(tx, requestId, userId, tokenId, unlimitedToken, BillingReservationSourceWallet, int64(amount))
		if err != nil || !claimed {
			return err
		}
		if err := reserveUserQuotaTx(tx, userId, int64(amount)); err != nil {
			return err
		}
		if err := reserveTokenQuotaTx(tx, tokenId, int64(amount), unlimitedToken); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if reserved {
		syncBillingReservationCache(userId, tokenKeyForCacheSync(tokenId, tokenKey), -amount, -amount)
	}
	return reserved, nil
}

// ReserveSubscriptionBillingQuota 在同一个事务内原子预占订阅额度与 Key 硬上限。
// 订阅预占沿用 SubscriptionPreConsumeRecord 的 requestId 幂等合同。
func ReserveSubscriptionBillingQuota(requestId string, userId, tokenId int, tokenKey string, amount int64, unlimitedToken bool) (*SubscriptionPreConsumeResult, bool, error) {
	if err := validateBillingReservationArgs(requestId, userId, amount); err != nil {
		return nil, false, err
	}
	if amount <= 0 {
		return nil, false, errors.New("amount must be > 0")
	}
	var result *SubscriptionPreConsumeResult
	reserved := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		claimed, err := claimBillingReservationTx(tx, requestId, userId, tokenId, unlimitedToken, BillingReservationSourceSubscription, amount)
		if err != nil {
			return err
		}
		// preConsumeUserSubscriptionTx 自带 requestId 幂等：重放时返回既有结果、不再扣订阅。
		result, err = preConsumeUserSubscriptionTx(tx, requestId, userId, amount)
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		if err := reserveTokenQuotaTx(tx, tokenId, amount, unlimitedToken); err != nil {
			return err
		}
		if err := tx.Model(&BillingReservationRecord{}).Where("request_id = ?", requestId).
			Update("user_subscription_id", result.UserSubscriptionId).Error; err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if reserved {
		syncBillingReservationCache(userId, tokenKeyForCacheSync(tokenId, tokenKey), 0, -int(amount))
	}
	return result, reserved, nil
}

// ReserveUserQuota 独立的条件性账户扣减，用于没有预占记录的路径（WSS 增量
// 计费等）。并发下不会把余额扣成负数。
func ReserveUserQuota(userId int, amount int) error {
	if userId <= 0 || amount <= 0 {
		return nil
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		return reserveUserQuotaTx(tx, userId, int64(amount))
	})
	if err != nil {
		return err
	}
	if err := cacheIncrUserQuota(userId, -int64(amount)); err != nil {
		common.SysLog("failed to sync user reserve cache: " + err.Error())
	}
	return nil
}

// ReserveTokenQuota 独立的条件性 Key 硬上限扣减，用于没有预占记录的路径。
func ReserveTokenQuota(tokenId int, tokenKey string, amount int, unlimitedToken bool) error {
	if tokenId <= 0 || amount <= 0 {
		return nil
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		return reserveTokenQuotaTx(tx, tokenId, int64(amount), unlimitedToken)
	})
	if err != nil {
		return err
	}
	if common.RedisEnabled {
		if err := cacheIncrTokenQuota(tokenKey, int64(-amount)); err != nil {
			common.SysLog("failed to sync token reserve cache: " + err.Error())
		}
	}
	return nil
}

// AppendBillingReservation 对已存在的预占追加额度（重试切换到更贵的分组时）。
// 记录、资金与 Key 扣减在同一事务内完成：账户余额与 Key 额度都是条件扣减，
// 任一不足整体回滚、零账变（不会调用 Provider——追加发生在发送前）。
// 记录不存在时返回 ErrBillingReservationNotFound。
func AppendBillingReservation(requestId string, delta int) error {
	if delta <= 0 {
		return nil
	}
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheTokenDelta int
	err := DB.Transaction(func(tx *gorm.DB) error {
		var record BillingReservationRecord
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrBillingReservationNotFound
			}
			return err
		}
		if record.State != BillingReservationStateReserved {
			// provider_started 及之后不允许追加：Provider 可能已收到请求。
			return ErrBillingReservationStateConflict
		}
		switch record.Source {
		case BillingReservationSourceWallet:
			if err := reserveUserQuotaTx(tx, record.UserId, int64(delta)); err != nil {
				return err
			}
			cacheUserId = record.UserId
		case BillingReservationSourceSubscription:
			if err := postConsumeUserSubscriptionDeltaTx(tx, record.UserSubscriptionId, int64(delta)); err != nil {
				return err
			}
		}
		if err := reserveTokenQuotaTx(tx, record.TokenId, int64(delta), record.UnlimitedToken); err != nil {
			return err
		}
		cacheTokenDelta = delta
		record.Amount += int64(delta)
		record.TopUpAmount += int64(delta)
		return tx.Save(&record).Error
	})
	if err != nil {
		return err
	}
	if cacheUserId > 0 {
		syncBillingReservationCache(cacheUserId, "", -delta, 0)
	}
	if cacheTokenDelta != 0 {
		syncBillingReservationCache(0, billingReservationTokenKey(requestId), 0, -cacheTokenDelta)
	}
	return nil
}

// MarkBillingReservationProviderStarted 在 Provider 请求发出前调用：此后结果
// 未知，恢复任务不得自动释放；仅结算或显式人工对账可以收口。
// 幂等：重复标记 provider_started 是 no-op。缺记录、已释放/已结算或 CAS
// 未命中且未能证明是重复标记时返回错误；调用方必须 fail closed，绝不能触达
// Provider 后再留下 reserved 记录供恢复任务退款。
func MarkBillingReservationProviderStarted(requestId string) error {
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	res := DB.Model(&BillingReservationRecord{}).
		Where("request_id = ? AND state = ?", requestId, BillingReservationStateReserved).
		Update("state", BillingReservationStateProviderStarted)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}

	var record BillingReservationRecord
	if err := DB.Select("state").Where("request_id = ?", requestId).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBillingReservationNotFound
		}
		return err
	}
	if record.State == BillingReservationStateProviderStarted {
		return nil
	}
	return ErrBillingReservationStateConflict
}

// SettleBillingReservation 幂等结算：delta = 实际用量 - 预占。
// delta>0 补扣、delta<0 退还。资金或 Key 任一差额调整失败时整体回滚并返回
// 错误——记录保持在 reserved/provider_started，调用方可重试，或由对账收口；
// 不会终结成"部分结算"。重放（已 settled）与记录不存在分别返回幂等成功与
// ErrBillingReservationNotFound。
func SettleBillingReservation(requestId string, delta int) error {
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheUserDelta int
	var cacheTokenDelta int
	err := transactionWithBillingReservationRetry(func() error {
		cacheUserId, cacheUserDelta, cacheTokenDelta = 0, 0, 0
		return DB.Transaction(func(tx *gorm.DB) error {
			var record BillingReservationRecord
			if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrBillingReservationNotFound
				}
				return err
			}
			if record.State == BillingReservationStateSettled || record.State == BillingReservationStateReleased {
				// 已终结：结算重放按幂等 no-op 处理（账变只会发生一次）。
				return nil
			}
			// SQLite has no SELECT FOR UPDATE equivalent in lockForUpdate. Claim the
			// terminal transition with a state CAS before applying any balance delta,
			// so a concurrent explicit release or stale recovery cannot settle an old
			// provider_started snapshot after it has already refunded the reservation.
			transition := tx.Model(&BillingReservationRecord{}).
				Where("id = ? AND state = ?", record.Id, record.State).
				Update("state", BillingReservationStateSettled)
			if transition.Error != nil {
				return transition.Error
			}
			if transition.RowsAffected == 0 {
				return nil
			}
			if delta != 0 {
				var applyErr error
				switch record.Source {
				case BillingReservationSourceWallet:
					applyErr = adjustUserQuotaDeltaTx(tx, record.UserId, int64(delta))
					if applyErr == nil {
						cacheUserId = record.UserId
						cacheUserDelta = -delta
					}
				case BillingReservationSourceSubscription:
					applyErr = postConsumeUserSubscriptionDeltaTx(tx, record.UserSubscriptionId, int64(delta))
				}
				// 资金侧失败即整体回滚：不再继续调整 Key，保证账变原子。
				if applyErr != nil {
					return applyErr
				}
				// Key 额度与资金同步差额：补扣可把 remain 扣为负（上限已在预占守住）。
				if tokenErr := adjustTokenQuotaDeltaTx(tx, record.TokenId, int64(delta)); tokenErr != nil {
					return tokenErr
				}
				cacheTokenDelta = -delta
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if cacheUserDelta != 0 || cacheTokenDelta != 0 {
		tokenKey := ""
		if cacheTokenDelta != 0 {
			tokenKey = billingReservationTokenKey(requestId)
		}
		syncBillingReservationCache(cacheUserId, tokenKey, cacheUserDelta, cacheTokenDelta)
	}
	return nil
}

// billingReservationTokenKey 结算/追加的缓存同步需要 Key 派生的缓存键，从
// tokens 表解析（记录中不复制 Key 明文）。
func billingReservationTokenKey(requestId string) string {
	var record BillingReservationRecord
	if err := DB.Select("id", "token_id").Where("request_id = ?", requestId).First(&record).Error; err != nil || record.TokenId <= 0 {
		return ""
	}
	var token Token
	if err := DB.Select("id", commonKeyCol).Where("id = ?", record.TokenId).First(&token).Error; err != nil {
		return ""
	}
	return token.Key
}

// ReleaseBillingReservation 幂等释放预占：退还全部预占（含追加部分）并推进
// released，恰好一次。允许从 reserved 与 provider_started 两个状态释放——
// 调用方是进程存活、已知本次结果的处理路径（Provider 明确失败/取消/请求
// 未成功）。自动恢复任务必须使用 ReleaseUnsentBillingReservation（只释放
// 可证明未发送的 reserved 记录），防止误退可能已被 Provider 接受的请求。
// 返回值表示本次调用是否实际完成 released 终态迁移；已 settled/released 的
// 幂等调用返回 false, nil。记录不存在时返回 ErrBillingReservationNotFound
// （调用方回退旧路径）。
func ReleaseBillingReservation(requestId string, tokenKey string) (bool, error) {
	return releaseBillingReservation(requestId, tokenKey, false)
}

// ReleaseUnsentBillingReservation 仅供自动恢复任务使用：只释放仍处于
// reserved（可证明 Provider 未触达）的记录。provider_started（结果未知）
// 不释放，防止把已发送的请求退款。返回值表示本次调用是否实际完成释放。
func ReleaseUnsentBillingReservation(requestId string, tokenKey string) (bool, error) {
	return releaseBillingReservation(requestId, tokenKey, true)
}

func releaseBillingReservation(requestId string, tokenKey string, reservedOnly bool) (bool, error) {
	if requestId == "" {
		return false, ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheUserDelta int
	var cacheTokenDelta int
	released := false
	err := transactionWithBillingReservationRetry(func() error {
		cacheUserId, cacheUserDelta, cacheTokenDelta = 0, 0, 0
		released = false
		return DB.Transaction(func(tx *gorm.DB) error {
			var record BillingReservationRecord
			if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&record).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrBillingReservationNotFound
				}
				return err
			}
			if record.State != BillingReservationStateReserved {
				if reservedOnly || record.State != BillingReservationStateProviderStarted {
					return nil
				}
			}
			// Claim the terminal release before refunding. This CAS is required in
			// addition to lockForUpdate because SQLite deliberately omits FOR UPDATE;
			// without it a release can refund a stale provider_started snapshot after
			// a concurrent settle has already committed.
			transition := tx.Model(&BillingReservationRecord{}).
				Where("id = ? AND state = ?", record.Id, record.State).
				Update("state", BillingReservationStateReleased)
			if transition.Error != nil {
				return transition.Error
			}
			if transition.RowsAffected == 0 {
				return nil
			}
			released = true
			switch record.Source {
			case BillingReservationSourceWallet:
				if err := tx.Model(&User{}).Where("id = ?", record.UserId).
					Update("quota", gorm.Expr("quota + ?", record.Amount)).Error; err != nil {
					return err
				}
				cacheUserId = record.UserId
				cacheUserDelta = int(record.Amount)
			case BillingReservationSourceSubscription:
				// SubscriptionPreConsumeRecord 退还原始预占（幂等）；追加部分单独退还。
				if err := refundSubscriptionPreConsumeTx(tx, requestId); err != nil {
					return err
				}
				if record.TopUpAmount > 0 {
					if err := postConsumeUserSubscriptionDeltaTx(tx, record.UserSubscriptionId, -record.TopUpAmount); err != nil {
						return err
					}
				}
			}
			if err := refundTokenQuotaTx(tx, record.TokenId, record.Amount); err != nil {
				return err
			}
			cacheTokenDelta = int(record.Amount)
			return nil
		})
	})
	if err != nil {
		return false, err
	}
	if released && (cacheUserDelta != 0 || cacheTokenDelta != 0) {
		if tokenKey == "" {
			tokenKey = billingReservationTokenKey(requestId)
		}
		syncBillingReservationCache(cacheUserId, tokenKey, cacheUserDelta, cacheTokenDelta)
	}
	return released, nil
}

// ReleaseStaleBillingReservations 恢复任务：仅释放仍处于 reserved（可证明
// Provider 未触达）且超过陈旧窗口的预占。provider_started（结果未知）不释放，
// 防止把已发送的请求退款。释放幂等，重复执行不会重复退款。返回释放的记录数。
func ReleaseStaleBillingReservations(olderThanSeconds int64, limit int) (int64, error) {
	if olderThanSeconds < billingReservationStaleFloorSeconds {
		olderThanSeconds = billingReservationStaleFloorSeconds
	}
	if limit <= 0 {
		limit = 100
	}
	cutoff := GetDBTimestamp() - olderThanSeconds
	var records []BillingReservationRecord
	if err := DB.Where("state = ? AND updated_at < ?", BillingReservationStateReserved, cutoff).
		Order("updated_at asc").Limit(limit).Find(&records).Error; err != nil {
		return 0, err
	}
	var released int64
	for _, record := range records {
		tokenKey := ""
		if record.TokenId > 0 {
			var token Token
			if err := DB.Select("id", commonKeyCol).Where("id = ?", record.TokenId).First(&token).Error; err == nil {
				tokenKey = token.Key
			}
		}
		didRelease, err := ReleaseUnsentBillingReservation(record.RequestId, tokenKey)
		if err != nil {
			return released, err
		}
		if didRelease {
			released++
		}
	}
	return released, nil
}

// CleanupBillingReservationRecords 清理已终结的预占记录，保持表规模有界。
// provider_started 的记录必须先经结算/人工对账收口，不参与自动清理。
func CleanupBillingReservationRecords(olderThanSeconds int64) (int64, error) {
	if olderThanSeconds <= 0 {
		olderThanSeconds = 7 * 24 * 3600
	}
	cutoff := GetDBTimestamp() - olderThanSeconds
	res := DB.Where("state IN ? AND updated_at < ?", []string{
		BillingReservationStateSettled, BillingReservationStateReleased,
	}, cutoff).Delete(&BillingReservationRecord{})
	return res.RowsAffected, res.Error
}
