package crypto_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

// These tests state what a JWTProvider does, through the interface: the
// tokens it issues are standard JWTs (RFC 7519) that another library can
// verify, it accepts standard tokens signed with its keys, and it refuses
// everything else with a typed error and a code. Tokens a test needs that
// the provider would never issue are made with golang-jwt and signed with
// the provider's own keys, so that each one fails one check only.

const (
	testIssuer       = "https://auth.example.com"
	internalAudience = "behemoth"
	externalAudience = "https://api.example.com"
	otherAudience    = "https://reports.example.com"
	internalTTL      = 15 * time.Minute
	externalTTL      = time.Hour
)

var (
	secretV1 = strings.Repeat("ab", 32)
	secretV2 = strings.Repeat("cd", 32)
)

var kinds = []crypto.JWTKind{crypto.JWTInternal, crypto.JWTExternal}

func jwtConfig() crypto.JWTConfig {
	return crypto.JWTConfig{
		Issuer: testIssuer,
		Audiences: map[crypto.JWTKind][]string{
			crypto.JWTInternal: {internalAudience},
			crypto.JWTExternal: {externalAudience, otherAudience},
		},
		TTL: map[crypto.JWTKind]time.Duration{crypto.JWTInternal: internalTTL, crypto.JWTExternal: externalTTL},
	}
}

// audienceOf is the default audience of kind: the first configured one.
func audienceOf(kind crypto.JWTKind) string { return jwtConfig().Audiences[kind][0] }

// jwtFixture is a provider and what a test needs to make tokens of its own:
// the key manager behind internal tokens and the key set behind external
// ones.
type jwtFixture struct {
	provider crypto.JWTProvider
	impl     *crypto.JWTProviderImpl
	suite    crypto.Services
	keys     crypto.ExternalKeySet
}

// newJWTFixture builds a provider whose keys come from secrets, current
// being the current version: the internal HMAC keys and the external
// Ed25519 keys are both derived from them. This is the one place that knows
// how a provider is constructed.
func newJWTFixture(t *testing.T, secrets map[int]string, current int) *jwtFixture {
	t.Helper()
	suite, err := crypto.New(context.Background(), crypto.Config{
		Secrets: crypto.StaticSecretSource{Secrets: secrets, Current: current},
	}, nil)
	require.NoError(t, err)
	keys := crypto.NewDerivedKeySet(suite.Keys)
	impl := crypto.NewJWTProvider(suite.JWTSign, keys, jwtConfig())
	return &jwtFixture{provider: impl, impl: impl, suite: suite, keys: keys}
}

func defaultFixture(t *testing.T) *jwtFixture {
	return newJWTFixture(t, map[int]string{1: secretV1}, 1)
}

// hmacKey is the key internal tokens of version are signed with.
func (fx *jwtFixture) hmacKey(t *testing.T, version int) []byte {
	t.Helper()
	key, err := fx.suite.Keys.GetByVersion(crypto.KeyPurposeJWTSign, version)
	require.NoError(t, err)
	return key
}

// externalKey is the private key external tokens of version are signed
// with: the Ed25519 key whose seed is the key manager's key of purpose
// jwt_external (ADR-0002).
func (fx *jwtFixture) externalKey(t *testing.T, version int) ed25519.PrivateKey {
	t.Helper()
	seed, err := fx.suite.Keys.GetByVersion(crypto.KeyPurposeJWTExternal, version)
	require.NoError(t, err)
	return ed25519.NewKeyFromSeed(seed)
}

// currentKid is the kid of the current external key: the current version.
func (fx *jwtFixture) currentKid() string { return strconv.Itoa(fx.suite.Keys.CurrentVersion()) }

// claims returns the claims of a valid token of kind, as a standard
// library writes them.
func claims(kind crypto.JWTKind) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": testIssuer, "aud": audienceOf(kind), "sub": "user-1",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

// standardToken signs c the way the provider's kind does, with its current
// key: HS256 with the key manager's key, or EdDSA with the key set's key.
func (fx *jwtFixture) standardToken(t *testing.T, kind crypto.JWTKind, c jwt.MapClaims) string {
	t.Helper()
	if kind == crypto.JWTInternal {
		return signWith(t, jwt.SigningMethodHS256, fx.hmacKey(t, 1), "1", c)
	}
	return signWith(t, jwt.SigningMethodEdDSA, fx.externalKey(t, fx.suite.Keys.CurrentVersion()), fx.currentKid(), c)
}

