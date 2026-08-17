package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

func getUserQuotaMutationFenceKey(userId int) string {
	return fmt.Sprintf("user:quota:fence:%d", userId)
}

func getTokenQuotaMutationFenceKey(key string) string {
	return fmt.Sprintf("token:quota:fence:%s", common.GenerateHMAC(key))
}

func midjourneyBillingCacheOwner(taskId int, phase string) string {
	return fmt.Sprintf("midjourney:%d:%s", taskId, phase)
}

func midjourneyBillingCacheOperationKey(taskId int, phase string, target string) string {
	return fmt.Sprintf("midjourney:billing:%d:%s:%s", taskId, phase, target)
}

const acquireMidjourneyBillingFenceScript = `
local current = redis.call('GET', KEYS[1])
if current and current ~= ARGV[1] then
  return 0
end
redis.call('SET', KEYS[1], ARGV[1])
return 1`

const releaseMidjourneyBillingFenceScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

const applyMidjourneyUserCacheDeltaScript = `
if redis.call('GET', KEYS[2]) ~= ARGV[3] then
  return -2
end
if redis.call('EXISTS', KEYS[3]) == 1 then
  return 2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[1])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local delta = tonumber(ARGV[4])
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if ARGV[5] == '1' and quota + delta < 0 then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', delta)
redis.call('SET', KEYS[3], 1)
return 1`

const applyMidjourneyTokenCacheDeltaScript = `
if redis.call('GET', KEYS[2]) ~= ARGV[2] then
  return -2
end
if redis.call('EXISTS', KEYS[3]) == 1 then
  return 2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[1])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local delta = tonumber(ARGV[3])
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
local unlimited = redis.call('HGET', KEYS[1], 'UnlimitedQuota')
if delta < 0 and unlimited ~= 'true' and unlimited ~= '1' and remain + delta < 0 then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', delta)
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -delta)
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[4])
redis.call('SET', KEYS[3], 1)
return 1`

const compensateMidjourneyUserCacheDeltaScript = `
if redis.call('GET', KEYS[2]) ~= ARGV[3] then
  return -2
end
if redis.call('EXISTS', KEYS[3]) == 0 then
  return 2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') == tonumber(ARGV[1])
  and tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') == tonumber(ARGV[2])
  and redis.call('HEXISTS', KEYS[1], 'Quota') == 1 then
  redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[4]))
end
redis.call('DEL', KEYS[3])
return 1`

const compensateMidjourneyTokenCacheDeltaScript = `
if redis.call('GET', KEYS[2]) ~= ARGV[2] then
  return -2
end
if redis.call('EXISTS', KEYS[3]) == 0 then
  return 2
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') == tonumber(ARGV[1])
  and redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 1
  and redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 1 then
  redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[3]))
  redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[3]))
end
redis.call('DEL', KEYS[3])
return 1`

type midjourneyBillingCacheMutation struct {
	taskId   int
	userId   int
	tokenId  int
	tokenKey string
	phase    string
	owner    string
}

func newMidjourneyBillingCacheMutation(task *Midjourney, tokenKey string, phase string) midjourneyBillingCacheMutation {
	return midjourneyBillingCacheMutation{
		taskId:   task.Id,
		userId:   task.UserId,
		tokenId:  task.TokenId,
		tokenKey: tokenKey,
		phase:    phase,
		owner:    midjourneyBillingCacheOwner(task.Id, phase),
	}
}

func (mutation *midjourneyBillingCacheMutation) prepare(databaseMutationApplied bool) error {
	if !common.RedisEnabled {
		return nil
	}
	if mutation.tokenId > 0 {
		var token Token
		err := DB.Unscoped().Select("id", commonKeyCol, "deleted_at").First(&token, mutation.tokenId).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			mutation.tokenId = 0
			mutation.tokenKey = ""
		} else if err != nil {
			return midjourneyBillingCacheRetryError("resolve token quota cache", err)
		} else if token.DeletedAt.Valid {
			mutation.tokenId = 0
			mutation.tokenKey = ""
		} else {
			mutation.tokenKey = token.Key
		}
	}
	if err := mutation.acquireFence(getUserQuotaMutationFenceKey(mutation.userId)); err != nil {
		return err
	}
	if mutation.tokenId > 0 {
		if mutation.tokenKey == "" {
			return midjourneyBillingCacheRetryError("hydrate token quota cache", errors.New("token key is empty"))
		}
		if err := mutation.acquireFence(getTokenQuotaMutationFenceKey(mutation.tokenKey)); err != nil {
			return err
		}
	}
	user, err := GetUserById(mutation.userId, false)
	if err != nil {
		return midjourneyBillingCacheRetryError("load user quota cache snapshot", err)
	}
	if err := writeUserCacheWithQuotaFence(
		user.ToBaseUser(),
		true,
		mutation.owner,
		midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "user"),
		databaseMutationApplied,
	); err != nil {
		return midjourneyBillingCacheRetryError("hydrate user quota cache", err)
	}
	if mutation.tokenId > 0 {
		token, err := GetTokenByKey(mutation.tokenKey, true)
		if err != nil {
			return midjourneyBillingCacheRetryError("load token quota cache snapshot", err)
		}
		if token.Id != mutation.tokenId {
			return midjourneyBillingCacheRetryError("load token quota cache snapshot", errors.New("token id changed"))
		}
		code, err := cacheInitTokenWithQuotaFence(
			*token,
			mutation.owner,
			midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "token"),
			databaseMutationApplied,
		)
		if err != nil {
			return midjourneyBillingCacheRetryError("hydrate token quota cache", err)
		}
		if code == 0 {
			return midjourneyBillingCacheRetryError("hydrate token quota cache", ErrQuotaCacheMutationPending)
		}
	}
	return nil
}

