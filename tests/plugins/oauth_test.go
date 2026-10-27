package plugins_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/plugins/oauth"
	sqliteAdapter "github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/store"
	"github.com/go-behemoth/behemoth/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a Provider that keeps what the flows handed it. For Begin
// it answers with a URL that carries all of it, or with err, or with url.
// For Complete it answers with identity and tokens, or with exchangeErr. It
// refreshes with refresh, which a test sets before a refresh is due, and
// revokes by keeping the tokens, answering revokeErr.
type fakeProvider struct {
	id  string
	err error
	url string

	identity    oauth.Identity
	tokens      oauth.Tokens
	exchangeErr error
	refresh     func(refreshToken string) (*oauth.Tokens, error)
	revokeErr   error

	mu        sync.Mutex
	seen      []oauth.AuthorizationRequest
	exchanged []oauth.ExchangeRequest
	refreshed []string
	revoked   []oauth.Tokens
}

func (f *fakeProvider) ID() string { return f.id }

func (f *fakeProvider) AuthorizationURL(_ context.Context, req oauth.AuthorizationRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, req)
	if f.err != nil {
		return "", f.err
	}
	if f.url != "" {
		return f.url, nil
	}
	query := url.Values{
		"state": {req.State}, "redirect_uri": {req.RedirectURI},
		"code_challenge": {req.CodeChallenge}, "nonce": {req.Nonce},
	}
	return "https://id.example.com/authorize?" + query.Encode(), nil
}

func (f *fakeProvider) Exchange(_ context.Context, req oauth.ExchangeRequest) (*oauth.ExchangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanged = append(f.exchanged, req)
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	return &oauth.ExchangeResult{Identity: f.identity, Tokens: f.tokens}, nil
}

func (f *fakeProvider) Refresh(_ context.Context, req oauth.RefreshRequest) (*oauth.Tokens, error) {
	f.mu.Lock()
	f.refreshed = append(f.refreshed, req.RefreshToken)
	refresh := f.refresh
	f.mu.Unlock()
	if refresh == nil {
		return nil, errors.New("the test did not expect a refresh")
	}
	return refresh(req.RefreshToken)
}

func (f *fakeProvider) Revoke(_ context.Context, tokens oauth.Tokens) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, tokens)
	return f.revokeErr
}

// refreshes is how often the provider was asked to refresh a token.
func (f *fakeProvider) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refreshed)
}

// exchanges is how often the provider was asked to redeem a code.
func (f *fakeProvider) exchanges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchanged)
}

func (f *fakeProvider) last(t *testing.T) oauth.AuthorizationRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.seen, "the provider was not asked for a URL")
	return f.seen[len(f.seen)-1]
}

type oauthApp struct {
	plugin  *oauth.Plugin
	ac      *core.AuthContext
	db      *sql.DB
	google  *fakeProvider
	mounted mountedRoutes
}

// bootOAuth boots the plugin alone over a fresh database, with one trusted
// origin and the header delivery, and mounts its routes. Options left
// empty get a provider "google" and a callback URL. configure may change
// the BootConfig before Boot. It returns Boot's error, for the tests of the
// options.
func bootOAuth(t *testing.T, opts oauth.Options, hookFn func(reg core.HookRegistry) error, configure ...func(*engine.BootConfig)) (*oauthApp, error) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "oauth.db")+"?_foreign_keys=on")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(magicLinkSchema)
	require.NoError(t, err)

	app := &oauthApp{db: db, google: &fakeProvider{id: "google"}, mounted: mountedRoutes{}}
	if opts.Providers == nil && opts.CustomProviders == nil {
		opts.CustomProviders = []oauth.Provider{app.google}
	}
	if opts.CallbackURL == "" {
		opts.CallbackURL = "https://app.example.com/auth/callback"
	}
	app.plugin = oauth.New(opts)

	prepared, err := engine.Prepare([]core.Plugin{app.plugin}, engine.PrepareConfig{})
	require.NoError(t, err)
	cfg := engine.BootConfig{
		Crypto: crypto.Config{
			Secrets: crypto.StaticSecretSource{Secrets: map[int]string{1: strings.Repeat("ab", 32)}, Current: 1},
		},
		Session: core.SessionConfig{ExpiresIn: time.Hour, PendingExpiresIn: time.Minute},
		Router:  httpapi.RouterConfig{Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader}, TrustedOrigins: []string{"https://app.example.com"}},
		Hooks:   hookFn,
		HTTP:    app.mounted,
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	app.ac, err = engine.Boot(ctx, prepared, sqliteAdapter.NewSQLiteAdapter(db, nil), cfg)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { app.ac.Mailer.Close(context.Background()) })
	return app, nil
}

