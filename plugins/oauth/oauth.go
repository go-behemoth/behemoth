// Package oauth is the OAuth sign-in plugin: a user signs in through an
// OAuth 2.0 or OpenID Connect provider, such as Google or GitHub.
//
// A sign-in has two flows. Begin starts it: it creates the state, the PKCE
// verifier, the nonce and the binding value of one sign-in, keeps them as a
// single-use token of kind core.TokenKindOAuthState, and returns the URL at
// the provider to send the browser to. The provider sends the browser back
// to a page of the application (Options.CallbackURL) with an authorization
// code and the state. Complete takes both with the binding value, redeems
// the code at the provider, finds or creates the user, and creates a
// session. It fires core's auth.signIn.* points, and auth.signUp.* for a
// user it creates, so a handler written for another plugin's sign-ins sees
// these too.
//
// A signed-in user can also link a provider account to their own account,
// so that the provider signs them in too. BeginLink starts a link the way
// Begin starts a sign-in, and Complete finishes it: the provider account
// is attached to the user who started the link, in the session that
// started it, and no session is created. Unlink removes a provider
// account, unless it is the user's last way to sign in, and Accounts lists
// them. A first sign-in whose email belongs to a user is refused, unless
// the provider is in Options.LinkByVerifiedEmail and both sides verified
// the email.
//
// For a provider in Options.StoreTokens, the plugin keeps the provider's
// tokens, sealed, in the user's account. AccessToken hands the application
// a valid access token, to call the provider for the user, and refreshes
// it when it expired. Unlink revokes the tokens at the provider.
//
// The routes, POST /sign-in/oauth, POST /oauth/link and POST
// /oauth/complete, are JSON calls of the application's own pages. The
// binding value travels the way the session token does: in an HttpOnly
// cookie under the cookie deliveries, and in the response body for a
// client that keeps the session token itself.
//
// Three kinds of provider ship with the plugin and are configured as data
// (ProviderConfig): any OpenID Connect provider, Google, and GitHub. An
// application can add a Provider of its own.
package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/core/hooks"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/httpapi"
	"github.com/go-behemoth/behemoth/models"
	"github.com/go-behemoth/behemoth/outbound"
	"github.com/go-behemoth/behemoth/telemetry"
	"golang.org/x/sync/singleflight"
)

// PluginName is the name the plugin registers under (PluginMeta.Name). It
// owns the plugin's hook points and its token kind, and is the Method of the
// sign-ins it makes.
const PluginName = "oauth"

// pluginVersion is what Version reports.
const pluginVersion = "0.1.0"

// DefaultStateTTL is how long a started sign-in can be finished when
// Options.StateTTL is zero.
const DefaultStateTTL = 10 * time.Minute

// The points the plugin declares and fires around Begin.
const (
	// HookBeginBefore runs on Begin's input: "provider", "redirect_url" and
	// the keys of BeginRequest.Extra. A handler may rewrite them or stop the
	// sign-in.
	HookBeginBefore core.HookPoint = "auth.oauth.beforeBegin"
	// HookBeginAfter runs once a sign-in has started, with a *BeginResult
	// whose AuthorizationURL and Binding are empty.
	HookBeginAfter core.HookPoint = "auth.oauth.afterBegin"
	// HookBeginFailed fires for a sign-in that a handler or a rate limit
	// refused to start: code "rejected_by_hook".
	HookBeginFailed core.HookPoint = "auth.oauth.beginFailed"
)

// The points the plugin declares and fires around a link and an unlink.
// The after and failed points are audited: each changes how a user can
// sign in.
const (
	// HookLinkBefore runs before a provider account is linked, with the
	// payload "userID", "provider" and "by" (LinkedByUser or
	// LinkedByVerifiedEmail). Veto only. For a link the user started, it
	// runs before the callback is checked.
	HookLinkBefore core.HookPoint = "auth.oauth.beforeLink"
	// HookLinkAfter runs once a provider account is linked, with a
	// *LinkResult.
	HookLinkAfter core.HookPoint = "auth.oauth.afterLink"
	// HookLinkFailed fires for a link that was refused. The metadata has the
	// provider.
	HookLinkFailed core.HookPoint = "auth.oauth.linkFailed"
	// HookUnlinkBefore runs before a provider account is removed, with the
	// payload "userID" and "provider". Veto only.
	HookUnlinkBefore core.HookPoint = "auth.oauth.beforeUnlink"
	// HookUnlinkAfter runs once a provider account is removed, with an
	// *UnlinkResult.
	HookUnlinkAfter core.HookPoint = "auth.oauth.afterUnlink"
	// HookUnlinkFailed fires for an unlink that was refused. The metadata
	// has the provider.
	HookUnlinkFailed core.HookPoint = "auth.oauth.unlinkFailed"
)

