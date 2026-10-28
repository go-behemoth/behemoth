package core

import (
	"fmt"
	"time"

	"github.com/go-behemoth/behemoth"
)

// type JWTConfig struct {
// 	Secret        string
// 	Expiry        time.Duration
// 	SigningMethod jwt.SigningMethod
// 	Claims        jwt.Claims
// }

// Defaults of SessionConfig, applied by SessionConfig.WithDefaults to the
// fields left at zero.
const (
	DefaultSessionExpiresIn        = 7 * 24 * time.Hour
	DefaultSessionPendingExpiresIn = 10 * time.Minute
	// DefaultFreshAge is how long a session counts as fresh.
	DefaultFreshAge = 15 * time.Minute
	// DefaultUnversionedCacheTTL is how long a session stays in a session
	// cache that can't order its writes.
	DefaultUnversionedCacheTTL = time.Minute
)

// SessionConfig configures the session manager. The zero value works: a
// field left at zero takes the default named on it (WithDefaults), or means
// what its comment says zero means.
//
// How the session token travels over HTTP, and the session cookie, are the
// token carrier's settings (httpapi.RouterConfig.Carrier).
type SessionConfig struct {
	// ExpiresIn is how long an active session lasts. Default 7 days.
	ExpiresIn time.Duration
	// PendingExpiresIn is how long a session that waits for a second factor
	// lasts. Default 10 minutes.
	PendingExpiresIn time.Duration
	// UpdateAge turns the rolling expiration on: a session in use is
	// extended by ExpiresIn once less than UpdateAge is left of it. Zero,
	// the default, means a session is not extended and ends ExpiresIn after
	// its sign-in. It has no other default on purpose: a value that is not
	// smaller than ExpiresIn extends the session, with a write, on every
	// request.
	//
	// "In use" means a request that carries the session's token: the
	// authenticator extends the session there (Authenticator, Touch). With
	// a cookie delivery that response sets the cookie again, so that it
	// expires with the extended session.
	UpdateAge time.Duration
	// FreshAge is how long after a sign-in a session counts as fresh
	// (SessionManager.IsFresh). Default DefaultFreshAge, 15 minutes.
	FreshAge time.Duration
	// MaxConcurrent limits the sessions a user can use at once: active ones
	// that have not expired. A session that waits for a second factor does
	// not count. Zero, the default, means no limit.
	MaxConcurrent int
	// EvictOldestOnLimit decides what happens at the limit: true ends the
	// user's oldest active session to make room, false refuses the new
	// session. A sign-in whose session waits for a second factor makes room
	// only once the second factor has passed (SessionManager.Promote).
	// Without eviction it is refused at once.
	EvictOldestOnLimit bool
	// UnversionedCacheTTL is how long a session stays in the session cache
	// (BootConfig.KV) when the key-value storage can't order its writes:
	// it doesn't implement VersionedCache. A revoke that races a cache
	// write can leave the session valid from such a cache for this long.
	// Default DefaultUnversionedCacheTTL, one minute. With a storage that
	// implements VersionedCache, such as the Redis adapter, a session stays
	// cached for its whole life, and the setting is not used.
	UnversionedCacheTTL time.Duration
	SecondaryStorage    behemoth.KeyValueStorage // nil = DB-only, no cache layer
	CaptureIPAndAgent   bool
}

// WithDefaults returns c with every field left at zero set to its default.
// The session manager applies it, so a SessionConfig read anywhere else may
// still hold zeros.
func (c SessionConfig) WithDefaults() SessionConfig {
	if c.ExpiresIn == 0 {
		c.ExpiresIn = DefaultSessionExpiresIn
	}
	if c.PendingExpiresIn == 0 {
		c.PendingExpiresIn = DefaultSessionPendingExpiresIn
	}
	if c.FreshAge == 0 {
		c.FreshAge = DefaultFreshAge
	}
	if c.UnversionedCacheTTL == 0 {
		c.UnversionedCacheTTL = DefaultUnversionedCacheTTL
	}
	return c
}

// Validate reports a setting no default can repair: a negative duration.
// Boot calls it. A zero field is not an error; it takes its default.
func (c SessionConfig) Validate() error {
	for name, d := range map[string]time.Duration{
		"ExpiresIn": c.ExpiresIn, "PendingExpiresIn": c.PendingExpiresIn, "UpdateAge": c.UpdateAge, "FreshAge": c.FreshAge,
		"UnversionedCacheTTL": c.UnversionedCacheTTL,
	} {
		if d < 0 {
			return fmt.Errorf("SessionConfig.%s is negative: %s", name, d)
		}
	}
	return nil
}

// var DefaultJWTConfig = JWTConfig{
// 	Secret:        utils.GenerateRandomString(64), // Use a secure random string for the secret
// 	Expiry:        24 * time.Hour,
// 	SigningMethod: jwt.SigningMethodHS256,
// }
