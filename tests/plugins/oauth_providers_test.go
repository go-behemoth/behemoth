package plugins_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/outbound"
	"github.com/go-behemoth/behemoth/plugins/oauth"
	"github.com/go-behemoth/behemoth/telemetry"
	"github.com/go-behemoth/behemoth/telemetry/telemetrytest"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIssuer stands in for a provider over TLS: an OpenID Connect issuer
// (discovery, keys, token endpoint), and GitHub's token endpoint and API.
// It signs the ID token with the nonce of the authorization URL the test
// hands it.
type fakeIssuer struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	issuer string // the issuer the documents name: the server's URL, or Google's

	mu        sync.Mutex
	subject   string
	email     string
	nonce     string
	idIss     string // the iss of the next ID token; the issuer when empty
	expiresIn int    // the lifetime of an access token a code redeems, in seconds; none when zero
	github    []map[string]any
	token     []url.Values
	auth      []string
	hosts     []string
	agents    []string
	grants    []string // the access tokens of GitHub grants deleted
}

func newFakeIssuer(t *testing.T, issuer string) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeIssuer{key: key, subject: "user-42", email: "ada@example.com"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(f.srv.Close)
	f.issuer = issuer
	if f.issuer == "" {
		f.issuer = f.srv.URL
	}
	return f
}

