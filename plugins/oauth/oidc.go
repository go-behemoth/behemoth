package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/keysource"
	"github.com/go-behemoth/behemoth/outbound"
)

// How long a discovery document and a key set are used before they are
// fetched again. An ID token signed with a key the set does not have makes
// the provider fetch the set again, at most once per keyRefetchInterval, so
// a rotation is picked up before keysTTL is over. The key set is a
// crypto.CachedKeySet with these durations, which also keeps using the keys
// for crypto.DefaultKeySetMaxStale while the set can't be fetched.
const (
	discoveryTTL       = 24 * time.Hour
	keysTTL            = time.Hour
	keyRefetchInterval = time.Minute
)

// idTokenLeeway is the clock skew allowed on an ID token's exp and iat.
const idTokenLeeway = time.Minute

// signingAlgs are the algorithms an ID token may be signed with: RSA and
// ECDSA. "none" and the HMAC algorithms are not among them: an HMAC token
// would be checked with a public key as its secret.
var signingAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

// discoveryDocument is what the plugin reads of an issuer's
// /.well-known/openid-configuration (OpenID Connect Discovery 1.0, section
// 3, RFC 8414, section 2, and RFC 9207, section 3).
type discoveryDocument struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
	IssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
	// RevocationEndpoint is empty for an issuer that offers no revocation.
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	RevocationAuthMethods []string `json:"revocation_endpoint_auth_methods_supported"`
}

// oidcProvider is the built-in OpenID Connect provider: the generic one, and
// Google. It reads the issuer's discovery document on first use, and checks
// every ID token against the issuer's keys. It refreshes and revokes tokens
// at the endpoints the document names.
type oidcProvider struct {
	id     string
	issuer string
	// issuers are the values an ID token's iss may have: the issuer, and
	// for Google also "accounts.google.com", which Google uses as well.
	issuers []string
	auth    clientAuth
	scopes  []string
	// authParams are parameters of the authorization URL besides the
	// protocol's own, such as Google's access_type. They never replace one
	// of the protocol's.
	authParams url.Values
	http       providerHTTP
	now        func() time.Time

	mu         sync.Mutex
	discovery  *discoveryDocument
	discovered time.Time
	// keys is the key set at the discovery document's jwks_uri. A new
	// document that names the same jwks_uri keeps it, and its keys.
	keys *crypto.CachedKeySet
}

func (p *oidcProvider) ID() string { return p.id }

// AuthorizationURL implements [Provider].
func (p *oidcProvider) AuthorizationURL(ctx context.Context, req AuthorizationRequest) (string, error) {
	doc, _, err := p.document(ctx)
	if err != nil {
		return "", err
	}
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.auth.clientID},
		"redirect_uri":          {req.RedirectURI},
		"scope":                 {strings.Join(p.scopes, " ")},
		"state":                 {req.State},
		"nonce":                 {req.Nonce},
		"code_challenge":        {req.CodeChallenge},
		"code_challenge_method": {"S256"},
	}
	for key, values := range p.authParams {
		if _, own := params[key]; !own {
			params[key] = values
		}
	}
	return withQuery(doc.AuthorizationEndpoint, params)
}

// Exchange implements [Provider]. It redeems the code, checks the ID token,
// and reads the user's claims from it. When the ID token has no email and
// the issuer has a userinfo endpoint, the claims the token lacks come from
// there.
func (p *oidcProvider) Exchange(ctx context.Context, req ExchangeRequest) (*ExchangeResult, error) {
	doc, keys, err := p.document(ctx)
	if err != nil {
		return nil, err
	}
	// RFC 9207: an authorization response that names its issuer has to name
	// this one, and an issuer that says it names itself has to.
	switch {
	case req.Issuer != "" && req.Issuer != doc.Issuer:
		return nil, fmt.Errorf("%w: the authorization response names the issuer %q", ErrExchangeRefused, req.Issuer)
	case req.Issuer == "" && doc.IssParameterSupported:
		return nil, fmt.Errorf("%w: the authorization response does not name its issuer", ErrExchangeRefused)
	}

	tokens, err := p.http.redeemCode(ctx, doc.TokenEndpoint, p.auth, authMethod(doc.AuthMethods, p.auth.secret != nil), req)
	if err != nil {
		return nil, err
	}
	if tokens.IDToken == "" {
		return nil, errors.New("token endpoint: the answer has no ID token")
	}
	claims, err := p.verifyIDToken(ctx, keys, tokens.IDToken, req.Nonce)
	if err != nil {
		return nil, err
	}
	identity := identityFromClaims(claims)

	if identity.Email == "" && doc.UserinfoEndpoint != "" {
		var info map[string]any
		if _, err := p.http.getJSON(ctx, doc.UserinfoEndpoint, http.Header{"Authorization": {"Bearer " + tokens.AccessToken}}, &info); err != nil {
			return nil, fmt.Errorf("userinfo endpoint: %w", err)
		}
		// OpenID Connect Core 1.0, section 5.3.2: the sub of the userinfo
		// answer has to be the ID token's, or the answer is not about the
		// user who signed in.
		if sub, _ := info["sub"].(string); sub != identity.Subject {
			return nil, fmt.Errorf("%w: the userinfo answer is about another subject", ErrExchangeRefused)
		}
		identity = mergeIdentity(identity, identityFromClaims(info))
	}
	return &ExchangeResult{Identity: identity, Tokens: withRequestedScope(tokens.tokens(p.now()), p.scopes)}, nil
}