// tokens counts the rows of the tokens table that match where.
func (app *oauthApp) tokens(t *testing.T, where string) int {
	t.Helper()
	return app.count(t, "tokens", where)
}

// count counts the rows of table that match where.
func (app *oauthApp) count(t *testing.T, table, where string) int {
	t.Helper()
	var n int
	require.NoError(t, app.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+where).Scan(&n))
	return n
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Begin hands the provider a state, a redirect URI of its own, an S256
// challenge and a nonce, and keeps the sign-in as a single-use token whose
// metadata holds no secret as it is.
func TestOAuthBeginStartsASignIn(t *testing.T) {
	ctx := context.Background()
	var issued []behemoth.M
	app, err := bootOAuth(t, oauth.Options{}, func(reg core.HookRegistry) error {
		// What a handler on the token points gets to see of the sign-in.
		return reg.OnAfter(hooks.HookTokenAfterIssue, func(_ *core.HookContext, result any) error {
			issued = append(issued, result.(*models.Token).MetadataJSON)
			return nil
		}, nil)
	})
	require.NoError(t, err)

	result, err := app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "google", RedirectURL: "/dashboard"})
	require.NoError(t, err)
	assert.Equal(t, "google", result.Provider)
	assert.Equal(t, oauth.IntentSignIn, result.Intent)
	assert.NotEmpty(t, result.Binding)

	// The URL is the provider's, with what Begin created for the sign-in.
	sent := app.google.last(t)
	authURL, err := url.Parse(result.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "id.example.com", authURL.Host)
	assert.Equal(t, sent.State, authURL.Query().Get("state"))
	assert.Equal(t, sent.State, result.State)
	assert.Equal(t, "https://app.example.com/auth/callback/google", sent.RedirectURI)
	assert.NotEmpty(t, sent.State)
	assert.NotEmpty(t, sent.Nonce)
	assert.NotContains(t, result.AuthorizationURL, result.Binding, "the binding value is in no URL")

	// The state is the raw token of the sign-in.
	token, err := app.ac.TokenManager.Verify(ctx, core.TokenKindOAuthState, sent.State)
	require.NoError(t, err)
	assert.Equal(t, result.TransactionID, token.ID)
	assert.Empty(t, token.Subject, "nobody is signed in yet")
	assert.WithinDuration(t, time.Now().Add(oauth.DefaultStateTTL), token.ExpiresAt, 5*time.Second)
	assert.WithinDuration(t, token.ExpiresAt, result.ExpiresAt, time.Second)

	meta := token.MetadataJSON
	assert.Equal(t, "google", meta["provider"])
	assert.Equal(t, "sign_in", meta["intent"])
	assert.NotContains(t, meta, "session_id", "a sign-in is not bound to a session")
	assert.Equal(t, "/dashboard", meta["redirect_url"])
	assert.Equal(t, sent.Nonce, meta["nonce"])
	assert.Equal(t, utils.LookupHashOf(result.Binding), meta["binding_hash"], "only the hash of the binding value is kept")

	// The verifier is kept sealed, and it is the one the challenge is of.
	sealed, _ := meta["verifier_secret"].(string)
	verifier, err := app.ac.Store.Open(sealed, store.SealBinding{Purpose: "oauth.pkce_verifier", Owner: utils.LookupHashOf(result.Binding)})
	require.NoError(t, err)
	assert.Len(t, verifier, 43, "a PKCE verifier is 43 to 128 characters")
	assert.Equal(t, s256(verifier), sent.CodeChallenge)

	require.Len(t, issued, 1)
	for key, value := range issued[0] {
		assert.NotEqual(t, verifier, value, "%s holds the verifier", key)
		assert.NotEqual(t, result.Binding, value, "%s holds the binding value", key)
	}

	// A second sign-in shares nothing with the first.
	second, err := app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "google"})
	require.NoError(t, err)
	again := app.google.last(t)
	assert.NotEqual(t, sent.State, again.State)
	assert.NotEqual(t, sent.Nonce, again.Nonce)
	assert.NotEqual(t, sent.CodeChallenge, again.CodeChallenge)
	assert.NotEqual(t, result.Binding, second.Binding)
	secondToken, err := app.ac.TokenManager.Verify(ctx, core.TokenKindOAuthState, again.State)
	require.NoError(t, err)
	assert.NotContains(t, secondToken.MetadataJSON, "redirect_url", "no redirect URL was asked for")
	assert.Equal(t, 2, app.tokens(t, "kind = 'oauth_state'"))
}

