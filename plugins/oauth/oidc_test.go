package oauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/keysource"
	"github.com/go-behemoth/behemoth/outbound"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNonce = "nonce-1"

func exchange(p Provider, nonce string) (*ExchangeResult, error) {
	return p.Exchange(context.Background(), ExchangeRequest{
		Code: "code-1", RedirectURI: "https://app.example.com/cb/acme", CodeVerifier: "verifier-1", Nonce: nonce,
	})
}

// The authorization URL is the issuer's endpoint with the request's values,
// and the parameters the endpoint already had.
func TestOIDCAuthorizationURL(t *testing.T) {
	iss := newTestIssuer(t)
	p := iss.provider(&testClock{at: time.Now()})

	raw, err := p.AuthorizationURL(context.Background(), AuthorizationRequest{
		State: "state-1", RedirectURI: "https://app.example.com/cb/acme", CodeChallenge: "challenge-1", Nonce: testNonce,
	})
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, iss.srv.URL+"/authorize", u.Scheme+"://"+u.Host+u.Path)
	assert.Equal(t, url.Values{
		"prompt": {"select_account"}, "response_type": {"code"}, "client_id": {"client-1"},
		"redirect_uri": {"https://app.example.com/cb/acme"}, "scope": {"openid email profile"},
		"state": {"state-1"}, "nonce": {testNonce},
		"code_challenge": {"challenge-1"}, "code_challenge_method": {"S256"},
	}, u.Query())
}

// A sign-in redeems the code with HTTP Basic, checks the ID token, and
// reads the claims. The discovery document and the keys are fetched once.
func TestOIDCExchange(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))

	result, err := exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, Identity{
		Subject: "user-42", Email: "ada@example.com", EmailVerified: true, Name: "Ada Lovelace",
		GivenName: "Ada", FamilyName: "Lovelace", Picture: "https://img.example.com/ada.png",
	}, result.Identity)

	token := iss.last(t, "/token")
	assert.Equal(t, http.MethodPost, token.method)
	assert.Equal(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {"code-1"},
		"redirect_uri": {"https://app.example.com/cb/acme"}, "code_verifier": {"verifier-1"},
	}, token.form, "the secret is not in the form")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:s3cret")), token.header.Get("Authorization"))
	assert.Equal(t, outbound.DefaultUserAgent, token.header.Get("User-Agent"))

	_, err = exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, 1, iss.count("/.well-known/openid-configuration"), "the document is cached")
	assert.Equal(t, 1, iss.count("/jwks"), "the keys are cached")
	assert.Zero(t, iss.count("/userinfo"), "the ID token had the email")
}

