package crypto

import (
	"context"
	"encoding/hex"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
)

// cheapParams keep the tests of the limit fast: 1 MiB, one pass, one lane.
var cheapParams = &Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func newPasswordKeys(t *testing.T) KeyManager {
	t.Helper()
	km, err := NewDefaultKeyManager(t.Context(), KeyManagerConfig{}, NewSecretSource(nil), tel)
	require.NoError(t, err)
	return km
}

func newHasher(t *testing.T, km KeyManager, params *Params, limit PasswordLimit, tel *telemetry.Telemetry) *Argon2idHasher {
	t.Helper()
	h, err := NewArgon2idHasher(params, limit, km, tel)
	require.NoError(t, err)
	return h
}

func TestArgon2idHasher_HashAndVerify(t *testing.T) {
	ctx := t.Context()
	h := newHasher(t, newPasswordKeys(t), nil, PasswordLimit{}, nil) // uses DefaultParams

	password := "correct horse battery staple"
	hash, err := h.Hash(ctx, password)
	assert.NoError(t, err)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$"))

	ok, err := h.Verify(ctx, hash, password)
	assert.NoError(t, err)
	assert.True(t, ok)

	ok, err = h.Verify(ctx, hash, "wrong password")
	assert.NoError(t, err)
	assert.False(t, ok)

	ok, err = h.Verify(ctx, "not-a-hash", password)
	assert.Error(t, err)
	assert.False(t, ok)
}

func TestArgon2idHasher_NeedsRehash(t *testing.T) {
	km := newPasswordKeys(t)
	strong := newHasher(t, km, &Params{
		Memory:      128 * 1024,
		Iterations:  4,
		Parallelism: 4,
		SaltLength:  16,
		KeyLength:   32,
	}, PasswordLimit{}, nil)
	weak := newHasher(t, km, &Params{
		Memory:      32 * 1024,
		Iterations:  1,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}, PasswordLimit{}, nil)

	hash, err := weak.Hash(t.Context(), "secret")
	assert.NoError(t, err)

	assert.True(t, strong.NeedsRehash(hash), "weaker params should need rehash")
	assert.False(t, weak.NeedsRehash(hash), "same params should not need rehash")
}

func TestArgon2idHasher_DifferentSalts(t *testing.T) {
	h := newHasher(t, newPasswordKeys(t), nil, PasswordLimit{}, nil)
	hash1, err := h.Hash(t.Context(), "password")
	assert.NoError(t, err)
	hash2, err := h.Hash(t.Context(), "password")
	assert.NoError(t, err)

	assert.NotEqual(t, hash1, hash2, "each hash must use a unique salt")
}

// The hasher's spans are children of the span on the caller's context.
func TestArgon2idHasher_TracesUnderTheCallersContext(t *testing.T) {
	tel, rec := telemetrytest.New()
	h := newHasher(t, newPasswordKeys(t), cheapParams, PasswordLimit{}, tel)
	ctx, parent := tel.StartSpan(t.Context(), "caller", nil)

	hash, err := h.Hash(ctx, "password")
	require.NoError(t, err)
	ok, err := h.Verify(ctx, hash, "wrong password")
	require.NoError(t, err)
	require.False(t, ok)

	for _, name := range []string{telemetry.SpanPasswordHash, telemetry.SpanPasswordVerify} {
		spans := rec.Tracer.Named(name)
		require.Len(t, spans, 1, name)
		assert.Same(t, parent, spans[0].Parent, name)
		assert.NoError(t, spans[0].Err(), "%s: a wrong password is not a failed span", name)
		assert.Equal(t, 1, spans[0].Ended(), name)
	}
}

// A zero limit means GOMAXPROCS hashes and a two-second wait. A negative
// field is a configuration error, from the constructor and from New.
func TestPasswordLimit_DefaultsAndValidation(t *testing.T) {
	km := newPasswordKeys(t)
	h := newHasher(t, km, cheapParams, PasswordLimit{}, nil)
	assert.Equal(t, int64(runtime.GOMAXPROCS(0))*int64(cheapParams.Memory), h.roomSize)
	assert.Equal(t, 2*time.Second, h.maxWait)

	h = newHasher(t, km, cheapParams, PasswordLimit{MaxConcurrent: 3, MaxWait: time.Second}, nil)
	assert.Equal(t, 3*int64(cheapParams.Memory), h.roomSize)
	assert.Equal(t, time.Second, h.maxWait)

	for _, limit := range []PasswordLimit{{MaxConcurrent: -1}, {MaxWait: -time.Second}} {
		_, err := NewArgon2idHasher(cheapParams, limit, km, nil)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%+v: %v", limit, err)

		_, err = New(t.Context(), Config{Secrets: NewSecretSource(nil), PasswordLimit: limit}, nil)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%+v: %v", limit, err)
	}
}

