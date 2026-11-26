// Package identity defines who an operation runs as and where the call
// originates. Flows, hook handlers, and rate-limit rules read the Caller
// from the context. Transports establish callers through an authenticator,
// while code running inside the process identifies itself using System.
// The zero Caller represents an anonymous user with no permissions, ensuring
// unidentified calls are never trusted.
//
// This package has no dependencies on other Behemoth packages, allowing
// any package to access the caller.
package identity

import (
	"context"
	"errors"
	"time"
)

// PrincipalType defines what kind of identity a Principal names.
type PrincipalType string

const (
	PrincipalUser    PrincipalType = "user"    // ID is a user id
	PrincipalService PrincipalType = "service" // ID is a client id: another system with a key of its own
	PrincipalSystem  PrincipalType = "system"  // ID names code in the process, such as "purge-sessions"
)

// Principal identifies who an operation runs on behalf of.
// The zero value represents no principal.
type Principal struct {
	Type PrincipalType
	ID   string
}

// IsZero reports whether p names nobody.
func (p Principal) IsZero() bool { return p.Type == "" }

// SessionRef describes the session used to authenticate a caller. It
// contains only the data needed by flows and the audit log, never secrets.
type SessionRef struct {
	ID             string
	UserID         string
	State          string // "active", or "pending" while a second factor is outstanding
	ExpiresAt      time.Time
	FreshAt        time.Time // when the user last proved a credential
	Factors        []string  // Sign-in factors, in order using core's Factor names (e.g. "password")
	ImpersonatorID string    // ID of the user impersonating UserID, if any
}

// Peer describes what the transport knows about a request beyond its
// credentials.
type Peer struct {
	Transport string // The protocol used , such as "http"; empty for in-process calls.
	IPAddress string // the client's address, resolved with the trusted proxies
	UserAgent string
}

// Caller identifies who an operation runs for and where the request
// originated. Its fields are unexported so callers can only be created
// through System, For, or an authenticator using Anonymous,
// Unauthenticated, or SignedIn.
//
// The zero value represents an anonymous caller with no known identity
// or permissions.
type Caller struct {
	principal Principal
	delegate  Principal
	session   *SessionRef
	grant     grant
	authErr   error
	peer      Peer
}

// grant represents the permissions a caller holds. Its zero value grants none.
// Delegated credentials cannot specify individual permissions; callers either have
// all permissions of a given kind or none.
type grant struct {
	self  bool // all the self-service permission on the caller's account
	admin bool // all admin permission
}

// Principal returns who the operation runs for. It is zero for an anonymous
// caller.
func (c Caller) Principal() Principal { return c.principal }

// Delegate returns who acts on behalf of the principal, if anyone.
// This is either the system acting for a user (For) or a user impersonating
// the principal. It is zero when the principal acts on their own.
func (c Caller) Delegate() Principal { return c.delegate }

// Session returns the session used to authenticate the user.
// ok is false when no session exists, as with system and service callers,
// or anonymous callers waiting for a second factor.
func (c Caller) Session() (session SessionRef, ok bool) {
	if c.session == nil {
		return SessionRef{}, false
	}
	return *c.session, true
}

// AuthError returns the reason authentication failed when the caller presented
// a credential, such as an expired session. It is nil if no credential was presented
// or authentication succeeded.
func (c Caller) AuthError() error { return c.authErr }

// Peer returns the information the transport has about the caller. It is zero for
// calls made from within the process.
func (c Caller) Peer() Peer { return c.peer }

// IsAnonymous reports whether the caller has no authenticated identity.
func (c Caller) IsAnonymous() bool { return c.principal.IsZero() }

// Holds reports whether the caller holds permission. admin says whether the
// permission is an admin permission, which acts on an account named in the
// input, or a self-service one, which acts on the caller's own account. A
// signed-in user holds every self-service permission, a system caller holds
// every permission, and an anonymous caller holds none.
func (c Caller) Holds(permission string, admin bool) bool {
	if admin {
		return c.grant.admin
	}
	return c.grant.self
}

// System returns a caller for code in the process, such as a job or a CLI
// command, named for the audit log. Its principal is the system with ID
// name, and it holds every permission. A transport never builds one.
func System(name string) Caller {
	return Caller{
		principal: Principal{Type: PrincipalSystem, ID: name},
		grant:     grant{self: true, admin: true},
	}
}

// ErrInvalidFor is the AuthError of the caller For returns when it is called
// on a caller that isn't a system caller, or with an empty user id.
var ErrInvalidFor = errors.New("identity: For needs a system caller and a user id")

// For returns a caller that acts for the user with userID: its principal is
// the user, and its delegate is the system caller c, so that the audit log
// records who acted. It holds every self-service permission over the user's
// account, and no admin permission. On a caller that isn't a system caller,
// or with an empty userID, For returns an anonymous caller whose AuthError is
// ErrInvalidFor.
func (c Caller) For(userID string) Caller {
	if c.principal.Type != PrincipalSystem || !c.delegate.IsZero() || userID == "" {
		return Caller{authErr: ErrInvalidFor, peer: c.peer}
	}
	return Caller{
		principal: Principal{Type: PrincipalUser, ID: userID},
		delegate:  c.principal,
		grant:     grant{self: true},
		peer:      c.peer,
	}
}

// Anonymous returns the caller of a call that presented no credential, with
// what its transport knew about it.
func Anonymous(peer Peer) Caller {
	return Caller{peer: peer}
}

// Unauthenticated returns a caller whose credential did not identify anyone.
// reason explains why authentication failed. It is returned to flows that
// require a user and may be a session error (invalid, revoked, expired, or
// pending) or a lookup failure. pending is the session awaiting a second
// factor, if any, so that a second-factor flow can continue authentication.
func Unauthenticated(peer Peer, reason error, pending *SessionRef) Caller {
	c := Caller{peer: peer, authErr: reason}
	if pending != nil {
		s := *pending
		c.session = &s
	}
	return c
}

// SignedIn returns a caller authenticated by a session. The session's user
// becomes the principal and receives all self-service permissions on their
// account. If the session represents an impersonation, the impersonating user
// becomes the delegate. This constructor is intended for authenticators;
// within Behemoth, only package auth may call it, enforced by a test.
func SignedIn(session SessionRef, peer Peer) Caller {
	s := session
	c := Caller{
		principal: Principal{Type: PrincipalUser, ID: session.UserID},
		session:   &s,
		grant:     grant{self: true},
		peer:      peer,
	}
	if session.ImpersonatorID != "" {
		c.delegate = Principal{Type: PrincipalUser, ID: session.ImpersonatorID}
	}
	return c
}

type callerKey struct{}

// WithCaller returns ctx with c attached, replacing any existing caller.
// Transports call it once per request with the caller built by their
// authenticator. Code running in the process uses System or
// System(...).For(userID).
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller ctx carries, or the zero Caller, which is
// anonymous, when it carries none.
func CallerFrom(ctx context.Context) Caller {
	if ctx == nil {
		return Caller{}
	}
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}