// Refresh implements [Refresher], at the token endpoint of the discovery
// document. An ID token in the answer is dropped: it is not checked, and the
// plugin keeps the one of the sign-in.
func (p *oidcProvider) Refresh(ctx context.Context, req RefreshRequest) (*Tokens, error) {
	doc, _, err := p.document(ctx)
	if err != nil {
		return nil, err
	}
	answer, err := p.http.refreshTokens(ctx, doc.TokenEndpoint, p.auth, authMethod(doc.AuthMethods, p.auth.secret != nil), req.RefreshToken)
	if err != nil {
		return nil, err
	}
	tokens := answer.tokens(p.now())
	tokens.IDToken = ""
	return &tokens, nil
}

// Revoke implements [Revoker], at the revocation endpoint of the discovery
// document (RFC 7009): the refresh token when there is one, else the access
// token. An issuer whose document names no revocation endpoint can't revoke,
// and Revoke returns nil.
func (p *oidcProvider) Revoke(ctx context.Context, tokens Tokens) error {
	token, hint := revocable(tokens)
	if token == "" {
		return nil
	}
	doc, _, err := p.document(ctx)
	if err != nil {
		return err
	}
	if doc.RevocationEndpoint == "" {
		return nil
	}
	return p.http.revokeToken(ctx, doc.RevocationEndpoint, p.auth, authMethod(doc.RevocationAuthMethods, p.auth.secret != nil), token, hint)
}

// authMethod returns how to authenticate at the token endpoint: HTTP Basic
// when the issuer supports it or does not say (the default of OpenID
// Connect Discovery), the form when it supports only that, and the client
// id alone for a client without a secret.
func authMethod(supported []string, hasSecret bool) string {
	switch {
	case !hasSecret:
		return authNone
	case len(supported) == 0 || slices.Contains(supported, authSecretBasic):
		return authSecretBasic
	case slices.Contains(supported, authSecretPost):
		return authSecretPost
	}
	return authSecretBasic
}

// document returns the issuer's discovery document and its key set,
// fetching the document when there is none or it is older than
// discoveryTTL. The key set is built for the document's jwks_uri on the
// plugin's client, and fetches nothing until an ID token needs a key.
func (p *oidcProvider) document(ctx context.Context) (*discoveryDocument, *crypto.CachedKeySet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discovery != nil && p.now().Sub(p.discovered) < discoveryTTL {
		return p.discovery, p.keys, nil
	}
	var doc discoveryDocument
	if _, err := p.http.getJSON(ctx, strings.TrimRight(p.issuer, "/")+"/.well-known/openid-configuration", nil, &doc); err != nil {
		return nil, nil, fmt.Errorf("discovery of %s: %w", p.issuer, err)
	}
	// OpenID Connect Discovery 1.0, section 4.3: a document that names
	// another issuer is not this issuer's.
	if doc.Issuer != p.issuer {
		return nil, nil, fmt.Errorf("discovery of %s: the document names the issuer %q", p.issuer, doc.Issuer)
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint, "token_endpoint": doc.TokenEndpoint, "jwks_uri": doc.JWKSURI,
	} {
		if err := outbound.CheckURL(endpoint); err != nil {
			return nil, nil, fmt.Errorf("discovery of %s: %s: %w", p.issuer, name, err)
		}
	}
	for name, endpoint := range map[string]string{
		"userinfo_endpoint": doc.UserinfoEndpoint, "revocation_endpoint": doc.RevocationEndpoint,
	} {
		if endpoint == "" {
			continue // optional
		}
		if err := outbound.CheckURL(endpoint); err != nil {
			return nil, nil, fmt.Errorf("discovery of %s: %s: %w", p.issuer, name, err)
		}
	}
	if p.keys == nil || p.discovery.JWKSURI != doc.JWKSURI {
		source, err := keysource.NewJWKSet(p.http.client, doc.JWKSURI)
		if err != nil {
			return nil, nil, fmt.Errorf("discovery of %s: jwks_uri: %w", p.issuer, err)
		}
		keys, err := crypto.NewCachedKeySet(source, crypto.CacheOptions{TTL: keysTTL, RefetchInterval: keyRefetchInterval, Now: p.now})
		if err != nil {
			return nil, nil, err
		}
		p.keys = keys
	}
	p.discovery, p.discovered = &doc, p.now()
	return p.discovery, p.keys, nil
}

