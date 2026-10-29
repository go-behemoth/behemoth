package crypto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/golang-jwt/jwt/v5"
)

// VerifyOptions says what a token has to be for VerifyJWT to accept it.
type VerifyOptions struct {
	// Issuers are the values iss may have. Required: a token is accepted
	// from an issuer the caller trusts, and from no other.
	Issuers []string

	// Audiences are the names of the caller, one of which aud has to hold.
	// Required: a token issued for another service is refused.
	Audiences []string

	// Algorithms are the JWS algorithms a token may be signed with. Empty
	// means every asymmetric one a key of ParseJWKSet signs with: EdDSA,
	// ES256, ES384, ES512, RS256, RS384, RS512, PS256, PS384, and PS512.
	// "none" and the HMAC algorithms can't be listed: a public key is never
	// used as an HMAC secret.
	Algorithms []string

	// Leeway is the clock skew allowed on exp, nbf, and iat. Zero means
	// none. It is at most five minutes: a longer one is a configuration
	// error.
	Leeway time.Duration

	// CheckIssuedAt refuses a token whose iat is later than now plus
	// Leeway. exp is always required, and nbf is checked when present.
	CheckIssuedAt bool

	// AllowMissingKID lets a token that names no kid be checked against
	// every key of the set that fits its algorithm. Without it, such a
	// token is refused with ErrorCodeJWTUnknownKey.
	AllowMissingKID bool

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// VerifiedJWT is a token VerifyJWT accepted.
type VerifiedJWT struct {
	// KID and Algorithm are the header's kid and alg.
	KID       string
	Algorithm string

	// The registered claims (RFC 7519, section 4.1). Audience holds every
	// value of aud. IssuedAt and NotBefore are zero when the token has
	// none, and ID is empty.
	Issuer    string
	Subject   string
	Audience  []string
	ID        string
	ExpiresAt time.Time
	IssuedAt  time.Time
	NotBefore time.Time

	payload []byte
}

// Claims decodes the token's claims into v, as encoding/json does: into a
// struct with the claims the caller reads, or into a map[string]any for all
// of them.
func (t *VerifiedJWT) Claims(v any) error {
	return json.Unmarshal(t.payload, v)
}

// VerifyJWT checks a JWT signed with a key of keys, and returns it.
//
// A token over 64 KiB, or with a header over 8 KiB, is refused before it is
// decoded, with errs.CodeJWTTooLarge. The token is three segments of
// base64url without padding, in their one encoding. The header's alg has to
// be one of opts.Algorithms and fit the key: the key its kid names, which
// signs with its own algorithm or, when it names none, with one its type
// allows. The algorithm is never taken from the header alone, and neither is
// a key. The signature is checked before the claims are decoded. Then exp is
// required, nbf and iat are checked, iss has to be one of opts.Issuers, and
// aud has to hold one of opts.Audiences.
//
// A refused token is a CategoryToken error with one of the codes of
// errs/jwt.go. When keys can't get its keys, its error is returned: a
// failure of the system. Options without an issuer or an audience, with an
// algorithm that is not asymmetric, or with a leeway over five minutes are a
// configuration error.
func VerifyJWT(ctx context.Context, token string, keys KeySet, opts VerifyOptions) (*VerifiedJWT, error) {
	const op = "crypto.VerifyJWT"
	algs, err := opts.check(keys)
	if err != nil {
		return nil, errs.NewConfigurationError(op, err.Error(), err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	p, err := parse(op, token)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(algs, p.alg) {
		return nil, errs.NewJWTError(op, errs.CodeJWTAlgorithmMismatch, nil)
	}
	candidates, err := candidateKeys(ctx, op, keys, p.kid, p.alg, opts.AllowMissingKID)
	if err != nil {
		return nil, err
	}
	method := jwt.GetSigningMethod(p.alg) // known: every algorithm of algs is golang-jwt's
	var verifyErr error
	for _, key := range candidates {
		if verifyErr = method.Verify(p.signingString, p.signature, key.Key); verifyErr == nil {
			break
		}
	}
	if verifyErr != nil {
		return nil, errs.NewJWTError(op, errs.CodeJWTInvalidSignature, verifyErr)
	}

	claims := jwt.MapClaims{}
	if err := p.decodeClaims(op, &claims); err != nil {
		return nil, err
	}
	checks := []jwt.ParserOption{
		jwt.WithExpirationRequired(), jwt.WithAudience(opts.Audiences...),
		jwt.WithLeeway(opts.Leeway), jwt.WithTimeFunc(now),
	}
	if opts.CheckIssuedAt {
		checks = append(checks, jwt.WithIssuedAt())
	}
	if err := checkClaims(op, jwt.NewValidator(checks...), claims); err != nil {
		return nil, err
	}
	// golang-jwt checks one issuer only, and OpenID Connect allows two
	// forms of Google's.
	issuer, err := claims.GetIssuer()
	if err != nil || !slices.Contains(opts.Issuers, issuer) {
		return nil, errs.NewJWTError(op, errs.CodeJWTIssuerMismatch, err)
	}
	return verifiedOf(op, p, claims)
}

// maxLeeway is the most clock skew VerifyOptions.Leeway may allow. Servers
// whose clocks NTP keeps are within a second of each other. A longer leeway
// only lets a token be used for that much longer after it expires.
const maxLeeway = 5 * time.Minute

// check returns the algorithms a token may name, or why the options can't
// work.
func (o VerifyOptions) check(keys KeySet) ([]string, error) {
	switch {
	case keys == nil:
		return nil, errors.New("a key set is required")
	case len(o.Issuers) == 0 || slices.Contains(o.Issuers, ""):
		return nil, errors.New("VerifyOptions.Issuers is empty or holds an empty issuer; a token is accepted from an issuer the caller trusts")
	case len(o.Audiences) == 0 || slices.Contains(o.Audiences, ""):
		return nil, errors.New("VerifyOptions.Audiences is empty or holds an empty audience; a token is accepted by the service it was issued for")
	case o.Leeway < 0:
		return nil, fmt.Errorf("VerifyOptions.Leeway is negative: %s", o.Leeway)
	case o.Leeway > maxLeeway:
		return nil, fmt.Errorf("VerifyOptions.Leeway is %s, over the most allowed, %s", o.Leeway, maxLeeway)
	}
	if len(o.Algorithms) == 0 {
		return asymmetricAlgs, nil
	}
	for _, alg := range o.Algorithms {
		if !slices.Contains(asymmetricAlgs, alg) {
			return nil, fmt.Errorf("VerifyOptions.Algorithms: %q is not an asymmetric JWS algorithm", alg)
		}
	}
	return o.Algorithms, nil
}

// candidateKeys returns the keys of keys that may have signed a token with
// kid and alg. A kid the set lacks is refused by the set. A token without a
// kid, when it is allowed, may have been signed by any key that fits alg.
func candidateKeys(ctx context.Context, op string, keys KeySet, kid, alg string, allowMissingKID bool) ([]PublishedKey, error) {
	if kid == "" {
		if !allowMissingKID {
			return nil, errs.NewJWTError(op, errs.CodeJWTUnknownKey, errors.New("the token names no kid"))
		}
		all, err := keys.PublicKeys(ctx)
		if err != nil {
			return nil, err
		}
		found := keysWhere(all, func(k PublishedKey) bool { return fits(k, alg) })
		if len(found) == 0 {
			return nil, errs.NewJWTError(op, errs.CodeJWTUnknownKey, errors.New("no key fits the token's algorithm"))
		}
		return found, nil
	}
	key, err := keys.PublicKey(ctx, kid)
	if err != nil {
		return nil, err
	}
	if fits(key, alg) {
		return []PublishedKey{key}, nil
	}
	// RFC 7517, section 4.5: keys of different types may share a kid.
	all, err := keys.PublicKeys(ctx)
	if err != nil {
		return nil, err
	}
	found := keysWhere(all, func(k PublishedKey) bool { return k.KID == kid && fits(k, alg) })
	if len(found) == 0 {
		return nil, errs.NewJWTError(op, errs.CodeJWTAlgorithmMismatch, nil)
	}
	return found, nil
}

// keysWhere returns the keys for which keep is true, in a new slice: the
// one a KeySet returned may be its own.
func keysWhere(keys []PublishedKey, keep func(PublishedKey) bool) []PublishedKey {
	var found []PublishedKey
	for _, k := range keys {
		if keep(k) {
			found = append(found, k)
		}
	}
	return found
}

// fits reports whether key may check a token signed with alg: alg is the
// key's own algorithm when it names one, and in any case one the key's type
// signs with.
func fits(key PublishedKey, alg string) bool {
	if key.Algorithm != "" && key.Algorithm != alg {
		return false
	}
	return slices.Contains(algorithmsOf(key.Key), alg)
}

// verifiedOf returns the token VerifyJWT accepted. A registered claim of the
// wrong type, such as a numeric sub, refuses it.
func verifiedOf(op string, p *parsed, claims jwt.MapClaims) (*VerifiedJWT, error) {
	invalid := func(err error) error {
		return errs.NewJWTError(op, errs.CodeJWTInvalidClaims, err)
	}
	t := &VerifiedJWT{KID: p.kid, Algorithm: p.alg, payload: p.payload}
	var err error
	if t.Issuer, err = claims.GetIssuer(); err != nil {
		return nil, invalid(err)
	}
	if t.Subject, err = claims.GetSubject(); err != nil {
		return nil, invalid(err)
	}
	audience, err := claims.GetAudience()
	if err != nil {
		return nil, invalid(err)
	}
	t.Audience = audience
	if id, ok := claims["jti"]; ok {
		if t.ID, ok = id.(string); !ok {
			return nil, invalid(errors.New("jti is not a string"))
		}
	}
	for _, d := range []struct {
		get  func() (*jwt.NumericDate, error)
		into *time.Time
	}{{claims.GetExpirationTime, &t.ExpiresAt}, {claims.GetIssuedAt, &t.IssuedAt}, {claims.GetNotBefore, &t.NotBefore}} {
		date, err := d.get()
		if err != nil {
			return nil, invalid(err)
		}
		if date != nil {
			*d.into = date.Time
		}
	}
	return t, nil
}
