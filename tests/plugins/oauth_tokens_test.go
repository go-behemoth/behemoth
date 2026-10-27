package plugins_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/oauth"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linkedWithTokens gives user an account at provider with subject that
// holds tokens, as a sign-in with the provider's tokens stored would have.
func (app *oauthApp) linkedWithTokens(t *testing.T, user *models.User, provider, subject string, tokens oauth.Tokens) {
	t.Helper()
	account := &models.Account{
		UserID: user.ID, ProviderID: provider, AccountID: subject,
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, Scope: tokens.Scope,
	}
	if !tokens.ExpiresAt.IsZero() {
		at := tokens.ExpiresAt.UTC()
		account.AccessTokenExpiresAt = &at
	}
	if !tokens.RefreshExpiresAt.IsZero() {
		at := tokens.RefreshExpiresAt.UTC()
		account.RefreshTokenExpiresAt = &at
	}
	require.NoError(t, app.ac.Store.CreateAccount(context.Background(), account))
}

// accountAt reads the account at provider with subject, its tokens opened.
func (app *oauthApp) accountAt(t *testing.T, provider, subject string) *models.Account {
	t.Helper()
	account, err := app.ac.Store.FindAccount(context.Background(), provider, subject)
	require.NoError(t, err)
	return account
}

// rawColumn reads a column of that account as the database holds it.
func (app *oauthApp) rawColumn(t *testing.T, column, provider, subject string) string {
	t.Helper()
	var value sql.NullString
	require.NoError(t, app.db.QueryRow("SELECT "+column+" FROM accounts WHERE provider_id = ? AND account_id = ?", provider, subject).Scan(&value))
	return value.String
}

// plainProvider is a Provider that can neither refresh nor revoke.
type plainProvider struct{ id string }

func (p plainProvider) ID() string { return p.id }
func (p plainProvider) AuthorizationURL(context.Context, oauth.AuthorizationRequest) (string, error) {
	return "https://plain.example.com/authorize", nil
}
func (p plainProvider) Exchange(context.Context, oauth.ExchangeRequest) (*oauth.ExchangeResult, error) {
	return nil, errors.New("not in these tests")
}

// For a provider in StoreTokens, the tokens of a sign-up land in the account,
// sealed in the database and in no hook payload. A later sign-in replaces
// them, and keeps a refresh token, a scope, and their expiry when the
// provider issued none again.
func TestOAuthStoresTheProviderTokens(t *testing.T) {
	ctx := context.Background()
	var signIns signInTrace
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, signIns.register())
	require.NoError(t, err)
	app.google.identity = ada
	first := oauth.Tokens{
		AccessToken: "access-1", RefreshToken: "refresh-1", IDToken: "id-1", Scope: "openid email",
		ExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	app.google.tokens = first

	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	require.NoError(t, err)
	account := app.accountAt(t, "google", "g-1")
	assert.Equal(t, "access-1", account.AccessToken)
	assert.Equal(t, "refresh-1", account.RefreshToken)
	assert.Equal(t, "id-1", account.IDToken)
	assert.Equal(t, "openid email", account.Scope)
	require.NotNil(t, account.AccessTokenExpiresAt)
	assert.WithinDuration(t, first.ExpiresAt, *account.AccessTokenExpiresAt, time.Second)
	require.NotNil(t, account.RefreshTokenExpiresAt)
	assert.WithinDuration(t, first.RefreshExpiresAt, *account.RefreshTokenExpiresAt, time.Second)

	for column, plain := range map[string]string{"access_token": "access-1", "refresh_token": "refresh-1", "id_token": "id-1"} {
		raw := app.rawColumn(t, column, "google", "g-1")
		assert.True(t, strings.HasPrefix(raw, "v"), "%s is sealed: %q", column, raw)
		assert.NotContains(t, raw, plain, "%s", column)
	}
	for _, payload := range append(signIns.signInBefore, signIns.signUpBefore...) {
		for key, value := range payload {
			assert.NotContains(t, []any{"access-1", "refresh-1", "id-1"}, value, "%s holds a token", key)
		}
	}

	second := oauth.Tokens{AccessToken: "access-2", IDToken: "id-2", ExpiresAt: time.Now().Add(2 * time.Hour)}
	app.google.tokens = second
	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	require.NoError(t, err)
	account = app.accountAt(t, "google", "g-1")
	assert.Equal(t, "access-2", account.AccessToken)
	assert.Equal(t, "id-2", account.IDToken)
	assert.WithinDuration(t, second.ExpiresAt, *account.AccessTokenExpiresAt, time.Second)
	assert.Equal(t, "refresh-1", account.RefreshToken, "a refresh token the provider did not issue again is kept")
	require.NotNil(t, account.RefreshTokenExpiresAt, "with its expiry")
	assert.Equal(t, "openid email", account.Scope, "the answer named no scope")
}