func (f *fakeIssuer) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NoError(t, r.ParseForm())
	answer := func(body any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}
	base := f.issuer
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		answer(map[string]any{
			"issuer": f.issuer, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "jwks_uri": base + "/jwks",
		})
	case "/jwks":
		answer(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	case "/token":
		f.token = append(f.token, r.PostForm)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if r.PostForm.Get("grant_type") == "refresh_token" {
			answer(map[string]any{"access_token": "access-2", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		iss := f.idIss
		if iss == "" {
			iss = f.issuer
		}
		idToken := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": iss, "aud": "client-1", "sub": f.subject, "nonce": f.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
			"email": f.email, "email_verified": true, "given_name": "Ada",
		})
		idToken.Header["kid"] = "k1"
		signed, err := idToken.SignedString(f.key)
		require.NoError(t, err)
		tokens := map[string]any{"access_token": "access-1", "token_type": "Bearer", "id_token": signed, "refresh_token": "refresh-1"}
		if f.expiresIn > 0 {
			tokens["expires_in"] = f.expiresIn
		}
		answer(tokens)
	case "/login/oauth/access_token":
		f.token = append(f.token, r.PostForm)
		answer(map[string]any{"access_token": "gho_1", "token_type": "bearer"})
	case "/applications/gh-client/grant":
		var body struct {
			AccessToken string `json:"access_token"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if id, secret, ok := r.BasicAuth(); ok && id == "gh-client" && secret == "gh-secret" {
			f.grants = append(f.grants, body.AccessToken)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	case "/user":
		answer(map[string]any{"id": 583231, "name": "The Octocat", "avatar_url": "https://avatars.example.com/u/583231"})
	case "/user/emails":
		answer(f.github)
	default:
		http.NotFound(w, r)
	}
}

// transport sends every request to the fake, whichever host it names, and
// records the hosts and the user agents.
func (f *fakeIssuer) transport() http.RoundTripper {
	target, _ := url.Parse(f.srv.URL)
	base := f.srv.Client().Transport
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		f.mu.Lock()
		f.hosts = append(f.hosts, req.URL.Host)
		f.agents = append(f.agents, req.Header.Get("User-Agent"))
		f.mu.Unlock()
		out := req.Clone(req.Context())
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		return base.RoundTrip(out)
	})
}

// client is a client of the plugin's own, Options.HTTPClient, that sends
// everything to the fake.
func (f *fakeIssuer) client() *http.Client {
	return outbound.NewClient(outbound.Options{Transport: f.transport()}, nil)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

// expect makes the next ID token carry the nonce of an authorization URL.
func (f *fakeIssuer) expect(t *testing.T, authorizationURL string) url.Values {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	require.NoError(t, err)
	f.mu.Lock()
	f.nonce = u.Query().Get("nonce")
	f.mu.Unlock()
	return u.Query()
}

// rotatingSecret is a SecretSource whose current secret a test changes.
type rotatingSecret struct {
	mu      sync.Mutex
	secrets map[int]string
	current int
}

func (s *rotatingSecret) Load(context.Context) (map[int]string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secrets, s.current, nil
}

func secretOf(value string) crypto.SecretSource {
	return crypto.StaticSecretSource{Secrets: map[int]string{1: value}, Current: 1}
}

// A generic OpenID Connect provider, configured as data, signs a user in
// through the two routes: the user is created on the first sign-in and
// found by the subject on the next one.
func TestOAuthWithAnOIDCProvider(t *testing.T) {
	fake := newFakeIssuer(t, "")
	secret := &rotatingSecret{secrets: map[int]string{1: "first"}, current: 1}
	app, err := bootOAuth(t, oauth.Options{
		Providers:  []oauth.ProviderConfig{{Kind: oauth.KindOIDC, ID: "acme", Issuer: fake.srv.URL, ClientID: "client-1", ClientSecret: secret}},
		HTTPClient: fake.client(),
	}, nil, withDelivery(httpapi.DeliveryCookie))
	require.NoError(t, err)

	signIn := func(iss string) routeAnswer {
		t.Helper()
		begun := app.post(t, oauth.PathBegin, `{"provider":"acme"}`)
		require.Equal(t, http.StatusOK, begun.StatusCode, begun.body)
		query := fake.expect(t, begun.body["authorization_url"].(string))
		assert.Equal(t, "https://app.example.com/auth/callback/acme", query.Get("redirect_uri"))
		assert.Equal(t, "S256", query.Get("code_challenge_method"))
		return app.post(t, oauth.PathComplete, completeBodyFor("acme", query.Get("state"), `,"iss":"`+iss+`"`),
			begun.cookie(t, "__Host-"+oauth.BindingCookiePrefix))
	}

	first := signIn(fake.srv.URL)
	require.Equal(t, http.StatusOK, first.StatusCode, first.body)
	user := first.body["user"].(map[string]any)
	assert.Equal(t, "ada@example.com", user["email"])
	assert.Equal(t, true, user["email_verified"])
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:first")), fake.auth[0])

	// The secret is rotated at the provider and in the source.
	secret.mu.Lock()
	secret.secrets[2], secret.current = "second", 2
	secret.mu.Unlock()
	fake.email = "renamed@example.com"
	again := signIn("")
	require.Equal(t, http.StatusOK, again.StatusCode, again.body)
	assert.Equal(t, user["id"], again.body["user"].(map[string]any)["id"], "the same subject is the same user")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:second")), fake.auth[1], "the new secret is used")

	refused := signIn("https://evil.example.com")
	assert.Equal(t, http.StatusUnauthorized, refused.StatusCode, "an iss parameter of another issuer")
	assert.Equal(t, oauth.ErrorCodeInvalidCallback, refused.body["code"])
}

// Google is the generic provider with Google's issuer, and accepts the
// issuer without its scheme, which Google's ID tokens may carry.
func TestOAuthWithGoogle(t *testing.T) {
	fake := newFakeIssuer(t, "https://accounts.google.com")
	app, err := bootOAuth(t, oauth.Options{
		Providers:  []oauth.ProviderConfig{{Kind: oauth.KindGoogle, ClientID: "client-1", ClientSecret: secretOf("g-secret")}},
		HTTPClient: fake.client(),
	}, nil)
	require.NoError(t, err)

	for _, iss := range []string{"https://accounts.google.com", "accounts.google.com"} {
		fake.idIss = iss
		begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "google"})
		require.NoError(t, err)
		authURL, _ := url.Parse(begun.AuthorizationURL)
		assert.Equal(t, "accounts.google.com", authURL.Host)
		assert.Equal(t, "openid email profile", authURL.Query().Get("scope"))
		fake.expect(t, begun.AuthorizationURL)

		result, err := app.plugin.Complete(context.Background(), oauth.CompleteRequest{
			Provider: "google", Code: "code-1", State: begun.State, Binding: begun.Binding,
		})
		require.NoError(t, err, "iss %q", iss)
		assert.Equal(t, "ada@example.com", result.User.Email)
	}
	assert.Equal(t, 1, app.count(t, "accounts", "provider_id = 'google' AND account_id = 'user-42'"))
	assert.Subset(t, fake.hosts, []string{"accounts.google.com"})
}

// GitHub has no ID token: the user comes from its API, keyed by the numeric
// id, with the primary address GitHub verified. Without one, a first
// sign-in is refused.
func TestOAuthWithGitHub(t *testing.T) {
	fake := newFakeIssuer(t, "")
	app, err := bootOAuth(t, oauth.Options{
		Providers:  []oauth.ProviderConfig{{Kind: oauth.KindGitHub, ClientID: "gh-client", ClientSecret: secretOf("gh-secret")}},
		HTTPClient: fake.client(),
	}, nil)
	require.NoError(t, err)
	signIn := func() (*oauth.CompleteResult, error) {
		begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "github"})
		require.NoError(t, err)
		authURL, _ := url.Parse(begun.AuthorizationURL)
		assert.Equal(t, "github.com", authURL.Host)
		return app.plugin.Complete(context.Background(), oauth.CompleteRequest{
			Provider: "github", Code: "code-1", State: begun.State, Binding: begun.Binding,
		})
	}

	fake.github = []map[string]any{{"email": "octocat@example.com", "primary": true, "verified": false}}
	_, err = signIn()
	assert.True(t, hasCode(err, errs.CategoryForbidden, oauth.ErrorCodeEmailNotProvided), "no verified address: %v", err)

	fake.github = []map[string]any{{"email": "octocat@example.com", "primary": true, "verified": true}}
	result, err := signIn()
	require.NoError(t, err)
	assert.Equal(t, "octocat@example.com", result.User.Email)
	assert.True(t, result.User.EmailVerified)
	assert.Equal(t, "The Octocat", result.User.Firstname)
	assert.Equal(t, 1, app.count(t, "accounts", "provider_id = 'github' AND account_id = '583231'"))
	assert.Equal(t, "gh-secret", fake.token[len(fake.token)-1].Get("client_secret"))
	assert.Subset(t, fake.hosts, []string{"github.com", "api.github.com"})
}

// Without a client of its own, the plugin calls out with the one Boot
// builds from BootConfig.Outbound: its transport and its user agent, with a
// span per call on the application's telemetry. A client of its own wins.
func TestOAuthUsesTheSharedClient(t *testing.T) {
	signIn := func(t *testing.T, fake *fakeIssuer, opts oauth.Options, configure func(*engine.BootConfig)) {
		t.Helper()
		fake.github = []map[string]any{{"email": "octocat@example.com", "primary": true, "verified": true}}
		opts.Providers = []oauth.ProviderConfig{{Kind: oauth.KindGitHub, ClientID: "gh-client", ClientSecret: secretOf("gh-secret")}}
		app, err := bootOAuth(t, opts, nil, configure)
		require.NoError(t, err)
		begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "github"})
		require.NoError(t, err)
		_, err = app.plugin.Complete(context.Background(), oauth.CompleteRequest{
			Provider: "github", Code: "code-1", State: begun.State, Binding: begun.Binding,
		})
		require.NoError(t, err)
	}

	t.Run("the shared client", func(t *testing.T) {
		fake := newFakeIssuer(t, "")
		tel, rec := telemetrytest.New()
		signIn(t, fake, oauth.Options{}, func(cfg *engine.BootConfig) {
			cfg.Telemetry = tel
			cfg.Outbound = outbound.Options{UserAgent: "shop-auth", Transport: fake.transport()}
		})
		assert.Equal(t, []string{"github.com", "api.github.com", "api.github.com"}, fake.hosts)
		assert.Equal(t, []string{"shop-auth", "shop-auth", "shop-auth"}, fake.agents)

		spans := rec.Tracer.Named(telemetry.SpanOutboundRequest)
		require.Len(t, spans, 3, "a span per call")
		for i, method := range []string{http.MethodPost, http.MethodGet, http.MethodGet} {
			assert.Equal(t, method, spans[i].Attrs()[telemetry.AttrMethod])
			assert.Equal(t, fake.hosts[i], spans[i].Attrs()[telemetry.AttrHost])
			assert.Equal(t, http.StatusOK, spans[i].Attrs()[telemetry.AttrStatus])
			assert.Equal(t, 1, spans[i].Ended())
		}
	})

	t.Run("a client of the plugin's own", func(t *testing.T) {
		fake := newFakeIssuer(t, "")
		signIn(t, fake, oauth.Options{HTTPClient: fake.client()}, func(cfg *engine.BootConfig) {
			cfg.Outbound.Transport = roundTripper(func(req *http.Request) (*http.Response, error) {
				t.Errorf("the shared client called %s", req.URL.Host)
				return nil, errors.New("not this client")
			})
		})
		assert.Len(t, fake.hosts, 3)
		assert.Equal(t, outbound.DefaultUserAgent, fake.agents[0])
	})

	t.Run("a negative timeout fails Boot", func(t *testing.T) {
		_, err := bootOAuth(t, oauth.Options{}, nil, func(cfg *engine.BootConfig) { cfg.Outbound.Timeout = -time.Second })
		assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
	})
}

// A configuration the plugin can't work with fails Boot, and one that names
// a provider that is down does not: Init calls no provider.
func TestOAuthChecksTheProviderConfigs(t *testing.T) {
	secret := secretOf("s3cret")
	oidc := func(change func(*oauth.ProviderConfig)) oauth.Options {
		cfg := oauth.ProviderConfig{Kind: oauth.KindOIDC, ID: "acme", Issuer: "https://login.example.com", ClientID: "client-1", ClientSecret: secret}
		change(&cfg)
		return oauth.Options{Providers: []oauth.ProviderConfig{cfg}}
	}
	cases := map[string]oauth.Options{
		"an unknown kind":                              oidc(func(c *oauth.ProviderConfig) { c.Kind = "facebook" }),
		"an OpenID Connect provider without an id":     oidc(func(c *oauth.ProviderConfig) { c.ID = "" }),
		"an OpenID Connect provider without an issuer": oidc(func(c *oauth.ProviderConfig) { c.Issuer = "" }),
		"an issuer over http":                          oidc(func(c *oauth.ProviderConfig) { c.Issuer = "http://login.example.com" }),
		"an issuer with a query":                       oidc(func(c *oauth.ProviderConfig) { c.Issuer = "https://login.example.com?tenant=1" }),
		"no client id":                                 oidc(func(c *oauth.ProviderConfig) { c.ClientID = "" }),
		"scopes without openid":                        oidc(func(c *oauth.ProviderConfig) { c.Scopes = []string{"email"} }),
		"a secret source without a current secret": oidc(func(c *oauth.ProviderConfig) {
			c.ClientSecret = crypto.StaticSecretSource{Secrets: map[int]string{1: "old"}, Current: 2}
		}),
		"an id a provider of the application has": {
			Providers:       []oauth.ProviderConfig{{Kind: oauth.KindGoogle, ClientID: "c", ClientSecret: secret}},
			CustomProviders: []oauth.Provider{&fakeProvider{id: "google"}},
		},
		"Google without a secret":             {Providers: []oauth.ProviderConfig{{Kind: oauth.KindGoogle, ClientID: "c"}}},
		"GitHub without a secret":             {Providers: []oauth.ProviderConfig{{Kind: oauth.KindGitHub, ClientID: "c"}}},
		"Google with an issuer":               {Providers: []oauth.ProviderConfig{{Kind: oauth.KindGoogle, ClientID: "c", ClientSecret: secret, Issuer: "https://login.example.com"}}},
		"GitHub with an id of the wrong form": {Providers: []oauth.ProviderConfig{{Kind: oauth.KindGitHub, ID: "Git Hub", ClientID: "c", ClientSecret: secret}}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := bootOAuth(t, opts, nil)
			assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "got %v", err)
		})
	}

	t.Run("a provider that is down, a public client, and a loopback issuer", func(t *testing.T) {
		app, err := bootOAuth(t, oauth.Options{Providers: []oauth.ProviderConfig{
			{Kind: oauth.KindOIDC, ID: "down", Issuer: "https://login.invalid", ClientID: "c"},
			{Kind: oauth.KindOIDC, ID: "local", Issuer: "http://localhost:8080/realms/dev", ClientID: "c", ClientSecret: secret},
			{Kind: oauth.KindGitHub, ClientID: "c", ClientSecret: secret, Scopes: []string{"read:user"}},
		}}, nil)
		require.NoError(t, err)

		_, err = app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "down"})
		require.Error(t, err, "the provider is called at the first sign-in")
		assert.False(t, errs.IsRejection(err))
		assert.NotContains(t, strings.ToLower(err.Error()), "s3cret")
	})
}

// With its tokens stored, an OpenID Connect provider's tokens land in the
// account at the sign-in, and AccessToken refreshes an access token that
// expires within a minute at the issuer's token endpoint.
func TestOAuthRefreshesAtTheIssuer(t *testing.T) {
	fake := newFakeIssuer(t, "")
	fake.expiresIn = 30
	app, err := bootOAuth(t, oauth.Options{
		Providers:   []oauth.ProviderConfig{{Kind: oauth.KindOIDC, ID: "acme", Issuer: fake.srv.URL, ClientID: "client-1", ClientSecret: secretOf("s3cret")}},
		HTTPClient:  fake.client(),
		StoreTokens: []string{"acme"},
	}, nil)
	require.NoError(t, err)

	begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "acme"})
	require.NoError(t, err)
	fake.expect(t, begun.AuthorizationURL)
	signedIn, err := app.plugin.Complete(context.Background(), oauth.CompleteRequest{
		Provider: "acme", Code: "code-1", State: begun.State, Binding: begun.Binding,
	})
	require.NoError(t, err)
	account, err := app.ac.Store.FindAccount(context.Background(), "acme", "user-42")
	require.NoError(t, err)
	assert.Equal(t, "access-1", account.AccessToken)
	assert.Equal(t, "refresh-1", account.RefreshToken)
	assert.NotEmpty(t, account.IDToken, "the ID token the provider checked")
	assert.Equal(t, "openid email profile", account.Scope)

	caller, _ := app.sessionOf(t, signedIn.User.ID)
	token, err := app.plugin.AccessToken(caller, oauth.AccessTokenRequest{Provider: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "access-2", token.AccessToken)
	assert.WithinDuration(t, time.Now().Add(time.Hour), token.ExpiresAt, 5*time.Second)
	refresh := fake.token[len(fake.token)-1]
	assert.Equal(t, "refresh_token", refresh.Get("grant_type"))
	assert.Equal(t, "refresh-1", refresh.Get("refresh_token"))
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:s3cret")), fake.auth[len(fake.auth)-1])
}

// Unlinking a GitHub account whose tokens are stored deletes the
// application's grant at GitHub.
func TestOAuthRevokesAtGitHub(t *testing.T) {
	fake := newFakeIssuer(t, "")
	fake.github = []map[string]any{{"email": "octocat@example.com", "primary": true, "verified": true}}
	app, err := bootOAuth(t, oauth.Options{
		Providers:   []oauth.ProviderConfig{{Kind: oauth.KindGitHub, ClientID: "gh-client", ClientSecret: secretOf("gh-secret")}},
		HTTPClient:  fake.client(),
		StoreTokens: []string{"github"},
	}, nil)
	require.NoError(t, err)
	begun, err := app.plugin.Begin(context.Background(), oauth.BeginRequest{Provider: "github"})
	require.NoError(t, err)
	signedIn, err := app.plugin.Complete(context.Background(), oauth.CompleteRequest{
		Provider: "github", Code: "code-1", State: begun.State, Binding: begun.Binding,
	})
	require.NoError(t, err)
	// A password, so that the GitHub account is not the last way in.
	require.NoError(t, app.ac.Store.CreateAccount(context.Background(), &models.Account{
		UserID: signedIn.User.ID, ProviderID: models.ProviderCredential, AccountID: signedIn.User.ID, PasswordHash: "$argon2id$not-a-real-hash",
	}))

	caller, _ := app.sessionOf(t, signedIn.User.ID)
	_, err = app.plugin.Unlink(caller, oauth.UnlinkRequest{Provider: "github"})
	require.NoError(t, err)
	assert.Equal(t, []string{"gho_1"}, fake.grants)
	assert.Contains(t, fake.hosts, "api.github.com")
}
