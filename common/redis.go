package common

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

var RDB *redis.Client
var RedisEnabled = true

func RedisKeyCacheSeconds() int {
	return SyncFrequency
}

// InitRedisClient This function is called after init()
//
// Supported REDIS_CONN_STRING forms:
//
//   - redis://[:pass@]host:port/db                 (single instance)
//   - rediss://[:pass@]host:port/db                (single instance + TLS)
//   - redis-sentinel://[:pass@]h1:p1,h2:p2,h3:p3/db?master=<name>[&sentinel_password=<pw>]
//     → Redis Sentinel HA. Pass is used to authenticate against the master/
//     replica (AUTH); sentinel_password is used to authenticate against the
//     Sentinel processes themselves if they run requirepass. The URL accepts
//     2+ sentinel addresses separated by commas in the host part.
//
// Example:
//
//	REDIS_CONN_STRING=redis-sentinel://:mypw@10.0.0.1:26379,10.0.0.2:26379,10.0.0.3:26379/0?master=mymaster
func InitRedisClient() (err error) {
	connStr := os.Getenv("REDIS_CONN_STRING")
	if connStr == "" {
		RedisEnabled = false
		SysLog("REDIS_CONN_STRING not set, Redis is not enabled")
		return nil
	}
	if os.Getenv("SYNC_FREQUENCY") == "" {
		SysLog("SYNC_FREQUENCY not set, use default value 60")
		SyncFrequency = 60
	}
	SysLog("Redis is enabled")

	poolSize := GetEnvOrDefault("REDIS_POOL_SIZE", 10)
	if strings.HasPrefix(connStr, "redis-sentinel://") {
		failoverOpt, err := parseSentinelURL(connStr)
		if err != nil {
			FatalLog("failed to parse Redis Sentinel connection string: " + err.Error())
		}
		failoverOpt.PoolSize = poolSize
		RDB = redis.NewFailoverClient(failoverOpt)
		if DebugEnabled {
			SysLog(fmt.Sprintf("Redis Sentinel: master=%s sentinels=%v db=%d",
				failoverOpt.MasterName, failoverOpt.SentinelAddrs, failoverOpt.DB))
		}
	} else {
		opt, err := redis.ParseURL(connStr)
		if err != nil {
			FatalLog("failed to parse Redis connection string: " + err.Error())
		}
		opt.PoolSize = poolSize
		RDB = redis.NewClient(opt)
		if DebugEnabled {
			SysLog(fmt.Sprintf("Redis connected to %s db=%d", opt.Addr, opt.DB))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = RDB.Ping(ctx).Result()
	if err != nil {
		FatalLog("Redis ping test failed: " + err.Error())
	}
	return err
}

// parseSentinelURL parses redis-sentinel:// URLs. The format intentionally
// mirrors redis:// so operators can mechanically convert (add the -sentinel
// suffix, list multiple host:port pairs separated by commas, add
// ?master=<name>).
//
// The master (AUTH) password comes from the URL userinfo; the Sentinel
// password (used to AUTH against Sentinel processes themselves) comes from
// the ?sentinel_password= query parameter or SENTINEL_PASSWORD env var.
func parseSentinelURL(raw string) (*redis.FailoverOptions, error) {
	// Strip the custom scheme so net/url can parse the rest; we'll put it
	// back as a pseudo 'redis://' for the built-in parser. Simpler to just
	// strip manually.
	trimmed := strings.TrimPrefix(raw, "redis-sentinel://")

	// Split userinfo (before @) from hostinfo+path+query (after @).
	var userinfo, rest string
	if at := strings.LastIndex(trimmed, "@"); at >= 0 {
		userinfo = trimmed[:at]
		rest = trimmed[at+1:]
	} else {
		rest = trimmed
	}

	// Split path/query from the host list. rest looks like:
	//   host1:26379,host2:26379,host3:26379/0?master=mymaster&foo=bar
	hosts := rest
	pathAndQuery := ""
	if slash := strings.Index(rest, "/"); slash >= 0 {
		hosts = rest[:slash]
		pathAndQuery = rest[slash:]
	}
	if hosts == "" {
		return nil, fmt.Errorf("no sentinel hosts in %q", raw)
	}

	// Parse each sentinel addr; be strict about host:port.
	addrs := strings.Split(hosts, ",")
	for i, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" || !strings.Contains(a, ":") {
			return nil, fmt.Errorf("invalid sentinel addr %q in %q", a, raw)
		}
		addrs[i] = a
	}

	opt := &redis.FailoverOptions{
		SentinelAddrs: addrs,
	}

	// Master password is the "password" part of userinfo. Username is
	// ignored (Sentinel protocol uses AUTH without username for the master
	// in traditional deployments).
	if userinfo != "" {
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			opt.Password = userinfo[colon+1:]
		} else {
			// Userinfo with no colon: treat whole thing as password for
			// compatibility with "redis-sentinel://secret@host:port/..."
			opt.Password = userinfo
		}
	}

	// Parse path ("/<db>") and query (?master=... &sentinel_password=...).
	if pathAndQuery != "" {
		parsed, err := url.Parse("http://dummy" + pathAndQuery)
		if err != nil {
			return nil, fmt.Errorf("parse path/query: %w", err)
		}
		db := strings.TrimPrefix(parsed.Path, "/")
		if db != "" {
			n, err := strconv.Atoi(db)
			if err != nil {
				return nil, fmt.Errorf("invalid db number %q", db)
			}
			opt.DB = n
		}
		q := parsed.Query()
		opt.MasterName = q.Get("master")
		if sp := q.Get("sentinel_password"); sp != "" {
			opt.SentinelPassword = sp
		}
	}

	// Allow env override for sentinel password (operators often dislike
	// putting it in the URL).
	if sp := os.Getenv("SENTINEL_PASSWORD"); sp != "" {
		opt.SentinelPassword = sp
	}

	if opt.MasterName == "" {
		return nil, fmt.Errorf("missing ?master=<name> in %q", raw)
	}
	return opt, nil
}

