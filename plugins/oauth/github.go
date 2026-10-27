package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth/outbound"
)

// GitHub's endpoints. GitHub is OAuth 2.0 without OpenID Connect: there is
// no discovery document and no ID token, and the user is read from its API.
const (
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubAPIURL       = "https://api.github.com"
	// githubGrantPath deletes the application's grant for a user, and with
	// it every token GitHub issued the application for that user. The
	// placeholder is the client id.
	githubGrantPath = "/applications/%s/grant"
)

// githubAPIVersion is the version of GitHub's REST API the provider speaks.
const githubAPIVersion = "2022-11-28"

// githubProvider is the built-in GitHub provider.
type githubProvider struct {
	id     string
	auth   clientAuth
	scopes []string
	http   providerHTTP
	now    func() time.Time
}

func (p *githubProvider) ID() string { return p.id }

// AuthorizationURL implements [Provider]. GitHub has no nonce.
func (p *githubProvider) AuthorizationURL(_ context.Context, req AuthorizationRequest) (string, error) {
	return withQuery(githubAuthorizeURL, url.Values{
		"client_id":             {p.auth.clientID},
		"redirect_uri":          {req.RedirectURI},
		"scope":                 {strings.Join(p.scopes, " ")},
		"state":                 {req.State},
		"code_challenge":        {req.CodeChallenge},
		"code_challenge_method": {"S256"},
	})
}

// Exchange implements [Provider]. The subject is the user's numeric id: a
// login can be renamed and then taken by somebody else. The email is the
// user's primary address, and only when GitHub has verified it.
//
// GitHub names no issuer, so the iss of an authorization response is not
// checked.
func (p *githubProvider) Exchange(ctx context.Context, req ExchangeRequest) (*ExchangeResult, error) {
	tokens, err := p.http.redeemCode(ctx, githubTokenURL, p.auth, authSecretPost, req)
	if err != nil {
		return nil, err
	}
	header := http.Header{
		"Authorization":        {"Bearer " + tokens.AccessToken},
		"Accept":               {"application/vnd.github+json"},
		"X-Github-Api-Version": {githubAPIVersion},
	}

	var user struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if _, err := p.http.getJSON(ctx, githubAPIURL+"/user", header, &user); err != nil {
		return nil, fmt.Errorf("github user: %w", err)
	}
	if user.ID == 0 {
		return nil, errors.New("github user: the answer has no id")
	}
	identity := Identity{Subject: strconv.FormatInt(user.ID, 10), Name: user.Name, Picture: user.AvatarURL}

	// The addresses need the user:email scope. Without it GitHub answers
	// 404, and the identity has no email.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	status, err := p.http.getJSON(ctx, githubAPIURL+"/user/emails", header, &emails)
	switch {
	case err == nil:
		for _, e := range emails {
			if e.Primary && e.Verified {
				identity.Email, identity.EmailVerified = e.Email, true
			}
		}
	case status == http.StatusNotFound || status == http.StatusForbidden:
	default:
		return nil, fmt.Errorf("github emails: %w", err)
	}
	return &ExchangeResult{Identity: identity, Tokens: withRequestedScope(githubTokens(tokens, p.now()), p.scopes)}, nil
}

// Refresh implements [Refresher]. Only the user tokens of a GitHub App that
// expire come with a refresh token: an OAuth App's tokens don't expire.
// GitHub replaces the refresh token at every refresh.
func (p *githubProvider) Refresh(ctx context.Context, req RefreshRequest) (*Tokens, error) {
	answer, err := p.http.refreshTokens(ctx, githubTokenURL, p.auth, authSecretPost, req.RefreshToken)
	if err != nil {
		return nil, err
	}
	tokens := githubTokens(answer, p.now())
	return &tokens, nil
}

// Revoke implements [Revoker]. It deletes the application's grant for the
// user, which revokes every token GitHub issued the application for them.
// GitHub takes the client id and secret as Basic credentials, and an access
// token of the user in the body. An access token that expired can't name the
// grant any more, and GitHub refuses the call.
func (p *githubProvider) Revoke(ctx context.Context, tokens Tokens) error {
	if tokens.AccessToken == "" {
		return nil
	}
	secret, err := currentSecret(ctx, p.auth.secret)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"access_token": tokens.AccessToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, githubAPIURL+fmt.Sprintf(githubGrantPath, url.PathEscape(p.auth.clientID)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.SetBasicAuth(p.auth.clientID, secret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Github-Api-Version", githubAPIVersion)
	status, _, err := outbound.Do(p.http.client, req, 0)
	if err != nil {
		return fmt.Errorf("github grant: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("github grant: answered %d", status)
	}
	return nil
}

// githubTokens returns GitHub's answer as Tokens. GitHub separates the
// scopes with commas, where OAuth 2.0 uses spaces.
func githubTokens(answer *tokenResponse, now time.Time) Tokens {
	tokens := answer.tokens(now)
	tokens.Scope = strings.Join(strings.FieldsFunc(tokens.Scope, func(r rune) bool { return r == ',' || r == ' ' }), " ")
	return tokens
}