// A link stores the provider's tokens like a sign-in does, both the link the
// user starts and the one a sign-in makes by a verified email.
func TestOAuthLinkStoresTheProviderTokens(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}, LinkByVerifiedEmail: []string{"google"}}, nil)
	require.NoError(t, err)
	app.google.tokens = oauth.Tokens{AccessToken: "access-1", RefreshToken: "refresh-1"}

	user := app.member(t, "grace@example.com", true)
	caller, _ := app.sessionOf(t, user.ID)
	req := app.beginLink(t, caller, app.google, "")
	app.google.identity = oauth.Identity{Subject: "g-grace"}
	_, err = app.plugin.Complete(caller, req)
	require.NoError(t, err)
	assert.Equal(t, "access-1", app.accountAt(t, "google", "g-grace").AccessToken, "an explicit link")

	app.member(t, "ada@example.com", true)
	app.google.identity = ada
	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	require.NoError(t, err)
	assert.Equal(t, "refresh-1", app.accountAt(t, "google", "g-1").RefreshToken, "a link by a verified email")
}

// Without StoreTokens the plugin keeps no token. A sign-in removes the
// tokens its account holds from a time it did.
func TestOAuthKeepsNoTokensUnlessAsked(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{}, nil)
	require.NoError(t, err)
	app.google.identity = ada
	app.google.tokens = oauth.Tokens{AccessToken: "access-1", RefreshToken: "refresh-1", IDToken: "id-1", ExpiresAt: time.Now().Add(time.Hour)}

	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	require.NoError(t, err)
	account := app.accountAt(t, "google", "g-1")
	assert.Empty(t, account.AccessToken)
	assert.Empty(t, account.RefreshToken)
	assert.Empty(t, account.IDToken)
	assert.Nil(t, account.AccessTokenExpiresAt)

	_, err = app.ac.Store.UpdateAccount(ctx, account.ID, behemoth.M{
		models.AccountAccessToken: "old-access", models.AccountRefreshToken: "old-refresh", models.AccountScope: "openid",
	})
	require.NoError(t, err)
	_, err = app.plugin.Complete(ctx, app.begin(t, app.google, ""))
	require.NoError(t, err)
	account = app.accountAt(t, "google", "g-1")
	assert.Empty(t, account.AccessToken, "the next sign-in removed them")
	assert.Empty(t, account.RefreshToken)
	assert.Empty(t, account.Scope)
}

// AccessToken hands out a stored access token that is valid, to the user and
// to a system caller acting for the user, without asking the provider.
func TestOAuthAccessToken(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	caller, _ := app.sessionOf(t, user.ID)
	expiresAt := time.Now().Add(time.Hour)
	app.linkedWithTokens(t, user, "google", "g-1", oauth.Tokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: expiresAt, Scope: "openid email"})

	for name, ctx := range map[string]context.Context{"the user": caller, "a job acting for the user": actingFor(ctx, user.ID)} {
		token, err := app.plugin.AccessToken(ctx, oauth.AccessTokenRequest{Provider: "google"})
		require.NoError(t, err, name)
		assert.Equal(t, &oauth.ProviderToken{Provider: "google", AccessToken: "access-1", ExpiresAt: token.ExpiresAt, Scope: "openid email"}, token, name)
		assert.WithinDuration(t, expiresAt, token.ExpiresAt, time.Second, name)
	}
	assert.Zero(t, app.google.refreshes(), "a valid token is not refreshed")

	// A token whose expiry the provider did not give is used as it is.
	other := app.member(t, "grace@example.com", true)
	otherCaller, _ := app.sessionOf(t, other.ID)
	app.linkedWithTokens(t, other, "google", "g-2", oauth.Tokens{AccessToken: "forever"})
	token, err := app.plugin.AccessToken(otherCaller, oauth.AccessTokenRequest{Provider: "google"})
	require.NoError(t, err)
	assert.Equal(t, "forever", token.AccessToken)
	assert.True(t, token.ExpiresAt.IsZero())
	assert.Zero(t, app.google.refreshes())
}