func RedisSet(key string, value string, expiration time.Duration) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis SET: key=%s, value=%s, expiration=%v", key, value, expiration))
	}
	ctx := context.Background()
	return RDB.Set(ctx, key, value, expiration).Err()
}

func RedisGet(key string) (string, error) {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis GET: key=%s", key))
	}
	ctx := context.Background()
	val, err := RDB.Get(ctx, key).Result()
	return val, err
}

//func RedisExpire(key string, expiration time.Duration) error {
//	ctx := context.Background()
//	return RDB.Expire(ctx, key, expiration).Err()
//}
//
//func RedisGetEx(key string, expiration time.Duration) (string, error) {
//	ctx := context.Background()
//	return RDB.GetSet(ctx, key, expiration).Result()
//}

func RedisDel(key string) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis DEL: key=%s", key))
	}
	ctx := context.Background()
	return RDB.Del(ctx, key).Err()
}

func RedisDelKey(key string) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis DEL Key: key=%s", key))
	}
	ctx := context.Background()
	return RDB.Del(ctx, key).Err()
}

func RedisHSetObj(key string, obj interface{}, expiration time.Duration) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis HSET: key=%s, obj=%+v, expiration=%v", key, obj, expiration))
	}
	ctx := context.Background()

	data := make(map[string]interface{})

	// 使用反射遍历结构体字段
	v := reflect.ValueOf(obj).Elem()
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		value := v.Field(i)

		// Skip DeletedAt field
		if field.Type.String() == "gorm.DeletedAt" {
			continue
		}

		// 处理指针类型
		if value.Kind() == reflect.Ptr {
			if value.IsNil() {
				data[field.Name] = ""
				continue
			}
			value = value.Elem()
		}

		// 处理布尔类型
		if value.Kind() == reflect.Bool {
			data[field.Name] = strconv.FormatBool(value.Bool())
			continue
		}

		// 其他类型直接转换为字符串
		data[field.Name] = fmt.Sprintf("%v", value.Interface())
	}

	txn := RDB.TxPipeline()
	txn.HSet(ctx, key, data)

	// 只有在 expiration 大于 0 时才设置过期时间
	if expiration > 0 {
		txn.Expire(ctx, key, expiration)
	}

	_, err := txn.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to execute transaction: %w", err)
	}
	return nil
}

