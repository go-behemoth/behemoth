package oauth

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/outbound"
)

// Kinds of the providers that ship with the plugin (ProviderConfig.Kind).
const (
	// KindOIDC is any OpenID Connect provider, named by its issuer. The
	// plugin reads the issuer's discovery document and checks every ID
	// token against the issuer's keys.
	KindOIDC = "oidc"
	// KindGoogle is Google, an OpenID Connect provider with the issuer
	// https://accounts.google.com.
	KindGoogle = "google"
	// KindGitHub is GitHub, an OAuth 2.0 provider without ID tokens. The
	// user is read from GitHub's API.
	KindGitHub = "github"
)

// googleIssuer is Google's issuer. Google's ID tokens name it with and
// without the scheme.
const googleIssuer = "https://accounts.google.com"

// defaultScopes are the scopes of each kind when ProviderConfig.Scopes is
// empty: what a sign-in needs, and nothing more.
var defaultScopes = map[string][]string{
	KindOIDC:   {"openid", "email", "profile"},
	KindGoogle: {"openid", "email", "profile"},
	KindGitHub: {"read:user", "user:email"},
}

// ProviderConfig is a provider that ships with the plugin, as data: what an
// application registered at the provider, and which kind of provider it is.
// Options.Providers takes a list of them.
type ProviderConfig struct {
	// Kind is KindOIDC, KindGoogle or KindGitHub. Required.
	Kind string
	// ID is the provider's id (Provider.ID). Empty means the kind, for
	// KindGoogle and KindGitHub. Required for KindOIDC, as the name of the
	// provider, such as "okta".
	ID string
	// ClientID is the client id the provider issued to the application.
	// Required.
	ClientID string
	// ClientSecret is the source of the client secret the provider issued.
	// The plugin reads it at Init, to fail there when it is missing, and
	// again for every code it redeems, so a rotated secret takes effect
	// without a restart. Its current secret is the one used. Required for
	// KindGoogle and KindGitHub. Nil for KindOIDC means a public client,
	// which authenticates with PKCE alone.
	ClientSecret crypto.SecretSource
	// Issuer is the issuer of a KindOIDC provider, such as
	// "https://login.example.com". Required for KindOIDC: https, or http
	// on a loopback address, and exactly the issuer the discovery document
	// names. Empty for the other kinds, which have theirs built in.
	Issuer string
	// Scopes replaces the kind's scopes: "openid email profile" for
	// KindOIDC and KindGoogle, which have to keep "openid", and
	// "read:user user:email" for KindGitHub.
	Scopes []string
}

// newBuiltinProvider returns the provider cfg describes, or a configuration
// error naming the field that is wrong. at is cfg's place in
// Options.Providers, for the error. storeTokens holds the ids of
// Options.StoreTokens.
func newBuiltinProvider(at string, cfg ProviderConfig, client providerHTTP, now func() time.Time, storeTokens map[string]bool) (Provider, error) {
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes[cfg.Kind]
	}
	id := cfg.ID
	if id == "" && cfg.Kind != KindOIDC {
		id = cfg.Kind
	}

	switch cfg.Kind {
	case KindOIDC, KindGoogle, KindGitHub:
	default:
		return nil, fmt.Errorf("%s.Kind is %q; it is %q, %q or %q", at, cfg.Kind, KindOIDC, KindGoogle, KindGitHub)
	}
	if id == "" {
		return nil, fmt.Errorf("%s.ID is empty; a provider of kind %q needs one", at, cfg.Kind)
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("%s.ClientID is empty", at)
	}
	if cfg.ClientSecret == nil && cfg.Kind != KindOIDC {
		return nil, fmt.Errorf("%s.ClientSecret is nil; %s needs the client secret", at, cfg.Kind)
	}
	if cfg.ClientSecret != nil {
		if _, err := currentSecret(context.Background(), cfg.ClientSecret); err != nil {
			return nil, fmt.Errorf("%s.ClientSecret: %w", at, err)
		}
	}
	if cfg.Kind != KindGitHub && !slices.Contains(scopes, "openid") {
		return nil, fmt.Errorf("%s.Scopes has no \"openid\"; an OpenID Connect provider needs it", at)
	}
	auth := clientAuth{clientID: cfg.ClientID, secret: cfg.ClientSecret}

	switch cfg.Kind {
	case KindGitHub:
		if cfg.Issuer != "" {
			return nil, fmt.Errorf("%s.Issuer is set; GitHub has its endpoints built in", at)
		}
		return &githubProvider{id: id, auth: auth, scopes: scopes, http: client, now: now}, nil
	case KindGoogle:
		if cfg.Issuer != "" {
			return nil, fmt.Errorf("%s.Issuer is set; Google's is built in", at)
		}
		google := &oidcProvider{
			id: id, issuer: googleIssuer, issuers: []string{googleIssuer, "accounts.google.com"},
			auth: auth, scopes: scopes, http: client, now: now,
		}
		if storeTokens[id] {
			// Google issues a refresh token only to a client that asks for
			// offline access, and ignores the scope offline_access.
			google.authParams = url.Values{"access_type": {"offline"}}
		}
		return google, nil
	}

	if err := outbound.CheckURL(cfg.Issuer); err != nil {
		return nil, fmt.Errorf("%s.Issuer: %w", at, err)
	}
	if u, _ := url.Parse(cfg.Issuer); u.RawQuery != "" || u.ForceQuery {
		// OpenID Connect Discovery 1.0, section 2: an issuer has no query.
		return nil, fmt.Errorf("%s.Issuer %q has a query", at, cfg.Issuer)
	}
	return &oidcProvider{
		id: id, issuer: cfg.Issuer, issuers: []string{cfg.Issuer},
		auth: auth, scopes: scopes, http: client, now: now,
	}, nil
}
