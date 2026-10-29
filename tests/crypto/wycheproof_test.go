package crypto_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
)

// Project Wycheproof's test vectors, run through Behemoth: AES-GCM through
// the encryptor, and the signatures of every JWS algorithm VerifyJWT
// accepts through the steps of VerifyJWT that check a signature. The files
// are in testdata/wycheproof, whose README says where they come from.

// hexBytes is a Wycheproof hex string, decoded.
type hexBytes []byte

func (b *hexBytes) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	decoded, err := hex.DecodeString(s)
	*b = decoded
	return err
}

// wycheproofFile is what these tests read of a Wycheproof file. A member
// the file's schema lacks stays empty.
type wycheproofFile struct {
	TestGroups []wycheproofGroup `json:"testGroups"`
}

type wycheproofGroup struct {
	KeySize int `json:"keySize"`
	IVSize  int `json:"ivSize"`
	TagSize int `json:"tagSize"`
	// The public key of a signature group, as a JWK. The RSA PKCS #1
	// files name it keyJwk.
	PublicKeyJWK json.RawMessage `json:"publicKeyJwk"`
	KeyJWK       json.RawMessage `json:"keyJwk"`
	// The public key as numbers: the coordinates of an ECDSA key, or an
	// Ed25519 key's bytes.
	PublicKey struct {
		Curve string   `json:"curve"`
		Wx    hexBytes `json:"wx"`
		Wy    hexBytes `json:"wy"`
		PK    hexBytes `json:"pk"`
	} `json:"publicKey"`
	Tests []struct {
		ID      int      `json:"tcId"`
		Comment string   `json:"comment"`
		Flags   []string `json:"flags"`
		// Result is valid, invalid, or acceptable: valid, but of a kind an
		// implementation may refuse.
		Result string   `json:"result"`
		Key    hexBytes `json:"key"`
		IV     hexBytes `json:"iv"`
		AAD    hexBytes `json:"aad"`
		Msg    hexBytes `json:"msg"`
		CT     hexBytes `json:"ct"`
		Tag    hexBytes `json:"tag"`
		Sig    hexBytes `json:"sig"`
	} `json:"tests"`
}

// jwk returns the group's public key as a JWK. The groups of special keys
// Wycheproof added later give the key as numbers only, which jwk writes as
// a JWK the way an issuer would.
func (g wycheproofGroup) jwk(t *testing.T) []byte {
	t.Helper()
	switch {
	case g.PublicKeyJWK != nil:
		return g.PublicKeyJWK
	case g.KeyJWK != nil:
		return g.KeyJWK
	}
	b64 := base64.RawURLEncoding.EncodeToString
	pk := g.PublicKey
	switch pk.Curve {
	case "edwards25519":
		return fmt.Appendf(nil, `{"kty":"OKP","crv":"Ed25519","x":%q}`, b64(pk.PK))
	case "secp256r1", "secp384r1", "secp521r1":
		crv, size := map[string]string{"secp256r1": "P-256", "secp384r1": "P-384", "secp521r1": "P-521"}[pk.Curve],
			map[string]int{"secp256r1": 32, "secp384r1": 48, "secp521r1": 66}[pk.Curve]
		return fmt.Appendf(nil, `{"kty":"EC","crv":%q,"x":%q,"y":%q}`, crv, b64(fixedSize(t, pk.Wx, size)), b64(fixedSize(t, pk.Wy, size)))
	}
	t.Fatalf("a group key on curve %q", pk.Curve)
	return nil
}

// fixedSize returns the big-endian integer b in size bytes. Wycheproof
// writes a coordinate as an ASN.1 integer, with a zero byte first when its
// top bit is set.
func fixedSize(t *testing.T, b []byte, size int) []byte {
	t.Helper()
	b = bytes.TrimLeft(b, "\x00")
	require.LessOrEqual(t, len(b), size)
	return append(make([]byte, size-len(b)), b...)
}

func loadWycheproof(t *testing.T, name string) wycheproofFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "wycheproof", name))
	require.NoError(t, err)
	var f wycheproofFile
	require.NoError(t, json.Unmarshal(data, &f), name)
	require.NotEmpty(t, f.TestGroups, name)
	return f
}

// oneKey is a KeyManager that returns key for every purpose and version.
type oneKey struct{ key []byte }

func (k oneKey) Current(crypto.KeyPurpose) ([]byte, int, error)      { return k.key, 1, nil }
func (k oneKey) GetByVersion(crypto.KeyPurpose, int) ([]byte, error) { return k.key, nil }
func (k oneKey) CurrentVersion() int                                 { return 1 }
func (k oneKey) Versions() []int                                     { return []int{1} }
func (k oneKey) RemoveVersion(int) error                             { return nil }

// oneNonce is a Randomizer whose random bytes are nonce.
type oneNonce struct{ nonce []byte }

func (r oneNonce) SecureRandomBytes(n int) ([]byte, error) {
	if n != len(r.nonce) {
		return nil, fmt.Errorf("asked for %d bytes, have %d", n, len(r.nonce))
	}
	return r.nonce, nil
}

func (r oneNonce) SecureRandomString(int) (string, error) { return "", errors.New("not used") }

