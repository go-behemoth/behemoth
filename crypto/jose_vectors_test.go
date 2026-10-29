package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The JOSE examples of RFC 7515, 7517, 7520, and 8037: their keys through
// ParseJWKSet and MarshalJWKSet, and their signatures through the steps of
// VerifyJWT that check one. The keys and tokens are in
// jose_vectors_data_test.go. Behemoth signs no JWS with these algorithms,
// so only verification has a known answer to check. The Ed25519 signing of
// RFC 8037 is in vectors_test.go.

// setOf returns a JWK set of keys, each a JWK.
func setOf(keys ...string) []byte {
	return []byte(`{"keys":[` + strings.Join(keys, ",") + `]}`)
}

// ParseJWKSet keeps the keys of the examples that verify signatures and
// leaves out the others, as RFC 7517 advises, and MarshalJWKSet writes the
// members of a kept key as the RFC prints them.
func TestJOSEExampleKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []byte
		// kept are the positions in set of the keys ParseJWKSet keeps, and
		// algs their algorithms.
		kept []int
		algs []string
	}{
		{"RFC 7517 A.1: the EC key's use is enc", []byte(rfc7517A1Set), []int{1}, []string{"RS256"}},
		{"RFC 7517 A.2: private keys", []byte(rfc7517A2Set), nil, nil},
		{"RFC 7517 A.3: symmetric keys", []byte(rfc7517A3Set), nil, nil},
		{"RFC 7517 B: a key with x5c", setOf(rfc7517BKey), []int{0}, []string{""}},
		{"RFC 7520 3.1: P-521", setOf(rfc7520ECKey), []int{0}, []string{"ES512"}},
		{"RFC 7520 3.3: RSA", setOf(rfc7520RSAKey), []int{0}, []string{""}},
		{"RFC 8037 A.1: a private key", setOf(rfc8037PrivateKey), nil, nil},
		{"RFC 8037 A.2: Ed25519", setOf(rfc8037PublicKey), []int{0}, []string{"EdDSA"}},
		{"RFC 7515 A.2 to A.4", setOf(rfc7515A2Key, rfc7515A3Key, rfc7515A4Key), []int{0, 1, 2}, []string{"", "ES256", "ES512"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var source struct {
				Keys []map[string]any `json:"keys"`
			}
			require.NoError(t, json.Unmarshal(tc.set, &source))

			keys, err := ParseJWKSet(tc.set)
			require.NoError(t, err)
			require.Len(t, keys, len(tc.kept))
			if len(keys) == 0 {
				return
			}

			out, err := MarshalJWKSet(keys)
			require.NoError(t, err)
			var written struct {
				Keys []map[string]any `json:"keys"`
			}
			require.NoError(t, json.Unmarshal(out, &written))
			require.Len(t, written.Keys, len(keys))
			for i, pos := range tc.kept {
				src := source.Keys[pos]
				kid, _ := src["kid"].(string)
				assert.Equal(t, kid, keys[i].KID, "key %d: kid", i)
				assert.Equal(t, tc.algs[i], keys[i].Algorithm, "key %d: algorithm", i)
				for _, member := range []string{"kty", "crv", "x", "y", "n", "e"} {
					assert.Equal(t, src[member], written.Keys[i][member], "key %d: %s", i, member)
				}
			}
		})
	}
}

// compactParts splits a compact JWS into its header's alg and kid, its
// signing input, and its signature, without reading the payload: parse
// reads the payload as claims, which only RFC 7515's A.2 and A.3 have.
func compactParts(t *testing.T, token string) (alg, kid, signingString string, signature []byte) {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	require.NoError(t, json.Unmarshal(header, &h))
	signature, err = base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	return h.Alg, h.Kid, parts[0] + "." + parts[1], signature
}

// The signature of each example verifies with the key ParseJWKSet reads
// from the RFC's JWK, through the method VerifyJWT takes for the token's
// alg, and fails with a byte changed. The examples have no audience, which
// VerifyJWT requires, so they are not run through it whole.
func TestJOSEExampleSignatures(t *testing.T) {
	for _, tc := range []struct {
		name, key, token, alg string
		claims                bool // the payload is a JSON object, which parse reads
	}{
		{"RFC 7515 A.2", rfc7515A2Key, rfc7515A2JWS, "RS256", true},
		{"RFC 7515 A.3", rfc7515A3Key, rfc7515A3JWS, "ES256", true},
		{"RFC 7515 A.4", rfc7515A4Key, rfc7515A4JWS, "ES512", false},
		{"RFC 7520 4.1", rfc7520RSAKey, rfc7520RS256JWS, "RS256", false},
		{"RFC 7520 4.2", rfc7520RSAKey, rfc7520PS384JWS, "PS384", false},
		{"RFC 7520 4.3", rfc7520ECKey, rfc7520ES512JWS, "ES512", false},
		{"RFC 8037 A.5", rfc8037PublicKey, rfc8037JWS, "EdDSA", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := ParseJWKSet(setOf(tc.key))
			require.NoError(t, err)
			require.Len(t, keys, 1)
			key := keys[0]

			alg, kid, signingString, signature := compactParts(t, tc.token)
			require.Equal(t, tc.alg, alg)
			if tc.claims {
				p, err := parse("test", tc.token, jwt.MapClaims{})
				require.NoError(t, err)
				assert.Equal(t, alg, p.alg)
				assert.Equal(t, kid, p.kid)
				assert.Equal(t, signingString, p.signingString)
				assert.Equal(t, signature, p.signature)
			}
			assert.Equal(t, kid, key.KID)
			require.True(t, fits(key, alg), "the key does not fit %s", alg)

			method := jwt.GetSigningMethod(alg)
			assert.NoError(t, method.Verify(signingString, signature, key.Key))
			changed := bytes.Clone(signature)
			changed[len(changed)/2] ^= 1
			assert.Error(t, method.Verify(signingString, changed, key.Key), "a changed signature")
		})
	}
}
