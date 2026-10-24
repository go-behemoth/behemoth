package crypto

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/sync/semaphore"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
)

// Params holds the Argon2id configuration.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams provides reasonable interactive defaults
// (≈64 MiB, 3 iterations, parallelism 4).
var DefaultParams = &Params{
	Memory:      64 * 1024,
	Iterations:  3,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// PasswordLimit bounds the password hashes that run at once in one process.
// An Argon2id hash holds Params.Memory for as long as it runs, 64 MiB with
// DefaultParams, so without a bound a burst of sign-ins can exhaust the
// process's memory before a rate limit refuses a request.
//
// A Hash or Verify call over the bound waits for a running hash to finish.
// A call that waits longer than MaxWait fails with an error of category
// errs.CategoryUnavailable and code errs.CodeServerBusy, whose RetryAfter is
// MaxWait. Over HTTP it is a 503 with a Retry-After header. A call whose
// context ends while it waits returns the context's error. A hash that has
// started runs to the end.
//
// The bound holds per process: each replica of a deployment has its own.
type PasswordLimit struct {
	// MaxConcurrent is how many hashes made with the hasher's Params may run
	// at once. Zero means runtime.GOMAXPROCS(0) when the hasher is built.
	// Verifying a stored hash that was made with more memory than
	// Params.Memory counts for more than one hash, so the memory the running
	// hashes hold stays within MaxConcurrent × Params.Memory.
	MaxConcurrent int

	// MaxWait is how long a call waits for its turn. Zero means two seconds.
	MaxWait time.Duration
}

const defaultPasswordMaxWait = 2 * time.Second

// withDefaults returns l with its zero fields filled in. A negative field is
// a configuration error, reported when the hasher is built and not at the
// first sign-in.
func (l PasswordLimit) withDefaults() (PasswordLimit, error) {
	if l.MaxConcurrent < 0 || l.MaxWait < 0 {
		return l, errs.NewConfigurationError("crypto.PasswordLimit",
			fmt.Sprintf("the password limit is invalid: MaxConcurrent %d, MaxWait %s", l.MaxConcurrent, l.MaxWait), nil)
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = runtime.GOMAXPROCS(0)
	}
	if l.MaxWait == 0 {
		l.MaxWait = defaultPasswordMaxWait
	}
	return l, nil
}

// Argon2idHasher implements PasswordHasher using Argon2id + PHC string format.
// It bounds the hashes that run at once as its PasswordLimit says, and
// traces each Hash and Verify under the caller's context.
type Argon2idHasher struct {
	params *Params
	km     KeyManager
	tel    *telemetry.Telemetry

	// room is the memory, in KiB, that running hashes may hold together:
	// MaxConcurrent × Params.Memory. A hash takes the memory it is made
	// with, which for Verify is the stored hash's, so the bound holds while
	// hashes made with older params remain in the database. Nothing rehashes
	// them at sign-in yet. See ADR-0010.
	room     *semaphore.Weighted
	roomSize int64
	maxWait  time.Duration
}

// NewArgon2idHasher returns a hasher that uses the supplied parameters and
// bounds its concurrent hashes with limit. If params is nil, DefaultParams
// is used. tel may be nil. A negative field of limit is a configuration
// error.
func NewArgon2idHasher(params *Params, limit PasswordLimit, km KeyManager, tel *telemetry.Telemetry) (*Argon2idHasher, error) {
	if params == nil {
		params = DefaultParams
	}
	limit, err := limit.withDefaults()
	if err != nil {
		return nil, err
	}
	size := int64(limit.MaxConcurrent) * max(int64(params.Memory), 1)
	return &Argon2idHasher{
		params:   params,
		km:       km,
		tel:      telemetry.OrDefault(tel),
		room:     semaphore.NewWeighted(size),
		roomSize: size,
		maxWait:  limit.MaxWait,
	}, nil
}

// Hash produces a PHC-formatted Argon2id hash of the password. It waits for
// room as the hasher's PasswordLimit says.
func (h *Argon2idHasher) Hash(ctx context.Context, password string) (hash string, err error) {
	ctx, span := h.tel.StartSpan(ctx, telemetry.SpanPasswordHash, nil)
	defer func() { telemetry.FinishSpan(span, err) }()

	pepper, version, err := h.km.Current(KeyPurposePasswordHash)
	if err != nil {
		return "", fmt.Errorf("get current pepper: %w", err)
	}

	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	// Derive a peppered password (HMAC-SHA256) before Argon2.
	peppered := hmacSHA256(pepper, []byte(password))

	key, err := h.deriveKey(ctx, "Argon2idHasher.Hash", peppered, salt, h.params)
	if err != nil {
		return "", err
	}

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Key := base64.RawStdEncoding.EncodeToString(key)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d,kv=%d$%s$%s",
		argon2.Version,
		h.params.Memory,
		h.params.Iterations,
		h.params.Parallelism,
		version,
		b64Salt,
		b64Key,
	), nil
}