// The encryptor seals and opens as Wycheproof's AES-GCM vectors give it,
// for the ones it can express: a 256-bit key, which is what the key manager
// derives, a 12-byte nonce, and a 16-byte tag. The vector's key is the key
// manager's key, and its nonce the randomizer's bytes. A valid vector seals
// to its nonce, ciphertext, and tag, and opens to its message. An invalid
// one does not open.
func TestWycheproofAESGCM(t *testing.T) {
	f := loadWycheproof(t, "aes_gcm_test.json")
	count := map[string]int{}
	for _, g := range f.TestGroups {
		if g.KeySize != 256 || g.IVSize != 96 || g.TagSize != 128 {
			continue
		}
		for _, tc := range g.Tests {
			count[tc.Result]++
			enc := crypto.NewEncryptor(oneKey{tc.Key}, oneNonce{tc.IV})
			sealed := append(append(append([]byte{}, tc.IV...), tc.CT...), tc.Tag...)
			plaintext, err := enc.Decrypt(sealed, 1, tc.AAD)
			switch tc.Result {
			case "valid":
				if assert.NoError(t, err, "tcId %d (%s): Decrypt", tc.ID, tc.Comment) {
					assert.Equal(t, hex.EncodeToString(tc.Msg), hex.EncodeToString(plaintext), "tcId %d: Decrypt", tc.ID)
				}
				got, _, err := enc.Encrypt(tc.Msg, tc.AAD)
				if assert.NoError(t, err, "tcId %d: Encrypt", tc.ID) {
					assert.Equal(t, hex.EncodeToString(sealed), hex.EncodeToString(got), "tcId %d: Encrypt", tc.ID)
				}
			case "invalid":
				assert.True(t, errs.IsCode(err, "invalid_payload"), "tcId %d (%s %v): want invalid_payload, got %v", tc.ID, tc.Comment, tc.Flags, err)
			default:
				t.Errorf("tcId %d: result %q", tc.ID, tc.Result)
			}
		}
	}
	require.NotZero(t, count["valid"])
	require.NotZero(t, count["invalid"])
	t.Logf("valid %d, invalid %d", count["valid"], count["invalid"])
}

// Wycheproof's signature vectors of every JWS algorithm VerifyJWT accepts,
// through the steps of VerifyJWT that check a signature: the group's public
// key, as a JWK, goes through ParseJWKSet, and the signature through the
// method VerifyJWT takes for the algorithm. A valid signature verifies and
// an invalid one does not. An acceptable one may do either, and the test
// logs what it did. A key ParseJWKSet leaves out may have no valid
// signature.
//
// One kind of invalid signature verifies: a PSS signature with a salt
// length other than the hash's, which RFC 7518 section 3.5 sets. golang-jwt's
// PSS methods find the salt length rather than require it. Only the holder
// of the key can make such a signature. docs/ongoing.md has the entry; the
// test counts these and fails on any other invalid signature that verifies.
func TestWycheproofSignatures(t *testing.T) {
	for _, tc := range []struct{ alg, file string }{
		{"ES256", "ecdsa_secp256r1_sha256_p1363_test.json"},
		{"ES384", "ecdsa_secp384r1_sha384_p1363_test.json"},
		{"ES512", "ecdsa_secp521r1_sha512_p1363_test.json"},
		{"RS256", "rsa_signature_2048_sha256_test.json"},
		{"RS384", "rsa_signature_3072_sha384_test.json"},
		{"RS512", "rsa_signature_3072_sha512_test.json"},
		{"PS256", "rsa_pss_2048_sha256_mgf1_32_test.json"},
		{"PS384", "rsa_pss_4096_sha384_mgf1_48_test.json"},
		{"PS512", "rsa_pss_4096_sha512_mgf1_64_test.json"},
		{"EdDSA", "ed25519_test.json"},
	} {
		t.Run(tc.alg, func(t *testing.T) {
			f := loadWycheproof(t, tc.file)
			method := jwt.GetSigningMethod(tc.alg)
			count := map[string]int{}
			for gi, g := range f.TestGroups {
				jwk := g.jwk(t)
				keys, err := crypto.ParseJWKSet([]byte(`{"keys":[` + string(jwk) + `]}`))
				require.NoError(t, err)
				if len(keys) == 0 {
					count["refused keys"]++
					for _, v := range g.Tests {
						assert.NotEqual(t, "valid", v.Result, "tcId %d: a valid signature by a key ParseJWKSet left out: %s", v.ID, jwk)
					}
					continue
				}
				key := keys[0]
				assert.Contains(t, []string{"", tc.alg}, key.Algorithm, "group %d", gi)
				for _, v := range g.Tests {
					count[v.Result]++
					err := method.Verify(string(v.Msg), v.Sig, key.Key)
					switch v.Result {
					case "valid":
						assert.NoError(t, err, "tcId %d (%s %v)", v.ID, v.Comment, v.Flags)
					case "invalid":
						if err == nil && strings.HasPrefix(tc.alg, "PS") && strings.HasPrefix(v.Comment, "s_len changed to") {
							count["other salt lengths"]++
							continue
						}
						assert.Error(t, err, "tcId %d (%s %v): verified", v.ID, v.Comment, v.Flags)
					case "acceptable":
						t.Logf("tcId %d (%s %v), acceptable: verified %v", v.ID, v.Comment, v.Flags, err == nil)
					default:
						t.Errorf("tcId %d: result %q", v.ID, v.Result)
					}
				}
			}
			require.NotZero(t, count["valid"])
			require.NotZero(t, count["invalid"])
			t.Logf("valid %d, invalid %d (of which other salt lengths that verified %d), acceptable %d, keys left out %d",
				count["valid"], count["invalid"], count["other salt lengths"], count["acceptable"], count["refused keys"])
		})
	}
}