func signWith(t *testing.T, method jwt.SigningMethod, key any, kid string, c jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, c)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

// verifyStandard checks token with golang-jwt, as a third party would: the
// algorithm, the key, the issuer, the audience, and the expiry.
func (fx *jwtFixture) verifyStandard(t *testing.T, kind crypto.JWTKind, token string) (jwt.MapClaims, error) {
	t.Helper()
	method := "HS256"
	if kind == crypto.JWTExternal {
		method = "EdDSA"
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{method}), jwt.WithIssuer(testIssuer),
		jwt.WithAudience(audienceOf(kind)), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
	)
	parsed := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(token, parsed, func(tok *jwt.Token) (any, error) {
		kid, _ := tok.Header["kid"].(string)
		if kind == crypto.JWTInternal {
			version, err := strconv.Atoi(kid)
			if err != nil {
				return nil, err
			}
			return fx.suite.Keys.GetByVersion(crypto.KeyPurposeJWTSign, version)
		}
		key, err := fx.keys.PublicKey(context.Background(), kid)
		if err != nil {
			return nil, err
		}
		return key.Key, nil
	})
	return parsed, err
}

// segments decodes a token's header and payload as JSON objects.
func segments(t *testing.T, token string) (header, payload map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "a JWT has three segments")
	decode := func(s string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		require.NoError(t, err, "a segment is base64url without padding")
		m := map[string]any{}
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}
	return decode(parts[0]), decode(parts[1])
}

func encodeSegment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// refusedWith asserts that err is a typed error with code.
func refusedWith(t *testing.T, err error, code string, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	var de *errs.DomainError
	require.True(t, errors.As(err, &de), "the error is typed: %v", err)
	assert.True(t, errs.IsCode(err, code), "want code %q, got %v", code, err)
	switch code {
	case errs.CodeJWTUnknownKind, errs.CodeJWTExternalNotConfigured:
		assert.Equal(t, errs.CategoryConfiguration, de.Category, "a mistake of the caller's setup, not a refused token")
	default:
		assert.Equal(t, errs.CategoryToken, de.Category, "a refused token, which a route answers with 401")
		assert.Equal(t, "invalid or expired token", de.PublicMessage, "the message does not say which check failed")
	}
}

// A token the provider issues verifies, and carries the subject, the
// issuer, the kind's audience, the issue time, the expiry after the kind's
// TTL, and the custom claims.
func TestJWTRoundTrip(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	custom := behemoth.M{"role": "admin", "plan": "pro"}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			token, err := fx.provider.Sign(ctx, kind, crypto.JWTRequest{Subject: "user-1", Custom: custom})
			require.NoError(t, err)

			got, err := fx.provider.Verify(ctx, kind, token)
			require.NoError(t, err)
			assert.Equal(t, "user-1", got.Subject)
			assert.Equal(t, testIssuer, got.Issuer)
			assert.Equal(t, audienceOf(kind), got.Audience)
			assert.WithinDuration(t, time.Now(), got.IssuedAt, 5*time.Second)
			assert.WithinDuration(t, got.IssuedAt.Add(jwtConfig().TTL[kind]), got.ExpiresAt, 2*time.Second)
			assert.Equal(t, custom, got.Custom)
		})
	}
}

// A token the provider issues is a standard JWT: a header with the
// algorithm and the key's id, numeric dates (RFC 7519 NumericDate), and a
// signature that a standard library verifies with the same key.
func TestJWTIssuesStandardTokens(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			token, err := fx.provider.Sign(ctx, kind, crypto.JWTRequest{Subject: "user-1", Custom: behemoth.M{"role": "admin"}})
			require.NoError(t, err)

			header, payload := segments(t, token)
			wantAlg, wantKid := "HS256", "1"
			if kind == crypto.JWTExternal {
				wantAlg, wantKid = "EdDSA", fx.currentKid()
			}
			assert.Equal(t, wantAlg, header["alg"])
			assert.Equal(t, "JWT", header["typ"])
			assert.Equal(t, wantKid, header["kid"], "the kid names the key that signed")

			assert.Equal(t, testIssuer, payload["iss"])
			assert.Equal(t, audienceOf(kind), payload["aud"])
			assert.Equal(t, "user-1", payload["sub"])
			for _, date := range []string{"exp", "iat"} {
				assert.IsType(t, float64(0), payload[date], "%s is a NumericDate: seconds as a number", date)
			}
			if nbf, present := payload["nbf"]; present {
				assert.IsType(t, float64(0), nbf, "nbf, when present, is a NumericDate")
			}
			assert.Equal(t, map[string]any{"role": "admin"}, payload["custom"])

			parsed, err := fx.verifyStandard(t, kind, token)
			require.NoError(t, err, "a standard library verifies the token")
			assert.Equal(t, "user-1", parsed["sub"])
		})
	}
}