// Verify performs a constant-time comparison of the password against the
// stored PHC hash. It returns false for any malformed hash or mismatch. It
// waits for room as the hasher's PasswordLimit says, for the memory the
// stored hash was made with.
func (h *Argon2idHasher) Verify(ctx context.Context, hash, password string) (ok bool, err error) {
	ctx, span := h.tel.StartSpan(ctx, telemetry.SpanPasswordVerify, nil)
	defer func() { telemetry.FinishSpan(span, err) }()

	params, keyVersion, salt, key, err := decodeHash(hash)
	if err != nil {
		return false, err
	}

	pepper, err := h.km.GetByVersion(KeyPurposePasswordHash, keyVersion)
	if err != nil {
		return false, err
	}

	peppered := hmacSHA256(pepper, []byte(password))

	otherKey, err := h.deriveKey(ctx, "Argon2idHasher.Verify", peppered, salt, params)
	if err != nil {
		return false, err
	}

	return subtle.ConstantTimeCompare(key, otherKey) == 1, nil
}

// deriveKey runs Argon2id with params once there is room for it. The room
// is given back by a defer, so a panic in argon2 does not keep it.
func (h *Argon2idHasher) deriveKey(ctx context.Context, op string, password, salt []byte, params *Params) ([]byte, error) {
	release, err := h.acquire(ctx, op, params.Memory)
	if err != nil {
		return nil, err
	}
	defer release()
	return argon2.IDKey(password, salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength), nil
}

// acquire waits for room for a hash of memory KiB, for at most maxWait and
// while ctx lasts, and returns the function that gives the room back. The
// semaphore serves waiters in order, so a large hash is not overtaken by
// small ones.
func (h *Argon2idHasher) acquire(ctx context.Context, op string, memory uint32) (release func(), err error) {
	// A stored hash that needs more than the whole room takes all of it: a
	// larger request would wait until its deadline every time.
	n := min(max(int64(memory), 1), h.roomSize)
	release = func() { h.room.Release(n) }
	if h.room.TryAcquire(n) {
		h.observeWait(ctx, 0)
		return release, nil
	}

	start := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, h.maxWait)
	defer cancel()
	err = h.room.Acquire(waitCtx, n)
	h.observeWait(ctx, time.Since(start))
	switch {
	case err == nil:
		return release, nil
	case ctx.Err() != nil:
		// The caller gave up, or its deadline passed: nobody waits for a
		// busy answer.
		return nil, fmt.Errorf("%s: wait for room to hash: %w", op, ctx.Err())
	default:
		if h.tel.MetricsEnabled() {
			h.tel.Count(ctx, telemetry.MetricPasswordBusy, nil)
		}
		return nil, errs.NewUnavailable(op, errs.CodeServerBusy, "the server is busy, please try again later", h.maxWait)
	}
}

func (h *Argon2idHasher) observeWait(ctx context.Context, waited time.Duration) {
	if h.tel.MetricsEnabled() {
		h.tel.Metrics.Histogram(ctx, telemetry.MetricPasswordWait, waited.Seconds(), nil)
	}
}

// NeedsRehash reports whether the stored hash used weaker Argon2 parameters
// or an older pepper version than the current configuration.
func (h *Argon2idHasher) NeedsRehash(hash string) bool {
	params, keyVersion, _, _, err := decodeHash(hash)
	if err != nil {
		return true
	}

	_, currentVersion, err := h.km.Current(KeyPurposePasswordHash)
	if err != nil {
		return true
	}

	return keyVersion < currentVersion ||
		params.Memory < h.params.Memory ||
		params.Iterations < h.params.Iterations ||
		params.Parallelism < h.params.Parallelism ||
		params.KeyLength < h.params.KeyLength
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func decodeHash(encoded string) (params *Params, keyVersion int, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return nil, 0, nil, nil, errors.New("invalid hash format")
	}
	if parts[1] != "argon2id" {
		return nil, 0, nil, nil, errors.New("unsupported algorithm")
	}

	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, 0, nil, nil, err
	}
	if version != argon2.Version {
		return nil, 0, nil, nil, errors.New("incompatible argon2 version")
	}

	params = &Params{}
	// Accept both old format (no kv) and new format (with kv=).
	n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d,kv=%d",
		&params.Memory, &params.Iterations, &params.Parallelism, &keyVersion)
	if err != nil || n != 4 {
		// fallback for hashes created without a key version
		if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
			&params.Memory, &params.Iterations, &params.Parallelism); err != nil {
			return nil, 0, nil, nil, err
		}
		keyVersion = 0 // treat as unversioned / legacy
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, 0, nil, nil, err
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, 0, nil, nil, err
	}

	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(key))
	return params, keyVersion, salt, key, nil
}