// The permissions of the flows on a user's provider accounts. Each is a
// self-service permission: the flows act on the caller's own account.
const (
	// PermissionRead is the permission Accounts requires: listing the
	// caller's provider accounts.
	PermissionRead core.Permission = "oauth_account:read"
	// PermissionManage is the permission BeginLink and Unlink require, with
	// a fresh session, and the completion of a link requires.
	PermissionManage core.Permission = "oauth_account:manage"
	// PermissionAccessToken is the permission AccessToken requires: reading
	// the access token of one of the caller's provider accounts.
	PermissionAccessToken core.Permission = "oauth_token:read"
)

// providerID is the form of a provider's id. The length is that of the
// accounts table's provider_id column.
var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Options configures the plugin. CallbackURL is required, and so is at
// least one provider, in Providers or CustomProviders. Every provider has
// an id of its own.
type Options struct {
	// Providers are the providers that ship with the plugin, configured as
	// data: any OpenID Connect provider, Google, and GitHub.
	Providers []ProviderConfig

	// CustomProviders are providers the application implements.
	CustomProviders []Provider

	// HTTPClient makes the calls of the built-in providers: discovery, keys,
	// the code exchange, and GitHub's API. Nil means AuthContext.HTTPClient,
	// the client Boot builds from BootConfig.Outbound for every plugin. Set
	// it to give the providers a client of their own, such as one that
	// sends them to a test server. Build it with outbound.NewClient, or make
	// sure it follows no redirect: the code exchange sends the client
	// secret.
	HTTPClient *http.Client

	// CallbackURL is the page of the application a provider sends the
	// browser back to, such as "https://app.example.com/auth/callback".
	// Required, absolute, http or https, without a query or a fragment.
	//
	// Each provider has a redirect URI of its own under it, CallbackURL
	// followed by "/" and the provider's id (RedirectURI):
	// "https://app.example.com/auth/callback/google". That is the URI to
	// register at the provider, and the page the application serves.
	CallbackURL string

	// StateTTL is how long a started sign-in can be finished: the time a
	// user has to sign in at the provider. Zero means DefaultStateTTL.
	// TokenConfig.TTLOverrides for core.TokenKindOAuthState takes
	// precedence.
	StateTTL time.Duration

	// LinkByVerifiedEmail lists the ids of the providers whose first
	// sign-in may link to an existing user by email. When such a provider
	// names a subject that has no account, and a user has the provider's
	// email, Complete links the subject to that user and signs the user in.
	// It does so only when the provider says it verified the email, the
	// user's own email_verified is true, and the user has no account at
	// the provider yet. Otherwise, and for every provider not listed, the
	// sign-in is refused with ErrorCodeAccountNotLinked, and the user links
	// the provider from a signed-in session (BeginLink). Empty by default.
	//
	// A listed provider can sign in to every account whose email it
	// reports as verified, so an account is as safe as the weakest listed
	// provider. List only a provider that verifies the addresses it
	// reports, for the users it reports them for.
	LinkByVerifiedEmail []string

	// StoreTokens lists the ids of the providers whose tokens the plugin
	// keeps in the user's account: the access token, the refresh token, the
	// ID token, when they expire, and the scope the provider granted. The
	// tokens are sealed in the accounts table. AccessToken then returns a
	// valid access token for the user, and refreshes it when it expired,
	// and Unlink revokes the tokens at the provider. Empty by default: the
	// plugin discards a provider's tokens once the provider has named the
	// user.
	//
	// Each sign-in and link replaces the account's tokens with the ones the
	// provider just issued, and keeps a refresh token the provider did not
	// issue again. A sign-in with a provider that is not listed removes the
	// tokens its account holds.
	//
	// With its tokens stored, a Google provider asks for offline access,
	// without which Google issues no refresh token. Another OpenID Connect
	// provider usually needs the scope "offline_access" in
	// ProviderConfig.Scopes for one.
	StoreTokens []string
}