func RedisHGetObj(key string, obj interface{}) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis HGETALL: key=%s", key))
	}
	ctx := context.Background()

	result, err := RDB.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to load hash from Redis: %w", err)
	}

	if len(result) == 0 {
		return fmt.Errorf("key %s not found in Redis", key)
	}

	// Handle both pointer and non-pointer values
	val := reflect.ValueOf(obj)
	if val.Kind() != reflect.Ptr {
		return fmt.Errorf("obj must be a pointer to a struct, got %T", obj)
	}

	v := val.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("obj must be a pointer to a struct, got pointer to %T", v.Interface())
	}

	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		fieldName := field.Name
		if value, ok := result[fieldName]; ok {
			fieldValue := v.Field(i)

			// Handle pointer types
			if fieldValue.Kind() == reflect.Ptr {
				if value == "" {
					continue
				}
				if fieldValue.IsNil() {
					fieldValue.Set(reflect.New(fieldValue.Type().Elem()))
				}
				fieldValue = fieldValue.Elem()
			}

			// Enhanced type handling for Token struct
			switch fieldValue.Kind() {
			case reflect.String:
				fieldValue.SetString(value)
			case reflect.Int, reflect.Int64:
				intValue, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return fmt.Errorf("failed to parse int field %s: %w", fieldName, err)
				}
				fieldValue.SetInt(intValue)
			case reflect.Bool:
				boolValue, err := strconv.ParseBool(value)
				if err != nil {
					return fmt.Errorf("failed to parse bool field %s: %w", fieldName, err)
				}
				fieldValue.SetBool(boolValue)
			case reflect.Struct:
				// Special handling for gorm.DeletedAt
				if fieldValue.Type().String() == "gorm.DeletedAt" {
					if value != "" {
						timeValue, err := time.Parse(time.RFC3339, value)
						if err != nil {
							return fmt.Errorf("failed to parse DeletedAt field %s: %w", fieldName, err)
						}
						fieldValue.Set(reflect.ValueOf(gorm.DeletedAt{Time: timeValue, Valid: true}))
					}
				}
			default:
				return fmt.Errorf("unsupported field type: %s for field %s", fieldValue.Kind(), fieldName)
			}
		}
	}

	return nil
}

// RedisIncr Add this function to handle atomic increments
func RedisIncr(key string, delta int64) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis INCR: key=%s, delta=%d", key, delta))
	}
	// 检查键的剩余生存时间
	ttlCmd := RDB.TTL(context.Background(), key)
	ttl, err := ttlCmd.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("failed to get TTL: %w", err)
	}

	// 只有在 key 存在且有 TTL 时才需要特殊处理
	if ttl > 0 {
		ctx := context.Background()
		// 开始一个Redis事务
		txn := RDB.TxPipeline()

		// 减少余额
		decrCmd := txn.IncrBy(ctx, key, delta)
		if err := decrCmd.Err(); err != nil {
			return err // 如果减少失败，则直接返回错误
		}

		// 重新设置过期时间，使用原来的过期时间
		txn.Expire(ctx, key, ttl)

		// 执行事务
		_, err = txn.Exec(ctx)
		return err
	}
	return nil
}

func RedisHIncrBy(key, field string, delta int64) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis HINCRBY: key=%s, field=%s, delta=%d", key, field, delta))
	}
	ttlCmd := RDB.TTL(context.Background(), key)
	ttl, err := ttlCmd.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("failed to get TTL: %w", err)
	}

	if ttl > 0 {
		ctx := context.Background()
		txn := RDB.TxPipeline()

		incrCmd := txn.HIncrBy(ctx, key, field, delta)
		if err := incrCmd.Err(); err != nil {
			return err
		}

		txn.Expire(ctx, key, ttl)

		_, err = txn.Exec(ctx)
		return err
	}
	return nil
}

func RedisHSetField(key, field string, value interface{}) error {
	if DebugEnabled {
		SysLog(fmt.Sprintf("Redis HSET field: key=%s, field=%s, value=%v", key, field, value))
	}
	ttlCmd := RDB.TTL(context.Background(), key)
	ttl, err := ttlCmd.Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("failed to get TTL: %w", err)
	}

	if ttl > 0 {
		ctx := context.Background()
		txn := RDB.TxPipeline()

		hsetCmd := txn.HSet(ctx, key, field, value)
		if err := hsetCmd.Err(); err != nil {
			return err
		}

		txn.Expire(ctx, key, ttl)

		_, err = txn.Exec(ctx)
		return err
	}
	return nil
}
