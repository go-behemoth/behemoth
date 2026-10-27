package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/outbound"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testGitHub answers like GitHub's token endpoint and API.
type testGitHub struct {
	mu       sync.Mutex
	token    func() (int, any)
	user     any
	emails   func() (int, any)
	grant    int // the status of the grant's deletion; 204 when zero
	requests []recorded
}

func (g *testGitHub) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		require.NoError(t, r.ParseForm())
		body, err := io.ReadAll(r.Body) // what ParseForm left: a body that is not a form
		require.NoError(t, err)
		g.requests = append(g.requests, recorded{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), form: r.PostForm, body: body})
		answer := func(status int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			require.NoError(t, json.NewEncoder(w).Encode(body))
		}
		switch r.URL.Path {
		case "/login/oauth/access_token":
			answer(g.token())
		case "/user":
			answer(http.StatusOK, g.user)
		case "/user/emails":
			answer(g.emails())
		case "/applications/gh-client/grant":
			status := g.grant
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}
}

func (g *testGitHub) last(t *testing.T, path string) recorded {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.requests) - 1; i >= 0; i-- {
		if g.requests[i].path == path {
			return g.requests[i]
		}
	}
	require.Failf(t, "no request", "nothing was sent to %s", path)
	return recorded{}
}

func newTestGitHub(t *testing.T) (*testGitHub, *githubProvider, *rewrite) {
	g := &testGitHub{
		token: func() (int, any) {
			return http.StatusOK, map[string]any{"access_token": "gho_1", "token_type": "bearer", "scope": "read:user,user:email"}
		},
		user: map[string]any{"id": 583231, "login": "octocat", "name": "The Octocat", "avatar_url": "https://avatars.example.com/u/583231"},
		emails: func() (int, any) {
			return http.StatusOK, []map[string]any{
				{"email": "octo@old.example.com", "primary": false, "verified": true},
				{"email": "octocat@example.com", "primary": true, "verified": true},
			}
		},
	}
	srv := httptest.NewTLSServer(g.serve(t))
	t.Cleanup(srv.Close)
	client, rw := clientFor(srv)
	return g, &githubProvider{
		id: "github", auth: clientAuth{clientID: "gh-client", secret: staticSecret("gh-secret")},
		scopes: defaultScopes[KindGitHub], http: providerHTTP{client: client}, now: time.Now,
	}, rw
}

func TestGitHubAuthorizationURL(t *testing.T) {
	_, p, _ := newTestGitHub(t)
	raw, err := p.AuthorizationURL(context.Background(), AuthorizationRequest{
		State: "state-1", RedirectURI: "https://app.example.com/cb/github", CodeChallenge: "challenge-1", Nonce: "unused",
	})
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, githubAuthorizeURL, u.Scheme+"://"+u.Host+u.Path)
	assert.Equal(t, url.Values{
		"client_id": {"gh-client"}, "redirect_uri": {"https://app.example.com/cb/github"},
		"scope": {"read:user user:email"}, "state": {"state-1"},
		"code_challenge": {"challenge-1"}, "code_challenge_method": {"S256"},
	}, u.Query(), "GitHub has no nonce")
}

// The subject is the numeric id, and the email is the primary one GitHub
// verified. The secret goes in the form, and every call has a user agent.
func TestGitHubExchange(t *testing.T) {
	g, p, rw := newTestGitHub(t)
	result, err := exchange(p, "")
	require.NoError(t, err)
	assert.Equal(t, Identity{
		Subject: "583231", Email: "octocat@example.com", EmailVerified: true,
		Name: "The Octocat", Picture: "https://avatars.example.com/u/583231",
	}, result.Identity)
	assert.Equal(t, []string{"github.com", "api.github.com", "api.github.com"}, rw.hosts)

	token := g.last(t, "/login/oauth/access_token")
	assert.Equal(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {"code-1"}, "redirect_uri": {"https://app.example.com/cb/acme"},
		"code_verifier": {"verifier-1"}, "client_id": {"gh-client"}, "client_secret": {"gh-secret"},
	}, token.form)
	assert.Equal(t, "application/json", token.header.Get("Accept"), "or GitHub answers with a form")
	user := g.last(t, "/user")
	assert.Equal(t, "Bearer gho_1", user.header.Get("Authorization"))
	assert.Equal(t, outbound.DefaultUserAgent, user.header.Get("User-Agent"))
}