// An access token that expired, or expires within a minute, is refreshed
// with the stored refresh token, and the new tokens are stored. The ID token
// of the sign-in stays, and a refresh token the provider rotates replaces
// the old one.
func TestOAuthAccessTokenRefreshes(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	caller, _ := app.sessionOf(t, user.ID)
	app.linkedWithTokens(t, user, "google", "g-1", oauth.Tokens{
		AccessToken: "access-1", RefreshToken: "refresh-1", IDToken: "id-1", Scope: "openid email",
		ExpiresAt: time.Now().Add(30 * time.Second),
	})
	newExpiry := time.Now().Add(time.Hour)
	app.google.refresh = func(string) (*oauth.Tokens, error) {
		return &oauth.Tokens{AccessToken: "access-2", ExpiresAt: newExpiry}, nil
	}

	token, err := app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
	require.NoError(t, err)
	assert.Equal(t, "access-2", token.AccessToken, "a token that expires within a minute is refreshed")
	assert.WithinDuration(t, newExpiry, token.ExpiresAt, time.Second)
	assert.Equal(t, []string{"refresh-1"}, app.google.refreshed)
	account := app.accountAt(t, "google", "g-1")
	assert.Equal(t, "access-2", account.AccessToken)
	assert.Equal(t, "refresh-1", account.RefreshToken, "the provider kept the refresh token")
	assert.Equal(t, "id-1", account.IDToken, "a refresh keeps the ID token of the sign-in")
	assert.Equal(t, "openid email", account.Scope)

	_, err = app.ac.Store.UpdateAccount(ctx, account.ID, behemoth.M{models.AccountAccessTokenExpiresAt: time.Now().Add(-time.Hour).UTC()})
	require.NoError(t, err)
	app.google.refresh = func(string) (*oauth.Tokens, error) {
		return &oauth.Tokens{AccessToken: "access-3", RefreshToken: "refresh-2", ExpiresAt: newExpiry, Scope: "openid"}, nil
	}
	token, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
	require.NoError(t, err)
	assert.Equal(t, "access-3", token.AccessToken, "an expired token")
	account = app.accountAt(t, "google", "g-1")
	assert.Equal(t, "refresh-2", account.RefreshToken, "the rotated refresh token")
	assert.Equal(t, "openid", account.Scope)
	assert.Equal(t, []string{"refresh-1", "refresh-1"}, app.google.refreshed)
}

// Calls that find one account's token expired share one refresh, and every
// one of them gets the new token.
func TestOAuthAccessTokenSharesARefresh(t *testing.T) {
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	caller, _ := app.sessionOf(t, user.ID)
	app.linkedWithTokens(t, user, "google", "g-1", oauth.Tokens{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute)})
	var calls atomic.Int32
	release := make(chan struct{})
	app.google.refresh = func(string) (*oauth.Tokens, error) {
		calls.Add(1)
		<-release
		return &oauth.Tokens{AccessToken: "access-2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	const callers = 5
	got := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			token, err := app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
			if err != nil {
				got[i] = err.Error()
				return
			}
			got[i] = token.AccessToken
		})
	}
	time.Sleep(200 * time.Millisecond) // every call reaches the refresh, or waits on it
	close(release)
	wg.Wait()
	for i, token := range got {
		assert.Equal(t, "access-2", token, "call %d", i)
	}
	assert.Equal(t, int32(1), calls.Load())
}