// A provider the plugin does not have and a redirect URL outside the trusted
// origins are refused before anything is written.
func TestOAuthBeginRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	app, err := bootOAuth(t, oauth.Options{}, nil)
	require.NoError(t, err)

	cases := []struct {
		name string
		req  oauth.BeginRequest
		code string
	}{
		{"unknown provider", oauth.BeginRequest{Provider: "github"}, "provider_invalid_input"},
		{"no provider", oauth.BeginRequest{}, "provider_invalid_input"},
		{"redirect to another origin", oauth.BeginRequest{Provider: "google", RedirectURL: "https://evil.example.com/"}, "redirect_url_invalid_input"},
		{"redirect that a browser reads as another site", oauth.BeginRequest{Provider: "google", RedirectURL: "//evil.example.com"}, "redirect_url_invalid_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.plugin.Begin(ctx, tc.req)
			assert.True(t, hasCode(err, errs.CategoryValidation, tc.code), "got %v", err)
		})
	}
	assert.Zero(t, app.tokens(t, "1 = 1"), "a refused sign-in leaves no token")

	_, err = app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "google", RedirectURL: "https://app.example.com/welcome"})
	assert.NoError(t, err, "a URL on a trusted origin passes")
}

// The before point sees the input with its extra fields and can rewrite or
// refuse it. The after point learns that a sign-in started, without the URL
// and the binding value.
func TestOAuthBeginHooks(t *testing.T) {
	ctx := context.Background()
	var payloads []behemoth.M
	var started []*oauth.BeginResult
	var failures []core.FailureReason
	app, err := bootOAuth(t, oauth.Options{}, func(reg core.HookRegistry) error {
		if err := reg.OnBefore(oauth.HookBeginBefore, func(_ *core.HookContext, payload behemoth.M) (behemoth.M, error) {
			payloads = append(payloads, payload)
			switch payload["captcha"] {
			case "wrong":
				return nil, errs.NewUnauthorized("test", "captcha_failed", "captcha failed")
			case "rewrite":
				payload["provider"], payload["intent"] = "google", "link"
			}
			return payload, nil
		}, nil); err != nil {
			return err
		}
		if err := reg.OnAfter(oauth.HookBeginAfter, func(_ *core.HookContext, result any) error {
			started = append(started, result.(*oauth.BeginResult))
			return nil
		}, nil); err != nil {
			return err
		}
		return reg.OnFailed(oauth.HookBeginFailed, func(_ *core.HookContext, reason core.FailureReason) error {
			failures = append(failures, reason)
			return nil
		}, nil)
	})
	require.NoError(t, err)

	result, err := app.plugin.Begin(ctx, oauth.BeginRequest{
		Provider: "google", RedirectURL: "/dashboard", Extra: behemoth.M{"captcha": "ok"},
	})
	require.NoError(t, err)
	require.Len(t, payloads, 1)
	assert.Equal(t, behemoth.M{"provider": "google", "redirect_url": "/dashboard", "intent": "sign_in", "captcha": "ok"}, payloads[0])
	require.Len(t, started, 1)
	assert.Equal(t, result.TransactionID, started[0].TransactionID)
	assert.Equal(t, "google", started[0].Provider)
	assert.Equal(t, oauth.IntentSignIn, started[0].Intent)
	assert.Empty(t, started[0].AuthorizationURL, "a handler does not get the URL")
	assert.Empty(t, started[0].State, "a handler does not get the state")
	assert.Empty(t, started[0].Binding, "a handler does not get the binding value")
	assert.NotEmpty(t, result.AuthorizationURL, "the caller's result is not the handler's copy")
	assert.NotEmpty(t, result.Binding)

	// A handler's rewrite is what the flow runs with, except for the
	// intent, which is the flow's.
	rewritten, err := app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "elsewhere", Extra: behemoth.M{"captcha": "rewrite"}})
	require.NoError(t, err)
	assert.Equal(t, "google", rewritten.Provider)
	assert.Equal(t, oauth.IntentSignIn, rewritten.Intent)

	// A refusal fires the failed point and starts nothing.
	before := app.tokens(t, "1 = 1")
	_, err = app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "google", Extra: behemoth.M{"captcha": "wrong"}})
	assert.True(t, hasCode(err, errs.CategoryUnauthorized, "captcha_failed"), "the handler's error is returned: %v", err)
	require.Len(t, failures, 1)
	assert.Equal(t, core.FailureRejectedByHook, failures[0].Code)
	assert.Equal(t, before, app.tokens(t, "1 = 1"))
	assert.Len(t, started, 2, "the after point did not fire for the refused sign-in")
}

