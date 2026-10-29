package crypto_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/keysource"
)

// Wycheproof's JOSE vectors: the JWS file and the key set file, run
// through VerifyJWT with the public halves of each group's keys in a
// CachedKeySet.

// joseFile is what these tests read of a Wycheproof JOSE file.
type joseFile struct {
	TestGroups []struct {
		Comment string `json:"comment"`
		// Private is a private JWK, or a set of them.
		Private json.RawMessage `json:"private"`
		Tests   []struct {
			ID      int      `json:"tcId"`
			Comment string   `json:"comment"`
			Flags   []string `json:"flags"`
			// JWS is the compact form, a string, or a JSON serialization.
			JWS    json.RawMessage `json:"jws"`
			Result string          `json:"result"`
		} `json:"tests"`
	} `json:"testGroups"`
}

// publicSet returns the public halves of private, a private JWK or a set of
// them, as a JWK set, and whether every key of it is symmetric.
func publicSet(t *testing.T, private json.RawMessage) (set []byte, symmetric bool) {
	t.Helper()
	var keys struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(private, &keys); err != nil || keys.Keys == nil {
		var key map[string]any
		require.NoError(t, json.Unmarshal(private, &key))
		keys.Keys = []map[string]any{key}
	}
	symmetric = true
	for _, key := range keys.Keys {
		symmetric = symmetric && key["kty"] == "oct"
		for _, member := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
			delete(key, member)
		}
	}
	set, err := json.Marshal(keys)
	require.NoError(t, err)
	return set, symmetric
}

// jwkSource is a KeySource of a JWK set, which reads it as keysource.JWKSet
// reads the set it fetches: a set without a usable key is an error.
type jwkSource []byte

func (s jwkSource) Keys(context.Context) ([]crypto.PublishedKey, error) {
	keys, err := crypto.ParseJWKSet(s)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, keysource.ErrNoUsableKey
	}
	return keys, nil
}

var joseOptions = crypto.VerifyOptions{
	Issuers:   []string{"https://wycheproof.example.com"},
	Audiences: []string{"https://behemoth.example.com"},
	// A vector without a kid is checked against every key that fits it.
	AllowMissingKID: true,
}

// pastSignature reports whether VerifyJWT, which returned err for jws, got
// past the signature. The vectors' payloads are not claims, so one whose
// signature verifies is refused once its claims are decoded, with
// malformed_token, the code of a token refused for its structure too. The
// same token with zeros for its signature tells the two apart: when the
// structure is fine, it is refused with invalid_signature.
func pastSignature(keys crypto.KeySet, jws string, err error) bool {
	var refused *errs.DomainError
	switch {
	case err == nil:
		return true
	case !errors.As(err, &refused) || refused.Category != errs.CategoryToken:
		return false // the key set's error: none of its keys can verify
	case refused.Code != errs.CodeJWTMalformed:
		atOrBefore := []string{errs.CodeJWTAlgorithmMismatch, errs.CodeJWTUnknownKey, errs.CodeJWTInvalidSignature, errs.CodeJWTTooLarge}
		return !slices.Contains(atOrBefore, refused.Code)
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 || strings.ContainsAny(parts[2], "\r\n") {
		return false
	}
	signature, decodeErr := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if decodeErr != nil || len(signature) == 0 {
		return false
	}
	zeros := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, len(signature)))
	_, zerosErr := crypto.VerifyJWT(context.Background(), zeros, keys, joseOptions)
	return errs.IsCode(zerosErr, errs.CodeJWTInvalidSignature)
}

// joseDeviations are the vectors Wycheproof calls invalid that get past
// their signature, by file and tcId, with the reason. Each has its entry in
// docs/ongoing.md.
var joseDeviations = map[string]map[int]string{
	"json_web_signature_test.json": {
		281: pssSaltLength, 282: pssSaltLength, 283: pssSaltLength,
		284: pssSaltLength, 285: pssSaltLength, 286: pssSaltLength,
	},
	"json_web_key_test.json": {
		7: "a key with the ROCA weakness (CVE-2017-15361) is not detected (M40)",
	},
}

