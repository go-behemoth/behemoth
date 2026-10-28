// Package redis provides RedisAdapter, a behemoth.KeyValueStorage backed by
// Redis. It lives in its own module, like the database adapters, so the core
// module does not depend on go-redis.
package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/storage/adapters"
	"github.com/redis/go-redis/v9"
)

// RedisAdapter implements behemoth.KeyValueStorage on a Redis client.
//
// It also counts for the rate limiter (Increment, the
// core.AtomicIncrementer capability), so with Redis configured rate-limit
// counters are kept there and not in the database. And it keeps versioned
// values (SetIfNewer and GetVersioned, the core.VersionedCache capability),
// which the session manager caches sessions with.
type RedisAdapter struct {
	redisClient *redis.Client
}

// NewRedisAdapter wraps client.
func NewRedisAdapter(client *redis.Client) *RedisAdapter {
	return &RedisAdapter{redisClient: client}
}

func (rkv *RedisAdapter) Get(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", errs.NewEmptyKey("Get", nil)
	}

	value, err := rkv.redisClient.Get(ctx, key).Result()
	if err != nil {
		return "", adapters.WrapWithCaller(err, EntityKVPair, handleRedisError)
	}
	return value, nil
}

func (rkv *RedisAdapter) Set(
	ctx context.Context,
	key string,
	value string,
	ttl int,
) error {
	if key == "" {
		return errs.NewEmptyKey(adapters.OpSet, nil)
	}
	err := rkv.redisClient.Set(ctx, key, value, time.Duration(ttl*int(time.Second))).Err()

	return err
}

func (rkv *RedisAdapter) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errs.NewEmptyKey(adapters.OpDelete, nil)
	}

	err := rkv.redisClient.Del(ctx, key).Err()
	if err != nil {
		return err
	}

	return nil
}

// rateLimitPrefix keeps counters apart from the other keys behemoth stores
// (session cache entries, tokens).
const rateLimitPrefix = "ratelimit:"

// incrementScript counts one attempt and returns the count and the key's
// remaining lifetime in milliseconds. It runs as one script so the expiry is
// set with the first increment: INCR followed by a separate EXPIRE would
// leave a counter that never resets if the process stopped in between. A
// counter found without an expiry is given one for the same reason.
var incrementScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if count == 1 or ttl < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
	ttl = tonumber(ARGV[1])
end
return {count, ttl}
`)

// Increment implements the rate limiter's counter (core.AtomicIncrementer):
// it counts one attempt against key and returns the count in the current
// window and when the window ends. The first attempt opens a window of ttl,
// and Redis removes the key when it has passed.
//
// The end of the window is this process's clock plus the key's remaining
// lifetime, so it does not depend on the clocks of Redis and the application
// agreeing.
func (rkv *RedisAdapter) Increment(ctx context.Context, key string, ttl time.Duration) (int64, time.Time, error) {
	if key == "" {
		return 0, time.Time{}, errs.NewEmptyKey("Increment", nil)
	}
	if ttl < time.Millisecond {
		return 0, time.Time{}, errs.NewValidationError("Increment", EntityKVPair, fmt.Errorf("the window must be at least a millisecond, got %s", ttl))
	}
	res, err := incrementScript.Run(ctx, rkv.redisClient, []string{rateLimitPrefix + key}, ttl.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, time.Time{}, adapters.WrapWithCaller(err, EntityKVPair, handleRedisError)
	}
	if len(res) != 2 {
		return 0, time.Time{}, fmt.Errorf("redis: the increment script returned %d values, want 2", len(res))
	}
	return res[0], time.Now().Add(time.Duration(res[1]) * time.Millisecond), nil
}

// versionedSetScript writes a value with its version unless the key holds
// one of the same or a later version. The key is a hash of the two fields,
// so that the version is compared and written in one step, and its expiry
// is set in the same step. A key of another type holds a value the plain Set
// wrote, which has no version and is replaced. Lua compares the versions as
// numbers with 53 bits of precision, far more than a row is ever updated.
var versionedSetScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[1]).ok
if kind == 'hash' then
	local stored = tonumber(redis.call('HGET', KEYS[1], 'version'))
	if stored and stored >= tonumber(ARGV[2]) then
		return 0
	end
elseif kind ~= 'none' then
	redis.call('DEL', KEYS[1])
end
redis.call('HSET', KEYS[1], 'version', ARGV[2], 'value', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return 1
`)

// SetIfNewer implements core.VersionedCache: it writes value under key with
// version, unless key holds a value of the same or a later version, and
// reports whether it wrote. The value lives for ttl.
func (rkv *RedisAdapter) SetIfNewer(ctx context.Context, key, value string, version int64, ttl time.Duration) (bool, error) {
	const op = "SetIfNewer"
	if key == "" {
		return false, errs.NewEmptyKey(op, nil)
	}
	if ttl < time.Millisecond {
		return false, errs.NewValidationError(op, EntityKVPair, fmt.Errorf("the lifetime must be at least a millisecond, got %s", ttl))
	}
	wrote, err := versionedSetScript.Run(ctx, rkv.redisClient, []string{key}, value, version, ttl.Milliseconds()).Int()
	if err != nil {
		return false, adapters.WrapWithCaller(err, EntityKVPair, handleRedisError)
	}
	return wrote == 1, nil
}

// GetVersioned implements core.VersionedCache: it returns the value
// SetIfNewer wrote under key, and its version.
func (rkv *RedisAdapter) GetVersioned(ctx context.Context, key string) (string, int64, error) {
	const op = "GetVersioned"
	if key == "" {
		return "", 0, errs.NewEmptyKey(op, nil)
	}
	fields, err := rkv.redisClient.HMGet(ctx, key, "version", "value").Result()
	if err != nil {
		return "", 0, adapters.WrapWithCaller(err, EntityKVPair, handleRedisError)
	}
	stored, hasVersion := fields[0].(string)
	value, hasValue := fields[1].(string)
	if !hasVersion || !hasValue {
		return "", 0, errs.NewKeyNotFound(op, nil)
	}
	version, err := strconv.ParseInt(stored, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("redis: the version under %q is not a number: %w", key, err)
	}
	return value, version, nil
}

// EntityKVPair is the entity name reported in errors about a key.
const EntityKVPair string = "Key-Value Pair"

func handleRedisError(op, entity string, err error) error {
	if err == nil {
		return nil
	}

	switch err {
	case redis.Nil:
		return errs.NewKeyNotFound(op, err)
		// case redis.

		// case errs.ErrEmptyKey:
		// 	return errs.NewEmptyKey(op, err)
	}
	return errs.NewDatabaseError(op, err)
}