// Plugin is the OAuth sign-in plugin. It declares the oauth_state token
// kind, the auth.oauth.* points around Begin, a link, and an unlink, and
// fires core's auth.signIn.* and auth.signUp.* points when a sign-in is
// completed. It needs no table of its own: a started sign-in is a row in
// core's tokens table, and a provider account is a row in accounts.
type Plugin struct {
	opts        Options
	providers   map[string]Provider
	authContext *core.AuthContext
	log         telemetry.Logger
	// linkByEmail holds the ids of Options.LinkByVerifiedEmail, and
	// storeTokens those of Options.StoreTokens.
	linkByEmail map[string]bool
	storeTokens map[string]bool
	// refreshes makes the AccessToken calls for one account that find its
	// access token expired share one refresh, within this process.
	refreshes    singleflight.Group
	complete     func(hctx *core.HookContext, in completeInput) (*core.SignInResult, error)
	signUp       func(hctx *core.HookContext, in signUpInput) (*models.User, error)
	completeLink func(hctx *core.HookContext, in linkCompletion) (*LinkResult, error)
	autoLink     func(hctx *core.HookContext, in autoLinkInput) (*LinkResult, error)
	unlink       func(hctx *core.HookContext, in unlinkInput) (*UnlinkResult, error)
}

// New returns the plugin configured with opts. Values left at zero take
// their defaults. Missing or invalid options are reported by Init, as a
// configuration error from Boot.
func New(opts Options) *Plugin {
	if opts.StateTTL == 0 {
		opts.StateTTL = DefaultStateTTL
	}
	return &Plugin{opts: opts}
}

// RedirectURI returns the redirect URI of the provider with the id
// providerID, for an application whose Options.CallbackURL is callbackURL:
// callbackURL followed by "/" and the id. It is the URI the application
// registers at the provider, and the one Begin hands to the provider.
func RedirectURI(callbackURL, providerID string) string {
	return strings.TrimRight(callbackURL, "/") + "/" + providerID
}

// Meta implements [core.Plugin]. The plugin depends on no other plugin and
// has no mount path.
func (p *Plugin) Meta() core.PluginMeta {
	return core.PluginMeta{Name: PluginName}
}

// Version implements [core.Plugin].
func (p *Plugin) Version() string {
	return pluginVersion
}

// Declare implements [core.Plugin]. The plugin declares:
//   - the token kind core.TokenKindOAuthState: single use, and kept in the
//     database, so that the plugin works without a key-value storage;
//   - the permissions PermissionRead, PermissionManage, and
//     PermissionAccessToken;
//   - the auth.oauth.* points around Begin, none of them audited: Begin
//     needs no session, and a start changes no account;
//   - the auth.oauth.* points around a link and an unlink, whose after and
//     failed points are audited;
//   - a rate-limit rule: starts of a sign-in or a link per client address.
//     A completion of a sign-in is a sign-in, which core's rule per
//     address counts.
//
// It declares no sign-in points: core declares auth.signIn.* and
// auth.signUp.*.
func (p *Plugin) Declare(ic *core.PluginInitContext) error {
	if err := ic.Tokens.Declare(core.TokenKindDef{
		Kind: core.TokenKindOAuthState, SingleUse: true, DefaultTTL: p.opts.StateTTL, Backend: core.TokenBackendDB,
	}); err != nil {
		return err
	}
	for _, def := range []core.PermissionDef{
		{Name: PermissionRead, Kind: core.PermissionSelf, Description: "See the provider accounts linked to your account"},
		{Name: PermissionManage, Kind: core.PermissionSelf, Description: "Link provider accounts to your account, and unlink them"},
		{Name: PermissionAccessToken, Kind: core.PermissionSelf, Description: "Use the access tokens of your provider accounts, to call a provider for you"},
	} {
		if err := ic.Permissions.Declare(def); err != nil {
			return err
		}
	}
	for _, def := range []core.HookPointDef{
		{Point: HookBeginBefore, Phase: core.BeforeHookPhase},
		{Point: HookBeginAfter, Phase: core.AfterHookPhase},
		{Point: HookBeginFailed, Phase: core.FailedHookPhase},
		{Point: HookLinkBefore, Phase: core.BeforeHookPhase},
		{Point: HookLinkAfter, Phase: core.AfterHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookLinkFailed, Phase: core.FailedHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookUnlinkBefore, Phase: core.BeforeHookPhase},
		{Point: HookUnlinkAfter, Phase: core.AfterHookPhase, Audit: &core.AuditSpec{}},
		{Point: HookUnlinkFailed, Phase: core.FailedHookPhase, Audit: &core.AuditSpec{}},
	} {
		if err := ic.Hooks.Declare(def); err != nil {
			return err
		}
	}

	// Per client address on Begin's before point, like the magic link
	// plugin's: it slows one client down and can't be used to keep another
	// one out. Begin needs no session and writes a token for every call.
	// BeginLink fires the same point, so starts of a link count here too.
	return ic.RateLimits.DeclareHookRateLimitRule(core.HookRateLimitRule{
		Name: RuleBeginAddress, Point: HookBeginBefore,
		KeyFunc: core.KeyByClientIP(), Limit: core.Limit{Max: 10, Window: time.Minute},
	})
}