// A standard token signed with the provider's key verifies, in both forms
// RFC 7519 allows for the audience: a string, and an array.
func TestJWTAcceptsStandardTokens(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	for _, kind := range kinds {
		for name, aud := range map[string]any{
			"one audience":      audienceOf(kind),
			"an audience array": []string{"another-service", audienceOf(kind)},
		} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				c := claims(kind)
				c["aud"] = aud
				c["custom"] = map[string]any{"role": "admin"}
				got, err := fx.provider.Verify(ctx, kind, fx.standardToken(t, kind, c))
				require.NoError(t, err)
				assert.Equal(t, "user-1", got.Subject)
				assert.Equal(t, testIssuer, got.Issuer)
				assert.WithinDuration(t, time.Unix(c["exp"].(int64), 0), got.ExpiresAt, time.Second)
				assert.Equal(t, behemoth.M{"role": "admin"}, got.Custom)
			})
		}
	}
}

// A token that was changed after it was signed, or signed with another
// key, is refused with invalid_signature.
func TestJWTRefusesTamperedTokens(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	other := newJWTFixture(t, map[int]string{1: secretV2}, 1)
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			token, err := fx.provider.Sign(ctx, kind, crypto.JWTRequest{Subject: "user-1"})
			require.NoError(t, err)
			parts := strings.Split(token, ".")
			_, payload := segments(t, token)
			payload["sub"] = "admin"

			flipped, err := base64.RawURLEncoding.DecodeString(parts[2])
			require.NoError(t, err)
			flipped[0] ^= 0xff

			var foreign string
			if kind == crypto.JWTInternal {
				// Another key manager's key, under the same version.
				foreign = signWith(t, jwt.SigningMethodHS256, other.hmacKey(t, 1), "1", claims(kind))
			} else {
				// A key that is not the key set's, under the id of one that is.
				_, priv, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				foreign = signWith(t, jwt.SigningMethodEdDSA, priv, fx.currentKid(), claims(kind))
			}

			for name, bad := range map[string]string{
				"a changed payload":   parts[0] + "." + encodeSegment(t, payload) + "." + parts[2],
				"a changed signature": parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(flipped),
				"no signature":        parts[0] + "." + parts[1] + ".",
				"another key":         foreign,
			} {
				_, err := fx.provider.Verify(ctx, kind, bad)
				refusedWith(t, err, errs.CodeJWTInvalidSignature, name)
			}
		})
	}
}

// The algorithm is the kind's, whatever the token's header says. A token
// with another algorithm is refused before its signature is checked, with
// algorithm_mismatch.
func TestJWTPinsTheAlgorithm(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	unsigned := func(kind crypto.JWTKind) string {
		tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims(kind)).SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)
		return tok
	}
	internalToken, err := fx.provider.Sign(ctx, crypto.JWTInternal, crypto.JWTRequest{Subject: "user-1"})
	require.NoError(t, err)
	externalToken, err := fx.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1"})
	require.NoError(t, err)
	kid := fx.currentKid()
	public := fx.externalKey(t, 1).Public().(ed25519.PublicKey)

	cases := map[crypto.JWTKind]map[string]string{
		crypto.JWTInternal: {
			"none":                    unsigned(crypto.JWTInternal),
			"HS512 with the same key": signWith(t, jwt.SigningMethodHS512, fx.hmacKey(t, 1), "1", claims(crypto.JWTInternal)),
			"RS256":                   signWith(t, jwt.SigningMethodRS256, rsaKey, "1", claims(crypto.JWTInternal)),
			"an external token":       externalToken,
		},
		crypto.JWTExternal: {
			"none":                                unsigned(crypto.JWTExternal),
			"HS256 with the public key as secret": signWith(t, jwt.SigningMethodHS256, []byte(public), kid, claims(crypto.JWTExternal)),
			"ES256":                               signWith(t, jwt.SigningMethodES256, ecKey, kid, claims(crypto.JWTExternal)),
			"an internal token":                   internalToken,
		},
	}
	for kind, tokens := range cases {
		for name, token := range tokens {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				_, err := fx.provider.Verify(ctx, kind, token)
				refusedWith(t, err, errs.CodeJWTAlgorithmMismatch)
			})
		}
	}
}