// Every check of OpenID Connect Core on an ID token refuses the sign-in
// with ErrExchangeRefused.
func TestOIDCRefusesBadIDTokens(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	other := rsaKey(t, 2048)
	small := rsaKey(t, 1024)
	iss.set(func(iss *testIssuer) {
		iss.keys = append(iss.keys, rsaJWK("small", &small.PublicKey), map[string]any{"kty": "oct", "kid": "hmac", "k": b64([]byte("shared"))})
	})
	valid := func(change func(jwt.MapClaims)) jwt.MapClaims {
		claims := iss.claims(testNonce, clock.now())
		change(claims)
		return claims
	}
	now := clock.now()

	cases := map[string]string{
		"another issuer":                 sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" })),
		"another audience":               sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["aud"] = "client-2" })),
		"no audience":                    sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { delete(c, "aud") })),
		"several audiences, no azp":      sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["aud"] = []string{"client-1", "client-2"} })),
		"issued to another party":        sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["azp"] = "client-2" })),
		"expired":                        sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"no expiry":                      sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { delete(c, "exp") })),
		"issued in the future":           sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["iat"] = now.Add(5 * time.Minute).Unix() })),
		"another nonce":                  sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { c["nonce"] = "nonce-2" })),
		"no nonce":                       sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { delete(c, "nonce") })),
		"no subject":                     sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) { delete(c, "sub") })),
		"signed by another key":          sign(t, jwt.SigningMethodRS256, other, "k1", valid(func(jwt.MapClaims) {})),
		"a key the issuer does not have": sign(t, jwt.SigningMethodRS256, other, "k9", valid(func(jwt.MapClaims) {})),
		"a key that is too small":        sign(t, jwt.SigningMethodRS256, small, "small", valid(func(jwt.MapClaims) {})),
		"HMAC with a shared secret":      sign(t, jwt.SigningMethodHS256, []byte("shared"), "hmac", valid(func(jwt.MapClaims) {})),
		"HMAC with the public key":       sign(t, jwt.SigningMethodHS256, iss.key.PublicKey.N.Bytes(), "k1", valid(func(jwt.MapClaims) {})),
		"not signed":                     unsigned(t, valid(func(jwt.MapClaims) {})),
		"not a token":                    "not.a.token",
	}
	for name, idToken := range cases {
		t.Run(name, func(t *testing.T) {
			iss.answerWith(idToken)
			_, err := exchange(iss.provider(clock), testNonce)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrExchangeRefused)
		})
	}

	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", valid(func(c jwt.MapClaims) {
		c["exp"] = now.Add(-30 * time.Second).Unix()
		c["iat"] = now.Add(30 * time.Second).Unix()
	})))
	_, err := exchange(iss.provider(clock), testNonce)
	assert.NoError(t, err, "a clock that is off by less than a minute is allowed")
}

func unsigned(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	return token
}

// A rotated key is fetched when a token names it, at most once a minute,
// and the key set is fetched again after an hour in any case.
func TestOIDCFollowsKeyRotation(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
	_, err := exchange(p, testNonce)
	require.NoError(t, err)
	require.Equal(t, 1, iss.count("/jwks"))

	// The issuer starts signing with a new key, an EC one.
	next := ecKey(t)
	iss.set(func(iss *testIssuer) { iss.keys = []map[string]any{ecJWK("k2", &next.PublicKey)} })
	signedWithNext := func() string { return sign(t, jwt.SigningMethodES256, next, "k2", iss.claims(testNonce, clock.now())) }

	iss.answerWith(signedWithNext())
	_, err = exchange(p, testNonce)
	assert.ErrorIs(t, err, ErrExchangeRefused, "within a minute of the last fetch, an unknown key is not fetched")
	assert.Equal(t, 1, iss.count("/jwks"))

	clock.add(keyRefetchInterval)
	iss.answerWith(signedWithNext())
	_, err = exchange(p, testNonce)
	require.NoError(t, err, "the new key is fetched")
	assert.Equal(t, 2, iss.count("/jwks"))

	// The old key is gone from the set, and so from the cache.
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
	_, err = exchange(p, testNonce)
	assert.ErrorIs(t, err, ErrExchangeRefused, "a key the issuer withdrew is not used")

	clock.add(keysTTL)
	iss.answerWith(signedWithNext())
	_, err = exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, 3, iss.count("/jwks"), "the set is fetched again after keysTTL")
}

// The token endpoint's errors: an OAuth error refuses the sign-in, and
// anything else that is not a token is a failure of the system.
func TestOIDCTokenEndpointAnswers(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	cases := []struct {
		name    string
		status  int
		body    any
		refused bool
	}{
		{"an OAuth error", http.StatusBadRequest, map[string]any{"error": "invalid_grant"}, true},
		{"an OAuth error with 200", http.StatusOK, map[string]any{"error": "invalid_grant"}, true},
		{"a server error", http.StatusInternalServerError, map[string]any{}, false},
		{"no access token", http.StatusOK, map[string]any{"id_token": "x"}, false},
		{"no ID token", http.StatusOK, map[string]any{"access_token": "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss.set(func(iss *testIssuer) { iss.token = func(url.Values) (int, any) { return tc.status, tc.body } })
			_, err := exchange(iss.provider(clock), testNonce)
			require.Error(t, err)
			assert.Equal(t, tc.refused, errors.Is(err, ErrExchangeRefused), "%v", err)
		})
	}
}