// Register implements [core.Plugin]. The plugin attaches no handlers to
// other points.
func (p *Plugin) Register(reg core.HookRegistry) error {
	return nil
}

// Middlewares implements [httpapi.Plugin]. The plugin has no middleware.
func (p *Plugin) Middlewares() []httpapi.Middleware {
	return nil
}

// Init implements [core.Plugin]. It checks the options, builds the
// built-in providers, keeps the AuthContext and wraps the flows with their
// hook points. A missing provider, a ProviderConfig that is incomplete or
// whose client secret can't be read, a provider id that is malformed,
// reserved or used twice, a CallbackURL that is not usable, and an id in
// LinkByVerifiedEmail or StoreTokens that names no provider are
// configuration errors.
// Init calls no provider: a provider that is down fails the sign-ins, not
// the boot.
func (p *Plugin) Init(ac *core.AuthContext) error {
	const op = "oauth.Init"
	if p.opts.StateTTL < 0 {
		return errs.NewConfigurationError(op, fmt.Sprintf("Options.StateTTL is negative: %s", p.opts.StateTTL), nil)
	}
	callbackURL, err := url.Parse(p.opts.CallbackURL)
	if err != nil || (callbackURL.Scheme != "http" && callbackURL.Scheme != "https") || callbackURL.Host == "" ||
		callbackURL.User != nil || callbackURL.RawQuery != "" || callbackURL.ForceQuery || callbackURL.Fragment != "" {
		return errs.NewConfigurationError(op,
			fmt.Sprintf("Options.CallbackURL %q is not an absolute http or https URL without a query or a fragment", p.opts.CallbackURL), err)
	}

	if len(p.opts.Providers)+len(p.opts.CustomProviders) == 0 {
		return errs.NewConfigurationError(op, "Options.Providers and Options.CustomProviders are empty; the plugin needs at least one provider", nil)
	}
	client := providerHTTP{client: p.opts.HTTPClient}
	if client.client == nil {
		client.client = ac.HTTPClient
	}
	if client.client == nil { // an AuthContext Boot did not build
		client.client = outbound.NewClient(outbound.Options{}, ac.Telemetry)
	}
	type placed struct {
		at       string
		provider Provider
	}
	// A provider's settings can depend on whether its tokens are kept, so
	// the set comes first. Its ids are checked once the providers exist.
	storeTokens := make(map[string]bool, len(p.opts.StoreTokens))
	for _, id := range p.opts.StoreTokens {
		storeTokens[id] = true
	}
	all := make([]placed, 0, len(p.opts.Providers)+len(p.opts.CustomProviders))
	for i, cfg := range p.opts.Providers {
		at := fmt.Sprintf("Options.Providers[%d]", i)
		provider, err := newBuiltinProvider(at, cfg, client, time.Now, storeTokens)
		if err != nil {
			return errs.NewConfigurationError(op, err.Error(), nil)
		}
		all = append(all, placed{at, provider})
	}
	for i, provider := range p.opts.CustomProviders {
		all = append(all, placed{fmt.Sprintf("Options.CustomProviders[%d]", i), provider})
	}
	providers := make(map[string]Provider, len(all))
	for _, entry := range all {
		if entry.provider == nil {
			return errs.NewConfigurationError(op, entry.at+" is nil", nil)
		}
		id := entry.provider.ID()
		switch {
		case !providerID.MatchString(id):
			return errs.NewConfigurationError(op, fmt.Sprintf(
				"%s has the id %q; an id is 1 to 64 lowercase letters, digits, \"-\" and \"_\", and starts with a letter or a digit", entry.at, id), nil)
		case id == models.ProviderCredential:
			// The provider_id of the account that holds a user's password.
			return errs.NewConfigurationError(op, fmt.Sprintf("%s has the reserved id %q", entry.at, id), nil)
		case providers[id] != nil:
			return errs.NewConfigurationError(op, fmt.Sprintf("two providers have the id %q", id), nil)
		}
		providers[id] = entry.provider
	}
	linkByEmail := make(map[string]bool, len(p.opts.LinkByVerifiedEmail))
	for i, id := range p.opts.LinkByVerifiedEmail {
		if providers[id] == nil {
			return errs.NewConfigurationError(op, fmt.Sprintf("Options.LinkByVerifiedEmail[%d] is %q, which is the id of no provider", i, id), nil)
		}
		linkByEmail[id] = true
	}
	for i, id := range p.opts.StoreTokens {
		if providers[id] == nil {
			return errs.NewConfigurationError(op, fmt.Sprintf("Options.StoreTokens[%d] is %q, which is the id of no provider", i, id), nil)
		}
	}

	p.providers = providers
	p.linkByEmail = linkByEmail
	p.storeTokens = storeTokens
	p.authContext = ac
	p.log = telemetry.NoOpLogger{}
	if ac.Telemetry != nil {
		p.log = telemetry.Named(ac.Telemetry.Logger, PluginName)
	}
	p.complete = core.WithLifecycle(ac.Dispatcher, hooks.HookSignInBefore, hooks.HookSignInAfter, hooks.HookSignInFailed, p.completeBody)
	p.signUp = core.WithLifecycle(ac.Dispatcher, hooks.HookSignUpBefore, hooks.HookSignUpAfter, hooks.HookSignUpFailed, p.signUpBody)
	// Both ways to link fire the same points, so a handler and the audit
	// log see every link, whichever way it was made.
	p.completeLink = core.WithLifecycle(ac.Dispatcher, HookLinkBefore, HookLinkAfter, HookLinkFailed, p.completeLinkBody)
	p.autoLink = core.WithLifecycle(ac.Dispatcher, HookLinkBefore, HookLinkAfter, HookLinkFailed, p.autoLinkBody)
	p.unlink = core.WithLifecycle(ac.Dispatcher, HookUnlinkBefore, HookUnlinkAfter, HookUnlinkFailed, p.unlinkBody)
	return nil
}