// A call that finds no room waits MaxWait, then is refused with a busy
// error that a transport answers with 503 and Retry-After. The refusal is
// counted, and is not a failed span.
func TestArgon2idHasher_RefusesWhenBusy(t *testing.T) {
	ctx := t.Context()
	tel, rec := telemetrytest.New()
	km := newPasswordKeys(t)
	h := newHasher(t, km, cheapParams, PasswordLimit{MaxConcurrent: 1, MaxWait: 20 * time.Millisecond}, tel)
	stored, err := newHasher(t, km, cheapParams, PasswordLimit{}, nil).Hash(ctx, "password")
	require.NoError(t, err)
	release, err := h.acquire(ctx, "test", cheapParams.Memory) // a hash in progress
	require.NoError(t, err)

	for _, call := range []func() error{
		func() error { _, err := h.Hash(ctx, "password"); return err },
		func() error { _, err := h.Verify(ctx, stored, "password"); return err },
	} {
		err := call()
		require.True(t, errs.IsCategory(err, errs.CategoryUnavailable), "%v", err)
		assert.True(t, errs.IsCode(err, errs.CodeServerBusy), "%v", err)
		de, _ := errors.AsType[*errs.DomainError](err)
		assert.Equal(t, 20*time.Millisecond, de.RetryAfter)
	}
	assert.Equal(t, int64(2), rec.Metrics.Count(telemetry.MetricPasswordBusy, nil))
	for _, name := range []string{telemetry.SpanPasswordHash, telemetry.SpanPasswordVerify} {
		spans := rec.Tracer.Named(name)
		require.Len(t, spans, 1, name)
		assert.NoError(t, spans[0].Err(), name)
		assert.Equal(t, errs.CodeServerBusy, spans[0].Attrs()[telemetry.AttrErrorCode], name)
	}

	release()
	hash, err := h.Hash(ctx, "password")
	require.NoError(t, err)
	for _, hash := range []string{hash, stored} {
		ok, err := h.Verify(ctx, hash, "password")
		require.NoError(t, err)
		assert.True(t, ok)
	}
}

// A call that finds no room gets it when a running hash finishes, and its
// wait is recorded.
func TestArgon2idHasher_WaitsForRoom(t *testing.T) {
	tel, rec := telemetrytest.New()
	h := newHasher(t, newPasswordKeys(t), cheapParams, PasswordLimit{MaxConcurrent: 1, MaxWait: 10 * time.Second}, tel)
	release, err := h.acquire(t.Context(), "test", cheapParams.Memory)
	require.NoError(t, err)
	time.AfterFunc(100*time.Millisecond, release)

	_, err = h.Hash(t.Context(), "password")
	require.NoError(t, err)

	var longest float64
	for _, o := range rec.Metrics.Observations(telemetry.MetricPasswordWait, nil) {
		longest = max(longest, o.Value)
	}
	assert.Greater(t, longest, 0.0, "the wait was not recorded")
	assert.Zero(t, rec.Metrics.Count(telemetry.MetricPasswordBusy, nil))
}

// A caller that gives up while it waits gets its context's error, not a
// busy error: nobody is left to read a 503.
func TestArgon2idHasher_StopsWaitingWithTheContext(t *testing.T) {
	tel, rec := telemetrytest.New()
	h := newHasher(t, newPasswordKeys(t), cheapParams, PasswordLimit{MaxConcurrent: 1, MaxWait: 10 * time.Second}, tel)
	release, err := h.acquire(t.Context(), "test", cheapParams.Memory)
	require.NoError(t, err)
	defer release()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.Hash(cancelled, "password")
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, errs.IsCategory(err, errs.CategoryUnavailable), "%v", err)

	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = h.Hash(short, "password")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	assert.Zero(t, rec.Metrics.Count(telemetry.MetricPasswordBusy, nil))
}

