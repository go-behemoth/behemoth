package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/outbound"
)

// This file is the part of the OAuth 2.0 protocol the built-in providers
// share: the authorization URL, the code exchange with PKCE, the refresh of
// an access token, the revocation of a token, client authentication, and
// reading a provider's answers. The calls go through outbound, which gives
// each a timeout, no redirects, a user agent, a span, and a bound on the
// answer (ADR-0003).

// providerHTTP makes the calls of a built-in provider, with the plugin's
// client: Options.HTTPClient, or else AuthContext.HTTPClient.
type providerHTTP struct {
	client *http.Client
}

// getJSON GETs rawURL with header and decodes the answer into into. An
// answer whose status is not 2xx is an error.
func (h providerHTTP) getJSON(ctx context.Context, rawURL string, header http.Header, into any) (status int, err error) {
	return outbound.GetJSON(ctx, h.client, rawURL, header, into)
}

// tokenResponse is a token endpoint's answer (RFC 6749, sections 5.1 and
// 5.2). Only what the plugin reads is decoded.
type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	IDToken      string  `json:"id_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    seconds `json:"expires_in"`
	// RefreshTokenExpiresIn is GitHub's, for the user tokens of a GitHub
	// App, which expire.
	RefreshTokenExpiresIn seconds `json:"refresh_token_expires_in"`
	Scope                 string  `json:"scope"`
	Error                 string  `json:"error"`
}

// tokens returns the answer as Tokens, with the lifetimes counted from now.
func (r *tokenResponse) tokens(now time.Time) Tokens {
	t := Tokens{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, IDToken: r.IDToken, Scope: r.Scope}
	if r.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	if r.RefreshTokenExpiresIn > 0 && r.RefreshToken != "" {
		t.RefreshExpiresAt = now.Add(time.Duration(r.RefreshTokenExpiresIn) * time.Second)
	}
	return t
}

// seconds is a lifetime in seconds: a JSON number, or a string of digits,
// which some providers send. Anything else reads as zero, a lifetime the
// provider did not give, and does not fail the answer. So does a lifetime
// past math.MaxInt32 seconds, about 68 years, which would overflow a
// time.Duration.
type seconds int64

func (s *seconds) UnmarshalJSON(b []byte) error {
	n, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt32 {
		n = 0
	}
	*s = seconds(n)
	return nil
}

// clientAuth is how a client authenticates at a token endpoint (RFC 6749,
// section 2.3.1).
type clientAuth struct {
	clientID string
	// secret is the client secret's source. Nil for a public client.
	secret crypto.SecretSource
}

// Client authentication methods, by their names in OpenID Connect
// Discovery.
const (
	authSecretBasic = "client_secret_basic"
	authSecretPost  = "client_secret_post"
	authNone        = "none"
)

// currentSecret returns the current value of the client secret.
func currentSecret(ctx context.Context, source crypto.SecretSource) (string, error) {
	secrets, current, err := source.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("client secret: %w", err)
	}
	if secret := secrets[current]; secret != "" {
		return secret, nil
	}
	return "", errors.New("client secret: the source has no current secret")
}

// authenticate adds the client's credentials to a request to a token or a
// revocation endpoint, the way method says (RFC 6749, section 2.3.1). The
// client secret is read from its source for every request.
func authenticate(ctx context.Context, auth clientAuth, method string, form url.Values, header http.Header) error {
	switch method {
	case authSecretBasic, authSecretPost:
		secret, err := currentSecret(ctx, auth.secret)
		if err != nil {
			return err
		}
		if method == authSecretBasic {
			// The id and the secret are form-encoded before they are
			// joined, so a ":" in either survives.
			credentials := url.QueryEscape(auth.clientID) + ":" + url.QueryEscape(secret)
			header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
		} else {
			form.Set("client_id", auth.clientID)
			form.Set("client_secret", secret)
		}
	default:
		form.Set("client_id", auth.clientID)
	}
	return nil
}

// redeemCode posts the authorization code of req to tokenURL, authenticated
// with method, and returns the provider's answer (requestTokens).
func (h providerHTTP) redeemCode(ctx context.Context, tokenURL string, auth clientAuth, method string, req ExchangeRequest) (*tokenResponse, error) {
	return h.requestTokens(ctx, tokenURL, auth, method, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {req.RedirectURI},
		"code_verifier": {req.CodeVerifier},
	})
}

// refreshTokens posts refreshToken to tokenURL (RFC 6749, section 6),
// authenticated with method, and returns the provider's answer
// (requestTokens). No scope is sent, so the new access token has the scope
// of the old one.
func (h providerHTTP) refreshTokens(ctx context.Context, tokenURL string, auth clientAuth, method, refreshToken string) (*tokenResponse, error) {
	return h.requestTokens(ctx, tokenURL, auth, method, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// requestTokens posts form to the token endpoint tokenURL, authenticated
// with method, and returns the provider's answer.
//
// An answer that names an OAuth error, with any status, wraps
// ErrExchangeRefused: GitHub answers a code it refuses with 200 and an
// error. Any other answer that holds no access token is a failure of the
// system.
func (h providerHTTP) requestTokens(ctx context.Context, tokenURL string, auth clientAuth, method string, form url.Values) (*tokenResponse, error) {
	header := http.Header{"Accept": {"application/json"}}
	if err := authenticate(ctx, auth, method, form, header); err != nil {
		return nil, err
	}
	status, body, err := outbound.PostForm(ctx, h.client, tokenURL, form, header)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	var answer tokenResponse
	decodeErr := json.Unmarshal(bytes.TrimSpace(body), &answer)
	if answer.Error != "" {
		return nil, fmt.Errorf("%w: the token endpoint answered %q", ErrExchangeRefused, answer.Error)
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("token endpoint answered %d", status)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("token endpoint: %w", decodeErr)
	}
	if answer.AccessToken == "" {
		return nil, errors.New("token endpoint: the answer has no access token")
	}
	return &answer, nil
}

// revokeToken asks the revocation endpoint revokeURL to revoke token (RFC
// 7009), authenticated with method. hint is "refresh_token" or
// "access_token". The endpoint answers 200 for a token it does not know as
// well, so any 2xx is a success.
func (h providerHTTP) revokeToken(ctx context.Context, revokeURL string, auth clientAuth, method, token, hint string) error {
	form := url.Values{"token": {token}, "token_type_hint": {hint}}
	header := http.Header{"Accept": {"application/json"}}
	if err := authenticate(ctx, auth, method, form, header); err != nil {
		return err
	}
	status, _, err := outbound.PostForm(ctx, h.client, revokeURL, form, header)
	if err != nil {
		return fmt.Errorf("revocation endpoint: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("revocation endpoint answered %d", status)
	}
	return nil
}

// revocable returns the token of t to revoke, and its type: the refresh
// token, whose revocation ends the grant at most providers, else the access
// token. Both are empty when t holds neither.
func revocable(t Tokens) (token, hint string) {
	switch {
	case t.RefreshToken != "":
		return t.RefreshToken, "refresh_token"
	case t.AccessToken != "":
		return t.AccessToken, "access_token"
	}
	return "", ""
}

// withRequestedScope returns t with scope as its Scope when the provider
// named none: an answer without a scope grants the one asked for (RFC 6749,
// section 5.1).
func withRequestedScope(t Tokens, scope []string) Tokens {
	if t.Scope == "" {
		t.Scope = strings.Join(scope, " ")
	}
	return t
}

// withQuery returns endpoint with params added to its query. A parameter
// the endpoint already has is replaced.
func withQuery(endpoint string, params url.Values) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := u.Query()
	for k, v := range params {
		query[k] = v
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}
