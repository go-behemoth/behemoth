package crypto_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
)

// Negative tests of token verification, one for each kind of attack: the
// none algorithm, algorithm confusion, the kid, the time claims, the issuer
// and the audience, the size, critical headers, and the encoding. Most run
// on the three paths a token is verified on: VerifyJWT with the keys of
// another issuer, and the internal and external tokens of the JWT provider.
// Wycheproof's JOSE vectors are in tests/crypto.

// countingKeys is a KeySet that records the lookups made in it.
type countingKeys struct {
	crypto.KeySet
	kids  []string
	lists int
}

func (k *countingKeys) PublicKey(ctx context.Context, kid string) (crypto.PublishedKey, error) {
	k.kids = append(k.kids, kid)
	return k.KeySet.PublicKey(ctx, kid)
}

func (k *countingKeys) PublicKeys(ctx context.Context) ([]crypto.PublishedKey, error) {
	k.lists++
	return k.KeySet.PublicKeys(ctx)
}

// countingExternalKeys is a provider's external key set whose lookups are
// recorded in counted.
type countingExternalKeys struct {
	crypto.ExternalKeySet
	counted *countingKeys
}

func (k countingExternalKeys) PublicKey(ctx context.Context, kid string) (crypto.PublishedKey, error) {
	return k.counted.PublicKey(ctx, kid)
}

func (k countingExternalKeys) PublicKeys(ctx context.Context) ([]crypto.PublishedKey, error) {
	return k.counted.PublicKeys(ctx)
}

// tokenPath is one of the paths a token is verified on, with the key its
// issuer signs with.
type tokenPath struct {
	name   string
	verify func(token string) error
	method jwt.SigningMethod
	key    any
	kid    string
	claims func() jwt.MapClaims
	// keys is the key set the path looks keys up in. Internal tokens have
	// none.
	keys *countingKeys
}

func tokenPaths(t *testing.T) []tokenPath {
	t.Helper()
	ctx := context.Background()
	f := newVerifyFixture(t)
	issuerKeys := &countingKeys{KeySet: f.keys}
	fx := defaultFixture(t)
	derived := &countingKeys{KeySet: fx.keys}
	provider := crypto.NewJWTProvider(fx.suite.JWTSign, countingExternalKeys{ExternalKeySet: fx.keys, counted: derived}, jwtConfig())
	return []tokenPath{
		{
			name: "VerifyJWT",
			verify: func(token string) error {
				_, err := crypto.VerifyJWT(ctx, token, issuerKeys, f.options)
				return err
			},
			method: jwt.SigningMethodEdDSA, key: f.ed, kid: "ed",
			claims: func() jwt.MapClaims { return validClaims() },
			keys:   issuerKeys,
		},
		{
			name: "internal",
			verify: func(token string) error {
				_, err := provider.Verify(ctx, crypto.JWTInternal, token)
				return err
			},
			method: jwt.SigningMethodHS256, key: fx.hmacKey(t, 1), kid: "1",
			claims: func() jwt.MapClaims { return claims(crypto.JWTInternal) },
		},
		{
			name: "external",
			verify: func(token string) error {
				_, err := provider.Verify(ctx, crypto.JWTExternal, token)
				return err
			},
			method: jwt.SigningMethodEdDSA, key: fx.externalKey(t, 1), kid: "1",
			claims: func() jwt.MapClaims { return claims(crypto.JWTExternal) },
			keys:   derived,
		},
	}
}

// sign returns a token of c whose header holds the path's alg and kid and
// the members of header, signed with the path's key.
func (p tokenPath) sign(t *testing.T, header map[string]any, c jwt.MapClaims) string {
	t.Helper()
	h := map[string]any{"alg": p.method.Alg(), "kid": p.kid}
	for name, value := range header {
		h[name] = value
	}
	return signRaw(t, p.method, p.key, mustJSON(t, h), mustJSON(t, c))
}

// lookups is how many times the path asked its key set for keys.
func (p tokenPath) lookups() int {
	if p.keys == nil {
		return 0
	}
	return len(p.keys.kids) + p.keys.lists
}

