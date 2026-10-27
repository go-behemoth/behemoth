package oauth

import (
	"context"
	"errors"
	"time"
)

// ErrExchangeRefused is the error a Provider's Exchange returns, or wraps,
// when the sign-in can't be accepted: the provider refused the code, because
// it is unknown, expired, already redeemed or does not match the verifier or
// the redirect URI, or what the provider answered with did not pass the
// checks of the Provider, such as an ID token with another nonce. Complete
// then refuses the sign-in. Any other error of Exchange is taken as a failure
// of the system: the provider could not be reached, or its answer could not
// be read. A Refresher's Refresh returns it, or wraps it, for a refresh token
// the provider refused.
var ErrExchangeRefused = errors.New("oauth: the provider refused the sign-in")

// Provider is one OAuth 2.0 or OpenID Connect provider a user can sign in
// with, such as Google or GitHub. The plugin's flows call it and do the rest
// themselves: the state, the binding to the client, the lookup of the user
// and the session.
//
// The plugin creates a PKCE verifier and a nonce for every sign-in and hands
// them to the provider. Using them is the provider's part of the protocol,
// and the plugin can't check it: an implementation that ignores the code
// challenge leaves an authorization code that leaked unprotected.
//
// An implementation must be safe for concurrent use.
type Provider interface {
	// ID returns the provider's id, such as "google". It is the provider a
	// request names, the last segment of the provider's redirect URI
	// (RedirectURI) and the provider_id of the accounts it signs in, so it
	// does not change once users have signed in with it. It is made of
	// lowercase letters, digits, "-" and "_", starts with a letter or a
	// digit, and is not "credential".
	ID() string

	// AuthorizationURL returns the URL at the provider that the browser is
	// sent to for one sign-in. It is absolute, http or https.
	AuthorizationURL(ctx context.Context, req AuthorizationRequest) (string, error)

	// Exchange redeems the authorization code of one sign-in at the provider
	// and returns who signed in, with the tokens the provider issued. The
	// result has an Identity with a Subject. An error that wraps
	// ErrExchangeRefused refuses the sign-in, and any other error is a
	// failure of the system.
	Exchange(ctx context.Context, req ExchangeRequest) (*ExchangeResult, error)
}

// AuthorizationRequest is what the plugin created for one sign-in and a
// Provider puts into its authorization URL.
type AuthorizationRequest struct {
	// State is the value the provider returns with the authorization code.
	// It goes into the URL as it is.
	State string
	// RedirectURI is where the provider sends the browser back to: the
	// provider's redirect URI (RedirectURI).
	RedirectURI string
	// CodeChallenge is the PKCE challenge for the sign-in's verifier, made
	// with the S256 method.
	CodeChallenge string
	// Nonce is the value an OpenID Connect provider puts into the ID token.
	// A provider without ID tokens does not use it.
	Nonce string
}

// ExchangeRequest is what a Provider needs to redeem the authorization code
// of one sign-in.
type ExchangeRequest struct {
	// Code is the authorization code the provider sent with the state.
	Code string
	// RedirectURI is the redirect URI the authorization URL was made with.
	RedirectURI string
	// CodeVerifier is the PKCE verifier whose challenge was in the
	// authorization URL.
	CodeVerifier string
	// Nonce is the nonce that was in the authorization URL. An OpenID
	// Connect provider compares it with the one in the ID token.
	Nonce string
	// Issuer is the "iss" parameter the provider sent with the code (RFC
	// 9207), or empty when it sent none. A provider that knows its issuer
	// refuses another one, and refuses an empty one when it says it sends
	// the parameter.
	Issuer string
}

// ExchangeResult is what a Provider returns for a redeemed code.
type ExchangeResult struct {
	// Identity is who signed in.
	Identity Identity
	// Tokens are what the provider issued with the code. The plugin keeps
	// them for a provider in Options.StoreTokens, and discards them for any
	// other. A provider that does not give them out leaves them empty.
	Tokens Tokens
}

// Tokens are the tokens a provider issued for a user, when a code was
// redeemed or an access token refreshed. They are credentials.
type Tokens struct {
	// AccessToken calls the provider's API for the user.
	AccessToken string
	// RefreshToken renews AccessToken (Refresher). Empty when the provider
	// issued none.
	RefreshToken string
	// IDToken is the ID token of an OpenID Connect provider, as the
	// provider checked it. Empty for a provider without ID tokens, and
	// after a refresh.
	IDToken string
	// ExpiresAt is when AccessToken expires, and RefreshExpiresAt when
	// RefreshToken does. Each is zero when the provider did not say.
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
	// Scope is the scope the provider granted, as space-separated values.
	// Empty when the provider did not say.
	Scope string
}

// RefreshRequest is what a Refresher needs to renew an access token.
type RefreshRequest struct {
	// RefreshToken is the refresh token the provider issued.
	RefreshToken string
}

// Refresher is a Provider that can renew an access token with a refresh
// token. The built-in providers implement it. AccessToken uses it for an
// access token that expired.
type Refresher interface {
	// Refresh redeems req.RefreshToken at the provider and returns the new
	// tokens. Their RefreshToken is empty when the provider keeps the old
	// one, and their Scope when the scope did not change. An error that
	// wraps ErrExchangeRefused says the provider refused the refresh token:
	// it expired, was revoked, or its grant is gone. Any other error is a
	// failure of the system.
	Refresh(ctx context.Context, req RefreshRequest) (*Tokens, error)
}

// Revoker is a Provider that can revoke a user's tokens at the provider.
// The built-in providers implement it. Unlink uses it once the account is
// removed.
type Revoker interface {
	// Revoke revokes tokens at the provider, and the grant they belong to
	// where the provider ties the two. A provider that offers no way to
	// revoke them returns nil.
	Revoke(ctx context.Context, tokens Tokens) error
}

// Identity is who a provider says signed in. It is plain data and holds no
// token.
type Identity struct {
	// Subject is the provider's id for the user: stable, unique at the
	// provider, and never reassigned. It becomes the account_id of the
	// user's account. An email address is not a subject.
	Subject string
	// Email is the user's address at the provider. It may be empty.
	Email string
	// EmailVerified reports whether the provider itself says it verified
	// Email. A provider that can't tell leaves it false.
	EmailVerified bool
	// Name is the user's full name, GivenName and FamilyName its parts.
	// Each may be empty.
	Name       string
	GivenName  string
	FamilyName string
	// Picture is the URL of the user's profile picture. It may be empty.
	Picture string
}