// When the provider can't give a URL, or gives one a browser can't be sent
// to, Begin fails as the system and ends the sign-in it had stored.
func TestOAuthBeginEndsASignInTheProviderFailed(t *testing.T) {
	ctx := context.Background()
	for name, broken := range map[string]*fakeProvider{
		"an error":            {id: "google", err: errors.New("discovery failed")},
		"not an http URL":     {id: "google", url: "javascript:alert(1)"},
		"not an absolute URL": {id: "google", url: "/authorize"},
	} {
		t.Run(name, func(t *testing.T) {
			app, err := bootOAuth(t, oauth.Options{CustomProviders: []oauth.Provider{broken}}, nil)
			require.NoError(t, err)

			_, err = app.plugin.Begin(ctx, oauth.BeginRequest{Provider: "google"})
			require.Error(t, err)
			assert.False(t, errs.IsRejection(err), "it is not the caller's mistake: %v", err)
			state := broken.last(t).State
			assert.NotContains(t, err.Error(), state, "the error does not quote the state")

			assert.Equal(t, 1, app.tokens(t, "revoked_at IS NOT NULL"), "the stored sign-in is ended")
			_, err = app.ac.TokenManager.Verify(ctx, core.TokenKindOAuthState, state)
			assert.Error(t, err, "its state no longer works")
		})
	}
}