func (mutation midjourneyBillingCacheMutation) apply(userDelta int, tokenDelta int, requireUserBalance bool) error {
	if !common.RedisEnabled || (userDelta == 0 && tokenDelta == 0) {
		return nil
	}
	checkBalance := "0"
	if requireUserBalance {
		checkBalance = "1"
	}
	if userDelta != 0 {
		userResult, err := common.RDB.Eval(context.Background(), applyMidjourneyUserCacheDeltaScript,
			[]string{
				getUserCacheKey(mutation.userId),
				getUserQuotaMutationFenceKey(mutation.userId),
				midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "user"),
			},
			mutation.userId, userCacheSchemaVersion, mutation.owner, userDelta, checkBalance,
		).Int()
		if err != nil {
			return midjourneyBillingCacheRetryError("apply user quota cache delta", err)
		}
		if userResult == 0 {
			return ErrUserQuotaInsufficient
		}
		if userResult != 1 && userResult != 2 {
			return midjourneyBillingCacheRetryError("apply user quota cache delta", fmt.Errorf("result %d", userResult))
		}
	}

	if mutation.tokenId <= 0 || tokenDelta == 0 {
		return nil
	}
	tokenResult, err := common.RDB.Eval(context.Background(), applyMidjourneyTokenCacheDeltaScript,
		[]string{
			getTokenCacheKey(mutation.tokenKey),
			getTokenQuotaMutationFenceKey(mutation.tokenKey),
			midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "token"),
		},
		mutation.tokenId, mutation.owner, tokenDelta, common.GetTimestamp(),
	).Int()
	if err == nil && tokenResult == 0 {
		return errors.Join(ErrTokenQuotaInsufficient, mutation.compensateUser(userDelta))
	}
	if err != nil || (tokenResult != 1 && tokenResult != 2) {
		compensateErr := mutation.compensateUser(userDelta)
		if err == nil {
			err = fmt.Errorf("result %d", tokenResult)
		}
		return midjourneyBillingCacheRetryError("apply token quota cache delta", errors.Join(err, compensateErr))
	}
	return nil
}

func (mutation midjourneyBillingCacheMutation) compensate(userDelta int, tokenDelta int) error {
	if !common.RedisEnabled || (userDelta == 0 && tokenDelta == 0) {
		return nil
	}
	var errs []error
	if mutation.tokenId > 0 && tokenDelta != 0 {
		result, err := common.RDB.Eval(context.Background(), compensateMidjourneyTokenCacheDeltaScript,
			[]string{
				getTokenCacheKey(mutation.tokenKey),
				getTokenQuotaMutationFenceKey(mutation.tokenKey),
				midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "token"),
			},
			mutation.tokenId, mutation.owner, tokenDelta,
		).Int()
		if err != nil || (result != 1 && result != 2) {
			errs = append(errs, fmt.Errorf("compensate token quota cache delta: result=%d error=%v", result, err))
		}
	}
	if err := mutation.compensateUser(userDelta); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (mutation midjourneyBillingCacheMutation) compensateUser(delta int) error {
	if delta == 0 {
		return nil
	}
	result, err := common.RDB.Eval(context.Background(), compensateMidjourneyUserCacheDeltaScript,
		[]string{
			getUserCacheKey(mutation.userId),
			getUserQuotaMutationFenceKey(mutation.userId),
			midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "user"),
		},
		mutation.userId, userCacheSchemaVersion, mutation.owner, delta,
	).Int()
	if err != nil || (result != 1 && result != 2) {
		return fmt.Errorf("compensate user quota cache delta: result=%d error=%v", result, err)
	}
	return nil
}

func (mutation midjourneyBillingCacheMutation) release() error {
	if !common.RedisEnabled {
		return nil
	}
	var errs []error
	if mutation.tokenId > 0 {
		if err := mutation.releaseFence(getTokenQuotaMutationFenceKey(mutation.tokenKey)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := mutation.releaseFence(getUserQuotaMutationFenceKey(mutation.userId)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (mutation midjourneyBillingCacheMutation) cleanupOperationKeys() {
	if !common.RedisEnabled {
		return
	}
	keys := []string{midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "user")}
	if mutation.tokenId > 0 {
		keys = append(keys, midjourneyBillingCacheOperationKey(mutation.taskId, mutation.phase, "token"))
	}
	if err := common.RDB.Del(context.Background(), keys...).Err(); err != nil {
		common.SysLog("failed to clean Midjourney billing cache operation keys: " + err.Error())
	}
}

func (mutation midjourneyBillingCacheMutation) acquireFence(key string) error {
	result, err := common.RDB.Eval(context.Background(), acquireMidjourneyBillingFenceScript, []string{key}, mutation.owner).Int()
	if err != nil {
		return midjourneyBillingCacheRetryError("acquire quota cache fence", err)
	}
	if result != 1 {
		return midjourneyBillingCacheRetryError("acquire quota cache fence", ErrQuotaCacheMutationPending)
	}
	return nil
}

func (mutation midjourneyBillingCacheMutation) releaseFence(key string) error {
	_, err := common.RDB.Eval(context.Background(), releaseMidjourneyBillingFenceScript, []string{key}, mutation.owner).Int()
	if err != nil {
		return midjourneyBillingCacheRetryError("release quota cache fence", err)
	}
	return nil
}

func midjourneyBillingCacheRetryError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrMidjourneyBillingRetryable, operation, err)
}
