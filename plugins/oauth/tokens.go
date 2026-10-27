package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/telemetry"
)

// accessTokenLeeway is how long before its expiry an access token counts as
// expired. AccessToken refreshes such a token, so that the application's
// call to the provider does not fail with a token that expires on the way.
const accessTokenLeeway = time.Minute

// AccessTokenRequest is the input of AccessToken.
type AccessTokenRequest struct {
	// Provider is the id of the provider, one of Options.StoreTokens.
	Provider string
}

// ProviderToken is an access token of a user at a provider, as AccessToken
// returns it.
type ProviderToken struct {
	// Provider is the provider's id.
	Provider string
	// AccessToken calls the provider's API for the user. It is a credential:
	// send it to the provider, and keep it out of logs, URLs, and answers to
	// a client.
	AccessToken string
	// ExpiresAt is when AccessToken expires. Zero when the provider did not
	// say.
	ExpiresAt time.Time
	// Scope is the scope the provider granted, as space-separated values.
	// Empty when the provider did not say.
	Scope string
}

// AccessToken returns a valid access token of the caller's account at the
// provider req.Provider, for the application to call the provider's API for
// the user. An access token that expires within a minute is refreshed first,
// when the account holds a refresh token and the provider can refresh
// (Refresher), and the new tokens are stored. Calls for one account that
// find its token expired share one refresh, within this process.
//
// The caller needs PermissionAccessToken (Access.RequireUser). A system
// caller acting for the user passes, for a job that calls the provider. A
// provider the plugin doesn't have is a validation error, and one that is
// not in Options.StoreTokens a configuration error. A provider the user has
// no account at is a conflict with ErrorCodeProviderNotLinked. An account
// without a usable token is a conflict with
// ErrorCodeProviderTokenUnavailable: no token was stored, or the access
// token expired and can't be refreshed. A refresh token the provider refuses
// is removed with the rest of the tokens. The user then signs in with the
// provider again, which stores new ones.
//
// No route calls it: a provider's token belongs on the application's
// server, not in its pages. The plugin must have been initialized by Boot.
func (p *Plugin) AccessToken(ctx context.Context, req AccessTokenRequest) (*ProviderToken, error) {
	const op = "oauth.AccessToken"
	if err := p.ready(op); err != nil {
		return nil, err
	}
	userID, err := p.authContext.Access.RequireUser(ctx, PermissionAccessToken)
	if err != nil {
		return nil, err
	}
	provider, ok := p.providers[req.Provider]
	if !ok {
		return nil, errs.NewInvalidInputError(op, keyProvider, "unknown provider", nil)
	}
	if !p.storeTokens[req.Provider] {
		return nil, errs.NewConfigurationError(op, fmt.Sprintf("the tokens of provider %q are not stored; add it to Options.StoreTokens", req.Provider), nil)
	}
	account, err := p.accountAt(ctx, userID, req.Provider)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, errProviderNotLinked(op)
	}
	if usable(account, p.authContext.Store.Now()) {
		return tokenOf(account), nil
	}

	// One refresh per account at a time: a provider that rotates refresh
	// tokens refuses the second of two refreshes with the same one. The
	// refresh does not end with the caller that started it, since others
	// may wait on it. The HTTP client's timeout bounds it.
	shared, err, _ := p.refreshes.Do(account.ID, func() (any, error) {
		return p.refresh(context.WithoutCancel(ctx), provider, account.ID)
	})
	if err != nil {
		return nil, err
	}
	token := *shared.(*ProviderToken) // a copy for each caller
	return &token, nil
}

// refresh renews the access token of the account with accountID with its
// refresh token, stores the new tokens, and returns the access token. It
// reads the account again first: a refresh may have finished while this
// call waited.
func (p *Plugin) refresh(ctx context.Context, provider Provider, accountID string) (*ProviderToken, error) {
	st := p.authContext.Store
	account, err := st.FindAccountByID(ctx, accountID)
	if errs.IsNotFound(err) {
		return nil, errProviderNotLinked("oauth.AccessToken") // unlinked meanwhile
	}
	if err != nil {
		return nil, err
	}
	now := st.Now()
	if usable(account, now) {
		return tokenOf(account), nil
	}
	refresher, ok := provider.(Refresher)
	if !ok || account.RefreshToken == "" || (account.RefreshTokenExpiresAt != nil && !account.RefreshTokenExpiresAt.After(now)) {
		return nil, errTokenUnavailable()
	}

	tokens, err := refresher.Refresh(ctx, RefreshRequest{RefreshToken: account.RefreshToken})
	switch {
	case errors.Is(err, ErrExchangeRefused):
		return p.forgetTokens(ctx, account, err)
	case err != nil:
		return nil, fmt.Errorf("oauth: refresh at provider %q: %w", account.ProviderID, err)
	case tokens == nil || tokens.AccessToken == "":
		return nil, fmt.Errorf("oauth: provider %q answered a refresh without an access token", account.ProviderID)
	}
	updated, err := st.UpdateAccount(ctx, account.ID, issuedChanges(*tokens))
	if err != nil {
		return nil, err
	}
	return tokenOf(updated), nil
}