const pssSaltLength = "a PSS signature with a salt length other than the hash's verifies (M36)"

// joseRefusals are the vectors Wycheproof calls valid that are refused
// before their signature on purpose, with the reason: the group's key is
// not one the RFCs let verify the token.
var joseRefusals = map[string]map[int]string{
	"json_web_signature_test.json": {
		346: algNamedByTheKey, 350: algNamedByTheKey,
		347: "the key's alg, ES521, is not a JWS algorithm (RFC 7518 names it ES512), so ParseJWKSet leaves the key out",
		351: "the key's alg, ES521, is not a JWS algorithm (RFC 7518 names it ES512), so ParseJWKSet leaves the key out",
		349: `the key's key_ops hold one operation, "sign, verify", which is not verify (RFC 7517, section 4.3), so ParseJWKSet leaves the key out`,
	},
}

const algNamedByTheKey = "the key's alg is PS256 and the token's PS384: a key that names an algorithm verifies that one only (RFC 7517, section 4.4)"

// Each vector of the two files, through VerifyJWT. A valid vector gets past
// its signature, and an invalid one does not, but for joseRefusals and
// joseDeviations. VerifyJWT takes no HMAC algorithm, so a valid vector whose
// keys are all symmetric is refused.
func TestWycheproofJOSE(t *testing.T) {
	for _, name := range []string{"json_web_signature_test.json", "json_web_key_test.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "wycheproof", name))
			require.NoError(t, err)
			var f joseFile
			require.NoError(t, json.Unmarshal(data, &f))
			require.NotEmpty(t, f.TestGroups)

			count := map[string]int{}
			for _, g := range f.TestGroups {
				set, symmetric := publicSet(t, g.Private)
				keys, err := crypto.NewCachedKeySet(jwkSource(set), crypto.CacheOptions{})
				require.NoError(t, err)
				for _, tc := range g.Tests {
					var jws string
					if json.Unmarshal(tc.JWS, &jws) != nil {
						jws = string(tc.JWS) // a JSON serialization, which VerifyJWT does not read
					}
					_, err := crypto.VerifyJWT(context.Background(), jws, keys, joseOptions)
					passed := pastSignature(keys, jws, err)
					what := fmt.Sprintf("tcId %d (%s: %s %v): %v", tc.ID, g.Comment, tc.Comment, tc.Flags, err)
					switch {
					case tc.Result == "valid" && symmetric:
						count["valid, symmetric"]++
						assert.False(t, passed, "an HMAC vector got past its signature: %s", what)
						assert.Error(t, err, what)
					case joseRefusals[name][tc.ID] != "":
						count["refusals"]++
						assert.False(t, passed, "a refusal that no longer is one: %s", what)
						t.Logf("tcId %d is refused: %s", tc.ID, joseRefusals[name][tc.ID])
					case tc.Result == "valid":
						count["valid"]++
						assert.True(t, passed, "a valid vector refused before its signature: %s", what)
					case joseDeviations[name][tc.ID] != "":
						count["deviations"]++
						assert.True(t, passed, "a deviation that no longer is one: %s", what)
						t.Logf("tcId %d gets past its signature: %s", tc.ID, joseDeviations[name][tc.ID])
					case tc.Result == "invalid":
						count["invalid"]++
						assert.False(t, passed, "an invalid vector got past its signature: %s", what)
					default:
						t.Errorf("tcId %d: result %q", tc.ID, tc.Result)
					}
				}
			}
			require.NotZero(t, count["valid"])
			require.NotZero(t, count["invalid"])
			t.Logf("valid %d, valid HMAC refused %d, valid refused on purpose %d, invalid %d, deviations %d",
				count["valid"], count["valid, symmetric"], count["refusals"], count["invalid"], count["deviations"])
		})
	}
}