// A correctly signed token is refused when a registered claim says it is
// not valid here or now.
func TestJWTChecksTheRegisteredClaims(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	now := time.Now()
	for _, kind := range kinds {
		otherAudience := audienceOf(crypto.JWTExternal)
		if kind == crypto.JWTExternal {
			otherAudience = audienceOf(crypto.JWTInternal)
		}
		with := func(change func(jwt.MapClaims)) string {
			c := claims(kind)
			change(c)
			return fx.standardToken(t, kind, c)
		}
		cases := []struct {
			name, code string
			token      string
		}{
			{"expired an hour ago", errs.CodeTokenExpired, with(func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Hour).Unix() })},
			{"not valid for an hour", errs.CodeJWTNotYetValid, with(func(c jwt.MapClaims) { c["nbf"] = now.Add(time.Hour).Unix() })},
			{"another issuer", errs.CodeJWTIssuerMismatch, with(func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" })},
			{"another audience", errs.CodeJWTAudienceMismatch, with(func(c jwt.MapClaims) { c["aud"] = "another-service" })},
			{"the other kind's audience", errs.CodeJWTAudienceMismatch, with(func(c jwt.MapClaims) { c["aud"] = otherAudience })},
			{"no expiry", "", with(func(c jwt.MapClaims) { delete(c, "exp") })},
			{"no audience", "", with(func(c jwt.MapClaims) { delete(c, "aud") })},
		}
		for _, tc := range cases {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				_, err := fx.provider.Verify(ctx, kind, tc.token)
				if tc.code == "" {
					require.Error(t, err, "a token without the claim is refused")
					return
				}
				refusedWith(t, err, tc.code)
			})
		}
	}
}

// Input that is not a JWT is refused with malformed_token, and does not
// make the provider panic.
func TestJWTRefusesMalformedTokens(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	body := encodeSegment(t, claims(crypto.JWTInternal))
	for _, kind := range kinds {
		for name, token := range map[string]string{
			"empty":                     "",
			"one segment":               "abc",
			"two segments":              "abc.def",
			"four segments":             "a.b.c.d",
			"not base64":                "!!!.!!!.!!!",
			"a header that is not JSON": notJSON + "." + body + ".c2ln",
		} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				var err error
				require.NotPanics(t, func() { _, err = fx.provider.Verify(ctx, kind, token) })
				refusedWith(t, err, errs.CodeJWTMalformed)
			})
		}
	}
}