// When no usable token can be had, AccessToken says so with one code, and
// the user signs in with the provider again. A refresh token the provider
// refuses is removed with the rest of the tokens, so the next call does not
// ask the provider again. A provider that fails is a failure of the system
// and changes nothing.
func TestOAuthAccessTokenWhenNoneIsUsable(t *testing.T) {
	ctx := context.Background()
	expired := time.Now().Add(-time.Minute)
	cases := []struct {
		name    string
		tokens  oauth.Tokens
		refresh func(string) (*oauth.Tokens, error)
		asked   int  // refreshes the provider got
		removed bool // the tokens are gone afterwards
	}{
		{"the provider refuses the refresh token", oauth.Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: expired},
			func(string) (*oauth.Tokens, error) {
				return nil, fmt.Errorf("invalid_grant: %w", oauth.ErrExchangeRefused)
			}, 1, true},
		{"no refresh token", oauth.Tokens{AccessToken: "a", ExpiresAt: expired}, nil, 0, false},
		{"a refresh token that expired", oauth.Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: expired, RefreshExpiresAt: expired}, nil, 0, false},
		{"no token stored", oauth.Tokens{}, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil)
			require.NoError(t, err)
			user := app.member(t, "ada@example.com", true)
			caller, _ := app.sessionOf(t, user.ID)
			app.linkedWithTokens(t, user, "google", "g-1", tc.tokens)
			app.google.refresh = tc.refresh

			for range 2 {
				_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
				assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderTokenUnavailable), "got %v", err)
			}
			assert.Equal(t, tc.asked, app.google.refreshes(), "the second call did not ask the provider again")
			account := app.accountAt(t, "google", "g-1")
			if tc.removed {
				assert.Empty(t, account.AccessToken)
				assert.Empty(t, account.RefreshToken)
			} else {
				assert.Equal(t, tc.tokens.AccessToken, account.AccessToken, "the tokens stay")
				assert.Equal(t, tc.tokens.RefreshToken, account.RefreshToken)
			}
		})
	}

	t.Run("a provider that can't refresh", func(t *testing.T) {
		app, err := bootOAuth(t, oauth.Options{CustomProviders: []oauth.Provider{plainProvider{id: "plain"}}, StoreTokens: []string{"plain"}}, nil)
		require.NoError(t, err)
		user := app.member(t, "ada@example.com", true)
		caller, _ := app.sessionOf(t, user.ID)
		app.linkedWithTokens(t, user, "plain", "p-1", oauth.Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: expired})
		_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "plain"})
		assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderTokenUnavailable), "got %v", err)
	})

	t.Run("the provider fails", func(t *testing.T) {
		app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil)
		require.NoError(t, err)
		user := app.member(t, "ada@example.com", true)
		caller, _ := app.sessionOf(t, user.ID)
		app.linkedWithTokens(t, user, "google", "g-1", oauth.Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: expired})
		app.google.refresh = func(string) (*oauth.Tokens, error) { return nil, errors.New("dial tcp: timeout") }
		_, err = app.plugin.AccessToken(ctx, oauth.AccessTokenRequest{Provider: "google"})
		require.Error(t, err)
		_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
		require.Error(t, err)
		assert.False(t, errs.IsRejection(err), "it is not the caller's mistake: %v", err)
		assert.Equal(t, "r", app.accountAt(t, "google", "g-1").RefreshToken, "the tokens stay for the next try")
	})
}

