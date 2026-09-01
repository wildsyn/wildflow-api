package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/bytedance/gopkg/util/gopool"
	"gorm.io/gorm"
)

type Token struct {
	Id                 int            `json:"id"`
	UserId             int            `json:"user_id" gorm:"index"`
	Key                string         `json:"key" gorm:"type:varchar(128);uniqueIndex"`
	Status             int            `json:"status" gorm:"default:1"`
	StatusVersion      int64          `json:"-"`
	Name               string         `json:"name" gorm:"index" `
	CreatedTime        int64          `json:"created_time" gorm:"bigint"`
	AccessedTime       int64          `json:"accessed_time" gorm:"bigint"`
	ExpiredTime        int64          `json:"expired_time" gorm:"bigint;default:-1"` // -1 means never expired
	RemainQuota        int            `json:"remain_quota" gorm:"default:0"`
	UnlimitedQuota     bool           `json:"unlimited_quota"`
	ModelLimitsEnabled bool           `json:"model_limits_enabled"`
	ModelLimits        string         `json:"model_limits" gorm:"type:text"`
	AllowIps           *string        `json:"allow_ips" gorm:"default:''"`
	UsedQuota          int            `json:"used_quota" gorm:"default:0"` // used quota
	Group              string         `json:"group" gorm:"default:''"`
	CrossGroupRetry    bool           `json:"cross_group_retry"` // 跨分组重试，仅auto分组有效
	AutoGroups         string         `json:"-" gorm:"type:text"`
	DeletedAt          gorm.DeletedAt `gorm:"index"`
}

