package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
)

const walletTrustInflightTTL = 6 * time.Hour

const reserveWalletTrustInflightScript = `
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
local delta = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
if current + delta > limit then
  return 0
end
redis.call("INCRBY", KEYS[1], delta)
redis.call("EXPIRE", KEYS[1], ttl)
return 1
`

const releaseWalletTrustInflightScript = `
local delta = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
local next = current - delta
if next <= 0 then
  redis.call("DEL", KEYS[1])
  return 0
end
redis.call("SET", KEYS[1], next, "EX", ttl)
return next
`

type walletTrustInflightEntry struct {
	quota     int
	expiresAt time.Time
}

var walletTrustInflightMu sync.Mutex
var walletTrustInflight = map[int]walletTrustInflightEntry{}

func walletTrustBypassAllowed(c *gin.Context, relayInfo *relaycommon.RelayInfo, quota int) bool {
	if !common.WalletTrustBypassEnabled || relayInfo == nil || quota <= 0 {
		return false
	}

	minQuota := common.GetWalletTrustBypassMinQuota()
	maxInflightQuota := common.GetWalletTrustBypassMaxInflightQuota()
	if minQuota <= 0 || maxInflightQuota <= 0 || quota > maxInflightQuota {
		return false
	}

	if relayInfo.UserQuota <= minQuota {
		return false
	}

	if relayInfo.TokenUnlimited {
		return true
	}
	return c.GetInt("token_quota") > minQuota
}

func reserveWalletTrustInflight(userID int, quota int, limit int) bool {
	if userID <= 0 || quota <= 0 || limit <= 0 {
		return false
	}
	if common.RedisEnabled && common.RDB != nil {
		key := walletTrustInflightKey(userID)
		ttlSeconds := int(walletTrustInflightTTL.Seconds())
		result, err := common.RDB.Eval(context.Background(), reserveWalletTrustInflightScript, []string{key}, quota, limit, ttlSeconds).Int()
		if err != nil {
			common.SysLog(fmt.Sprintf("wallet trust bypass redis reserve failed (userId=%d, quota=%d): %s", userID, quota, err.Error()))
			return false
		}
		return result == 1
	}

	return reserveWalletTrustInflightLocal(userID, quota, limit)
}

func releaseWalletTrustInflight(userID int, quota int) {
	if userID <= 0 || quota <= 0 {
		return
	}
	if common.RedisEnabled && common.RDB != nil {
		key := walletTrustInflightKey(userID)
		ttlSeconds := int(walletTrustInflightTTL.Seconds())
		if err := common.RDB.Eval(context.Background(), releaseWalletTrustInflightScript, []string{key}, quota, ttlSeconds).Err(); err != nil {
			common.SysLog(fmt.Sprintf("wallet trust bypass redis release failed (userId=%d, quota=%d): %s", userID, quota, err.Error()))
		}
		return
	}

	releaseWalletTrustInflightLocal(userID, quota)
}

func walletTrustInflightKey(userID int) string {
	return fmt.Sprintf("wallet_trust_bypass_inflight:%d", userID)
}

func reserveWalletTrustInflightLocal(userID int, quota int, limit int) bool {
	walletTrustInflightMu.Lock()
	defer walletTrustInflightMu.Unlock()

	now := time.Now()
	entry := walletTrustInflight[userID]
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		entry = walletTrustInflightEntry{}
	}
	if entry.quota+quota > limit {
		return false
	}
	entry.quota += quota
	entry.expiresAt = now.Add(walletTrustInflightTTL)
	walletTrustInflight[userID] = entry
	return true
}

func releaseWalletTrustInflightLocal(userID int, quota int) {
	walletTrustInflightMu.Lock()
	defer walletTrustInflightMu.Unlock()

	entry := walletTrustInflight[userID]
	entry.quota -= quota
	if entry.quota <= 0 {
		delete(walletTrustInflight, userID)
		return
	}
	entry.expiresAt = time.Now().Add(walletTrustInflightTTL)
	walletTrustInflight[userID] = entry
}