// AccessToken refuses what it can't serve: a provider the plugin doesn't
// have, one whose tokens are not stored, a provider the user has no account
// at, and a caller who is not signed in.
func TestOAuthAccessTokenRefusals(t *testing.T) {
	ctx := context.Background()
	providers := []oauth.Provider{&fakeProvider{id: "google"}, &fakeProvider{id: "github"}}
	app, err := bootOAuth(t, oauth.Options{CustomProviders: providers, StoreTokens: []string{"google"}}, nil)
	require.NoError(t, err)
	user := app.member(t, "ada@example.com", true)
	caller, _ := app.sessionOf(t, user.ID)

	_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "facebook"})
	assert.True(t, hasCode(err, errs.CategoryValidation, "provider_invalid_input"), "got %v", err)
	_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "github"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a provider whose tokens are not stored: %v", err)
	_, err = app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
	assert.True(t, hasCode(err, errs.CategoryConflict, oauth.ErrorCodeProviderNotLinked), "got %v", err)
	_, err = app.plugin.AccessToken(ctx, oauth.AccessTokenRequest{Provider: "google"})
	assert.True(t, hasCode(err, errs.CategorySession, errs.CodeSessionMissing), "got %v", err)

	unbooted := oauth.New(oauth.Options{CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}}, CallbackURL: "https://app.example.com/cb"})
	_, err = unbooted.AccessToken(caller, oauth.AccessTokenRequest{Provider: "google"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
}

// Unlink revokes the tokens the account held, at the provider, once the
// account is gone. A revocation that fails is logged and leaves the unlink
// done. An account without tokens has nothing to revoke.
func TestOAuthUnlinkRevokesTheTokens(t *testing.T) {
	var rec *telemetrytest.Recorder
	app, err := bootOAuth(t, oauth.Options{StoreTokens: []string{"google"}}, nil, withTelemetry(&rec))
	require.NoError(t, err)

	user := app.member(t, "ada@example.com", true)
	app.linkedWithTokens(t, user, "google", "g-1", oauth.Tokens{AccessToken: "access-1", RefreshToken: "refresh-1", Scope: "openid"})
	caller, _ := app.sessionOf(t, user.ID)
	_, err = app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "google"})
	require.NoError(t, err)
	require.Len(t, app.google.revoked, 1)
	assert.Equal(t, "access-1", app.google.revoked[0].AccessToken)
	assert.Equal(t, "refresh-1", app.google.revoked[0].RefreshToken)

	grace := app.member(t, "grace@example.com", true)
	app.linkedWithTokens(t, grace, "google", "g-2", oauth.Tokens{AccessToken: "access-2"})
	graceCaller, _ := app.sessionOf(t, grace.ID)
	app.google.revokeErr = errors.New("revocation endpoint answered 503")
	_, err = app.plugin.Unlink(graceCaller, oauth.UnlinkRequest{Provider: "google"})
	require.NoError(t, err, "the unlink stands")
	_, err = app.ac.Store.FindAccount(context.Background(), "google", "g-2")
	assert.True(t, errs.IsNotFound(err), "the account is gone: %v", err)
	warnings := rec.Logger.At(slog.LevelWarn)
	require.NotEmpty(t, warnings)
	last := warnings[len(warnings)-1]
	assert.Contains(t, last.Message, "not revoked")
	assert.Equal(t, "google", last.Fields["provider"])

	app.google.revokeErr = nil
	katherine := app.member(t, "katherine@example.com", true)
	app.linkedTo(t, katherine, "google", "g-3")
	katherineCaller, _ := app.sessionOf(t, katherine.ID)
	_, err = app.plugin.Unlink(katherineCaller, oauth.UnlinkRequest{Provider: "google"})
	require.NoError(t, err)
	assert.Len(t, app.google.revoked, 2, "no tokens, no revocation")
}

// The authorization URL of a Google provider asks for offline access when
// its tokens are stored, and only then.
func TestOAuthGoogleAsksForOfflineAccess(t *testing.T) {
	fake := newFakeIssuer(t, "https://accounts.google.com")
	for stored, want := range map[bool]string{true: "offline", false: ""} {
		opts := oauth.Options{
			Providers:  []oauth.ProviderConfig{{Kind: oauth.KindGoogle, ClientID: "client-1", ClientSecret: secretOf("g-secret")}},
			HTTPClient: fake.client(),
		}
		if stored {
			opts.StoreTokens = []string{"google"}
		}
		app, err := bootOAuth(t, opts, nil)
		require.NoError(t, err)
		begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "google"})
		require.NoError(t, err)
		authURL, err := url.Parse(begun.AuthorizationURL)
		require.NoError(t, err)
		assert.Equal(t, want, authURL.Query().Get("access_type"), "stored: %v", stored)
	}
}