// signRaw signs header and payload, JSON written as they are, with method
// and key.
func signRaw(t *testing.T, method jwt.SigningMethod, key any, header, payload string) string {
	t.Helper()
	input := b64url([]byte(header)) + "." + b64url([]byte(payload))
	signature, err := method.Sign(input, key)
	require.NoError(t, err)
	return input + "." + b64url(signature)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

// A token whose header names "none" in any case, an empty algorithm, or no
// algorithm is refused, whatever its signature segment holds: the algorithm
// is the verifier's choice, never the token's.
func TestVerifyRefusesTheNoneAlgorithm(t *testing.T) {
	for _, p := range tokenPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			payload := b64url([]byte(mustJSON(t, p.claims())))
			kid := `"kid":"` + p.kid + `"`
			for _, tc := range []struct{ name, header, code string }{
				{"none", `{"alg":"none"}`, errs.CodeJWTAlgorithmMismatch},
				{"None", `{"alg":"None"}`, errs.CodeJWTAlgorithmMismatch},
				{"NONE", `{"alg":"NONE"}`, errs.CodeJWTAlgorithmMismatch},
				{"nOnE", `{"alg":"nOnE"}`, errs.CodeJWTAlgorithmMismatch},
				{"none with the key's kid", `{"alg":"none",` + kid + `}`, errs.CodeJWTAlgorithmMismatch},
				{"an empty alg", `{"alg":"",` + kid + `}`, errs.CodeJWTAlgorithmMismatch},
				{"no alg", `{` + kid + `}`, errs.CodeJWTMalformed},
				{"a null alg", `{"alg":null,` + kid + `}`, errs.CodeJWTMalformed},
				{"an alg that is not text", `{"alg":["none"],` + kid + `}`, errs.CodeJWTMalformed},
			} {
				for _, signature := range []string{"", b64url([]byte("signature"))} {
					token := b64url([]byte(tc.header)) + "." + payload + "." + signature
					refusedWith(t, p.verify(token), tc.code, "%s, signature %q", tc.name, signature)
				}
			}
		})
	}
}