// Verify takes the memory the stored hash was made with, so a hash made
// with more memory than the hasher's params counts for more than one. A
// hash larger than the whole room takes all of it, and still verifies.
func TestArgon2idHasher_VerifyTakesTheStoredHashsMemory(t *testing.T) {
	ctx := t.Context()
	km := newPasswordKeys(t)
	with := func(memory uint32) *Params {
		p := *cheapParams
		p.Memory = memory
		return &p
	}
	h := newHasher(t, km, with(1024), PasswordLimit{MaxConcurrent: 2, MaxWait: 20 * time.Millisecond}, nil)
	double, err := newHasher(t, km, with(2048), PasswordLimit{}, nil).Hash(ctx, "password")
	require.NoError(t, err)
	huge, err := newHasher(t, km, with(8192), PasswordLimit{}, nil).Hash(ctx, "password")
	require.NoError(t, err)

	release, err := h.acquire(ctx, "test", 1024) // one of two hashes in progress
	require.NoError(t, err)
	_, err = h.Hash(ctx, "password")
	require.NoError(t, err, "a hash of the hasher's params still fits")
	_, err = h.Verify(ctx, double, "password")
	assert.True(t, errs.IsCode(err, errs.CodeServerBusy), "a hash of twice the memory does not fit: %v", err)
	release()

	for name, hash := range map[string]string{"double": double, "huge": huge} {
		ok, err := h.Verify(ctx, hash, "password")
		require.NoError(t, err, name)
		assert.True(t, ok, name)
	}
}

// A zero field of Params takes DefaultParams' value, nil being all zero.
// The hasher keeps a copy, and leaves the caller's Params as it was.
func TestParams_ZeroFieldsTakeTheDefaults(t *testing.T) {
	km := newPasswordKeys(t)
	for name, tc := range map[string]struct {
		params *Params
		want   Params
	}{
		"nil":          {nil, *DefaultParams},
		"all zero":     {&Params{}, *DefaultParams},
		"memory alone": {&Params{Memory: 19 * 1024}, Params{Memory: 19 * 1024, Iterations: 3, Parallelism: 4, SaltLength: 16, KeyLength: 32}},
		"complete":     {cheapParams, *cheapParams},
	} {
		h := newHasher(t, km, tc.params, PasswordLimit{}, nil)
		assert.Equal(t, tc.want, *h.params, name)
		assert.NotSame(t, DefaultParams, h.params, name)
	}

	// Before the fix, the zero salt and key lengths of this Params hashed
	// with no salt and panicked inside argon2.
	partial := &Params{Memory: 1024, Iterations: 1, Parallelism: 1}
	h := newHasher(t, km, partial, PasswordLimit{}, nil)
	assert.Equal(t, Params{Memory: 1024, Iterations: 1, Parallelism: 1}, *partial, "the caller's Params changed")
	hash, err := h.Hash(t.Context(), "password")
	require.NoError(t, err)
	_, _, salt, key, err := decodeHash(hash)
	require.NoError(t, err)
	assert.Len(t, salt, 16)
	assert.Len(t, key, 32)
	ok, err := h.Verify(t.Context(), hash, "password")
	require.NoError(t, err)
	assert.True(t, ok)
}

// A salt or a key under 16 bytes, and a memory under 8 KiB a lane, are
// configuration errors from the constructor and from New. The floors
// themselves are accepted.
func TestParams_Floors(t *testing.T) {
	km := newPasswordKeys(t)
	for name, params := range map[string]*Params{
		"short salt":             {SaltLength: 8},
		"short key":              {KeyLength: 8},
		"memory under the lanes": {Memory: 16, Parallelism: 4},
		"memory under one lane":  {Memory: 4, Parallelism: 1},
	} {
		_, err := NewArgon2idHasher(params, PasswordLimit{}, km, nil)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%s: %v", name, err)

		_, err = New(t.Context(), Config{Secrets: NewSecretSource(nil), Password: params}, nil)
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "%s: %v", name, err)
	}

	floors := &Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 16}
	h := newHasher(t, km, floors, PasswordLimit{}, nil)
	hash, err := h.Hash(t.Context(), "password")
	require.NoError(t, err)
	ok, err := h.Verify(t.Context(), hash, "password")
	require.NoError(t, err)
	assert.True(t, ok)
}

