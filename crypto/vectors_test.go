package crypto_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/crypto"
)

// Published test vectors of the primitives the key manager, the signer, the
// secret hasher, and the derived key set build on, run through them. The
// JOSE examples are in jose_vectors_test.go, the reference vectors of
// Argon2id in password_hasher_test.go, and Wycheproof's in tests/crypto.

// unhex decodes the hex of a test vector.
func unhex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// The signer and the secret hasher compute HMAC-SHA256 as RFC 4231 gives
// it: test cases 1 to 4, 6, and 7, each under the case's key as the key of
// the purpose. Case 5 truncates the output, which neither does. RFC 4231's
// HMAC-SHA-512 results have nothing to check: Behemoth does not use
// HMAC-SHA512.
func TestHMACSHA256_RFC4231(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key, data []byte
		mac       string
	}{
		{"case 1", bytes.Repeat([]byte{0x0b}, 20), []byte("Hi There"),
			"b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"},
		{"case 2: a key shorter than the output", []byte("Jefe"), []byte("what do ya want for nothing?"),
			"5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"},
		{"case 3", bytes.Repeat([]byte{0xaa}, 20), bytes.Repeat([]byte{0xdd}, 50),
			"773ea91e36800e46854db8ebd09181a72959098b3ef8c122d9635514ced565fe"},
		{"case 4", unhex("0102030405060708090a0b0c0d0e0f10111213141516171819"), bytes.Repeat([]byte{0xcd}, 50),
			"82558a389a443c0ea4cc819899f2083a85f0faa3e578f8077a2e3ff46729665b"},
		{"case 6: a key longer than the block", bytes.Repeat([]byte{0xaa}, 131),
			[]byte("Test Using Larger Than Block-Size Key - Hash Key First"),
			"60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"},
		{"case 7: a key and data longer than the block", bytes.Repeat([]byte{0xaa}, 131),
			[]byte("This is a test using a larger than block-size key and a larger than block-size data. The key needs to be hashed before being used by the HMAC algorithm."),
			"9b09ffa71b942fcb27635fbcd5b0e944bfdc63644f0713938a7f51535c3a35e2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := fixedKeys{key: tc.key}

			signer := crypto.NewSigner(keys, crypto.KeyPurposeJWTSign)
			mac, _, err := signer.Sign(tc.data)
			require.NoError(t, err)
			assert.Equal(t, tc.mac, hex.EncodeToString(mac), "Sign")
			ok, err := signer.Verify(tc.data, unhex(tc.mac), 1)
			require.NoError(t, err)
			assert.True(t, ok, "Verify")

			hasher := crypto.NewSecretHasher(keys)
			hash, _, err := hasher.Hash(string(tc.data))
			require.NoError(t, err)
			assert.Equal(t, tc.mac, hash, "Hash")
			ok, err = hasher.Verify(string(tc.data), tc.mac, 1)
			require.NoError(t, err)
			assert.True(t, ok, "Verify")
		})
	}
}

// The key manager derives a key as RFC 5869 gives HKDF-SHA256: test case 3,
// the one without a salt, as the key manager has none. Its info is empty
// where the key manager's is the purpose, and the key manager's 32-byte key
// is the start of its 42-byte output: the first bytes of HKDF's output are
// the same whatever length is asked for. Cases 1 and 2 have a salt, and
// cases 4 to 7 use SHA-1.
func TestKeyManagerDerivesWithHKDF_RFC5869(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	src := crypto.StaticSecretSource{Secrets: map[int]string{1: hex.EncodeToString(ikm)}, Current: 1}
	// The case's input key is 22 bytes, under the 32 a master secret needs.
	km, err := crypto.NewDefaultKeyManager(context.Background(), crypto.KeyManagerConfig{MinSecretBytes: 22}, src, nil)
	require.NoError(t, err)

	key, version, err := km.Current(crypto.KeyPurpose(""))
	require.NoError(t, err)
	assert.Equal(t, 1, version)
	const okm = "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8"
	assert.Equal(t, okm[:64], hex.EncodeToString(key))
}

// The derived key set signs as RFC 8032 gives Ed25519, tests 1 to 3, and as
// RFC 8037 appendix A.4 gives EdDSA over a JWS signing input: the test's
// secret key is the key manager's jwt_external key, which the set takes as
// the seed. The key it publishes is the test's public key, and the signature
// verifies with it through the method VerifyJWT takes for EdDSA.
func TestDerivedKeySetSigns_RFC8032(t *testing.T) {
	const test1Secret = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	const test1Public = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	for _, tc := range []struct {
		name           string
		secret, public string
		msg            []byte
		sig            string
	}{
		{"RFC 8032 test 1", test1Secret, test1Public, nil,
			"e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"},
		{"RFC 8032 test 2",
			"4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
			"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", unhex("72"),
			"92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"},
		{"RFC 8032 test 3",
			"c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7",
			"fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025", unhex("af82"),
			"6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a"},
		// RFC 8037's key is RFC 8032's test 1.
		{"RFC 8037 A.4", test1Secret, test1Public, []byte("eyJhbGciOiJFZERTQSJ9.RXhhbXBsZSBvZiBFZDI1NTE5IHNpZ25pbmc"),
			"860c98d2297f3060a33f42739672d61b53cf3adefed3d3c672f320dc021b411e9d59b8628dc351e248b88b29468e0e41855b0fb7d83bb15be902bfccb8cd0a02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			keys := crypto.NewDerivedKeySet(fixedKeys{key: unhex(tc.secret)})

			signing, err := keys.CurrentKey(ctx)
			require.NoError(t, err)
			assert.Equal(t, "EdDSA", signing.Algorithm())
			sig, err := signing.Sign(ctx, tc.msg)
			require.NoError(t, err)
			assert.Equal(t, tc.sig, hex.EncodeToString(sig))

			published, err := keys.PublicKey(ctx, signing.KID())
			require.NoError(t, err)
			assert.Equal(t, ed25519.PublicKey(unhex(tc.public)), published.Key)
			assert.NoError(t, jwt.GetSigningMethod("EdDSA").Verify(string(tc.msg), sig, published.Key))
		})
	}
}