// Without a primary address GitHub verified, or without the scope to read
// addresses, the identity has no email.
func TestGitHubEmails(t *testing.T) {
	cases := map[string]func() (int, any){
		"the primary address is not verified": func() (int, any) {
			return http.StatusOK, []map[string]any{{"email": "octocat@example.com", "primary": true, "verified": false}}
		},
		"no scope to read addresses": func() (int, any) { return http.StatusNotFound, map[string]any{"message": "Not Found"} },
	}
	for name, emails := range cases {
		t.Run(name, func(t *testing.T) {
			g, p, _ := newTestGitHub(t)
			g.emails = emails
			result, err := exchange(p, "")
			require.NoError(t, err)
			assert.Empty(t, result.Identity.Email)
			assert.False(t, result.Identity.EmailVerified)
		})
	}

	g, p, _ := newTestGitHub(t)
	g.emails = func() (int, any) { return http.StatusInternalServerError, map[string]any{} }
	_, err := exchange(p, "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExchangeRefused)
}

// GitHub answers a code it refuses with 200 and an error, which refuses
// the sign-in.
func TestGitHubRefusedCode(t *testing.T) {
	g, p, _ := newTestGitHub(t)
	g.token = func() (int, any) {
		return http.StatusOK, map[string]any{"error": "bad_verification_code", "error_description": "The code passed is incorrect or expired."}
	}
	_, err := exchange(p, "")
	assert.True(t, errors.Is(err, ErrExchangeRefused), "%v", err)

	g.token = func() (int, any) { return http.StatusOK, map[string]any{"access_token": "gho_1"} }
	g.user = map[string]any{"login": "octocat"}
	_, err = exchange(p, "")
	require.Error(t, err, "a user without an id")
	assert.NotErrorIs(t, err, ErrExchangeRefused)
}

// The tokens of an exchange: an OAuth App's never expire, and a GitHub App's
// expire, with a refresh token that expires too. GitHub's commas between
// scopes become spaces.
func TestGitHubExchangeTokens(t *testing.T) {
	g, p, _ := newTestGitHub(t)
	result, err := exchange(p, "")
	require.NoError(t, err)
	assert.Equal(t, Tokens{AccessToken: "gho_1", Scope: "read:user user:email"}, result.Tokens, "an OAuth App's token")

	g.token = func() (int, any) {
		return http.StatusOK, map[string]any{
			"access_token": "ghu_1", "token_type": "bearer", "scope": "",
			"expires_in": 28800, "refresh_token": "ghr_1", "refresh_token_expires_in": "15897600",
		}
	}
	before := time.Now()
	result, err = exchange(p, "")
	require.NoError(t, err)
	tokens := result.Tokens
	assert.Equal(t, "ghu_1", tokens.AccessToken)
	assert.Equal(t, "ghr_1", tokens.RefreshToken)
	assert.Equal(t, "read:user user:email", tokens.Scope, "no scope in the answer is the one asked for")
	assert.WithinDuration(t, before.Add(8*time.Hour), tokens.ExpiresAt, 5*time.Second)
	assert.WithinDuration(t, before.Add(15897600*time.Second), tokens.RefreshExpiresAt, 5*time.Second, "a lifetime sent as a string")
}

// A refresh posts the refresh token with the client's credentials, and a
// refresh token GitHub refuses wraps ErrExchangeRefused.
func TestGitHubRefresh(t *testing.T) {
	g, p, _ := newTestGitHub(t)
	g.token = func() (int, any) {
		return http.StatusOK, map[string]any{"access_token": "ghu_2", "expires_in": 28800, "refresh_token": "ghr_2", "refresh_token_expires_in": 15897600}
	}
	tokens, err := p.Refresh(context.Background(), RefreshRequest{RefreshToken: "ghr_1"})
	require.NoError(t, err)
	assert.Equal(t, "ghu_2", tokens.AccessToken)
	assert.Equal(t, "ghr_2", tokens.RefreshToken, "GitHub rotates the refresh token")
	assert.False(t, tokens.ExpiresAt.IsZero())
	assert.Equal(t, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {"ghr_1"}, "client_id": {"gh-client"}, "client_secret": {"gh-secret"},
	}, g.last(t, "/login/oauth/access_token").form)

	g.token = func() (int, any) { return http.StatusOK, map[string]any{"error": "bad_refresh_token"} }
	_, err = p.Refresh(context.Background(), RefreshRequest{RefreshToken: "ghr_1"})
	assert.ErrorIs(t, err, ErrExchangeRefused)
}

// Revoking deletes the application's grant, with the client's credentials
// and the user's access token. A token GitHub does not take is an error.
func TestGitHubRevoke(t *testing.T) {
	g, p, rw := newTestGitHub(t)
	require.NoError(t, p.Revoke(context.Background(), Tokens{AccessToken: "gho_1", RefreshToken: "ghr_1"}))
	grant := g.last(t, "/applications/gh-client/grant")
	assert.Equal(t, http.MethodDelete, grant.method)
	assert.JSONEq(t, `{"access_token":"gho_1"}`, string(grant.body))
	clientID, secret, ok := (&http.Request{Header: grant.header}).BasicAuth()
	require.True(t, ok)
	assert.Equal(t, "gh-client", clientID)
	assert.Equal(t, "gh-secret", secret)
	assert.Equal(t, githubAPIVersion, grant.header.Get("X-Github-Api-Version"))
	assert.Equal(t, []string{"api.github.com"}, rw.hosts)

	g.grant = http.StatusUnprocessableEntity
	assert.Error(t, p.Revoke(context.Background(), Tokens{AccessToken: "gho_expired"}))

	require.NoError(t, p.Revoke(context.Background(), Tokens{}), "nothing to revoke")
	assert.Len(t, rw.hosts, 2, "and no call for it")
}
