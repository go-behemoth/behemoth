package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/plugins/oauth"
)

// pathOAuthCallback is the page the providers send the browser back to.
// Each provider has a redirect URI of its own below it, such as
// /auth/callback/github, which is the one to register at the provider.
const pathOAuthCallback = "/auth/callback"

// oauthPlugin returns the OAuth plugin with the providers the environment
// configures, and their ids, or nil when it configures none: the plugin
// needs at least one provider, and the example runs without any.
//
//	GITHUB_CLIENT_ID, GITHUB_CLIENT_SECRET               a GitHub OAuth App or GitHub App
//	GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET               a Google OAuth client
//	OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET      any OpenID Connect provider, such as a local
//	                                                     Keycloak, under the id "oidc"; the secret is optional
//
// The client secrets come from the environment here, and from wherever the
// application keeps secrets in production.
func oauthPlugin() (*oauth.Plugin, []string) {
	var providers []oauth.ProviderConfig
	if id, secret := os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"); id != "" && secret != "" {
		providers = append(providers, oauth.ProviderConfig{Kind: oauth.KindGitHub, ClientID: id, ClientSecret: secretOf(secret)})
	}
	if id, secret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"); id != "" && secret != "" {
		providers = append(providers, oauth.ProviderConfig{Kind: oauth.KindGoogle, ClientID: id, ClientSecret: secretOf(secret)})
	}
	if issuer := os.Getenv("OIDC_ISSUER"); issuer != "" {
		cfg := oauth.ProviderConfig{Kind: oauth.KindOIDC, ID: "oidc", Issuer: issuer, ClientID: os.Getenv("OIDC_CLIENT_ID")}
		if secret := os.Getenv("OIDC_CLIENT_SECRET"); secret != "" {
			cfg.ClientSecret = secretOf(secret)
		}
		providers = append(providers, cfg)
	}
	if len(providers) == 0 {
		return nil, nil
	}
	ids := providerIDs(providers)
	return oauth.New(oauth.Options{
		Providers:   providers,
		CallbackURL: appURL() + pathOAuthCallback,
		// The example keeps every provider's tokens, so that the
		// oauth-token command has something to show. An application that
		// only signs users in lists none, and holds no token to lose.
		StoreTokens: ids,
		// LinkByVerifiedEmail is left empty: a first sign-in whose email
		// belongs to a user is refused, and the user links the provider from
		// the OAuth page while signed in.
	}), ids
}

// providerIDs returns the ids of the providers: the kind for GitHub and
// Google, and the configured id for the others.
func providerIDs(providers []oauth.ProviderConfig) []string {
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		id := p.ID
		if id == "" {
			id = p.Kind
		}
		ids = append(ids, id)
	}
	return ids
}

// secretOf returns a secret source holding one secret.
func secretOf(secret string) crypto.SecretSource {
	return crypto.StaticSecretSource{Secrets: map[int]string{1: secret}, Current: 1}
}

// oauthToken prints what AccessToken returns for the user with email at
// provider: a valid access token, refreshed first when it expired. The
// command names itself as a system caller acting for the user, as a job
// that calls the provider's API for a user would. The token is a
// credential, so only its first characters are printed.
func oauthToken(ctx context.Context, email, provider string) error {
	obs, err := newObservability(ctx)
	if err != nil {
		return err
	}
	defer obs.shutdown(ctx)
	ac, plugs, sqlDB, err := boot(ctx, nil, obs.tel)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	if plugs.oauth == nil {
		return errors.New("no provider is configured: set GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET, or see oauth.go")
	}

	user, err := ac.Store.FindUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("find %s: %w", email, err)
	}
	ctx = identity.WithCaller(ctx, identity.System("example-oauth-token").For(user.ID))
	token, err := plugs.oauth.AccessToken(ctx, oauth.AccessTokenRequest{Provider: provider})
	if err != nil {
		return fmt.Errorf("access token: %w", err)
	}
	shown := token.AccessToken
	if len(shown) > 6 {
		shown = shown[:6] + "..."
	}
	expires := "never, as far as the provider said"
	if !token.ExpiresAt.IsZero() {
		expires = token.ExpiresAt.Local().Format("2006-01-02 15:04:05")
	}
	fmt.Printf("%s access token of %s: %s\nexpires: %s\nscope: %s\n", token.Provider, user.Email, shown, expires, token.Scope)
	return nil
}