// The issuer's document has to name the issuer, and every endpoint has to
// be https. A failure is one of the system.
func TestOIDCDiscoveryChecks(t *testing.T) {
	for name, change := range map[string]func(doc map[string]any){
		"another issuer":                func(doc map[string]any) { doc["issuer"] = "https://evil.example.com" },
		"a token endpoint over http":    func(doc map[string]any) { doc["token_endpoint"] = "http://token.example.com/token" },
		"no key set":                    func(doc map[string]any) { delete(doc, "jwks_uri") },
		"a userinfo endpoint over http": func(doc map[string]any) { doc["userinfo_endpoint"] = "http://info.example.com" },
		"a revocation endpoint over http": func(doc map[string]any) {
			doc["revocation_endpoint"] = "http://revoke.example.com"
		},
	} {
		t.Run(name, func(t *testing.T) {
			iss := newTestIssuer(t)
			iss.set(func(iss *testIssuer) { change(iss.doc) })
			_, err := iss.provider(&testClock{at: time.Now()}).AuthorizationURL(context.Background(), AuthorizationRequest{})
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrExchangeRefused)
		})
	}
}

// RFC 9207: an iss parameter that names another issuer is refused, and so
// is a missing one when the issuer says it sends it.
func TestOIDCChecksTheIssParameter(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	run := func(issuer string) error {
		iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
		_, err := p.Exchange(context.Background(), ExchangeRequest{Code: "c", Nonce: testNonce, Issuer: issuer})
		return err
	}
	assert.NoError(t, run(""), "an issuer that does not send it")
	assert.NoError(t, run(iss.srv.URL))
	assert.ErrorIs(t, run("https://evil.example.com"), ErrExchangeRefused)

	iss = newTestIssuer(t)
	iss.set(func(iss *testIssuer) { iss.doc["authorization_response_iss_parameter_supported"] = true })
	p = iss.provider(clock)
	assert.ErrorIs(t, run(""), ErrExchangeRefused, "the issuer sends it, and it is missing")
	assert.NoError(t, run(iss.srv.URL))
}

// An ID token without an email is completed from the userinfo endpoint,
// whose answer has to be about the same subject.
func TestOIDCReadsTheUserinfoEndpoint(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	bare := iss.claims(testNonce, clock.now())
	for _, key := range []string{"email", "email_verified", "name", "given_name", "family_name", "picture"} {
		delete(bare, key)
	}
	answer := func(sub string, status int) {
		iss.set(func(iss *testIssuer) {
			iss.userinfo = func(r *http.Request) (int, any) {
				return status, map[string]any{"sub": sub, "email": "ada@example.com", "email_verified": "true", "name": "Ada"}
			}
		})
		iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", bare))
	}

	answer("user-42", http.StatusOK)
	result, err := exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, Identity{Subject: "user-42", Email: "ada@example.com", EmailVerified: true, Name: "Ada"}, result.Identity,
		`email_verified may be the string "true"`)
	assert.Equal(t, "Bearer access-1", iss.last(t, "/userinfo").header.Get("Authorization"))

	answer("user-43", http.StatusOK)
	_, err = exchange(p, testNonce)
	assert.ErrorIs(t, err, ErrExchangeRefused, "an answer about another subject")

	answer("user-42", http.StatusInternalServerError)
	_, err = exchange(p, testNonce)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExchangeRefused, "an endpoint that fails is a failure of the system")
}