// Keys rotate without ending the tokens they signed: an internal token
// names its key version, and an external one its key's id. A token whose
// key is gone is refused.
func TestJWTKeyRotation(t *testing.T) {
	ctx := context.Background()

	t.Run(string(crypto.JWTInternal), func(t *testing.T) {
		before := newJWTFixture(t, map[int]string{1: secretV1}, 1)
		old, err := before.provider.Sign(ctx, crypto.JWTInternal, crypto.JWTRequest{Subject: "user-1"})
		require.NoError(t, err)

		rotated := newJWTFixture(t, map[int]string{1: secretV1, 2: secretV2}, 2)
		_, err = rotated.provider.Verify(ctx, crypto.JWTInternal, old)
		require.NoError(t, err, "a token of the previous version still verifies")
		fresh, err := rotated.provider.Sign(ctx, crypto.JWTInternal, crypto.JWTRequest{Subject: "user-1"})
		require.NoError(t, err)
		header, _ := segments(t, fresh)
		assert.Equal(t, "2", header["kid"], "new tokens use the current version")

		retired := newJWTFixture(t, map[int]string{2: secretV2}, 2)
		var verifyErr error
		require.NotPanics(t, func() { _, verifyErr = retired.provider.Verify(ctx, crypto.JWTInternal, old) })
		refusedWith(t, verifyErr, errs.CodeJWTUnknownKey, "a version that was removed no longer verifies, and is a refused token")
		_, err = retired.provider.Verify(ctx, crypto.JWTInternal, fresh)
		assert.NoError(t, err)
	})

	t.Run(string(crypto.JWTExternal), func(t *testing.T) {
		before := newJWTFixture(t, map[int]string{1: secretV1}, 1)
		old, err := before.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1"})
		require.NoError(t, err)

		rotated := newJWTFixture(t, map[int]string{1: secretV1, 2: secretV2}, 2)
		_, err = rotated.provider.Verify(ctx, crypto.JWTExternal, old)
		require.NoError(t, err, "a token of the previous version still verifies")
		fresh, err := rotated.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1"})
		require.NoError(t, err)
		header, _ := segments(t, fresh)
		assert.Equal(t, "2", header["kid"], "new tokens use the current version")

		retired := newJWTFixture(t, map[int]string{2: secretV2}, 2)
		_, err = retired.provider.Verify(ctx, crypto.JWTExternal, old)
		refusedWith(t, err, errs.CodeJWTUnknownKey, "a version that was removed no longer verifies")

		_, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		for _, kid := range []string{"key-that-never-was", "7", "01", ""} {
			unknown := signWith(t, jwt.SigningMethodEdDSA, priv, kid, claims(crypto.JWTExternal))
			var verifyErr error
			require.NotPanics(t, func() { _, verifyErr = rotated.provider.Verify(ctx, crypto.JWTExternal, unknown) })
			refusedWith(t, verifyErr, errs.CodeJWTUnknownKey, "kid %q", kid)
		}
	})
}

// A kind the provider does not know is refused, and external tokens need a
// key set. Internal tokens work without one.
func TestJWTKinds(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	_, err := fx.provider.Sign(ctx, "session", crypto.JWTRequest{Subject: "user-1"})
	refusedWith(t, err, errs.CodeJWTUnknownKind)
	_, err = fx.provider.Verify(ctx, "session", "a.b.c")
	refusedWith(t, err, errs.CodeJWTUnknownKind)

	var withoutKeys crypto.JWTProvider = crypto.NewJWTProvider(fx.suite.JWTSign, nil, jwtConfig())
	_, err = withoutKeys.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1"})
	refusedWith(t, err, errs.CodeJWTExternalNotConfigured)
	_, err = withoutKeys.Verify(ctx, crypto.JWTExternal, "a.b.c")
	refusedWith(t, err, errs.CodeJWTExternalNotConfigured)
	token, err := withoutKeys.Sign(ctx, crypto.JWTInternal, crypto.JWTRequest{Subject: "user-1"})
	require.NoError(t, err)
	_, err = withoutKeys.Verify(ctx, crypto.JWTInternal, token)
	assert.NoError(t, err)
}

// Custom claims travel under "custom", and can't stand in for a registered
// claim.
func TestJWTCustomClaimsStayCustom(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			custom := behemoth.M{"sub": "admin", "iss": "https://evil.example.com", "aud": "another-service", "exp": "never"}
			token, err := fx.provider.Sign(ctx, kind, crypto.JWTRequest{Subject: "user-1", Custom: custom})
			require.NoError(t, err)

			_, payload := segments(t, token)
			assert.Equal(t, "user-1", payload["sub"])
			assert.Equal(t, testIssuer, payload["iss"])

			got, err := fx.provider.Verify(ctx, kind, token)
			require.NoError(t, err)
			assert.Equal(t, "user-1", got.Subject)
			assert.Equal(t, testIssuer, got.Issuer)
			assert.Equal(t, audienceOf(kind), got.Audience)
			assert.Equal(t, custom, got.Custom)
		})
	}
}