// A hash made by Argon2's reference implementation decodes to its
// parameters, and the hasher derives the same key from its password: the
// eight Argon2id vectors of version 1.3 in the reference implementation's
// src/test.c (github.com/P-H-C/phc-winner-argon2), each with its hex output
// and its encoded hash. They are not run through Verify, which peppers the
// password with HMAC-SHA256 first (checked against RFC 4231 in
// vectors_test.go). RFC 9106's vector needs Argon2's secret and associated
// data, which golang.org/x/crypto/argon2 takes no argument for; that
// package checks it in its own tests.
func TestArgon2idHasher_ReferenceVectors(t *testing.T) {
	// Room for one hash of the largest vector, 256 MiB.
	h := newHasher(t, newPasswordKeys(t), &Params{Memory: 256 * 1024}, PasswordLimit{MaxConcurrent: 1}, nil)
	for _, tc := range []struct {
		name, password, hex, encoded string
	}{
		{"t=2 m=2^16 p=1", "password", "09316115d5cf24ed5a15a31a3ba326e5cf32edc24702987c02b6566f61913cf7",
			"$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"},
		{"t=2 m=2^18 p=1", "password", "78fe1ec91fb3aa5657d72e710854e4c3d9b9198c742f9616c2f085bed95b2e8c",
			"$argon2id$v=19$m=262144,t=2,p=1$c29tZXNhbHQ$eP4eyR+zqlZX1y5xCFTkw9m5GYx0L5YWwvCFvtlbLow"},
		{"t=2 m=2^8 p=1", "password", "9dfeb910e80bad0311fee20f9c0e2b12c17987b4cac90c2ef54d5b3021c68bfe",
			"$argon2id$v=19$m=256,t=2,p=1$c29tZXNhbHQ$nf65EOgLrQMR/uIPnA4rEsF5h7TKyQwu9U1bMCHGi/4"},
		{"t=2 m=2^8 p=2", "password", "6d093c501fd5999645e0ea3bf620d7b8be7fd2db59c20d9fff9539da2bf57037",
			"$argon2id$v=19$m=256,t=2,p=2$c29tZXNhbHQ$bQk8UB/VmZZF4Oo79iDXuL5/0ttZwg2f/5U52iv1cDc"},
		{"t=1 m=2^16 p=1", "password", "f6a5adc1ba723dddef9b5ac1d464e180fcd9dffc9d1cbf76cca2fed795d9ca98",
			"$argon2id$v=19$m=65536,t=1,p=1$c29tZXNhbHQ$9qWtwbpyPd3vm1rB1GThgPzZ3/ydHL92zKL+15XZypg"},
		{"t=4 m=2^16 p=1", "password", "9025d48e68ef7395cca9079da4c4ec3affb3c8911fe4f86d1a2520856f63172c",
			"$argon2id$v=19$m=65536,t=4,p=1$c29tZXNhbHQ$kCXUjmjvc5XMqQedpMTsOv+zyJEf5PhtGiUghW9jFyw"},
		{"another password", "differentpassword", "0b84d652cf6b0c4beaef0dfe278ba6a80df6696281d7e0d2891b817d8c458fde",
			"$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$C4TWUs9rDEvq7w3+J4umqA32aWKB1+DSiRuBfYxFj94"},
		{"another salt", "password", "bdf32b05ccc42eb15d58fd19b1f856b113da1e9a5874fdcc544308565aa8141c",
			"$argon2id$v=19$m=65536,t=2,p=1$ZGlmZnNhbHQ$vfMrBczELrFdWP0ZsfhWsRPaHppYdP3MVEMIVlqoFBw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, keyVersion, salt, key, err := decodeHash(tc.encoded)
			require.NoError(t, err)
			assert.Equal(t, 0, keyVersion, "the reference format has no key version")
			assert.Equal(t, tc.hex, hex.EncodeToString(key))

			derived, err := h.deriveKey(t.Context(), "test", []byte(tc.password), salt, params)
			require.NoError(t, err)
			assert.Equal(t, key, derived)
		})
	}
}
