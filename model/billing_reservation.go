package model

import (
	"errors"
	"fmt"

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
//
// 生命周期（以 requestId 为幂等键，状态单向推进）：
//   reserved -> settled  结算：delta = 实际用量 - 预占；delta>0 无条件补扣
//                        （差额即估算误差，受请求校验上界约束，欠费有界），
//                        delta<0 退还。结算失败保留预占记录，不会重复扣费。
//   reserved -> released 释放：Provider 未调用/明确失败/取消时退还全部预占
//                        （含追加部分），幂等，恰好一次。
//   reserved -> released 恢复：结果未知（进程崩溃/重启）的陈旧预占由恢复任务
//                        释放，同样幂等，不会重复退款；已 settled 的记录不释放，
//                        已发生的消费不会被恢复任务误退。
//
// 异步任务（ForcePreConsume）的可靠性由 task 行 + RefundTaskQuota/
// RecalculateTaskQuota 的既有合同保证，不创建预占记录，避免双重账本；
// 其预扣仍走原子条件扣减，只是没有可恢复记录。
//
// 本表只是请求级预占/幂等记录，与 SubscriptionPreConsumeRecord 同一可靠性
// 合同；账户余额与 Key 额度仍是唯一账本，这里不复制任何余额。

const (
	BillingReservationStateReserved = "reserved"
	BillingReservationStateSettled  = "settled"
	BillingReservationStateReleased = "released"

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
// 通过预占取得资格，欠费有界），delta<0 退还。
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

// ReserveUserQuota 独立的条件性账户扣减，用于没有预占记录的路径（异步任务
// 预扣、WSS 增量计费）。并发下不会把余额扣成负数。
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
	if err := cacheIncrUserQuota(userId, int64(-amount)); err != nil {
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
// 记录、资金与 Key 扣减在同一事务内完成：钱包部分沿用既有合同——无条件扣减、
// 不足部分进入欠费（有界）；订阅部分与 Key 部分是条件扣减，任一失败整体回滚
// （钱包也不会被扣）。记录不存在时返回 ErrBillingReservationNotFound。
func AppendBillingReservation(requestId string, delta int) error {
	if delta <= 0 {
		return nil
	}
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheUserDelta int
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
			return nil
		}
		switch record.Source {
		case BillingReservationSourceWallet:
			if err := adjustUserQuotaDeltaTx(tx, record.UserId, int64(delta)); err != nil {
				return err
			}
			cacheUserId = record.UserId
			cacheUserDelta = delta
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
	if cacheUserDelta != 0 || cacheTokenDelta != 0 {
		tokenKey := ""
		if cacheTokenDelta != 0 {
			tokenKey = billingReservationTokenKey(requestId)
		}
		syncBillingReservationCache(cacheUserId, tokenKey, cacheUserDelta, cacheTokenDelta)
	}
	return nil
}

// SettleBillingReservation 幂等结算：delta = 实际用量 - 预占。
// delta>0 无条件补扣（欠费有界），delta<0 退还。即使差额调整失败（如订阅余额
// 溢出），记录也会标记为 settled 保留预占扣减并记录告警——已发生的消费不能被
// 恢复任务误退。记录不存在时返回 ErrBillingReservationNotFound（调用方回退）。
func SettleBillingReservation(requestId string, delta int) error {
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheUserDelta int
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
			return nil
		}
		if delta != 0 {
			var applyErr error
			switch record.Source {
			case BillingReservationSourceWallet:
				applyErr = adjustUserQuotaDeltaTx(tx, record.UserId, int64(delta))
				if applyErr == nil {
					cacheUserId = record.UserId
				}
			case BillingReservationSourceSubscription:
				applyErr = postConsumeUserSubscriptionDeltaTx(tx, record.UserSubscriptionId, int64(delta))
			}
			// Key 额度与资金同步差额：补扣可把 remain 扣为负（上限已在预占守住）。
			if tokenErr := adjustTokenQuotaDeltaTx(tx, record.TokenId, int64(delta)); tokenErr != nil {
				common.SysLog(fmt.Sprintf("billing reservation settle token delta failed (request=%s, delta=%d): %s",
					requestId, delta, tokenErr.Error()))
			} else if applyErr == nil {
				cacheTokenDelta = delta
			}
			if applyErr != nil {
				common.SysLog(fmt.Sprintf("billing reservation settle delta failed (request=%s, source=%s, delta=%d): %s",
					requestId, record.Source, delta, applyErr.Error()))
			} else {
				cacheUserDelta = delta
			}
		}
		record.State = BillingReservationStateSettled
		return tx.Save(&record).Error
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

// ReleaseBillingReservation 幂等释放预占：Provider 未调用、明确失败、取消或
// 结果未知恢复时退还全部预占（含追加部分），恰好一次。
// 已 settled 的记录不释放（消费已发生），防止重复退款。
// 记录不存在时返回 ErrBillingReservationNotFound（调用方回退旧路径）。
func ReleaseBillingReservation(requestId string, tokenKey string) error {
	if requestId == "" {
		return ErrBillingReservationNotFound
	}
	var cacheUserId int
	var cacheUserDelta int
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
			return nil
		}
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
		record.State = BillingReservationStateReleased
		return tx.Save(&record).Error
	})
	if err != nil {
		return err
	}
	if cacheUserDelta != 0 || cacheTokenDelta != 0 {
		if tokenKey == "" {
			tokenKey = billingReservationTokenKey(requestId)
		}
		syncBillingReservationCache(cacheUserId, tokenKey, cacheUserDelta, cacheTokenDelta)
	}
	return nil
}

// ReleaseStaleBillingReservations 恢复任务：释放结果未知（进程崩溃/重启）的
// 陈旧预占。释放幂等，重复执行不会重复退款。返回释放的记录数。
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
		if err := ReleaseBillingReservation(record.RequestId, tokenKey); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// CleanupBillingReservationRecords 清理已终结的预占记录，保持表规模有界。
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
