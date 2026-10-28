package models

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/errs"
	redisAdapter "github.com/go-behemoth/behemoth/storage/adapters/redis"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Boot finds the Redis adapter's counter through this interface, and the
// session manager its versioned values through the second.
var (
	_ core.AtomicIncrementer = (*redisAdapter.RedisAdapter)(nil)
	_ core.VersionedCache    = (*redisAdapter.RedisAdapter)(nil)
)

func TestRedisStorage(t *testing.T) {
	ctx := context.Background()
	client, cleanup := testutils.SetupRedisClient(t, ctx)
	defer cleanup()

	kvAdapter := redisAdapter.NewRedisAdapter(client)
	suite := NewKeyValueStorageTestSuite(kvAdapter)
	suite.RunAllTests(t)
}

// The Redis adapter counts for the rate limiter: concurrent attempts each get
// their own count, the window's end is the key's expiry, and the count starts
// again when Redis has dropped the key.
func TestRedisIncrement(t *testing.T) {
	ctx := context.Background()
	client, cleanup := testutils.SetupRedisClient(t, ctx)
	defer cleanup()
	kv := redisAdapter.NewRedisAdapter(client)

	const calls = 20
	start := time.Now()
	counts := make(chan int64, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			n, resetAt, err := kv.Increment(ctx, "signin:1.2.3.4", time.Minute)
			assert.NoError(t, err)
			assert.WithinDuration(t, start.Add(time.Minute), resetAt, 5*time.Second, "the end of the window the first attempt opened")
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	seen := map[int64]bool{}
	for n := range counts {
		seen[n] = true
	}
	assert.Len(t, seen, calls, "concurrent attempts each get their own count")
	assert.True(t, seen[1] && seen[calls], "counts run 1..%d: %v", calls, seen)

	other, _, err := kv.Increment(ctx, "signin:5.6.7.8", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 1, other, "keys count independently")

	// The counter lives under its own prefix, with an expiry from the start.
	ttl, err := client.PTTL(ctx, "ratelimit:signin:1.2.3.4").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 50*time.Second)
	assert.LessOrEqual(t, ttl, time.Minute)
	_, err = kv.Get(ctx, "signin:1.2.3.4")
	assert.Error(t, err, "the plain key is not used")

	// A short window: the second attempt reports the time left, not a new
	// window, and the count starts again once the key has expired.
	n, first, err := kv.Increment(ctx, "short", 300*time.Millisecond)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	time.Sleep(100 * time.Millisecond)
	n, second, err := kv.Increment(ctx, "short", 300*time.Millisecond)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	assert.WithinDuration(t, first, second, 50*time.Millisecond, "the window's end does not move with later attempts")
	time.Sleep(350 * time.Millisecond)
	n, _, err = kv.Increment(ctx, "short", 300*time.Millisecond)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "a passed window starts again")

	// A counter that somehow has no expiry is given one, or it would never reset.
	require.NoError(t, client.Set(ctx, "ratelimit:stuck", "7", 0).Err())
	n, resetAt, err := kv.Increment(ctx, "stuck", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 8, n)
	assert.WithinDuration(t, time.Now().Add(time.Minute), resetAt, 5*time.Second)

	_, _, err = kv.Increment(ctx, "", time.Minute)
	assert.Error(t, err, "an empty key")
	_, _, err = kv.Increment(ctx, "k", 0)
	assert.Error(t, err, "a window of no length")
}

// The Redis adapter keeps versioned values for the session cache. A write of
// an older or the same version is refused, whatever order the writes arrive
// in, a value the plain Set wrote is replaced, and a value expires with its
// lifetime.
func TestRedisVersionedCache(t *testing.T) {
	ctx := context.Background()
	client, cleanup := testutils.SetupRedisClient(t, ctx)
	defer cleanup()
	kv := redisAdapter.NewRedisAdapter(client)

	set := func(key, value string, version int64) bool {
		t.Helper()
		wrote, err := kv.SetIfNewer(ctx, key, value, version, time.Minute)
		require.NoError(t, err)
		return wrote
	}
	get := func(key string) (string, int64) {
		t.Helper()
		value, version, err := kv.GetVersioned(ctx, key)
		require.NoError(t, err)
		return value, version
	}

	assert.True(t, set("session:a", "second", 2), "an absent key")
	assert.False(t, set("session:a", "first", 1), "an older version")
	assert.False(t, set("session:a", "again", 2), "the same version")
	value, version := get("session:a")
	assert.Equal(t, "second", value)
	assert.EqualValues(t, 2, version)
	assert.True(t, set("session:a", "third", 3), "a later version")
	value, version = get("session:a")
	assert.Equal(t, "third", value)
	assert.EqualValues(t, 3, version)
	ttl, err := client.PTTL(ctx, "session:a").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 50*time.Second)
	assert.LessOrEqual(t, ttl, time.Minute)

	// The largest version, a deleted session's: nothing replaces it, and
	// it reads back exactly.
	assert.True(t, set("session:gone", "gone", math.MaxInt64))
	assert.False(t, set("session:gone", "copy", 1<<53))
	value, version = get("session:gone")
	assert.Equal(t, "gone", value)
	assert.EqualValues(t, int64(math.MaxInt64), version)

	// Of concurrent writes, the latest version stays.
	const writers = 20
	var wg sync.WaitGroup
	for v := range writers {
		wg.Go(func() {
			_, err := kv.SetIfNewer(ctx, "session:c", fmt.Sprint(v+1), int64(v+1), time.Minute)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	value, version = get("session:c")
	assert.Equal(t, fmt.Sprint(writers), value)
	assert.EqualValues(t, writers, version)

	// A value the plain Set wrote has no version, and is replaced.
	require.NoError(t, kv.Set(ctx, "session:plain", "unversioned", 60))
	_, _, err = kv.GetVersioned(ctx, "session:plain")
	assert.Error(t, err)
	assert.True(t, set("session:plain", "versioned", 1))
	value, version = get("session:plain")
	assert.Equal(t, "versioned", value)
	assert.EqualValues(t, 1, version)

	// A value lives for its lifetime, and then the key is absent.
	_, err = kv.SetIfNewer(ctx, "session:short", "brief", 1, 200*time.Millisecond)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	_, _, err = kv.GetVersioned(ctx, "session:short")
	assert.True(t, errs.IsCategory(err, errs.CategoryKeyNotFound), "an expired key: %v", err)
	_, _, err = kv.GetVersioned(ctx, "session:missing")
	assert.True(t, errs.IsCategory(err, errs.CategoryKeyNotFound), "a key never written: %v", err)

	_, err = kv.SetIfNewer(ctx, "", "v", 1, time.Minute)
	assert.Error(t, err, "an empty key")
	_, err = kv.SetIfNewer(ctx, "k", "v", 1, 0)
	assert.Error(t, err, "a lifetime of no length")
}