// forgetTokens answers a refresh token the provider refused. Another replica
// may have refreshed the account meanwhile, with a refresh token the
// provider rotated, so the account is read once more. When it still has no
// usable access token, its tokens are removed: none of them works, and every
// call that found them would ask the provider again.
func (p *Plugin) forgetTokens(ctx context.Context, account *models.Account, refused error) (*ProviderToken, error) {
	st := p.authContext.Store
	current, err := st.FindAccountByID(ctx, account.ID)
	if errs.IsNotFound(err) {
		return nil, errProviderNotLinked("oauth.AccessToken")
	}
	if err != nil {
		return nil, err
	}
	if usable(current, st.Now()) {
		return tokenOf(current), nil
	}
	p.log.Info(ctx, "the provider refused a refresh token, and the account's tokens are removed",
		telemetry.ErrorFields(refused, behemoth.M{keyProvider: account.ProviderID}))
	if _, err := st.UpdateAccount(ctx, account.ID, noTokens()); err != nil {
		return nil, err
	}
	return nil, errTokenUnavailable()
}

func errTokenUnavailable() error {
	return conflict("oauth.AccessToken", ErrorCodeProviderTokenUnavailable,
		"no usable token of this provider is stored; sign in with the provider again")
}

// usable reports whether the account's access token can be handed out at
// now: it is there, and it does not expire within accessTokenLeeway. A token
// whose expiry the provider did not give counts as usable.
func usable(a *models.Account, now time.Time) bool {
	return a.AccessToken != "" && (a.AccessTokenExpiresAt == nil || a.AccessTokenExpiresAt.After(now.Add(accessTokenLeeway)))
}

// tokenOf returns the access token of a as a ProviderToken.
func tokenOf(a *models.Account) *ProviderToken {
	token := &ProviderToken{Provider: a.ProviderID, AccessToken: a.AccessToken, Scope: a.Scope}
	if a.AccessTokenExpiresAt != nil {
		token.ExpiresAt = *a.AccessTokenExpiresAt
	}
	return token
}

// newAccount returns the account of subject at providerID for userID, to be
// created. It holds t when the plugin stores the provider's tokens.
func (p *Plugin) newAccount(userID, providerID, subject string, t Tokens) *models.Account {
	account := &models.Account{UserID: userID, ProviderID: providerID, AccountID: subject}
	if p.storeTokens[providerID] {
		account.AccessToken, account.RefreshToken, account.IDToken, account.Scope = t.AccessToken, t.RefreshToken, t.IDToken, t.Scope
		account.AccessTokenExpiresAt, account.RefreshTokenExpiresAt = timeOrNil(t.ExpiresAt), timeOrNil(t.RefreshExpiresAt)
	}
	return account
}

// keepTokens brings the tokens of account, which a sign-in found, up to
// date: the ones the provider just issued when the plugin stores the
// provider's tokens, and none when it does not. A provider that issued no
// access token leaves the stored tokens as they are.
func (p *Plugin) keepTokens(ctx context.Context, account *models.Account, t Tokens) error {
	var changes behemoth.M
	switch {
	case !p.storeTokens[account.ProviderID]:
		if !hasTokens(account) {
			return nil
		}
		changes = noTokens()
	case t.AccessToken == "":
		return nil
	default:
		changes = issuedChanges(t)
	}
	_, err := p.authContext.Store.UpdateAccount(ctx, account.ID, changes)
	return err
}

// issuedChanges returns the changes that give an account the tokens t, just
// issued by a sign-in or a refresh. A refresh token, an ID token, and a
// scope the provider did not issue again are kept: Google issues a refresh
// token at the first consent only, a refresh issues no ID token, and an
// answer without a scope keeps the one granted.
func issuedChanges(t Tokens) behemoth.M {
	changes := behemoth.M{
		models.AccountAccessToken:          t.AccessToken,
		models.AccountAccessTokenExpiresAt: nullableTime(t.ExpiresAt),
	}
	if t.RefreshToken != "" {
		changes[models.AccountRefreshToken] = t.RefreshToken
		changes[models.AccountRefreshTokenExpiresAt] = nullableTime(t.RefreshExpiresAt)
	}
	if t.IDToken != "" {
		changes[models.AccountIDToken] = t.IDToken
	}
	if t.Scope != "" {
		changes[models.AccountScope] = t.Scope
	}
	return changes
}

// noTokens returns the changes that remove every token of an account.
func noTokens() behemoth.M {
	return behemoth.M{
		models.AccountAccessToken: nil, models.AccountRefreshToken: nil, models.AccountIDToken: nil,
		models.AccountAccessTokenExpiresAt: nil, models.AccountRefreshTokenExpiresAt: nil, models.AccountScope: nil,
	}
}

// hasTokens reports whether a holds a token of its provider.
func hasTokens(a *models.Account) bool {
	return a.AccessToken != "" || a.RefreshToken != "" || a.IDToken != ""
}

// revokeTokens revokes the tokens an unlinked account held, at its provider,
// when the provider can (Revoker). The account is gone already, so a failure
// is logged and the unlink stands. The call does not end with a client that
// gives up: the HTTP client's timeout bounds it.
func (p *Plugin) revokeTokens(ctx context.Context, account *models.Account) {
	if !hasTokens(account) {
		return
	}
	revoker, ok := p.providers[account.ProviderID].(Revoker)
	if !ok {
		return
	}
	tokens := Tokens{AccessToken: account.AccessToken, RefreshToken: account.RefreshToken, IDToken: account.IDToken, Scope: account.Scope}
	if err := revoker.Revoke(context.WithoutCancel(ctx), tokens); err != nil {
		p.log.Warn(ctx, "the provider account is unlinked, and its tokens were not revoked at the provider",
			telemetry.ErrorFields(err, behemoth.M{keyProvider: account.ProviderID}))
	}
}

// timeOrNil returns t in UTC, or nil for the zero time.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// nullableTime returns t in UTC, or nil for the zero time, as an update
// takes a column's value.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