func (token *Token) GetAutoGroups() ([]string, error) {
	if token.AutoGroups == "" {
		return nil, nil
	}
	var groups []string
	if err := common.UnmarshalJsonStr(token.AutoGroups, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func (token *Token) SetAutoGroups(groups []string) error {
	if len(groups) == 0 {
		token.AutoGroups = ""
		return nil
	}
	data, err := common.Marshal(groups)
	if err != nil {
		return err
	}
	token.AutoGroups = string(data)
	return nil
}

func (token *Token) Clean() {
	token.Key = ""
}

func MaskTokenKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	if len(key) <= 8 {
		return key[:2] + "****" + key[len(key)-2:]
	}
	return key[:4] + "**********" + key[len(key)-4:]
}

func (token *Token) GetFullKey() string {
	return token.Key
}

func (token *Token) GetMaskedKey() string {
	return MaskTokenKey(token.Key)
}

func (token *Token) GetIpLimits() []string {
	if token.AllowIps == nil {
		return nil
	}
	// Must share the splitter with write-time validation
	// (common.ValidateIPCIDRList) so a comma/newline list that passed
	// validation is enforced with exactly the same entries. Entries that are
	// not valid IP/CIDR (legacy rows written before validation existed) are
	// skipped instead of being merged into a bogus address that could never
	// match and would silently reject legitimate callers.
	limits := common.SplitIPCIDRList(*token.AllowIps)
	entries := make([]string, 0, len(limits))
	for _, entry := range limits {
		if common.IsValidIPOrCIDR(entry) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func GetAllUserTokens(userId int, startIdx int, num int) ([]*Token, error) {
	var tokens []*Token
	var err error
	err = DB.Where("user_id = ?", userId).Order("id desc").Limit(num).Offset(startIdx).Find(&tokens).Error
	return tokens, err
}

// sanitizeLikePattern 校验并清洗用户输入的 LIKE 搜索模式。
// 规则：
//  1. 转义 ! 和 _（使用 ! 作为 ESCAPE 字符，兼容 MySQL/PostgreSQL/SQLite）
//  2. 连续的 % 合并为单个 %
//  3. 最多允许 2 个 %
//  4. 含 % 时（模糊搜索），去掉 % 后关键词长度必须 >= 2
//  5. 不含 % 时按精确匹配
func sanitizeLikePattern(input string) (string, error) {
	// 1. 先转义 ESCAPE 字符 ! 自身，再转义 _
	//    使用 ! 而非 \ 作为 ESCAPE 字符，避免 MySQL 中反斜杠的字符串转义问题
	input = strings.ReplaceAll(input, "!", "!!")
	input = strings.ReplaceAll(input, `_`, `!_`)

	if err := validateLikePattern(input); err != nil {
		return "", err
	}

	// 5. 无 % 时，精确全匹配
	return input, nil
}

func validateLikePattern(input string) error {
	// 1. 连续的 % 直接拒绝
	if strings.Contains(input, "%%") {
		return errors.New("搜索模式中不允许包含连续的 % 通配符")
	}

	// 2. 统计 % 数量，不得超过 2
	count := strings.Count(input, "%")
	if count > 2 {
		return errors.New("搜索模式中最多允许包含 2 个 % 通配符")
	}

	// 3. 含 % 时，去掉 % 后关键词长度必须 >= 2
	if count > 0 {
		stripped := strings.ReplaceAll(input, "%", "")
		if len(stripped) < 2 {
			return errors.New("使用模糊搜索时，关键词长度至少为 2 个字符")
		}
	}

	return nil
}

const searchHardLimit = 100

func SearchUserTokens(userId int, keyword string, token string, offset int, limit int) (tokens []*Token, total int64, err error) {
	// model 层强制截断
	if limit <= 0 || limit > searchHardLimit {
		limit = searchHardLimit
	}
	if offset < 0 {
		offset = 0
	}

	if token != "" {
		token = strings.TrimPrefix(token, "sk-")
	}

	// 超量用户（令牌数超过上限）只允许精确搜索，禁止模糊搜索
	maxTokens := operation_setting.GetMaxUserTokens()
	hasFuzzy := strings.Contains(keyword, "%") || strings.Contains(token, "%")
	if hasFuzzy {
		count, err := CountUserTokens(userId)
		if err != nil {
			common.SysLog("failed to count user tokens: " + err.Error())
			return nil, 0, errors.New("获取令牌数量失败")
		}
		if int(count) > maxTokens {
			return nil, 0, errors.New("令牌数量超过上限，仅允许精确搜索，请勿使用 % 通配符")
		}
	}

	baseQuery := DB.Model(&Token{}).Where("user_id = ?", userId)

	// 非空才加 LIKE 条件，空则跳过（不过滤该字段）
	if keyword != "" {
		keywordPattern, err := sanitizeLikePattern(keyword)
		if err != nil {
			return nil, 0, err
		}
		baseQuery = baseQuery.Where("name LIKE ? ESCAPE '!'", keywordPattern)
	}
	if token != "" {
		tokenPattern, err := sanitizeLikePattern(token)
		if err != nil {
			return nil, 0, err
		}
		baseQuery = baseQuery.Where(commonKeyCol+" LIKE ? ESCAPE '!'", tokenPattern)
	}

	// 先查匹配总数（用于分页，受 maxTokens 上限保护，避免全表 COUNT）
	err = baseQuery.Limit(maxTokens).Count(&total).Error
	if err != nil {
		common.SysError("failed to count search tokens: " + err.Error())
		return nil, 0, errors.New("搜索令牌失败")
	}

	// 再分页查数据
	err = baseQuery.Order("id desc").Offset(offset).Limit(limit).Find(&tokens).Error
	if err != nil {
		common.SysError("failed to search tokens: " + err.Error())
		return nil, 0, errors.New("搜索令牌失败")
	}
	return tokens, total, nil
}

func ValidateUserToken(key string) (token *Token, err error) {
	if key == "" {
		return nil, ErrTokenNotProvided
	}
	token, err = GetTokenByKey(key, false)
	if err == nil {
		if token.Status == common.TokenStatusDisabled {
			return token, ErrTokenDisabled
		}
		if token.Status == common.TokenStatusExpired {
			return token, ErrTokenExpired
		}
		if token.Status == common.TokenStatusExhausted {
			return token, ErrTokenQuotaExhausted
		}
		if token.Status != common.TokenStatusEnabled {
			return token, ErrTokenInvalid
		}
		if token.ExpiredTime != -1 && token.ExpiredTime < common.GetTimestamp() {
			if !common.RedisEnabled {
				token.Status = common.TokenStatusExpired
				err := token.SelectUpdate()
				if err != nil {
					common.SysLog("failed to update token status" + err.Error())
				}
			}
			return token, ErrTokenExpired
		}
		if !token.UnlimitedQuota && token.RemainQuota <= 0 {
			if !common.RedisEnabled {
				token.Status = common.TokenStatusExhausted
				err := token.SelectUpdate()
				if err != nil {
					common.SysLog("failed to update token status" + err.Error())
				}
			}
			return token, ErrTokenQuotaExhausted
		}
		return token, nil
	}
	common.SysLog("ValidateUserToken: failed to get token: " + err.Error())
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenNotFound
	}
	return nil, fmt.Errorf("%w: %v", ErrDatabase, err)
}

func GetTokenByIds(id int, userId int) (*Token, error) {
	if id == 0 || userId == 0 {
		return nil, errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	var err error = nil
	err = DB.First(&token, "id = ? and user_id = ?", id, userId).Error
	return &token, err
}

func GetTokenById(id int) (*Token, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	token := Token{Id: id}
	var err error = nil
	err = DB.First(&token, "id = ?", id).Error
	if shouldUpdateRedis(true, err) {
		tokenID, tokenKey := token.Id, token.Key
		goTokenCacheFill(func() {
			if err := refreshTokenCacheFromDatabase(tokenID, tokenKey); err != nil && !errors.Is(err, ErrTokenCacheRevocationPending) && !errors.Is(err, gorm.ErrRecordNotFound) {
				common.SysLog("failed to update user status cache: " + err.Error())
			}
		})
	}
	return &token, err
}

func GetTokenByKey(key string, fromDB bool) (token *Token, err error) {
	defer func() {
		// Update Redis cache asynchronously from the row that still exists when
		// the fill runs. The fill must not use this read's snapshot: a revoke can
		// commit while it is queued, and the finite revocation fence may expire
		// before it reaches Redis.
		if shouldUpdateRedis(fromDB, err) && token != nil {
			tokenID, tokenKey := token.Id, token.Key
			goTokenCacheFill(func() {
				if err := refreshTokenCacheFromDatabase(tokenID, tokenKey); err != nil && !errors.Is(err, ErrTokenCacheRevocationPending) && !errors.Is(err, gorm.ErrRecordNotFound) {
					common.SysLog("failed to update user status cache: " + err.Error())
				}
			})
		}
	}()
	if !fromDB && common.RedisEnabled {
		// Try Redis first
		token, err := cacheGetTokenByKey(key)
		if err == nil {
			return token, nil
		}
		// Don't return error - fall through to DB
	}
	fromDB = true
	err = DB.Where(commonKeyCol+" = ?", key).First(&token).Error
	return token, err
}

func (token *Token) Insert() error {
	var err error
	err = DB.Create(token).Error
	return err
}

// Update updates editable token fields. Status is deliberately excluded: a
// stale ordinary edit must never re-enable a token that was disabled after the
// edit read its snapshot. Status transitions use UpdateStatus instead.
func (token *Token) Update() (err error) {
	result := DB.Model(token).Select("name", "expired_time", "remain_quota", "unlimited_quota",
		"model_limits_enabled", "model_limits", "allow_ips", "group", "cross_group_retry", "auto_groups").Updates(token)
	err = result.Error
	if err != nil {
		return err
	}
	if shouldUpdateRedis(true, err) {
		// A zero-row update means the row no longer exists (soft-deleted by a
		// concurrent revoke that won the race): nothing was committed, so no
		// stale snapshot may enter the cache.
		if result.RowsAffected == 0 {
			return nil
		}
		// Refresh from the committed row, not this caller's stale snapshot. The
		// fence protects the immediate race; the database re-read also protects
		// a delayed ordinary edit after the finite fence TTL has elapsed.
		if cacheErr := refreshTokenCacheFromDatabase(token.Id, token.Key); cacheErr != nil && !errors.Is(cacheErr, ErrTokenCacheRevocationPending) {
			common.SysLog("failed to update token cache: " + cacheErr.Error())
			if deleteErr := cacheDeleteToken(token.Key); deleteErr != nil {
				common.SysLog("failed to invalidate token cache after update: " + deleteErr.Error())
			}
		}
	}
	return nil
}

// UpdateStatus performs a status-only transition using the status and version
// the caller originally read. A concurrent status change therefore wins
// instead of being overwritten by a stale enable request. Re-enabling releases
// only the fence epoch observed before the database compare-and-swap.
func (token *Token) UpdateStatus(previousStatus int) error {
	revoking := common.RedisEnabled && token.Status != common.TokenStatusEnabled
	var observedEpoch int64
	var err error
	if common.RedisEnabled {
		if revoking {
			if err := raiseTokenRevocationFences([]string{token.Key}); err != nil {
				return err
			}
		} else if previousStatus != common.TokenStatusEnabled {
			observedEpoch, err = readRevocationEpoch(token.Key)
			if err != nil {
				return fmt.Errorf("%w: read fence failed: %v", ErrTokenCacheRevocationPending, err)
			}
		}
	}

	result := DB.Model(&Token{}).
		Where("id = ? AND user_id = ? AND status = ? AND COALESCE(status_version, 0) = ?", token.Id, token.UserId, previousStatus, token.StatusVersion).
		Updates(map[string]any{
			"status":         token.Status,
			"status_version": gorm.Expr("COALESCE(status_version, 0) + ?", 1),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrTokenStatusChanged
	}
	token.StatusVersion++

	if revoking {
		if err := revokeTokensCacheCommitted([]string{token.Key}); err != nil {
			common.SysLog("failed to invalidate token cache after status revoke: " + err.Error())
			return err
		}
		return nil
	}
	if token.Status == common.TokenStatusEnabled && previousStatus != common.TokenStatusEnabled {
		if err := AllowTokenCacheRefresh(token.Key, observedEpoch); err != nil {
			if !errors.Is(err, ErrTokenCacheRevocationPending) {
				common.SysLog("failed to clear token revocation fence on re-enable: " + err.Error())
				return err
			}
			// A newer revocation owns the deny window. Do not cache the enabled
			// snapshot and, crucially, do not remove its fence.
			common.SysLog("token re-enable raced a newer revocation fence; fence kept for " + token.Key)
			return nil
		}
	}
	if shouldUpdateRedis(true, nil) {
		if err := cacheSetTokenRespectingRevocation(*token); err != nil && !errors.Is(err, ErrTokenCacheRevocationPending) {
			common.SysLog("failed to update token cache after status change: " + err.Error())
		}
	}
	return nil
}

func (token *Token) SelectUpdate() (err error) {
	defer func() {
		if shouldUpdateRedis(true, err) {
			tokenID, tokenKey := token.Id, token.Key
			goTokenCacheFill(func() {
				err := refreshTokenCacheFromDatabase(tokenID, tokenKey)
				if err != nil && !errors.Is(err, ErrTokenCacheRevocationPending) && !errors.Is(err, gorm.ErrRecordNotFound) {
					common.SysLog("failed to update token cache: " + err.Error())
				}
			})
		}
	}()
	// This can update zero values
	return DB.Model(token).Select("accessed_time", "status").Updates(token).Error
}

func (token *Token) Delete() (err error) {
	// Fail-closed revocation: raise the revocation fence BEFORE the database
	// delete so a racing cache fill on any node cannot outlive it. If Redis
	// fails, the delete aborts so the caller never sees success while a stale
	// grant could still authorize requests.
	if common.RedisEnabled {
		if err := raiseTokenRevocationFences([]string{token.Key}); err != nil {
			return err
		}
	}
	err = DB.Delete(token).Error
	if err != nil {
		return err
	}
	if common.RedisEnabled {
		if err := revokeTokensCacheCommitted([]string{token.Key}); err != nil {
			return err
		}
	}
	return nil
}

func (token *Token) IsModelLimitsEnabled() bool {
	return token.ModelLimitsEnabled
}

func (token *Token) GetModelLimits() []string {
	if token.ModelLimits == "" {
		return []string{}
	}
	return strings.Split(token.ModelLimits, ",")
}

func (token *Token) GetModelLimitsMap() map[string]bool {
	limits := token.GetModelLimits()
	limitsMap := make(map[string]bool)
	for _, limit := range limits {
		limitsMap[limit] = true
	}
	return limitsMap
}

func DisableModelLimits(tokenId int) error {
	token, err := GetTokenById(tokenId)
	if err != nil {
		return err
	}
	token.ModelLimitsEnabled = false
	token.ModelLimits = ""
	return token.Update()
}

func DeleteTokenById(id int, userId int) (err error) {
	// Why we need userId here? In case user want to delete other's token.
	if id == 0 || userId == 0 {
		return errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	err = DB.Where(token).First(&token).Error
	if err != nil {
		return err
	}
	return token.Delete()
}

func IncreaseTokenQuota(tokenId int, key string, quota int) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if common.RedisEnabled {
		gopool.Go(func() {
			err := cacheIncrTokenQuota(key, int64(quota))
			if err != nil {
				common.SysLog("failed to increase token quota: " + err.Error())
			}
		})
	}
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeTokenQuota, tokenId, quota)
		return nil
	}
	return increaseTokenQuota(tokenId, quota)
}

func increaseTokenQuota(id int, quota int) (err error) {
	err = DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"remain_quota":  gorm.Expr("remain_quota + ?", quota),
			"used_quota":    gorm.Expr("used_quota - ?", quota),
			"accessed_time": common.GetTimestamp(),
		},
	).Error
	return err
}

