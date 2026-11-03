package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type cacheQuotaResult int

const (
	cacheQuotaInsufficient cacheQuotaResult = iota
	cacheQuotaOK
	cacheQuotaMiss
	cacheQuotaFenced
)

var ErrQuotaCacheMutationPending = errors.New("quota cache mutation is pending")
var ErrTokenQuotaInsufficient = errors.New("token quota insufficient")

const userQuotaReserveScript = `
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota == nil or quota < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

const userQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[1])
return 1`

const tokenQuotaReserveScript = `
if redis.call('EXISTS', KEYS[2]) == 1 or redis.call('EXISTS', KEYS[3]) == 1 then
  return -2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
if remain == nil or remain < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

const tokenQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

func quotaResultFromLua(result int, err error) (cacheQuotaResult, error) {
	if err != nil {
		return cacheQuotaMiss, err
	}
	switch result {
	case 1:
		return cacheQuotaOK, nil
	case 0:
		return cacheQuotaInsufficient, nil
	case -2:
		return cacheQuotaFenced, nil
	default:
		return cacheQuotaMiss, nil
	}
}

func cacheTryReserveUserQuota(userID int, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaReserveScript,
		[]string{getUserCacheKey(userID), getUserQuotaMutationFenceKey(userID)}, amount, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyUserQuotaDelta(userID int, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaDeltaScript,
		[]string{getUserCacheKey(userID)}, delta, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheTryReserveTokenQuota(id int, key string, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaReserveScript,
		[]string{getTokenCacheKey(key), getTokenQuotaMutationFenceKey(key), getTokenCacheFenceKey(key)}, amount, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyTokenQuotaDelta(id int, key string, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaDeltaScript,
		[]string{getTokenCacheKey(key)}, delta, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

func hasPendingMidjourneyBillingForUser(userID int) (bool, error) {
	return hasPendingMidjourneyBilling("user_id", userID)
}

func hasPendingMidjourneyBillingForToken(tokenID int) (bool, error) {
	return hasPendingMidjourneyBilling("token_id", tokenID)
}

func hasPendingMidjourneyBilling(column string, id int) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	pendingStatuses := []string{
		MidjourneyBillingStatusPrepared,
		MidjourneyBillingStatusCharging,
		MidjourneyBillingStatusChargePending,
		MidjourneyBillingStatusRefunding,
		MidjourneyBillingStatusRefundPending,
	}
	var taskID int
	err := DB.Model(&Midjourney{}).
		Where(column+" = ? AND quota > 0", id).
		Where("billing_status IN ? OR (status = ? AND (billing_status = ? OR billing_status = ? OR billing_status IS NULL))",
			pendingStatuses, "FAILURE", MidjourneyBillingStatusCharged, "").
		Limit(1).
		Pluck("id", &taskID).Error
	return taskID != 0, err
}

// persistUserQuotaDelta durably records a cache-side reservation before the
// caller may send an upstream request. Balance deltas are never kept only in
// the in-process batch queue because Redis loss must be recoverable from DB.
func persistUserQuotaDelta(id int, delta int) error {
	return persistUserQuotaDeltaTx(DB, id, delta)
}

func persistUserQuotaDeltaTx(tx *gorm.DB, id int, delta int) error {
	query := tx.Model(&User{}).Where("id = ?", id)
	if delta < 0 {
		query = query.Where("quota >= ?", -delta)
	}
	result := query.Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		if delta < 0 {
			var count int64
			if countErr := tx.Model(&User{}).Where("id = ?", id).Count(&count).Error; countErr != nil {
				return countErr
			}
			if count == 0 {
				return gorm.ErrRecordNotFound
			}
			return ErrUserQuotaInsufficient
		}
		return gorm.ErrRecordNotFound
	}
	return nil
}

// debitUserQuotaWithBalance reads the balance after the conditional update in
// the same transaction, while its write lock still excludes other mutations.
func debitUserQuotaWithBalance(id int, quota int, change *common.WalletQuotaChange, reservation *WalletQuotaReservation) error {
	if change == nil && reservation == nil {
		return persistUserQuotaDelta(id, -quota)
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := persistUserQuotaDeltaTx(tx, id, -quota); err != nil {
			if reservation != nil && errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserQuotaInsufficient
			}
			return err
		}
		if reservation != nil {
			var current WalletQuotaReservation
			result := tx.Where("request_id = ?", reservation.RequestID).Limit(1).Find(&current)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected > 0 && current.UserID != id {
				return errors.New("wallet reservation owner mismatch")
			}
			amount := quota
			if current.ExpiresAt > time.Now().UnixMilli() {
				amount += current.Amount
			}
			return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "request_id"}}, DoUpdates: clause.Assignments(map[string]any{"amount": amount, "expires_at": reservation.ExpiresAt})}).
				Create(&WalletQuotaReservation{RequestID: reservation.RequestID, UserID: id, Amount: amount, ExpiresAt: reservation.ExpiresAt}).Error
		}
		if change != nil {
			balance, err := walletSettledBalanceTx(tx, id)
			if err != nil {
				return err
			}
			*change = common.WalletQuotaChange{Before: balance + quota, After: balance}
		}
		return nil
	})
}

func persistTokenQuotaDelta(id int, delta int) error {
	query := DB.Model(&Token{}).Where("id = ?", id)
	if delta < 0 {
		query = query.Where("remain_quota >= ?", -delta)
	}
	result := query.Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota + ?", delta),
			"used_quota":    gorm.Expr("used_quota - ?", delta),
			"accessed_time": common.GetTimestamp(),
		},
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		if delta < 0 {
			var count int64
			if countErr := DB.Model(&Token{}).Where("id = ?", id).Count(&count).Error; countErr != nil {
				return countErr
			}
			if count == 0 {
				return gorm.ErrRecordNotFound
			}
			return ErrTokenQuotaInsufficient
		}
		return gorm.ErrRecordNotFound
	}
	return nil
}

func reserveUserQuotaDB(id int, quota int, reservation *WalletQuotaReservation) (bool, error) {
	if reservation != nil {
		err := debitUserQuotaWithBalance(id, quota, nil, reservation)
		if errors.Is(err, ErrUserQuotaInsufficient) || errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	result := DB.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	return result.RowsAffected == 1, result.Error
}

func reserveTokenQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

// TryReserveUserQuota atomically checks and deducts a user's wallet quota.
// Redis serializes concurrent reservations, while every successful balance
// delta is synchronously persisted so cache loss can recover from the DB.
// Redis failures fall back to the same conditional database update unless a
// durable Midjourney billing mutation requires fail-closed reconciliation.
func TryReserveUserQuota(id int, quota int) (bool, error) {
	return tryReserveUserQuota(id, quota, nil)
}

// TryReserveWalletQuota reserves spendable money and creates or increases the
// request's temporary reminder marker atomically. requestID must be globally
// unique; expiresAt is a future Unix millisecond timestamp. Markers never admit
// additional spending and may expire without changing real money.
func TryReserveWalletQuota(id int, quota int, requestID string, expiresAt int64) (bool, error) {
	if requestID == "" || expiresAt <= time.Now().UnixMilli() {
		return false, errors.New("wallet reservation lease is invalid")
	}
	reserved, err := tryReserveUserQuota(id, quota, &WalletQuotaReservation{RequestID: requestID, UserID: id, Amount: quota, ExpiresAt: expiresAt})
	if err == nil && reserved {
		maybeCleanupWalletQuotaReservations(time.Now().UnixMilli())
	}
	return reserved, err
}

func tryReserveUserQuota(id int, quota int, reservation *WalletQuotaReservation) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota cannot be negative")
	}
	if quota == 0 {
		return true, nil
	}
	if !common.RedisEnabled {
		return reserveUserQuotaDB(id, quota, reservation)
	}

	result, err := cacheTryReserveUserQuota(id, int64(quota))
	if err != nil || result == cacheQuotaMiss {
		pending, pendingErr := hasPendingMidjourneyBillingForUser(id)
		if pendingErr != nil {
			return false, pendingErr
		}
		if pending {
			return false, ErrQuotaCacheMutationPending
		}
	}
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetUserCache(id); hydrateErr == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if err != nil {
			common.SysLog(common.LogText("user quota cache reserve unavailable, falling back to database: %s", err.Error()))
		}
		return reserveUserQuotaDB(id, quota, reservation)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if result == cacheQuotaFenced {
		return false, ErrQuotaCacheMutationPending
	}
	if err = debitUserQuotaWithBalance(id, quota, nil, reservation); err != nil {
		compensated, compensateErr := cacheApplyUserQuotaDelta(id, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(common.LogText("failed to compensate reserved user quota: result=%d error=%v", compensated, compensateErr))
		}
		if errors.Is(err, ErrUserQuotaInsufficient) {
			if invalidateErr := invalidateUserCache(id); invalidateErr != nil {
				common.SysLog("failed to invalidate stale user quota cache: " + invalidateErr.Error())
			}
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// TryReserveTokenQuota atomically checks and deducts a token quota. Unlimited
// tokens skip the balance check but still update remain/used accounting.
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota cannot be negative")
	}
	if quota == 0 {
		return true, nil
	}
	if unlimited {
		return true, DecreaseTokenQuota(id, key, quota)
	}
	if !common.RedisEnabled {
		return reserveTokenQuotaDB(id, quota)
	}

	result, err := cacheTryReserveTokenQuota(id, key, int64(quota))
	if err != nil || result == cacheQuotaMiss {
		pending, pendingErr := hasPendingMidjourneyBillingForToken(id)
		if pendingErr != nil {
			return false, pendingErr
		}
		if pending {
			return false, ErrQuotaCacheMutationPending
		}
	}
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetTokenByKey(key, true); hydrateErr == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if err != nil {
			common.SysLog(common.LogText("token quota cache reserve unavailable, falling back to database: %s", err.Error()))
		}
		return reserveTokenQuotaDB(id, quota)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if result == cacheQuotaFenced {
		return false, ErrQuotaCacheMutationPending
	}
	if err = persistTokenQuotaDelta(id, -quota); err != nil {
		compensated, compensateErr := cacheApplyTokenQuotaDelta(id, key, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(common.LogText("failed to compensate reserved token quota: result=%d error=%v", compensated, compensateErr))
		}
		if errors.Is(err, ErrTokenQuotaInsufficient) {
			if invalidateErr := invalidateTokenCacheForMutation(key); invalidateErr != nil {
				common.SysLog("failed to invalidate stale token quota cache: " + invalidateErr.Error())
			}
			return false, nil
		}
		return false, err
	}
	return true, nil
}
