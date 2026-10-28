package core

import (
	"context"
	"time"

	"github.com/go-behemoth/behemoth/models"
)

// SessionManager creates, resolves, extends and revokes sessions. It knows
// nothing of HTTP: the token carrier (httpapi.TokenCarrier) hands a session's token
// to a client and reads it from a request, and the authenticator
// (Authenticator) joins the two on every request.
type SessionManager interface {
	// Create starts a session for userID in meta.State and returns it with
	// its raw token. Pass the context of the call being handled: the
	// session's IP address and user agent are read from its caller (see
	// SessionMeta).
	Create(ctx context.Context, userID any, meta SessionMeta) (*models.Session, string /* raw token */, error)

	// Validate resolves a raw bearer token to its Session.
	// Only returns State == Active; Use Get() to resolve a Pending session.
	Validate(ctx context.Context, rawToken string) (*models.Session, error)

	// Get resolves a raw token to its Session regardless of State (Pending or
	// Active) only enforcing expiry/revocation.
	// This can be used by plugins like 2FA at verification stage to look up a Pending session before
	// promoting it.
	Get(ctx context.Context, rawToken string) (*models.Session, error)

	// Promote transitions Pending -> Active, once the caller has verified a
	// second factor, and records factor, the second factor's name, after
	// the first (models.Session.Factors). factor is a Factor constant or a
	// plugin's own name, in lowercase snake case.
	//
	// The session gets a new token, which Promote returns with it: the
	// pending token stops working, so that a token copied before the second
	// factor never authenticates the active session. The caller delivers
	// the new token as a sign-in delivers its own (httpapi.WriteSignIn
	// sets the cookie again with the active session's expiry). The state
	// and the token change in one write that requires the session to be
	// pending, so of two promotions of one session one succeeds, and the
	// other gets a session error with errs.CodeInvalidSessionState.
	//
	// SessionConfig.MaxConcurrent applies as when an active session is
	// created: at the limit the user's oldest active session is revoked, or,
	// without EvictOldestOnLimit, the promotion is refused with
	// errs.CodeSessionLimitReached and the session stays pending.
	Promote(ctx context.Context, sessionID, factor string) (*models.Session, string /* raw token */, error)

	// Touch applies the rolling expiration: it extends a session in use
	// once less than SessionConfig.UpdateAge is left of it. The
	// authenticator calls it on every request that carries a session's
	// token, with the session it resolved. Whether an extension is due is
	// read off that session, so a request that is not due costs no read and
	// no write. It returns the session with its new expiry when it extended
	// it, and nil when it did not. The caller then extends the client's
	// copy of the token, as the authentication stage does with
	// httpapi.TokenCarrier.Extend.
	Touch(ctx context.Context, session *models.Session) (*models.Session, error)

	Revoke(ctx context.Context, sessionID string, reason string) error

	RevokeAllForUser(ctx context.Context, userID any, reason string, except string) error

	// IsFresh reports whether session's credentials were checked recently:
	// it is active, and its last sign-in or second factor (FreshAt) is no
	// older than SessionConfig.FreshAge. A route that changes something
	// sensitive, such as the account's email, asks for a fresh session so
	// that a stolen long-lived session is not enough (httpapi.RequireFreshSession).
	// Nothing refreshes a session in place yet: signing in again is how a
	// user gets a fresh one.
	IsFresh(session *models.Session) bool

	// Evict removes sessions from the session cache and does nothing else.
	// It is for sessions whose rows are being deleted with their user
	// (Store.DeleteUser), and is safe to repeat.
	Evict(ctx context.Context, sessions []*models.Session)

	// Discard is for sessions whose rows no longer exist: they were deleted
	// with their user, and the delete has committed. It removes them from
	// the session cache and fires auth.session.afterRevoke with reason for
	// each one that was not revoked already, so a handler that reacts to a
	// session ending sees these too. It writes nothing to the database, and
	// auth.session.beforeRevoke does not fire: there is nothing left to
	// refuse. An implementation without revocation does nothing.
	Discard(ctx context.Context, sessions []*models.Session, reason string) error

	ListForUser(ctx context.Context, userID any) ([]*models.Session, error)

	// SupportsRevocation lets callers (admin UI, HTTP layer) know whether
	// Revoke/RevokeAllForUser are meaningful or a no-op; false for purely
	// stateless/self-verifying implementations.
	SupportsRevocation() bool
}

// VersionedCache is an optional capability of a behemoth.KeyValueStorage: a
// value that a write of an older version never replaces. The session manager
// finds it by type assertion and keeps its cache of sessions through it, with
// a session row's version (models.Session.Version). A copy of a session read
// before a revoke then can't replace the revoked one, whatever order the two
// writes reach the storage in. The Redis adapter has it. A storage without it
// still caches sessions, for SessionConfig.UnversionedCacheTTL at most.
type VersionedCache interface {
	// SetIfNewer writes value under key with version, as one atomic step,
	// unless key holds a value of the same or a later version, and reports
	// whether it wrote. The value lives for ttl, which is at least a
	// millisecond. A key that holds a value written by the storage's plain
	// Set has no version, and is replaced.
	SetIfNewer(ctx context.Context, key, value string, version int64, ttl time.Duration) (bool, error)

	// GetVersioned returns the value SetIfNewer wrote under key, and its
	// version. A key that holds none is a key-not-found error, as for Get.
	GetVersioned(ctx context.Context, key string) (value string, version int64, err error)
}

// SessionRevokedUserDeleted is the revoke reason of a session that ended
// because its user was deleted.
const SessionRevokedUserDeleted = "user_deleted"

// SessionMeta is caller-supplied context at creation time.
type SessionMeta struct {
	// IPAddress and UserAgent are optional. Left empty, the session manager
	// takes them from the caller on the context passed to Create
	// (identity.CallerFrom(ctx).Peer()), which the transport filled; for a
	// caller no transport authenticated they stay empty. Set them only to
	// override that, e.g. from a job acting on a recorded request. Neither
	// is stored unless SessionConfig.CaptureIPAndAgent is on.
	IPAddress string
	UserAgent string
	State     models.SessionState // Active or Pending: set by the caller (SignIn passes Active or Pending depending on whether 2FA is required)
	// Factor names what the sign-in checked: one of the Factor constants, or
	// a name of a plugin's own in lowercase snake case, at most 32
	// characters. The session records it as its first factor
	// (models.Session.Factors), which a flow reads on the caller's session.
	// Empty records none, for a session that no credential check created,
	// such as one in a test.
	Factor string
}

// The factors Behemoth's plugins record on a session: what a credential
// proved, named by its channel, so that the same name means the same proof
// whichever plugin records it. A sign-in passes its factor in SessionMeta
// and in the payload of auth.signIn.credentialsVerified.
const (
	FactorPassword     = "password"      // a password
	FactorEmail        = "email"         // control of a mailbox, such as a magic link
	FactorSMS          = "sms"           // control of a phone number, by a code sent in a text message
	FactorOAuth        = "oauth"         // the answer of an OAuth or OpenID Connect provider
	FactorTOTP         = "totp"          // a code from an authenticator app
	FactorRecoveryCode = "recovery_code" // a recovery code
)