// The external key set publishes every public key, and only public keys, in
// the form a third party uses to verify an external token. The route that
// serves it is tested in auth.
func TestJWTPublishesTheExternalKeys(t *testing.T) {
	ctx := context.Background()
	fx := newJWTFixture(t, map[int]string{1: secretV1, 2: secretV2}, 2)

	keys, err := fx.keys.PublicKeys(ctx)
	require.NoError(t, err)
	body, err := crypto.MarshalJWKSet(keys)
	require.NoError(t, err)

	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(body, &set))
	require.Len(t, set.Keys, 2, "one key per secret version, the previous one included")
	published := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		assert.Equal(t, "OKP", k["kty"])
		assert.Equal(t, "Ed25519", k["crv"])
		assert.Equal(t, "EdDSA", k["alg"])
		assert.Equal(t, "sig", k["use"])
		assert.NotContains(t, k, "d", "no private key")
		x, err := base64.RawURLEncoding.DecodeString(k["x"])
		require.NoError(t, err)
		published[k["kid"]] = ed25519.PublicKey(x)
	}
	assert.Equal(t, fx.externalKey(t, 1).Public(), published["1"])
	assert.Equal(t, fx.externalKey(t, 2).Public(), published["2"])

	// A third party verifies a token with the published key alone.
	token, err := fx.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1"})
	require.NoError(t, err)
	_, err = jwt.NewParser(jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience(externalAudience), jwt.WithExpirationRequired()).
		Parse(token, func(tok *jwt.Token) (any, error) {
			kid, _ := tok.Header["kid"].(string)
			key, ok := published[kid]
			if !ok {
				return nil, jwt.ErrTokenUnverifiable
			}
			return key, nil
		})
	assert.NoError(t, err, "a third party verifies an external token from the published keys")
}

// A token is issued for the kind's first audience, or for another one of
// its list, and for nothing outside the list. It carries the session it was
// issued for and an id of its own.
func TestJWTAudiencesSessionAndID(t *testing.T) {
	ctx := context.Background()
	fx := defaultFixture(t)

	first, err := fx.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1", SessionID: "session-9"})
	require.NoError(t, err)
	second, err := fx.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1", Audience: otherAudience})
	require.NoError(t, err)

	_, payload := segments(t, first)
	assert.Equal(t, externalAudience, payload["aud"], "no audience asked for: the first")
	assert.Equal(t, "session-9", payload["sid"])
	_, otherPayload := segments(t, second)
	assert.Equal(t, otherAudience, otherPayload["aud"])
	assert.NotContains(t, otherPayload, "sid", "no session, no sid")
	assert.NotEmpty(t, payload["jti"])
	assert.NotEqual(t, payload["jti"], otherPayload["jti"], "every token has an id of its own")

	got, err := fx.provider.Verify(ctx, crypto.JWTExternal, first)
	require.NoError(t, err)
	assert.Equal(t, "session-9", got.SessionID)
	assert.Equal(t, payload["jti"], got.ID)
	got, err = fx.provider.Verify(ctx, crypto.JWTExternal, second)
	require.NoError(t, err)
	assert.Equal(t, otherAudience, got.Audience, "the audience the token names")

	_, err = fx.provider.Sign(ctx, crypto.JWTExternal, crypto.JWTRequest{Subject: "user-1", Audience: internalAudience})
	assert.True(t, errs.IsCategory(err, errs.CategoryValidation) && errs.IsCode(err, "audience_invalid_input"),
		"an audience of another kind is not on the list: %v", err)
	_, err = fx.provider.Sign(ctx, crypto.JWTInternal, crypto.JWTRequest{})
	assert.True(t, errs.IsCode(err, "subject_invalid_input"), "a token is about someone: %v", err)
}

// The external keys are derived as ADR-0002 says: the seed of version v is
// HKDF-SHA256 over the secret of v, with "jwt_external" as the info, used
// as it is. A change to any part of it changes every published key, and
// every token issued before stops verifying.
func TestJWTExternalKeysAreDerivedFromTheMasterSecret(t *testing.T) {
	assert.Equal(t, crypto.KeyPurpose("jwt_external"), crypto.KeyPurposeJWTExternal, "the purpose string never changes")

	fx := newJWTFixture(t, map[int]string{1: secretV1}, 1)
	published, err := fx.keys.PublicKey(context.Background(), "1")
	require.NoError(t, err)

	secret, err := hex.DecodeString(secretV1)
	require.NoError(t, err)
	seed := make([]byte, ed25519.SeedSize)
	_, err = io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte("jwt_external")), seed)
	require.NoError(t, err)
	assert.Equal(t, ed25519.NewKeyFromSeed(seed).Public(), published.Key, "the seed is the HKDF output")

	assert.Equal(t, "bbde0ec5ce395dfc8b14cd56c22727d29dd3547b8171e19f81a8c20dffa678a3",
		hex.EncodeToString(published.Key.(ed25519.PublicKey)), "the key of a known secret")
	assert.Equal(t, "EdDSA", published.Algorithm)
}