// Init refuses options the plugin can't work with, so Boot fails and the
// first request does not have to.
func TestOAuthInitChecksTheOptions(t *testing.T) {
	google := func() oauth.Provider { return &fakeProvider{id: "google"} }
	cases := map[string]oauth.Options{
		"no provider":                     {CustomProviders: []oauth.Provider{}},
		"a nil provider":                  {CustomProviders: []oauth.Provider{nil}},
		"an empty id":                     {CustomProviders: []oauth.Provider{&fakeProvider{id: ""}}},
		"an id with an upper case":        {CustomProviders: []oauth.Provider{&fakeProvider{id: "Google"}}},
		"an id with a slash":              {CustomProviders: []oauth.Provider{&fakeProvider{id: "a/b"}}},
		"an id that is too long":          {CustomProviders: []oauth.Provider{&fakeProvider{id: strings.Repeat("a", 65)}}},
		"the id of password accounts":     {CustomProviders: []oauth.Provider{&fakeProvider{id: "credential"}}},
		"the same id twice":               {CustomProviders: []oauth.Provider{google(), google()}},
		"a relative callback URL":         {CustomProviders: []oauth.Provider{google()}, CallbackURL: "/auth/callback"},
		"a callback URL with a query":     {CustomProviders: []oauth.Provider{google()}, CallbackURL: "https://app.example.com/cb?x=1"},
		"a callback URL with a fragment":  {CustomProviders: []oauth.Provider{google()}, CallbackURL: "https://app.example.com/cb#x"},
		"a callback URL that is not http": {CustomProviders: []oauth.Provider{google()}, CallbackURL: "myapp://callback"},
		"a negative state TTL":            {CustomProviders: []oauth.Provider{google()}, StateTTL: -time.Minute},
		"a link by email for no provider": {CustomProviders: []oauth.Provider{google()}, LinkByVerifiedEmail: []string{"github"}},
		"stored tokens of no provider":    {CustomProviders: []oauth.Provider{google()}, StoreTokens: []string{"github"}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := bootOAuth(t, opts, nil)
			assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
		})
	}

	t.Run("several providers and a short TTL", func(t *testing.T) {
		github := &fakeProvider{id: "github"}
		app, err := bootOAuth(t, oauth.Options{
			CustomProviders: []oauth.Provider{google(), github}, CallbackURL: "http://localhost:5173/auth/callback/", StateTTL: time.Minute,
		}, nil)
		require.NoError(t, err)
		result, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "github"})
		require.NoError(t, err)
		assert.Equal(t, "http://localhost:5173/auth/callback/github", github.last(t).RedirectURI)
		assert.WithinDuration(t, time.Now().Add(time.Minute), result.ExpiresAt, 5*time.Second)
	})
}

// Begin on a plugin that Boot has not initialized is a configuration error,
// not a nil dereference.
func TestOAuthBeginBeforeBoot(t *testing.T) {
	plugin := oauth.New(oauth.Options{CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}}, CallbackURL: "https://app.example.com/cb"})
	_, err := plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "google"})
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
}

// A request keeps the fields the flow does not read, for the handlers on the
// before point, and refuses a field of the wrong type.
func TestOAuthBeginRequestMaps(t *testing.T) {
	var req oauth.BeginRequest
	require.NoError(t, req.FromMap(map[string]any{"provider": "google", "redirect_url": "/home", "captcha": "tok", "locale": "am", "intent": "link"}))
	assert.Equal(t, oauth.BeginRequest{Provider: "google", RedirectURL: "/home", Extra: behemoth.M{"captcha": "tok", "locale": "am"}}, req,
		"the intent is dropped: a body can't choose it")

	// An extra field can't stand in for one of the request's own.
	req.Extra["provider"] = "elsewhere"
	payload, err := req.ToMap()
	require.NoError(t, err)
	assert.Equal(t, "google", payload["provider"])
	assert.Equal(t, "tok", payload["captcha"])

	require.NoError(t, req.FromMap(map[string]any{}))
	assert.Equal(t, oauth.BeginRequest{}, req, "a missing key leaves its field empty, and Extra is replaced")

	assert.True(t, hasCode(req.FromMap(map[string]any{"provider": 7}), errs.CategoryValidation, "provider_invalid_input"))
	assert.True(t, hasCode(req.FromMap(map[string]any{"provider": "google", "redirect_url": []string{"/"}}), errs.CategoryValidation, "redirect_url_invalid_input"))
}

func TestOAuthRedirectURI(t *testing.T) {
	assert.Equal(t, "https://app.example.com/auth/callback/google", oauth.RedirectURI("https://app.example.com/auth/callback", "google"))
	assert.Equal(t, "https://app.example.com/auth/callback/google", oauth.RedirectURI("https://app.example.com/auth/callback/", "google"))
	assert.Equal(t, "https://app.example.com/github", oauth.RedirectURI("https://app.example.com", "github"))
}
