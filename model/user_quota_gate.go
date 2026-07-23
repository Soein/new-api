package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
)

const (
	defaultUserQuotaLockLease        = 2 * time.Minute
	defaultUserQuotaLockWaitTimeout  = time.Minute
	defaultUserQuotaLockRetryMinimum = 5 * time.Millisecond
	defaultUserQuotaLockRetryMaximum = 50 * time.Millisecond
	userQuotaRedisOperationTimeout   = time.Second
)

const releaseUserQuotaLockScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`

type userQuotaMutationGateConfig struct {
	lease        time.Duration
	waitTimeout  time.Duration
	retryMinimum time.Duration
	retryMaximum time.Duration
}

func (c userQuotaMutationGateConfig) normalized() userQuotaMutationGateConfig {
	if c.lease <= 0 {
		c.lease = defaultUserQuotaLockLease
	}
	if c.waitTimeout <= 0 {
		c.waitTimeout = defaultUserQuotaLockWaitTimeout
	}
	if c.retryMinimum <= 0 {
		c.retryMinimum = defaultUserQuotaLockRetryMinimum
	}
	if c.retryMaximum < c.retryMinimum {
		c.retryMaximum = defaultUserQuotaLockRetryMaximum
	}
	return c
}

type userQuotaLocalLock struct {
	mu   sync.Mutex
	refs int
}

type userQuotaMutationGate struct {
	redisClient func() *redis.Client
	config      func() userQuotaMutationGateConfig

	localMu sync.Mutex
	local   map[int]*userQuotaLocalLock

	lastFallbackLogUnix atomic.Int64
}

func newUserQuotaMutationGate(client *redis.Client, config userQuotaMutationGateConfig) *userQuotaMutationGate {
	config = config.normalized()
	return &userQuotaMutationGate{
		redisClient: func() *redis.Client { return client },
		config:      func() userQuotaMutationGateConfig { return config },
		local:       make(map[int]*userQuotaLocalLock),
	}
}

var defaultUserQuotaMutationGate = &userQuotaMutationGate{
	redisClient: func() *redis.Client {
		if !common.UserQuotaRedisLockEnabled || !common.RedisEnabled {
			return nil
		}
		return common.RDB
	},
	config: func() userQuotaMutationGateConfig {
		return userQuotaMutationGateConfig{
			lease:       common.UserQuotaLockLease,
			waitTimeout: common.UserQuotaLockWaitTimeout,
		}.normalized()
	},
	local: make(map[int]*userQuotaLocalLock),
}

// withUserQuotaMutation moves same-user waiting out of PostgreSQL. Every
// process first collapses its local contenders to one goroutine; when Redis is
// available, the remaining contender also coordinates with the other nodes.
// PostgreSQL remains authoritative and still provides the final atomic balance
// check. Redis failure therefore degrades to at most one writer per node
// instead of allowing every request to occupy a database connection.
func withUserQuotaMutation(userID int, mutate func() error) error {
	return defaultUserQuotaMutationGate.Do(context.Background(), userID, mutate)
}

func (g *userQuotaMutationGate) Do(ctx context.Context, userID int, mutate func() error) error {
	if userID <= 0 {
		return fmt.Errorf("invalid user id: %d", userID)
	}
	if mutate == nil {
		return errors.New("user quota mutation is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	releaseLocal := g.acquireLocal(userID)
	defer releaseLocal()

	client := g.redisClient()
	if client == nil {
		return mutate()
	}

	releaseDistributed, err := g.acquireDistributed(ctx, client, userID, g.config().normalized())
	if err != nil {
		g.logFallback(userID, err)
		return mutate()
	}
	defer releaseDistributed()
	return mutate()
}

func (g *userQuotaMutationGate) acquireLocal(userID int) func() {
	g.localMu.Lock()
	entry := g.local[userID]
	if entry == nil {
		entry = &userQuotaLocalLock{}
		g.local[userID] = entry
	}
	entry.refs++
	g.localMu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		g.localMu.Lock()
		entry.refs--
		if entry.refs == 0 && g.local[userID] == entry {
			delete(g.local, userID)
		}
		g.localMu.Unlock()
	}
}

func (g *userQuotaMutationGate) acquireDistributed(
	ctx context.Context,
	client *redis.Client,
	userID int,
	config userQuotaMutationGateConfig,
) (func(), error) {
	key := fmt.Sprintf("user_quota_mutation_lock:%d", userID)
	token := common.GetUUID()
	deadline := time.Now().Add(config.waitTimeout)
	retryDelay := config.retryMinimum

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timed out waiting for user quota lock after %s", config.waitTimeout)
		}

		opTimeout := minDuration(userQuotaRedisOperationTimeout, remaining)
		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		acquired, err := client.SetNX(opCtx, key, token, config.lease).Result()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("acquire Redis quota lock: %w", err)
		}
		if acquired {
			var once sync.Once
			return func() {
				once.Do(func() {
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), userQuotaRedisOperationTimeout)
					defer releaseCancel()
					if err := client.Eval(releaseCtx, releaseUserQuotaLockScript, []string{key}, token).Err(); err != nil {
						g.logFallback(userID, fmt.Errorf("release Redis quota lock: %w", err))
					}
				})
			}, nil
		}

		wait := minDuration(retryDelay, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
		if retryDelay < config.retryMaximum {
			retryDelay = minDuration(retryDelay*2, config.retryMaximum)
		}
	}
}

func (g *userQuotaMutationGate) logFallback(userID int, err error) {
	now := time.Now().Unix()
	last := g.lastFallbackLogUnix.Load()
	if now-last < 60 || !g.lastFallbackLogUnix.CompareAndSwap(last, now) {
		return
	}
	common.SysLog(fmt.Sprintf(
		"user quota Redis coordination issue; local one-writer-per-node gate remains active (userId=%d): %s",
		userID,
		err.Error(),
	))
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