// How the client authenticates follows the document, and a public client
// sends its id alone.
func TestOIDCClientAuthentication(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	iss.set(func(iss *testIssuer) {
		iss.doc["token_endpoint_auth_methods_supported"] = []string{"client_secret_post"}
	})
	p := iss.provider(clock)
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
	_, err := exchange(p, testNonce)
	require.NoError(t, err)
	token := iss.last(t, "/token")
	assert.Equal(t, "client-1", token.form.Get("client_id"))
	assert.Equal(t, "s3cret", token.form.Get("client_secret"))
	assert.Empty(t, token.header.Get("Authorization"))

	public := iss.provider(clock)
	public.auth.secret = nil
	_, err = exchange(public, testNonce)
	require.NoError(t, err)
	token = iss.last(t, "/token")
	assert.Equal(t, "client-1", token.form.Get("client_id"))
	assert.NotContains(t, token.form, "client_secret")
	assert.Empty(t, token.header.Get("Authorization"))

	assert.Equal(t, authSecretBasic, authMethod(nil, true), "the default of OpenID Connect Discovery")
	assert.Equal(t, authSecretBasic, authMethod([]string{"private_key_jwt"}, true))
}

// The calls of a provider follow outbound's rules: no redirect, and no more
// than outbound.DefaultMaxBody of an answer read.
func TestProviderHTTPLimits(t *testing.T) {
	iss := newTestIssuer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	// Where the redirect leads answers like a token endpoint, so only a
	// client that does not follow it fails.
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a"}`))
	})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"keys":"` + strings.Repeat("a", int(outbound.DefaultMaxBody)) + `"}`))
	})
	iss.srv.Config.Handler = mux
	client := providerHTTP{client: outbound.NewClient(outbound.Options{Transport: iss.srv.Client().Transport}, nil)}

	_, err := client.redeemCode(context.Background(), iss.srv.URL+"/moved", clientAuth{clientID: "c", secret: staticSecret("s")}, authSecretBasic, ExchangeRequest{})
	require.Error(t, err, "a redirect is not followed with the secret")
	assert.NotErrorIs(t, err, ErrExchangeRefused)
	_, err = providerHTTP{client: iss.srv.Client()}.redeemCode(context.Background(), iss.srv.URL+"/moved", clientAuth{clientID: "c"}, authNone, ExchangeRequest{})
	require.NoError(t, err, "the same call with a client that follows redirects")

	var into map[string]any
	_, err = client.getJSON(context.Background(), iss.srv.URL+"/huge", nil, &into)
	assert.ErrorIs(t, err, outbound.ErrTooLarge)
}

// While the issuer's key set can't be fetched, the keys of the last fetch
// keep checking ID tokens for crypto.DefaultKeySetMaxStale past keysTTL.
// Then sign-ins fail as the system does, and no token is refused for it.
func TestOIDCKeepsTheKeysWhileTheKeySetIsDown(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	signed := func() string {
		return sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now()))
	}
	iss.answerWith(signed())
	_, err := exchange(p, testNonce)
	require.NoError(t, err)

	iss.set(func(iss *testIssuer) { iss.jwksDown = true })
	clock.add(keysTTL)
	iss.answerWith(signed())
	_, err = exchange(p, testNonce)
	require.NoError(t, err, "the keys of the last fetch")
	assert.Equal(t, 2, iss.count("/jwks"), "the set was asked")

	clock.add(crypto.DefaultKeySetMaxStale)
	iss.answerWith(signed())
	_, err = exchange(p, testNonce)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExchangeRefused, "a failure of the system")
}

// A discovery document fetched again that names another jwks_uri gets a
// key set of its own.
func TestOIDCFollowsANewKeySetURI(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
	_, err := exchange(p, testNonce)
	require.NoError(t, err)

	next := ecKey(t)
	iss.set(func(iss *testIssuer) {
		iss.doc["jwks_uri"] = iss.srv.URL + "/jwks2"
		iss.keys = []map[string]any{ecJWK("k2", &next.PublicKey)}
	})
	clock.add(discoveryTTL)
	iss.answerWith(sign(t, jwt.SigningMethodES256, next, "k2", iss.claims(testNonce, clock.now())))
	_, err = exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, 1, iss.count("/jwks2"))
	assert.Equal(t, 1, iss.count("/jwks"), "the old URI is not asked again")
}

// An ID token without a kid is checked against every key of the set that
// fits its algorithm, so a provider with two keys of one type during a
// rotation is fine.
func TestOIDCAcceptsIDTokensWithoutKID(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	second := rsaKey(t, 2048)
	iss.set(func(iss *testIssuer) { iss.keys = append(iss.keys, rsaJWK("k2", &second.PublicKey)) })

	for name, key := range map[string]*rsa.PrivateKey{"the first key": iss.key, "the second key": second} {
		iss.answerWith(sign(t, jwt.SigningMethodRS256, key, "", iss.claims(testNonce, clock.now())))
		_, err := exchange(iss.provider(clock), testNonce)
		assert.NoError(t, err, name)
	}
	iss.answerWith(sign(t, jwt.SigningMethodES256, ecKey(t), "", iss.claims(testNonce, clock.now())))
	_, err := exchange(iss.provider(clock), testNonce)
	assert.ErrorIs(t, err, ErrExchangeRefused, "no key of the set fits")
}

// A key set without a key that can verify a signature is a failure of the
// system, and not a token to refuse: no ID token could be checked with it.
func TestOIDCNeedsAUsableKey(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	iss.set(func(iss *testIssuer) {
		iss.keys = []map[string]any{
			{"kty": "oct", "kid": "hmac", "k": b64([]byte("shared"))},
			rsaJWK("small", &rsaKey(t, 1024).PublicKey),
		}
	})
	iss.answerWith(sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now())))
	_, err := exchange(iss.provider(clock), testNonce)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExchangeRefused)
	assert.ErrorIs(t, err, keysource.ErrNoUsableKey)
}

// The algorithms an ID token may be signed with are asymmetric ones. An
// HMAC token would be checked with a public key as its secret.
func TestSigningAlgsAreAsymmetric(t *testing.T) {
	for _, alg := range signingAlgs {
		assert.False(t, strings.HasPrefix(alg, "HS") || strings.EqualFold(alg, "none"), alg)
	}
}

// An exchange returns the provider's tokens with their lifetimes, and the
// ID token it checked. An answer without a scope grants the one asked for.
func TestOIDCExchangeTokens(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	idToken := sign(t, jwt.SigningMethodRS256, iss.key, "k1", iss.claims(testNonce, clock.now()))
	iss.set(func(iss *testIssuer) {
		iss.token = func(url.Values) (int, any) {
			return http.StatusOK, map[string]any{
				"access_token": "access-1", "token_type": "Bearer", "id_token": idToken,
				"refresh_token": "refresh-1", "expires_in": 3600, "scope": "openid email",
			}
		}
	})
	result, err := exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, Tokens{
		AccessToken: "access-1", RefreshToken: "refresh-1", IDToken: idToken,
		ExpiresAt: clock.now().Add(time.Hour), Scope: "openid email",
	}, result.Tokens)

	iss.answerWith(idToken)
	result, err = exchange(p, testNonce)
	require.NoError(t, err)
	assert.Equal(t, "openid email profile", result.Tokens.Scope)
	assert.True(t, result.Tokens.ExpiresAt.IsZero(), "no expires_in, no expiry")
	assert.Empty(t, result.Tokens.RefreshToken)
}

// A refresh posts the refresh token to the token endpoint, authenticated as
// the code exchange is. An ID token in the answer is dropped. A refresh
// token the issuer refuses wraps ErrExchangeRefused, and an issuer that
// fails does not.
func TestOIDCRefresh(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	p := iss.provider(clock)
	iss.set(func(iss *testIssuer) {
		iss.token = func(url.Values) (int, any) {
			return http.StatusOK, map[string]any{"access_token": "access-2", "id_token": "not-checked", "expires_in": "3600"}
		}
	})
	tokens, err := p.Refresh(context.Background(), RefreshRequest{RefreshToken: "refresh-1"})
	require.NoError(t, err)
	assert.Equal(t, &Tokens{AccessToken: "access-2", ExpiresAt: clock.now().Add(time.Hour)}, tokens,
		"no refresh token: the issuer keeps the old one; no scope: it did not change")
	token := iss.last(t, "/token")
	assert.Equal(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"refresh-1"}}, token.form)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("client-1:s3cret")), token.header.Get("Authorization"))

	for status, refused := range map[int]bool{http.StatusBadRequest: true, http.StatusInternalServerError: false} {
		iss.set(func(iss *testIssuer) {
			iss.token = func(url.Values) (int, any) {
				if refused {
					return status, map[string]any{"error": "invalid_grant"}
				}
				return status, map[string]any{}
			}
		})
		_, err := p.Refresh(context.Background(), RefreshRequest{RefreshToken: "refresh-1"})
		require.Error(t, err)
		assert.Equal(t, refused, errors.Is(err, ErrExchangeRefused), "status %d: %v", status, err)
	}
}

// Revoking sends the refresh token, or the access token when there is none,
// to the revocation endpoint (RFC 7009). An issuer without one revokes
// nothing, and an endpoint that fails is an error.
func TestOIDCRevoke(t *testing.T) {
	clock := &testClock{at: time.Now()}
	iss := newTestIssuer(t)
	iss.set(func(iss *testIssuer) {
		iss.doc["revocation_endpoint"] = iss.srv.URL + "/revoke"
		iss.doc["revocation_endpoint_auth_methods_supported"] = []string{"client_secret_post"}
	})
	p := iss.provider(clock)

	require.NoError(t, p.Revoke(context.Background(), Tokens{AccessToken: "access-1", RefreshToken: "refresh-1"}))
	assert.Equal(t, url.Values{
		"token": {"refresh-1"}, "token_type_hint": {"refresh_token"}, "client_id": {"client-1"}, "client_secret": {"s3cret"},
	}, iss.last(t, "/revoke").form)
	require.NoError(t, p.Revoke(context.Background(), Tokens{AccessToken: "access-1"}))
	assert.Equal(t, "access_token", iss.last(t, "/revoke").form.Get("token_type_hint"))

	iss.set(func(iss *testIssuer) { iss.revoke = func(url.Values) int { return http.StatusServiceUnavailable } })
	assert.Error(t, p.Revoke(context.Background(), Tokens{AccessToken: "access-1"}))
	require.NoError(t, p.Revoke(context.Background(), Tokens{}), "nothing to revoke")
	assert.Equal(t, 3, iss.count("/revoke"))

	without := newTestIssuer(t)
	require.NoError(t, without.provider(clock).Revoke(context.Background(), Tokens{AccessToken: "access-1"}))
	assert.Zero(t, without.count("/revoke"), "an issuer without a revocation endpoint")
}

// Parameters of a provider's own join the authorization URL, and can't
// replace one of the protocol's.
func TestOIDCExtraAuthorizationParams(t *testing.T) {
	iss := newTestIssuer(t)
	p := iss.provider(&testClock{at: time.Now()})
	p.authParams = url.Values{"access_type": {"offline"}, "state": {"evil"}}
	raw, err := p.AuthorizationURL(context.Background(), AuthorizationRequest{State: "state-1", Nonce: testNonce})
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "offline", u.Query().Get("access_type"))
	assert.Equal(t, []string{"state-1"}, u.Query()["state"])
}

// A lifetime is a number or a string of digits. Anything else, a negative
// one, and one past an int32 of seconds read as unknown, without failing the
// answer.
func TestTokenLifetimes(t *testing.T) {
	for raw, want := range map[string]seconds{
		`3600`: 3600, `"3600"`: 3600, `null`: 0, `"soon"`: 0, `-5`: 0, `1.5`: 0, `99999999999`: 0,
	} {
		var answer tokenResponse
		require.NoError(t, json.Unmarshal([]byte(`{"access_token":"a","expires_in":`+raw+`}`), &answer), raw)
		assert.Equal(t, want, answer.ExpiresIn, raw)
	}

	now := time.Now()
	tokens := (&tokenResponse{AccessToken: "a", ExpiresIn: 60, RefreshTokenExpiresIn: 120}).tokens(now)
	assert.Equal(t, now.Add(time.Minute), tokens.ExpiresAt)
	assert.True(t, tokens.RefreshExpiresAt.IsZero(), "a lifetime of a refresh token that was not issued")
}
