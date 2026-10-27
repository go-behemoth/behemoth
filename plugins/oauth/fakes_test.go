package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/outbound"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// staticSecret is a SecretSource with one current secret.
type staticSecret string

func (s staticSecret) Load(context.Context) (map[int]string, int, error) {
	return map[int]string{1: string(s)}, 1, nil
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

// recorded is one request a fake provider received.
type recorded struct {
	method string
	path   string
	header http.Header
	form   url.Values
	body   []byte // a body that is not a form
}

// testIssuer is an OpenID Connect issuer over TLS: a discovery document, a
// key set, a token endpoint and a userinfo endpoint, each answering what
// the test sets.
type testIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	doc      map[string]any
	keys     []map[string]any
	jwksDown bool // the key set answers 503
	token    func(form url.Values) (int, any)
	userinfo func(r *http.Request) (int, any)
	revoke   func(form url.Values) int // the revocation endpoint's status; 200 when nil
	requests []recorded
	fetches  map[string]int
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	iss := &testIssuer{t: t, key: rsaKey(t, 2048), fetches: map[string]int{}}
	iss.srv = httptest.NewTLSServer(http.HandlerFunc(iss.serve))
	t.Cleanup(iss.srv.Close)
	iss.doc = map[string]any{
		"issuer":                 iss.srv.URL,
		"authorization_endpoint": iss.srv.URL + "/authorize?prompt=select_account",
		"token_endpoint":         iss.srv.URL + "/token",
		"jwks_uri":               iss.srv.URL + "/jwks",
		"userinfo_endpoint":      iss.srv.URL + "/userinfo",
	}
	iss.keys = []map[string]any{rsaJWK("k1", &iss.key.PublicKey)}
	return iss
}

func (iss *testIssuer) serve(w http.ResponseWriter, r *http.Request) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	require.NoError(iss.t, r.ParseForm())
	iss.requests = append(iss.requests, recorded{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), form: r.PostForm})
	iss.fetches[r.URL.Path]++
	answer := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		require.NoError(iss.t, json.NewEncoder(w).Encode(body))
	}
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		answer(http.StatusOK, iss.doc)
	case "/jwks", "/jwks2":
		// The keys are at the jwks_uri of the document, and nowhere else.
		if iss.jwksDown || iss.doc["jwks_uri"] != iss.srv.URL+r.URL.Path {
			answer(http.StatusServiceUnavailable, map[string]any{})
			return
		}
		answer(http.StatusOK, map[string]any{"keys": iss.keys})
	case "/token":
		if iss.token == nil {
			answer(http.StatusInternalServerError, map[string]any{})
			return
		}
		answer(iss.token(r.PostForm))
	case "/userinfo":
		if iss.userinfo == nil {
			answer(http.StatusNotFound, map[string]any{})
			return
		}
		answer(iss.userinfo(r))
	case "/revoke":
		status := http.StatusOK
		if iss.revoke != nil {
			status = iss.revoke(r.PostForm)
		}
		w.WriteHeader(status)
	default:
		http.NotFound(w, r)
	}
}

// last returns the last request to path.
func (iss *testIssuer) last(t *testing.T, path string) recorded {
	t.Helper()
	iss.mu.Lock()
	defer iss.mu.Unlock()
	for i := len(iss.requests) - 1; i >= 0; i-- {
		if iss.requests[i].path == path {
			return iss.requests[i]
		}
	}
	require.Failf(t, "no request", "nothing was sent to %s", path)
	return recorded{}
}

func (iss *testIssuer) count(path string) int {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	return iss.fetches[path]
}

func (iss *testIssuer) set(fn func(iss *testIssuer)) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	fn(iss)
}

// claims returns the claims of a valid ID token for nonce at the time now.
func (iss *testIssuer) claims(nonce string, now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": iss.srv.URL, "aud": "client-1", "sub": "user-42", "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"email": "ada@example.com", "email_verified": true, "name": "Ada Lovelace",
		"given_name": "Ada", "family_name": "Lovelace", "picture": "https://img.example.com/ada.png",
	}
}

// answerWith makes the token endpoint answer with idToken.
func (iss *testIssuer) answerWith(idToken string) {
	iss.set(func(iss *testIssuer) {
		iss.token = func(url.Values) (int, any) {
			return http.StatusOK, map[string]any{"access_token": "access-1", "token_type": "Bearer", "id_token": idToken}
		}
	})
}

// provider returns the generic provider for the issuer, with clock.
func (iss *testIssuer) provider(clock *testClock) *oidcProvider {
	return &oidcProvider{
		id: "acme", issuer: iss.srv.URL, issuers: []string{iss.srv.URL},
		auth:   clientAuth{clientID: "client-1", secret: staticSecret("s3cret")},
		scopes: defaultScopes[KindOIDC], http: providerHTTP{client: outbound.NewClient(outbound.Options{Transport: iss.srv.Client().Transport}, nil)},
		now: clock.now,
	}
}

func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

func rsaKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	return key
}

func ecKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, key *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
	}
}

func ecJWK(kid string, key *ecdsa.PublicKey) map[string]any {
	pad := func(n *big.Int) string { b := make([]byte, 32); n.FillBytes(b); return b64(b) }
	return map[string]any{"kty": "EC", "kid": kid, "crv": "P-256", "x": pad(key.X), "y": pad(key.Y)}
}

// rewrite is a transport that sends every request to target, whatever host
// its URL names, so a provider with built-in endpoints talks to a test
// server.
type rewrite struct {
	target *url.URL
	base   http.RoundTripper
	mu     sync.Mutex
	hosts  []string
}

func (rw *rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	rw.mu.Lock()
	rw.hosts = append(rw.hosts, req.URL.Host)
	rw.mu.Unlock()
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = rw.target.Scheme, rw.target.Host
	return rw.base.RoundTrip(out)
}

// clientFor returns a client that sends everything to srv.
func clientFor(srv *httptest.Server) (*http.Client, *rewrite) {
	target, _ := url.Parse(srv.URL)
	rw := &rewrite{target: target, base: srv.Client().Transport}
	return outbound.NewClient(outbound.Options{Transport: rw}, nil), rw
}