func DecreaseTokenQuota(id int, key string, quota int) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if common.RedisEnabled {
		gopool.Go(func() {
			err := cacheDecrTokenQuota(key, int64(quota))
			if err != nil {
				common.SysLog("failed to decrease token quota: " + err.Error())
			}
		})
	}
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeTokenQuota, id, -quota)
		return nil
	}
	return decreaseTokenQuota(id, quota)
}

func decreaseTokenQuota(id int, quota int) (err error) {
	err = DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		},
	).Error
	return err
}

// CountUserTokens returns total number of tokens for the given user, used for pagination
func CountUserTokens(userId int) (int64, error) {
	var total int64
	err := DB.Model(&Token{}).Where("user_id = ?", userId).Count(&total).Error
	return total, err
}

// BatchDeleteTokens 删除指定用户的一组令牌，返回成功删除数量
//
// Idempotent under retry: ids whose rows were already soft-deleted by a prior
// attempt (e.g. one whose cache cleanup failed) are still resolved through the
// soft-delete tombstones, so the fence-and-prove cache cleanup completes and
// the retry cannot report success while a revoked key still authorizes.
func BatchDeleteTokens(ids []int, userId int) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("ids 不能为空！")
	}

	tx := DB.Begin()

	var tokens []Token
	if err := tx.Where("user_id = ? AND id IN (?)", userId, ids).Find(&tokens).Error; err != nil {
		tx.Rollback()
		return 0, err
	}

	if len(tokens) == 0 {
		// Nothing live to delete — but a previous attempt of the same batch may
		// have committed the deletes and failed the cache cleanup. Resolve the
		// tombstones so the cleanup still runs before reporting success.
		if err := tx.Unscoped().Select("id", "user_id", commonKeyCol).
			Where("user_id = ? AND id IN (?)", userId, ids).Find(&tokens).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
		tx.Rollback()
		if len(tokens) == 0 {
			return 0, nil
		}
		keys := make([]string, 0, len(tokens))
		for _, t := range tokens {
			if t.Key != "" {
				keys = append(keys, t.Key)
			}
		}
		if err := RevokeTokensCacheRetry(keys); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Raise the revocation fences before the delete commits so every cache fill
	// that raced this batch is poisoned. A fence failure aborts the whole batch
	// with nothing changed — the caller never gets success while a deleted key
	// could still authorize through a stale cache entry.
	keys := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t.Key != "" {
			keys = append(keys, t.Key)
		}
	}
	if common.RedisEnabled {
		if err := raiseTokenRevocationFences(keys); err != nil {
			tx.Rollback()
			return 0, err
		}
	}

	if err := tx.Where("user_id = ? AND id IN (?)", userId, ids).Delete(&Token{}).Error; err != nil {
		tx.Rollback()
		return 0, err
	}

	if err := tx.Commit().Error; err != nil {
		return 0, err
	}

	// After commit: prove every cached hash is gone synchronously. Same
	// fail-closed contract as DeleteTokenById; fences stay raised on failure.
	if err := revokeTokensCacheCommitted(keys); err != nil {
		return len(tokens), err
	}

	return len(tokens), nil
}

func GetTokenKeysByIds(ids []int, userId int) ([]Token, error) {
	var tokens []Token
	err := DB.Select("id", commonKeyCol).
		Where("user_id = ? AND id IN (?)", userId, ids).
		Find(&tokens).Error
	return tokens, err
}

// InvalidateUserTokensCache 清理指定用户所有令牌在 Redis 中的缓存，
// 配合 InvalidateUserCache 使用，可在用户被禁用/删除时立即阻断其令牌的请求。
// 下一次请求将从数据库重新加载令牌及用户状态，从而立即识别出被禁用的用户。
func InvalidateUserTokensCache(userId int) error {
	if !common.RedisEnabled {
		return nil
	}
	if userId <= 0 {
		return errors.New("userId 无效")
	}
	var tokens []Token
	if err := DB.Unscoped().
		Select("id", commonKeyCol).
		Where("user_id = ?", userId).
		Find(&tokens).Error; err != nil {
		return err
	}
	return invalidateTokensCache(tokens)
}

func invalidateTokensCache(tokens []Token) error {
	if !common.RedisEnabled {
		return nil
	}
	var firstErr error
	for _, t := range tokens {
		if t.Key == "" {
			continue
		}
		if err := cacheDeleteToken(t.Key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
