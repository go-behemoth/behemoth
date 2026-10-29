package crypto

import (
	"bytes"
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

// The signature of each example verifies with the key ParseJWKSet reads
// from the RFC's JWK, through parse, fits, and the method VerifyJWT takes
// for the token's alg, and fails with a byte changed. The examples have no
// audience, which VerifyJWT requires, so they are not run through it whole.
// Most payloads are not claims either, which parse does not read.
func TestJOSEExampleSignatures(t *testing.T) {
	for _, tc := range []struct {
		name, key, token, alg, kid string
	}{
		{"RFC 7515 A.2", rfc7515A2Key, rfc7515A2JWS, "RS256", ""},
		{"RFC 7515 A.3", rfc7515A3Key, rfc7515A3JWS, "ES256", ""},
		{"RFC 7515 A.4", rfc7515A4Key, rfc7515A4JWS, "ES512", ""},
		{"RFC 7520 4.1", rfc7520RSAKey, rfc7520RS256JWS, "RS256", "bilbo.baggins@hobbiton.example"},
		{"RFC 7520 4.2", rfc7520RSAKey, rfc7520PS384JWS, "PS384", "bilbo.baggins@hobbiton.example"},
		{"RFC 7520 4.3", rfc7520ECKey, rfc7520ES512JWS, "ES512", "bilbo.baggins@hobbiton.example"},
		{"RFC 8037 A.5", rfc8037PublicKey, rfc8037JWS, "EdDSA", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := ParseJWKSet(setOf(tc.key))
			require.NoError(t, err)
			require.Len(t, keys, 1)
			key := keys[0]

			p, err := parse("test", tc.token)
			require.NoError(t, err)
			require.Equal(t, tc.alg, p.alg)
			assert.Equal(t, tc.kid, p.kid)
			parts := strings.Split(tc.token, ".")
			assert.Equal(t, parts[0]+"."+parts[1], p.signingString)
			assert.Equal(t, tc.kid, key.KID)
			require.True(t, fits(key, p.alg), "the key does not fit %s", p.alg)

			method := jwt.GetSigningMethod(p.alg)
			assert.NoError(t, method.Verify(p.signingString, p.signature, key.Key))
			changed := bytes.Clone(p.signature)
			changed[len(changed)/2] ^= 1
			assert.Error(t, method.Verify(p.signingString, changed, key.Key), "a changed signature")
		})
	}
}