// ready reports a configuration error when Boot has not initialized the
// plugin. A flow that asks Access about the caller calls it first.
func (p *Plugin) ready(op string) error {
	if p.authContext == nil {
		return errs.NewConfigurationError(op, "the plugin is not initialized; pass it to Prepare and Boot first", nil)
	}
	return nil
}

// operation builds the HookContext one flow call runs with. Its Values start
// as a copy of the operation ctx already belongs to, if any, and Request is
// the request ctx carries, or nil.
func (p *Plugin) operation(ctx context.Context, op string) (*core.HookContext, error) {
	if err := p.ready(op); err != nil {
		return nil, err
	}
	return &core.HookContext{
		Ctx:    ctx,
		Auth:   p.authContext,
		Values: core.NestedHookValues(ctx),
	}, nil
}

// Routes implements [httpapi.Plugin]. The routes on the caller's provider
// accounts have no session middleware: each flow reads the caller itself.
func (p *Plugin) Routes() []httpapi.Route {
	return []httpapi.Route{
		{Method: http.MethodPost, Path: PathBegin, Handler: p.handleBegin},
		{Method: http.MethodPost, Path: PathLink, Handler: p.handleLink},
		{Method: http.MethodPost, Path: PathComplete, Handler: p.handleComplete},
		{Method: http.MethodPost, Path: PathUnlink, Handler: p.handleUnlink},
		{Method: http.MethodGet, Path: PathAccounts, Handler: p.handleAccounts},
	}
}

var _ httpapi.Plugin = (*Plugin)(nil)
