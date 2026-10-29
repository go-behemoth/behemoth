package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/golang-jwt/jwt/v5"
)

// Tokens are encoded, parsed and their registered claims checked with
// golang-jwt, so that they are standard JWTs (RFC 7519): NumericDate dates,
// and an audience that may be a string or an array. The algorithm is never
// the token's choice: an internal token is HS256, and an external one is
// signed with the algorithm of the key its kid names. A token whose header
// names another is refused before its signature is looked at.

// asymmetricAlgs are the algorithms a key of a KeySet may sign with:
// asymmetric ones only, so that a public key is never used as an HMAC
// secret. They are the algorithms of the key types the JWK codec knows. The
// derived keys are EdDSA; the others are for a key set held by a KMS
// (ADR-0002) and for other issuers' keys (ADR-0003).
var asymmetricAlgs = []string{"EdDSA", "ES256", "ES384", "ES512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}

// wireClaims is a token's payload as it is parsed and checked.
type wireClaims struct {
	jwt.RegisteredClaims
	SessionID string     `json:"sid,omitempty"`
	Custom    behemoth.M `json:"custom,omitempty"`
}

// payload returns the claims of a new token as they are signed. A map, and
// not wireClaims, because golang-jwt writes a single audience as an array
// unless a package-wide setting says otherwise.
func payload(op string, kind JWTKind, cfg JWTConfig, req JWTRequest) (jwt.MapClaims, error) {
	if req.Subject == "" {
		return nil, errs.NewInvalidInputError(op, "subject", "a token needs a subject", nil)
	}
	audience := req.Audience
	allowed := cfg.Audiences[kind]
	switch {
	case audience == "" && len(allowed) > 0:
		audience = allowed[0]
	case audience != "" && !slices.Contains(allowed, audience):
		return nil, errs.NewInvalidInputError(op, "audience", "audience is not allowed", nil)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": cfg.Issuer,
		"sub": req.Subject,
		"iat": jwt.NewNumericDate(now),
		"exp": jwt.NewNumericDate(now.Add(cfg.TTL[kind])),
		"jti": rand.Text(),
	}
	if audience != "" {
		claims["aud"] = audience
	}
	if req.SessionID != "" {
		claims["sid"] = req.SessionID
	}
	if len(req.Custom) > 0 {
		claims["custom"] = req.Custom
	}
	return claims, nil
}

// maxTokenSize and maxHeaderSize bound a token and its header, as they are
// encoded. A longer one is refused before anything of it is decoded, so a
// token costs little to refuse whatever its size. A JWT is a few hundred
// bytes to a few kilobytes: the largest carry long lists of groups or
// roles, and a certificate chain (x5c) adds a few kilobytes to a header.
const (
	maxTokenSize  = 64 << 10
	maxHeaderSize = 8 << 10
)

// segmentEncoding decodes a segment: base64url without padding (RFC 7515,
// section 2), strictly, so that the unused bits of the last character are
// zero. With isBase64URL, which refuses the line breaks the decoder skips,
// a segment has one encoding: a token can't be changed into another string
// that verifies with the same signature.
var segmentEncoding = base64.RawURLEncoding.Strict()

// isBase64URL reports whether s holds only characters of the base64url
// alphabet.
func isBase64URL(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// parsed is a token read without its signature checked yet.
type parsed struct {
	alg           string
	kid           string
	signingString string
	signature     []byte
	payload       []byte // the claims as they were signed, decoded from base64url
}

// parse reads token without verifying anything: its header, its signature,
// and its payload, which decodeClaims reads as claims once the signature is
// checked. A forged token is refused before its claims are decoded.
//
// A token longer than maxTokenSize, or a header longer than maxHeaderSize,
// is refused with token_too_large before it is decoded. malformed_token
// refuses a token that is not three segments of canonical base64url (RFC
// 7515, section 7.1), a header that is not a JSON object whose alg is a
// string, a kid that is not a string, and a header with "crit": no
// extension is understood here, and RFC 7515 section 4.1.11 then fails the
// token.
func parse(op, token string) (*parsed, error) {
	malformed := func(err error) error { return errs.NewJWTError(op, errs.CodeJWTMalformed, err) }
	if len(token) > maxTokenSize {
		return nil, errs.NewJWTError(op, errs.CodeJWTTooLarge, fmt.Errorf("the token is %d bytes, over %d", len(token), maxTokenSize))
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return nil, malformed(fmt.Errorf("the token has %d segments, not 3", len(segments)))
	}
	if len(segments[0]) > maxHeaderSize {
		return nil, errs.NewJWTError(op, errs.CodeJWTTooLarge, fmt.Errorf("the header is %d bytes, over %d", len(segments[0]), maxHeaderSize))
	}
	var decoded [3][]byte
	for i, segment := range segments {
		if !isBase64URL(segment) {
			return nil, malformed(fmt.Errorf("segment %d is not base64url", i+1))
		}
		b, err := segmentEncoding.DecodeString(segment)
		if err != nil {
			return nil, malformed(fmt.Errorf("segment %d: %w", i+1, err))
		}
		decoded[i] = b
	}

	var header map[string]any
	if err := json.Unmarshal(decoded[0], &header); err != nil || header == nil {
		return nil, malformed(fmt.Errorf("the header is not a JSON object: %v", err))
	}
	if _, ok := header["crit"]; ok {
		return nil, malformed(errors.New("the header names critical extensions"))
	}
	alg, ok := header["alg"].(string)
	if !ok {
		return nil, malformed(errors.New("the header's alg is missing or not a string"))
	}
	var kid string
	if value, present := header["kid"]; present {
		if kid, ok = value.(string); !ok {
			return nil, malformed(errors.New("the header's kid is not a string"))
		}
	}
	return &parsed{alg: alg, kid: kid, signingString: segments[0] + "." + segments[1], signature: decoded[2], payload: decoded[1]}, nil
}

// decodeClaims decodes the payload of a token whose signature is checked
// into claims. A payload that is not a JSON object is refused with
// malformed_token: the token is a JWS, but not a JWT (RFC 7519, section
// 7.2).
func (p *parsed) decodeClaims(op string, claims any) error {
	if object := bytes.TrimLeft(p.payload, " \t\r\n"); len(object) == 0 || object[0] != '{' {
		return errs.NewJWTError(op, errs.CodeJWTMalformed, errors.New("the claims are not a JSON object"))
	}
	if err := json.Unmarshal(p.payload, claims); err != nil {
		return errs.NewJWTError(op, errs.CodeJWTMalformed, err)
	}
	return nil
}

// signWith encodes claims with a header naming alg and kid, and signs it
// with sign.
func signWith(op, alg, kid string, claims jwt.MapClaims, sign func([]byte) ([]byte, error)) (string, error) {
	method := jwt.GetSigningMethod(alg)
	if method == nil {
		return "", errs.NewSecurityError(op, errs.CodeJWTSigningFailed, fmt.Errorf("unknown algorithm %q", alg))
	}
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid
	signingString, err := token.SigningString()
	if err != nil {
		return "", errs.NewSecurityError(op, errs.CodeJWTEncodingFailed, err)
	}
	signature, err := sign([]byte(signingString))
	if err != nil {
		return "", errs.NewSecurityError(op, errs.CodeJWTSigningFailed, err)
	}
	return signingString + "." + b64(signature), nil
}

// validate checks the registered claims of an internal token whose
// signature is valid, and returns them as Claims.
func validate(op string, c *wireClaims, issuer string, audiences []string) (*Claims, error) {
	opts := []jwt.ParserOption{jwt.WithExpirationRequired()}
	if issuer != "" {
		opts = append(opts, jwt.WithIssuer(issuer))
	}
	if len(audiences) > 0 {
		opts = append(opts, jwt.WithAudience(audiences...))
	}
	if err := checkClaims(op, jwt.NewValidator(opts...), c); err != nil {
		return nil, err
	}
	return claimsOf(c, audiences), nil
}

// checkClaims runs v over claims, and turns its error into a refused token
// with the code of the check that failed.
func checkClaims(op string, v *jwt.Validator, claims jwt.Claims) error {
	err := v.Validate(claims)
	if err == nil {
		return nil
	}
	code := errs.CodeJWTInvalidClaims
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		code = errs.CodeTokenExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		code = errs.CodeJWTNotYetValid
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		code = errs.CodeJWTIssuerMismatch
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		code = errs.CodeJWTAudienceMismatch
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		code = errs.CodeJWTMissingClaim
	}
	return errs.NewJWTError(op, code, err)
}

// claimsOf returns the claims of a checked token as Claims.
func claimsOf(c *wireClaims, audiences []string) *Claims {
	out := &Claims{Issuer: c.Issuer, Subject: c.Subject, SessionID: c.SessionID, ID: c.ID, Custom: c.Custom}
	// Claims holds one audience: the first configured one the token names.
	if len(c.Audience) > 0 {
		out.Audience = c.Audience[0]
		for _, a := range audiences {
			if slices.Contains(c.Audience, a) {
				out.Audience = a
				break
			}
		}
	}
	for _, d := range []struct {
		from *jwt.NumericDate
		into *time.Time
	}{{c.ExpiresAt, &out.ExpiresAt}, {c.IssuedAt, &out.IssuedAt}, {c.NotBefore, &out.NotBefore}} {
		if d.from != nil {
			*d.into = d.from.Time
		}
	}
	return out
}

// internalJWT is an HS256 token signed with the key manager's key of
// purpose jwt_sign. Its kid is the key's version, so a token signed before
// a rotation still verifies.
type internalJWT struct {
	signer Signer
	cfg    JWTConfig
}

func (j *internalJWT) sign(req JWTRequest) (string, error) {
	const op = "JWT.Sign.Internal"
	claims, err := payload(op, JWTInternal, j.cfg, req)
	if err != nil {
		return "", err
	}
	// One snapshot of the key for the whole token: the kid and the
	// signature have to name the same version, and a rotation can land
	// between two calls to the signer.
	signingKey, err := j.signer.PrepareSigning()
	if err != nil {
		return "", err
	}
	return signWith(op, jwt.SigningMethodHS256.Alg(), strconv.Itoa(signingKey.Version()), claims,
		func(b []byte) ([]byte, error) { return signingKey.Sign(b), nil })
}

func (j *internalJWT) verify(token string) (*Claims, error) {
	const op = "JWT.Verify.Internal"
	p, err := parse(op, token)
	if err != nil {
		return nil, err
	}
	if p.alg != jwt.SigningMethodHS256.Alg() {
		return nil, errs.NewJWTError(op, errs.CodeJWTAlgorithmMismatch, nil)
	}
	// The kid is the version as sign writes it. Another form of a number,
	// such as "01" or "+1", names no key.
	version, err := strconv.Atoi(p.kid)
	if err != nil || strconv.Itoa(version) != p.kid {
		return nil, errs.NewJWTError(op, errs.CodeJWTUnknownKey, err)
	}
	// The signer checks the HMAC in constant time, with the key of the
	// token's version. A version the key manager does not hold is the
	// token's, and refuses it: the system has not failed.
	ok, err := j.signer.Verify([]byte(p.signingString), p.signature, version)
	if errs.IsCode(err, errs.CodeUnknownKeyVersion) {
		return nil, errs.NewJWTError(op, errs.CodeJWTUnknownKey, err)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.NewJWTError(op, errs.CodeJWTInvalidSignature, nil)
	}
	var c wireClaims
	if err := p.decodeClaims(op, &c); err != nil {
		return nil, err
	}
	return validate(op, &c, j.cfg.Issuer, j.cfg.Audiences[JWTInternal])
}

// externalJWT is signed with the current key of an ExternalKeySet, for
// other services to verify with the published keys. Its kid is the key's.
type externalJWT struct {
	keys ExternalKeySet
	cfg  JWTConfig
}

func (j *externalJWT) sign(ctx context.Context, req JWTRequest) (string, error) {
	const op = "JWT.Sign.External"
	claims, err := payload(op, JWTExternal, j.cfg, req)
	if err != nil {
		return "", err
	}
	key, err := j.keys.CurrentKey(ctx)
	if err != nil {
		return "", err
	}
	return signWith(op, key.Algorithm(), key.KID(), claims,
		func(b []byte) ([]byte, error) { return key.Sign(ctx, b) })
}

// verify checks an external token with VerifyJWT, the verification another
// service runs with the published keys: the key set holds the current key
// and the ones rotated out, which still verify the tokens they signed until
// those expire. An algorithm no key may have, none and the HMAC ones among
// them, is refused before the key set is asked.
func (j *externalJWT) verify(ctx context.Context, token string) (*Claims, error) {
	const op = "JWT.Verify.External"
	audiences := j.cfg.Audiences[JWTExternal]
	verified, err := VerifyJWT(ctx, token, j.keys, VerifyOptions{Issuers: []string{j.cfg.Issuer}, Audiences: audiences})
	if err != nil {
		return nil, err
	}
	var c wireClaims
	if err := verified.Claims(&c); err != nil {
		return nil, errs.NewJWTError(op, errs.CodeJWTMalformed, err)
	}
	return claimsOf(&c, audiences), nil
}

type JWTProviderImpl struct {
	internal *internalJWT
	external *externalJWT // not required
	cfg      JWTConfig
}

// NewJWTProvider returns the provider of both kinds of JWT. Internal tokens
// are signed by signer, external ones by keys. A nil keys leaves external
// tokens off: Sign and Verify of JWTExternal are then configuration errors.
func NewJWTProvider(signer Signer, keys ExternalKeySet, cfg JWTConfig) *JWTProviderImpl {
	p := &JWTProviderImpl{internal: &internalJWT{signer: signer, cfg: cfg}, cfg: cfg}
	if keys != nil {
		p.external = &externalJWT{keys: keys, cfg: cfg}
	}
	return p
}

// Sign implements [JWTProvider]. A request without a subject, or for
// an audience the kind does not list, is a validation error.
func (p *JWTProviderImpl) Sign(ctx context.Context, kind JWTKind, req JWTRequest) (string, error) {
	switch kind {
	case JWTInternal:
		return p.internal.sign(req)
	case JWTExternal:
		if p.external == nil {
			return "", errs.NewConfigurationErrorCode("JWT.Sign", errs.CodeJWTExternalNotConfigured, "external JWTs need a key set")
		}
		return p.external.sign(ctx, req)
	default:
		return "", errs.NewConfigurationErrorCode("JWT.Sign", errs.CodeJWTUnknownKind, "unknown JWT kind")
	}
}

// Verify implements [JWTProvider]. A refused token is a CategoryToken
// error, with one of the codes of errors/jwt.go.
func (p *JWTProviderImpl) Verify(ctx context.Context, kind JWTKind, token string) (*Claims, error) {
	switch kind {
	case JWTInternal:
		return p.internal.verify(token)
	case JWTExternal:
		if p.external == nil {
			return nil, errs.NewConfigurationErrorCode("JWT.Verify", errs.CodeJWTExternalNotConfigured, "external JWTs need a key set")
		}
		return p.external.verify(ctx, token)
	default:
		return nil, errs.NewConfigurationErrorCode("JWT.Verify", errs.CodeJWTUnknownKind, "unknown JWT kind")
	}
}