// verifyIDToken checks an ID token (OpenID Connect Core 1.0, section
// 3.1.3.7) and returns its claims. crypto.VerifyJWT checks the signature
// with one of the issuer's keys and an algorithm of signingAlgs, the issuer,
// the audience, the expiry, and the issue time. The rules of OpenID Connect
// are checked here: the authorized party, the nonce, and a subject. A token
// that fails a check wraps ErrExchangeRefused. Keys that can't be fetched
// are a failure of the system.
func (p *oidcProvider) verifyIDToken(ctx context.Context, keys *crypto.CachedKeySet, raw, nonce string) (map[string]any, error) {
	verified, err := crypto.VerifyJWT(ctx, raw, keys, crypto.VerifyOptions{
		Issuers:       p.issuers,
		Audiences:     []string{p.auth.clientID},
		Algorithms:    signingAlgs,
		Leeway:        idTokenLeeway,
		CheckIssuedAt: true,
		// A provider with one key may leave kid out.
		AllowMissingKID: true,
		Now:             p.now,
	})
	if err != nil {
		if errs.IsCategory(err, errs.CategoryToken) {
			return nil, fmt.Errorf("%w: ID token: %w", ErrExchangeRefused, err)
		}
		return nil, fmt.Errorf("ID token: %w", err)
	}
	var claims map[string]any
	if err := verified.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: ID token: %w", ErrExchangeRefused, err)
	}

	refuse := func(why string) error { return fmt.Errorf("%w: ID token: %s", ErrExchangeRefused, why) }
	_, hasAzp := claims["azp"]
	if azp, _ := claims["azp"].(string); (len(verified.Audience) > 1 || hasAzp) && azp != p.auth.clientID {
		// A token for several audiences has to name this client as the
		// party it was issued to.
		return nil, refuse("issued to another party")
	}
	if got, _ := claims["nonce"].(string); got == "" || got != nonce {
		return nil, refuse("another nonce")
	}
	if verified.Subject == "" {
		return nil, refuse("no subject")
	}
	return claims, nil
}

// identityFromClaims reads the standard claims of OpenID Connect Core 1.0,
// section 5.1. email_verified is true only as the boolean true or the
// string "true", which some providers send.
func identityFromClaims(claims map[string]any) Identity {
	str := func(key string) string { s, _ := claims[key].(string); return s }
	verified := false
	switch v := claims["email_verified"].(type) {
	case bool:
		verified = v
	case string:
		verified = v == "true"
	}
	return Identity{
		Subject:       str("sub"),
		Email:         str("email"),
		EmailVerified: verified,
		Name:          str("name"),
		GivenName:     str("given_name"),
		FamilyName:    str("family_name"),
		Picture:       str("picture"),
	}
}

// mergeIdentity fills what base lacks from more. The email and its
// verification come together, from the one that has the email.
func mergeIdentity(base, more Identity) Identity {
	if base.Email == "" {
		base.Email, base.EmailVerified = more.Email, more.EmailVerified
	}
	for _, field := range []struct{ into, from *string }{
		{&base.Name, &more.Name}, {&base.GivenName, &more.GivenName},
		{&base.FamilyName, &more.FamilyName}, {&base.Picture, &more.Picture},
	} {
		if *field.into == "" {
			*field.into = *field.from
		}
	}
	return base
}