// publicForms returns pub in the forms an attacker can get it in, to try as
// an HMAC secret: DER, PEM, the JWK of a key set, and its raw bytes.
func publicForms(t *testing.T, kid string, pub any) map[string][]byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	set, err := crypto.MarshalJWKSet([]crypto.PublishedKey{{KID: kid, Key: pub}})
	require.NoError(t, err)
	var published struct {
		Keys []json.RawMessage `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(set, &published))
	forms := map[string][]byte{
		"DER": der, "PEM": pemBlock, "PEM without its last newline": pemBlock[:len(pemBlock)-1],
		"its JWK": published.Keys[0], "its JWK set": set,
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		forms["PKCS #1 DER"] = x509.MarshalPKCS1PublicKey(k)
		forms["its modulus"] = k.N.Bytes()
	case *ecdsa.PublicKey:
		point, err := k.Bytes()
		require.NoError(t, err)
		forms["its point"] = point
	case ed25519.PublicKey:
		forms["its bytes"] = k
	}
	return forms
}

// A token signed with HMAC, whose secret is the verifier's public key in a
// form an attacker can get, is refused. VerifyJWT, and so external tokens,
// never take an HMAC algorithm. An internal token is HMAC, but its secret is
// never public: the published keys are not it.
func TestVerifyRefusesAlgorithmConfusion(t *testing.T) {
	ctx := context.Background()
	hmacs := []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodHS384, jwt.SigningMethodHS512}

	f := newVerifyFixture(t)
	for kid, pub := range map[string]any{"rsa": &f.rsa.(*rsa.PrivateKey).PublicKey, "ec": &f.ec256.PublicKey, "ed": f.ed.Public()} {
		for form, secret := range publicForms(t, kid, pub) {
			for _, method := range hmacs {
				_, err := crypto.VerifyJWT(ctx, tokenWith(t, method, secret, kid, validClaims()), f.keys, f.options)
				refusedWith(t, err, errs.CodeJWTAlgorithmMismatch, "%s under the %s key as %s", method.Alg(), kid, form)
			}
		}
	}

	fx := defaultFixture(t)
	published, err := fx.keys.PublicKeys(ctx)
	require.NoError(t, err)
	require.Len(t, published, 1)
	for form, secret := range publicForms(t, published[0].KID, published[0].Key) {
		for _, method := range hmacs {
			_, err := fx.provider.Verify(ctx, crypto.JWTExternal, signWith(t, method, secret, published[0].KID, claims(crypto.JWTExternal)))
			refusedWith(t, err, errs.CodeJWTAlgorithmMismatch, "an external %s token under the published key as %s", method.Alg(), form)
		}
		_, err := fx.provider.Verify(ctx, crypto.JWTInternal, signWith(t, jwt.SigningMethodHS256, secret, "1", claims(crypto.JWTInternal)))
		refusedWith(t, err, errs.CodeJWTInvalidSignature, "an internal token under the published key as %s", form)
	}
}

// A kid is looked up as it is, and one the key set lacks is refused: a kid
// that carries a path, a query, or a URL names no key. A kid that is not
// text is refused as well. A key the token carries itself (jwk, jku, x5u,
// x5c) is never used.
func TestVerifyRefusesAnUnknownOrInjectedKID(t *testing.T) {
	ctx := context.Background()

	t.Run("VerifyJWT", func(t *testing.T) {
		f := newVerifyFixture(t)
		keys := &countingKeys{KeySet: f.keys}
		injected := []string{
			"../../../../dev/null", "' OR '1'='1", "ed' --", "https://evil.example.com/jwks.json",
			"ed\x00", "ED", " ed", "ed ", "*", strings.Repeat("e", 4096),
		}
		for _, kid := range injected {
			_, err := crypto.VerifyJWT(ctx, tokenWith(t, jwt.SigningMethodEdDSA, f.ed, kid, validClaims()), keys, f.options)
			refusedWith(t, err, errs.CodeJWTUnknownKey, "kid %q", kid)
		}
		assert.Equal(t, injected, keys.kids, "each kid is looked up as it is, and nothing else is")
		assert.Zero(t, keys.lists)

		allowMissing := f.options
		allowMissing.AllowMissingKID = true
		payload := mustJSON(t, validClaims())
		for _, kid := range []string{`1`, `true`, `null`, `["ed"]`, `{"kid":"ed"}`} {
			token := signRaw(t, jwt.SigningMethodEdDSA, f.ed, `{"alg":"EdDSA","kid":`+kid+`}`, payload)
			for _, opts := range []crypto.VerifyOptions{f.options, allowMissing} {
				_, err := crypto.VerifyJWT(ctx, token, f.keys, opts)
				refusedWith(t, err, errs.CodeJWTMalformed, "kid %s, AllowMissingKID %v", kid, opts.AllowMissingKID)
			}
		}

		attackerPublic, attacker, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		der, err := x509.MarshalPKIXPublicKey(attackerPublic)
		require.NoError(t, err)
		for name, member := range map[string]string{
			"jwk": `"jwk":` + string(publicForms(t, "attacker", attackerPublic)["its JWK"]),
			"jku": `"jku":"https://evil.example.com/jwks.json"`,
			"x5u": `"x5u":"https://evil.example.com/key.pem"`,
			"x5c": `"x5c":["` + base64.StdEncoding.EncodeToString(der) + `"]`,
		} {
			withKID := signRaw(t, jwt.SigningMethodEdDSA, attacker, `{"alg":"EdDSA","kid":"ed",`+member+`}`, payload)
			_, err := crypto.VerifyJWT(ctx, withKID, f.keys, f.options)
			refusedWith(t, err, errs.CodeJWTInvalidSignature, "%s, under the kid of a key of the set", name)

			withoutKID := signRaw(t, jwt.SigningMethodEdDSA, attacker, `{"alg":"EdDSA",`+member+`}`, payload)
			_, err = crypto.VerifyJWT(ctx, withoutKID, f.keys, f.options)
			refusedWith(t, err, errs.CodeJWTUnknownKey, "%s, without a kid", name)
			_, err = crypto.VerifyJWT(ctx, withoutKID, f.keys, allowMissing)
			refusedWith(t, err, errs.CodeJWTInvalidSignature, "%s, without a kid: the set's keys are tried, and none is the token's", name)
		}
	})

	t.Run("external", func(t *testing.T) {
		fx := defaultFixture(t)
		for _, kid := range []string{"2", " 1", "1 ", "+1", "01", "1.0", "0x1", "-1", "１", "1\x00"} {
			token := signWith(t, jwt.SigningMethodEdDSA, fx.externalKey(t, 1), kid, claims(crypto.JWTExternal))
			_, err := fx.provider.Verify(ctx, crypto.JWTExternal, token)
			refusedWith(t, err, errs.CodeJWTUnknownKey, "kid %q", kid)
		}
	})

	t.Run("internal", func(t *testing.T) {
		fx := defaultFixture(t)
		// The token is signed with the key of version 1, so a kid that
		// would be read as 1 would verify.
		for _, kid := range []string{"2", "99", "-1", "0", "9223372036854775808", " 1", "1 ", "+1", "01", "1.0", "0x1", "one", "", "１"} {
			token := signWith(t, jwt.SigningMethodHS256, fx.hmacKey(t, 1), kid, claims(crypto.JWTInternal))
			_, err := fx.provider.Verify(ctx, crypto.JWTInternal, token)
			refusedWith(t, err, errs.CodeJWTUnknownKey, "kid %q", kid)
		}
		noKID := signRaw(t, jwt.SigningMethodHS256, fx.hmacKey(t, 1), `{"alg":"HS256"}`, mustJSON(t, claims(crypto.JWTInternal)))
		_, err := fx.provider.Verify(ctx, crypto.JWTInternal, noKID)
		refusedWith(t, err, errs.CodeJWTUnknownKey, "no kid")
	})
}

// exp is required and nbf and iat are checked when present, to the second,
// with the leeway on each side. A time claim that is not a number refuses
// the token. The leeway is at most five minutes: more is a configuration
// error.
func TestVerifyEnforcesTheTimeClaims(t *testing.T) {
	ctx := context.Background()
	f := newVerifyFixture(t)
	now := time.Unix(1_900_000_000, 0)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }
	verify := func(leeway time.Duration, checkIssuedAt bool, change func(jwt.MapClaims)) error {
		c := validClaims(func(c jwt.MapClaims) { c["iat"], c["exp"] = at(0), at(5*time.Minute) }, change)
		opts := f.options
		opts.Now = func() time.Time { return now }
		opts.Leeway = leeway
		opts.CheckIssuedAt = checkIssuedAt
		_, err := crypto.VerifyJWT(ctx, tokenWith(t, jwt.SigningMethodEdDSA, f.ed, "ed", c), f.keys, opts)
		return err
	}
	for _, tc := range []struct {
		name          string
		leeway        time.Duration
		checkIssuedAt bool
		change        func(jwt.MapClaims)
		code          string // empty when the token is accepted
	}{
		{"exp a second ahead", 0, false, func(c jwt.MapClaims) { c["exp"] = at(time.Second) }, ""},
		{"exp now", 0, false, func(c jwt.MapClaims) { c["exp"] = at(0) }, errs.CodeTokenExpired},
		{"exp past, within the leeway", time.Minute, false, func(c jwt.MapClaims) { c["exp"] = at(-59 * time.Second) }, ""},
		{"exp a leeway ago", time.Minute, false, func(c jwt.MapClaims) { c["exp"] = at(-time.Minute) }, errs.CodeTokenExpired},
		// golang-jwt reads a NumericDate of 0 as absent.
		{"exp at the epoch", 0, false, func(c jwt.MapClaims) { c["exp"] = 0 }, errs.CodeJWTMissingClaim},
		{"a negative exp", 0, false, func(c jwt.MapClaims) { c["exp"] = -1 }, errs.CodeTokenExpired},
		{"no exp", 0, false, func(c jwt.MapClaims) { delete(c, "exp") }, errs.CodeJWTMissingClaim},
		{"nbf now", 0, false, func(c jwt.MapClaims) { c["nbf"] = at(0) }, ""},
		{"nbf a second ahead", 0, false, func(c jwt.MapClaims) { c["nbf"] = at(time.Second) }, errs.CodeJWTNotYetValid},
		{"nbf a leeway ahead", time.Minute, false, func(c jwt.MapClaims) { c["nbf"] = at(time.Minute) }, ""},
		{"nbf more than a leeway ahead", time.Minute, false, func(c jwt.MapClaims) { c["nbf"] = at(61 * time.Second) }, errs.CodeJWTNotYetValid},
		{"iat ahead, not checked", 0, false, func(c jwt.MapClaims) { c["iat"] = at(time.Second) }, ""},
		{"iat a second ahead", 0, true, func(c jwt.MapClaims) { c["iat"] = at(time.Second) }, errs.CodeJWTInvalidClaims},
		{"iat a leeway ahead", time.Minute, true, func(c jwt.MapClaims) { c["iat"] = at(time.Minute) }, ""},
		{"the most leeway", 5 * time.Minute, false, func(c jwt.MapClaims) { c["exp"] = at(-299 * time.Second) }, ""},
		{"exp that is true", 0, false, func(c jwt.MapClaims) { c["exp"] = true }, errs.CodeJWTInvalidClaims},
		{"exp that is an object", 0, false, func(c jwt.MapClaims) { c["exp"] = map[string]any{"at": at(time.Hour)} }, errs.CodeJWTInvalidClaims},
		{"exp that is a list", 0, false, func(c jwt.MapClaims) { c["exp"] = []int64{at(time.Hour)} }, errs.CodeJWTInvalidClaims},
		{"exp that is the text of a number", 0, false, func(c jwt.MapClaims) { c["exp"] = "1900000300" }, errs.CodeJWTInvalidClaims},
		{"nbf that is text", 0, false, func(c jwt.MapClaims) { c["nbf"] = "now" }, errs.CodeJWTInvalidClaims},
		{"iat that is text", 0, false, func(c jwt.MapClaims) { c["iat"] = "now" }, errs.CodeJWTInvalidClaims},
	} {
		err := verify(tc.leeway, tc.checkIssuedAt, tc.change)
		if tc.code == "" {
			assert.NoError(t, err, tc.name)
			continue
		}
		refusedWith(t, err, tc.code, tc.name)
	}

	opts := f.options
	opts.Leeway = 5*time.Minute + time.Nanosecond
	_, err := crypto.VerifyJWT(ctx, tokenWith(t, jwt.SigningMethodEdDSA, f.ed, "ed", validClaims()), f.keys, opts)
	assert.True(t, errs.IsCategory(err, errs.CategoryConfiguration), "a leeway over five minutes: %v", err)
}

// iss and aud are compared as they are: a trailing slash, another case, or
// another scheme is another issuer or audience, and a claim of the wrong
// type refuses the token.
func TestVerifyRefusesAnotherIssuerOrAudience(t *testing.T) {
	ctx := context.Background()
	f := newVerifyFixture(t)
	for _, tc := range []struct {
		name  string
		claim string
		value any
		code  string
	}{
		{"an issuer with a trailing slash", "iss", verifyIssuer + "/", errs.CodeJWTIssuerMismatch},
		{"an issuer in capitals", "iss", strings.ToUpper(verifyIssuer), errs.CodeJWTIssuerMismatch},
		{"an issuer over http", "iss", strings.Replace(verifyIssuer, "https", "http", 1), errs.CodeJWTIssuerMismatch},
		{"an issuer after a space", "iss", " " + verifyIssuer, errs.CodeJWTIssuerMismatch},
		{"an issuer that starts with the trusted one", "iss", verifyIssuer + ".evil.example", errs.CodeJWTIssuerMismatch},
		{"an empty issuer", "iss", "", errs.CodeJWTIssuerMismatch},
		{"an issuer that is a number", "iss", 42, errs.CodeJWTIssuerMismatch},
		{"an issuer that is a list", "iss", []string{verifyIssuer}, errs.CodeJWTIssuerMismatch},
		{"an audience with a trailing slash", "aud", verifyAudience + "/", errs.CodeJWTAudienceMismatch},
		{"an audience in capitals", "aud", strings.ToUpper(verifyAudience), errs.CodeJWTAudienceMismatch},
		{"audiences without the verifier", "aud", []string{"https://billing.example.com", "https://reports.example.com"}, errs.CodeJWTAudienceMismatch},
		// golang-jwt reads an empty aud, and one that is neither text nor a
		// list, as absent.
		{"an empty audience", "aud", "", errs.CodeJWTMissingClaim},
		{"no audiences", "aud", []string{}, errs.CodeJWTMissingClaim},
		{"an audience that is a number", "aud", 42, errs.CodeJWTMissingClaim},
	} {
		c := validClaims(func(c jwt.MapClaims) { c[tc.claim] = tc.value })
		_, err := crypto.VerifyJWT(ctx, tokenWith(t, jwt.SigningMethodEdDSA, f.ed, "ed", c), f.keys, f.options)
		refusedWith(t, err, tc.code, tc.name)
	}

	for _, p := range tokenPaths(t) {
		c := p.claims()
		c["iss"] = c["iss"].(string) + "/"
		refusedWith(t, p.verify(p.sign(t, nil, c)), errs.CodeJWTIssuerMismatch, "%s: an issuer with a trailing slash", p.name)
	}
}

// The limits of a token as parse reads it: 64 KiB for the token and 8 KiB
// for its header, both as they are encoded.
const (
	maxTokenBytes  = 64 << 10
	maxHeaderBytes = 8 << 10
)

// sizedToken returns the largest token of the path, signed, whose segment
// grown by fill stays within limit, and the smallest one beyond it.
func sizedToken(t *testing.T, limit int, fill func(n int) string) (within, beyond string) {
	t.Helper()
	low, high := 0, limit
	for low < high {
		mid := (low + high + 1) / 2
		if len(fill(mid)) <= limit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	within, beyond = fill(low), fill(low+1)
	for len(beyond) <= limit {
		low++
		beyond = fill(low + 1)
	}
	return within, beyond
}

// A token over 64 KiB, or with a header over 8 KiB, is refused with
// token_too_large before it is decoded, and before a key is looked up. A
// token just within the limits is accepted, so that the limits are the
// ones refusing.
func TestVerifyRefusesOversizedTokens(t *testing.T) {
	for _, p := range tokenPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			byClaims := func(n int) string {
				c := p.claims()
				c["padding"] = strings.Repeat("x", n)
				return p.sign(t, nil, c)
			}
			within, beyond := sizedToken(t, maxTokenBytes, byClaims)
			require.NoError(t, p.verify(within), "a token of %d bytes", len(within))
			before := p.lookups()
			refusedWith(t, p.verify(beyond), errs.CodeJWTTooLarge, "a token of %d bytes", len(beyond))
			assert.Equal(t, before, p.lookups(), "no key is looked up for a token over the limit")
			refusedWith(t, p.verify(strings.Repeat("a", maxTokenBytes+1)), errs.CodeJWTTooLarge, "input over the limit that is not a token")

			byHeader := func(n int) string {
				return p.sign(t, map[string]any{"x5c": []string{strings.Repeat("A", n)}}, p.claims())
			}
			headerOf := func(token string) string { return strings.SplitN(token, ".", 2)[0] }
			withinHeader, beyondHeader := sizedToken(t, maxHeaderBytes, func(n int) string { return headerOf(byHeader(n)) })
			require.NoError(t, p.verify(tokenWithHeader(t, p, withinHeader)), "a header of %d bytes", len(withinHeader))
			before = p.lookups()
			refusedWith(t, p.verify(tokenWithHeader(t, p, beyondHeader)), errs.CodeJWTTooLarge, "a header of %d bytes", len(beyondHeader))
			assert.Equal(t, before, p.lookups(), "no key is looked up for a header over the limit")
		})
	}
}

// tokenWithHeader returns a token of the path's valid claims under header,
// an encoded segment, signed with the path's key.
func tokenWithHeader(t *testing.T, p tokenPath, header string) string {
	t.Helper()
	input := header + "." + b64url([]byte(mustJSON(t, p.claims())))
	signature, err := p.method.Sign(input, p.key)
	require.NoError(t, err)
	return input + "." + b64url(signature)
}

// A header with crit is refused, whatever it lists: no extension is
// understood, and RFC 7515 section 4.1.11 then fails the token. It is
// refused before a key is looked up.
func TestVerifyRefusesCriticalHeaders(t *testing.T) {
	for _, p := range tokenPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			for name, header := range map[string]map[string]any{
				"a registered claim":           {"crit": []string{"exp"}},
				"an unencoded payload":         {"crit": []string{"b64"}, "b64": false},
				"an extension the token names": {"crit": []string{"urn:example:x"}, "urn:example:x": 1},
				"an extension the token lacks": {"crit": []string{"urn:example:x"}},
				"an empty list":                {"crit": []string{}},
				"a name instead of a list":     {"crit": "exp"},
				"null":                         {"crit": nil},
			} {
				before := p.lookups()
				refusedWith(t, p.verify(p.sign(t, header, p.claims())), errs.CodeJWTMalformed, name)
				assert.Equal(t, before, p.lookups(), "%s: no key is looked up", name)
			}
		})
	}
}

// A token has one encoding: base64url without padding, whose unused bits
// are zero (RFC 7515, section 2, and RFC 4648, section 3.5). Another
// encoding of the same bytes is refused, so that a token can't be changed
// into another string that still verifies.
func TestVerifyRefusesNonCanonicalEncodings(t *testing.T) {
	for _, p := range tokenPaths(t) {
		t.Run(p.name, func(t *testing.T) {
			token := p.sign(t, nil, p.claims())
			require.NoError(t, p.verify(token))
			parts := strings.Split(token, ".")
			header, payload, signature := parts[0], parts[1], parts[2]
			middle := len(signature) / 2
			for name, changed := range map[string]string{
				"the signature's unused bits set":        header + "." + payload + "." + withUnusedBits(t, signature),
				"padding after the signature":            token + "=",
				"padding after the payload":              header + "." + payload + "==." + signature,
				"a line break in the signature":          header + "." + payload + "." + signature[:middle] + "\n" + signature[middle:],
				"a carriage return in the signature":     header + "." + payload + "." + signature[:middle] + "\r" + signature[middle:],
				"a line break in the header":             header[:4] + "\n" + header[4:] + "." + payload + "." + signature,
				"a space in the signature":               header + "." + payload + "." + signature[:middle] + " " + signature[middle:],
				"the standard alphabet in the signature": header + "." + payload + "." + signature[:middle] + "+" + signature[middle+1:],
			} {
				refusedWith(t, p.verify(changed), errs.CodeJWTMalformed, name)
			}
		})
	}
}

// withUnusedBits returns segment with the lowest unused bit of its last
// character set: another encoding of the same bytes, which a decoder that
// is not strict reads as the same.
func withUnusedBits(t *testing.T, segment string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(segment)
	require.NoError(t, err)
	require.NotZero(t, 6*len(segment)-8*len(decoded), "the segment has no unused bits")
	last := strings.IndexByte(alphabet, segment[len(segment)-1])
	changed := segment[:len(segment)-1] + string(alphabet[last|1])
	require.NotEqual(t, segment, changed)
	again, err := base64.RawURLEncoding.DecodeString(changed)
	require.NoError(t, err)
	require.Equal(t, decoded, again, "a lenient decoder reads the same bytes")
	return changed
}
