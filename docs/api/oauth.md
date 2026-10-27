# OAuth

The `oauth` plugin signs a user in through an OAuth 2.0 or OpenID Connect provider, such as Google or GitHub. The provider authenticates the user, and Behemoth signs in the user the provider names, or creates one on the first sign-in. A signed-in user can also link a provider account to their account, list the linked accounts, and unlink one. For the providers you choose, the plugin keeps the provider's tokens, so that your server can call the provider's API for the user.

Three kinds of provider ship with the plugin and are configured as data: any OpenID Connect provider, Google, and GitHub. You can add a provider of your own.


## How it works

| Step | Who | What happens |
| --- | --- | --- |
| 1 | your page | posts the provider's id to `POST /sign-in/oauth` |
| 2 | plugin | stores the sign-in, and answers with the provider's authorization URL and a binding value |
| 3 | your page | sends the browser to the authorization URL |
| 4 | user | signs in at the provider |
| 5 | provider | sends the browser to your callback page, with a `code` and a `state` in the URL |
| 6 | your callback page | posts the code, the state, and the binding value to `POST /oauth/complete` |
| 7 | plugin | checks the state and the binding value, redeems the code at the provider, and creates a session |

The provider sends the browser to your page and not to Behemoth, and every call to Behemoth is a JSON `POST`. A sign-in therefore answers like every other sign-in route, whichever token delivery you use.

The binding value ties step 7 to the client that made step 1. A browser that is sent to your callback page with somebody else's code and state does not have it, so the sign-in is refused.

## Setup

Pass the plugin to `Prepare`. It uses the core `users`, `accounts`, `sessions`, and `tokens` tables and has none of its own.

```go
import (
	"github.com/go-behemoth/behemoth/crypto"
	"github.com/go-behemoth/behemoth/plugins/oauth"
)

plugin := oauth.New(oauth.Options{
	Providers: []oauth.ProviderConfig{{
		Kind:         oauth.KindGoogle,
		ClientID:     "1234.apps.googleusercontent.com",
		ClientSecret: crypto.StaticSecretSource{Secrets: map[int]string{1: GOOGLE_CLIENT_SECRET}, Current: 1},
	}},
	CallbackURL: "https://app.example.com/auth/callback",
})

app, err := engine.Prepare([]core.Plugin{plugin}, engine.PrepareConfig{})
// ...
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Router: httpapi.RouterConfig{TrustedOrigins: []string{"https://app.example.com"}},
	// ...
})
```

Replace `GOOGLE_CLIENT_SECRET` with the client secret, read from where you keep secrets.

Register the redirect URI of each provider at the provider. It is `CallbackURL`, then `/`, then the provider's id. `oauth.RedirectURI(callbackURL, id)` returns it:

```go
oauth.RedirectURI("https://app.example.com/auth/callback", "google")
// "https://app.example.com/auth/callback/google"
```

Serve your callback page at that URI. See [Build the callback page](#build-the-callback-page).

[examples/init](../../examples/init) is a complete application with these pages, for GitHub, Google, or another OpenID Connect provider configured in its environment.

## Options

| Field | Default | Meaning |
| --- | --- | --- |
| `Providers` | none | The built-in providers a user can sign in with, as data. See [Configure a provider](#configure-a-provider). |
| `CustomProviders` | none | Providers you implement. See [Write a provider](#write-a-provider). |
| `CallbackURL` | required | The page of your application the providers send the browser back to. Absolute, `http` or `https`, without a query or a fragment. Each provider's redirect URI is below it. |
| `StateTTL` | 10 minutes | How long a started sign-in can be completed: the time a user has at the provider. `TokenConfig.TTLOverrides[core.TokenKindOAuthState]` takes precedence. |
| `HTTPClient` | `ac.HTTPClient`, the client of `BootConfig.Outbound` | The client the built-in providers call the provider with. Set it to give the plugin a client of its own, such as one for a test server. Build it with `outbound.NewClient`, or make sure it follows no redirect: the code exchange sends the client secret. See [Outbound calls](outbound.md). |
| `LinkByVerifiedEmail` | none | The ids of the providers whose first sign-in may link to an existing user with the same email, when both sides verified it. See [Link by a verified email](#link-by-a-verified-email). |
| `StoreTokens` | none | The ids of the providers whose tokens the plugin keeps, to call the provider's API for the user. See [Call the provider's API](#call-the-providers-api). |

`Boot` returns a configuration error for no provider at all, a `ProviderConfig` that is incomplete or whose secret can't be read, a nil provider, an id that is used twice, an id that is not 1 to 64 characters of `a-z`, `0-9`, `-`, and `_`, the id `credential`, a `CallbackURL` that is not usable, and an id in `LinkByVerifiedEmail` or `StoreTokens` that is no provider's. `Boot` calls no provider. A provider that is down fails its sign-ins, not the boot.

## Configure a provider

A `ProviderConfig` describes one built-in provider:

| Field | Meaning |
| --- | --- |
| `Kind` | `oauth.KindOIDC`, `oauth.KindGoogle`, or `oauth.KindGitHub`. Required. |
| `ID` | The provider's id. It defaults to the kind for Google and GitHub, and is required for `KindOIDC`, such as `"okta"`. It is stored with every account the provider signs in, so don't change it once users have signed in. |
| `ClientID` | The client id the provider issued to your application. Required. |
| `ClientSecret` | A `crypto.SecretSource` with the client secret. Its current secret is used. Required for Google and GitHub. Without it, a `KindOIDC` provider is a public client and authenticates with PKCE alone. |
| `Issuer` | The issuer of a `KindOIDC` provider, such as `"https://login.example.com"`. Required for `KindOIDC`, and empty for the other two. It is `https`, or `http` on `localhost` or a loopback address, and it has to be exactly the issuer the provider's discovery document names. |
| `Scopes` | Replaces the kind's scopes. An OpenID Connect provider needs `openid`. |

The kinds differ as follows:

| Kind | Scopes by default | Where the user comes from |
| --- | --- | --- |
| `KindOIDC` | `openid email profile` | the ID token. When it has no email, the issuer's userinfo endpoint. |
| `KindGoogle` | `openid email profile` | the ID token, as for `KindOIDC`, with the issuer `https://accounts.google.com` |
| `KindGitHub` | `read:user user:email` | GitHub's API. The subject is the user's numeric id, because a login can be renamed. The email is the primary address, and only when GitHub verified it. Without `user:email`, the identity has no email. |

The plugin reads the client secret at `Boot`, and again for every code it redeems. To rotate a secret, make the new one current in the source. No restart is needed.

### What the built-in providers check

The plugin calls an OpenID Connect provider's discovery document at the first sign-in, and every 24 hours after. It refuses a document that names another issuer, or an endpoint that is not `https`.

An ID token is accepted only when every one of these checks passes:

- **The signature.** A key of the issuer's key set, with RSA (at least 2048 bits) or ECDSA. `none` and the HMAC algorithms are refused. The key set is kept for an hour. A token signed with a key the set lacks makes the plugin fetch the set again, at most once a minute. While the key set can't be fetched, the keys of the last fetch are used for one more hour. A set without a key the plugin can use fails the sign-in as an error of the system, as an unreachable provider does.
- **The issuer, the audience, and the authorized party.** `iss` is the issuer, `aud` names your client id, and `azp`, when present or when there are several audiences, is your client id.
- **The time.** `exp` is required, and neither `exp` nor `iat` may be off by more than a minute.
- **The nonce and the subject.** `nonce` is the one of the sign-in, and `sub` is not empty.

When the provider sends an `iss` parameter with the code (RFC 9207), it has to be the issuer. When the discovery document says the provider sends one, it is required. See [Build the callback page](#build-the-callback-page).

The token request authenticates with HTTP Basic when the provider supports it or does not say, and in the form otherwise. GitHub gets the secret in the form. A provider's answer is read up to 1 MiB, and every call follows the rules in [Outbound calls](outbound.md).

## Write a provider

For a provider that is not built in, implement `Provider` and pass it in `CustomProviders`. A `Provider` has an id and two methods:

| Method | Receives | Returns |
| --- | --- | --- |
| `ID()` | | The provider's id. It is stored with every account the provider signs in, so don't change it once users have signed in. |
| `AuthorizationURL(ctx, req)` | the state, the redirect URI, the PKCE code challenge (S256), and the nonce | The URL at the provider to send the browser to. Absolute, `http` or `https`. |
| `Exchange(ctx, req)` | the authorization code, the redirect URI, the PKCE code verifier, the nonce, and the `iss` parameter when the provider sent one | An `ExchangeResult` with the user's `Identity`, and the `Tokens` the provider issued. |

An `Identity` has these fields:

| Field | Meaning |
| --- | --- |
| `Subject` | The provider's stable id for the user. Required. Never an email address. |
| `Email` | The user's email address at the provider. Optional for a user who already has an account, required on the first sign-in. |
| `EmailVerified` | `true` only when the provider says it verified `Email`. |
| `Name`, `GivenName`, `FamilyName` | The user's name. Each is optional. |
| `Picture` | The URL of the user's picture. Optional. |

`ExchangeResult.Tokens` holds what the provider issued with the code: the access token, the refresh token, the ID token, when the access and refresh tokens expire, and the scope it granted, as space-separated values. The plugin stores them for a provider in `StoreTokens`. Leave them empty when your provider has none to give.

`Exchange` reports a refusal by returning an error that wraps `oauth.ErrExchangeRefused`: a code the provider refused, or an ID token that fails your checks. The plugin then refuses the sign-in with `invalid_oauth_callback`. Any other error is a failure of the system, and the route answers `500`.

Two interfaces are optional. Implement them for the plugin to refresh and revoke your provider's tokens:

| Interface | Method | Called by |
| --- | --- | --- |
| `oauth.Refresher` | `Refresh(ctx, req)` redeems `req.RefreshToken` and returns the new `Tokens`. Leave their `RefreshToken` empty when the provider keeps the old one. Wrap `oauth.ErrExchangeRefused` for a refresh token the provider refused. | `AccessToken`, for an access token that expired |
| `oauth.Revoker` | `Revoke(ctx, tokens)` revokes the tokens at the provider. Return `nil` when the provider offers no way to. | `Unlink`, once the account is removed |

Put the code challenge and the nonce into the authorization URL, and send the verifier with the code. The plugin creates them for every sign-in, but it can't check that your provider uses them. Without them, an authorization code that leaks can be used by somebody else.

```go
type acmeProvider struct {
	clientID string
	// redeem posts the code, the redirect URI, and the verifier to Acme's
	// token endpoint, checks the ID token's signature, issuer, audience,
	// and expiry, and returns its claims.
	redeem func(ctx context.Context, req oauth.ExchangeRequest) (map[string]any, error)
}

func (p *acmeProvider) ID() string { return "acme" }

func (p *acmeProvider) AuthorizationURL(ctx context.Context, req oauth.AuthorizationRequest) (string, error) {
	query := url.Values{
		"response_type": {"code"}, "client_id": {p.clientID}, "scope": {"openid email profile"},
		"redirect_uri": {req.RedirectURI}, "state": {req.State}, "nonce": {req.Nonce},
		"code_challenge": {req.CodeChallenge}, "code_challenge_method": {"S256"},
	}
	return "https://id.acme.example/authorize?" + query.Encode(), nil
}

func (p *acmeProvider) Exchange(ctx context.Context, req oauth.ExchangeRequest) (*oauth.ExchangeResult, error) {
	claims, err := p.redeem(ctx, req)
	if err != nil {
		return nil, err // wrap oauth.ErrExchangeRefused when Acme refused the code
	}
	if claims["nonce"] != req.Nonce {
		return nil, fmt.Errorf("acme: the ID token has another nonce: %w", oauth.ErrExchangeRefused)
	}
	subject, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	verified, _ := claims["email_verified"].(bool)
	return &oauth.ExchangeResult{Identity: oauth.Identity{Subject: subject, Email: email, EmailVerified: verified}}, nil
}
```

A provider must be safe for concurrent use.

## Build the callback page

Your callback page receives the browser at `<CallbackURL>/<provider id>`, with `code` and `state` in the query, and `iss` when the provider sends it. It posts them to `POST /oauth/complete` with the provider's id, the last segment of its own path.

The binding value comes back the way it went out:

| Token delivery | `POST /sign-in/oauth` returns the binding value | Your page sends it back |
| --- | --- | --- |
| `DeliveryCookie`, the default | in an `HttpOnly` cookie. The body has no `binding`. | The browser sends the cookie. Call with `credentials: "include"` when Behemoth is on another origin. |
| `DeliveryHeader`, `DeliveryBody` | in the body, as `binding` | Keep it where the page can read it after the redirect, such as `sessionStorage`, and send it as `binding`. |
| `DeliveryBoth` | in both | Either way. |

Never put the binding value in a URL. Whoever has the code, the state, and the binding value can complete the sign-in.

```js
// On your sign-in page.
const begun = await fetch("/api/auth/sign-in/oauth", {
  method: "POST", credentials: "include",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ provider: "google", redirect_url: "/dashboard" }),
}).then((r) => r.json());
if (begun.binding) sessionStorage.setItem("oauth_binding", begun.binding); // header and body deliveries
location.assign(begun.authorization_url);

// On your callback page, at /auth/callback/google.
const query = new URLSearchParams(location.search);
history.replaceState(null, "", location.pathname); // take the code out of the address bar
const done = await fetch("/api/auth/oauth/complete", {
  method: "POST", credentials: "include",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({
    provider: "google", code: query.get("code"), state: query.get("state"), iss: query.get("iss") ?? undefined,
    binding: sessionStorage.getItem("oauth_binding") ?? undefined,
  }),
});
```

When the provider sends `error` instead of `code`, the user did not sign in at the provider. The page has nothing to post.

### The binding cookie

Under the cookie deliveries the cookie is named `__Host-behemoth_oauth_` followed by 16 characters derived from the state, so two sign-ins started in one browser keep a cookie each. It has these attributes:

- `HttpOnly`, `Path=/`, and no `Domain`. A browser sends it only to Behemoth's host.
- `Secure`, and the `SameSite` of the session cookie (`RouterConfig.Carrier.CookieSameSite`). It reaches `POST /oauth/complete` on the requests that the session cookie reaches.
- `Max-Age` until the sign-in expires. A completed sign-in removes it.

With `RouterConfig.Carrier.CookieInsecure` set, the cookie has no `Secure` attribute and its name has no `__Host-` prefix, which a browser accepts only with `Secure`.

## Link a provider account

A signed-in user can add a provider as a way to sign in. The link starts on a page where the user is signed in, and the rest is the sign-in's flow: the provider sends the browser to your callback page, which posts to `POST /oauth/complete`.

| Step | Who | What happens |
| --- | --- | --- |
| 1 | your page | posts the provider's id to `POST /oauth/link`, with the user's session |
| 2 | plugin | stores the link with the user and the session, and answers with the provider's authorization URL and a binding value |
| 3 | your page | sends the browser to the authorization URL |
| 4 | provider | sends the browser to your callback page, with a `code` and a `state` |
| 5 | your callback page | posts them to `POST /oauth/complete`, as for a sign-in, with the user's session |
| 6 | plugin | links the provider account to the user, and answers `{"status": "linked", ...}` |

Start the link like a sign-in, and send the session token the way your page sends it on every call of a signed-in user:

```js
// On your settings page.
const begun = await fetch("/api/auth/oauth/link", {
  method: "POST", credentials: "include",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ provider: "github", redirect_url: "/settings/accounts" }),
}).then((r) => r.json());
if (begun.binding) sessionStorage.setItem("oauth_binding", begun.binding); // header and body deliveries
location.assign(begun.authorization_url);
```

Your callback page needs no second version. Its call to `POST /oauth/complete` answers a link with `status` set to `linked`:

```js
// On your callback page, after the call to /api/auth/oauth/complete.
const body = await done.json();
if (body.status === "linked") {
  // body.account.provider is now a way for the user to sign in.
}
location.assign(body.redirect_url || "/");
```

A link follows these rules:

- **It needs a fresh session.** `POST /oauth/link` refuses a session that signed in more than `SessionConfig.FreshAge` ago (15 minutes by default) with `401` `session_not_fresh`. The user signs in again, and starts the link again. A link adds a way into the account, which a stolen session should not be enough for.
- **The session that started it completes it.** Another session, the user's own included, gets `401` `invalid_oauth_callback`, and a session that ended meanwhile gets its session error, such as `session_revoked`. The session need not be fresh any more when the link completes: the user may spend a while at the provider.
- **The provider's email does not matter.** The provider account is linked whatever email the provider reports, verified or not. Nothing of the provider's profile is written to the user.
- **A user has one account per provider.** A link to a provider the user has an account at gets `409` `provider_already_linked`, when it starts and when it completes. Unlink the account first to link another one at the same provider.
- **A provider account belongs to one user.** One that is linked to another user gets `409` `provider_account_taken`. The user signs in with the provider, to that other user, and unlinks it there.
- **No session is created.** The answer carries no session token, and the user's session stays as it was.

### Link by a verified email

By default, a first sign-in with a provider whose email belongs to a user is refused with `409` `account_not_linked`: the user signs in the way they did before, and links the provider from there. With a provider in `LinkByVerifiedEmail`, such a sign-in links the provider account to that user instead, and signs the user in:

```go
plugin := oauth.New(oauth.Options{
	Providers: []oauth.ProviderConfig{{
		Kind:         oauth.KindGoogle,
		ClientID:     "1234.apps.googleusercontent.com",
		ClientSecret: crypto.StaticSecretSource{Secrets: map[int]string{1: GOOGLE_CLIENT_SECRET}, Current: 1},
	}},
	CallbackURL:         "https://app.example.com/auth/callback",
	LinkByVerifiedEmail: []string{"google"},
})
```

Replace `GOOGLE_CLIENT_SECRET` with the client secret, as in [Setup](#setup).

The plugin links only when each of these holds:

- **The provider verified the email.** The identity's `EmailVerified` is `true`.
- **The user verified the email.** The user's `email_verified` is `true`. A user who signed up with an address and never confirmed it may not own it.
- **The user has no account at the provider.** One that has another subject there keeps it, and the sign-in is refused.

Otherwise the sign-in is refused with `account_not_linked`, as for a provider that is not listed.

Each listed provider can then sign in to every account whose email it reports as verified, so an account is as safe as the weakest listed provider. An address that its domain or mailbox provider gives to somebody else also passes the check for its new holder. List only a provider that verifies the addresses it reports, for the accounts it reports them for.

## Unlink a provider account

`POST /oauth/unlink` with the provider's id removes the user's account at that provider:

```js
await fetch("/api/auth/oauth/unlink", {
  method: "POST", credentials: "include",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ provider: "github" }),
});
```

An unlink follows these rules:

- **It needs a fresh session**, like a link.
- **The last way to sign in stays.** An unlink that would leave the user without a password and without an account at another configured provider gets `409` `last_sign_in_method`. A magic link or a code sent to a phone number signs a user in without such an account, and does not count. Set a password first, for example with a password reset, or link another provider.
- **A provider the user has no account at** gets `409` `provider_not_linked`.
- **It revokes the stored tokens** at the provider, for a provider whose tokens you store. See [Call the provider's API](#call-the-providers-api).
- **It ends no session.** A session the provider signed in stays valid until it expires or the user signs out.

## List the provider accounts

`GET /oauth/accounts` answers with the user's accounts at the configured providers, oldest first:

```json
{"accounts": [{"provider": "google", "created_at": "2026-10-25T09:14:03Z"}]}
```

`created_at` is when the account was linked, or when the user signed up with the provider. A password is not a provider account and is not listed, and neither is an account at a provider you removed from the options: it signs nobody in.

## Call the provider's API

By default, the plugin discards a provider's tokens once the provider has named the user. For a provider in `StoreTokens`, it keeps them in the user's account, and your server gets a valid access token with `AccessToken`:

```go
plugin := oauth.New(oauth.Options{
	Providers: []oauth.ProviderConfig{{
		Kind:         oauth.KindGitHub,
		ClientID:     "Iv1.8a61f9b3a7aba766",
		ClientSecret: crypto.StaticSecretSource{Secrets: map[int]string{1: GITHUB_CLIENT_SECRET}, Current: 1},
		Scopes:       []string{"read:user", "user:email", "repo"},
	}},
	CallbackURL: "https://app.example.com/auth/callback",
	StoreTokens: []string{"github"},
})
```

Replace `GITHUB_CLIENT_SECRET` with the client secret. Then, on your server:

```go
token, err := plugin.AccessToken(ctx, oauth.AccessTokenRequest{Provider: "github"})
if errs.IsCode(err, oauth.ErrorCodeProviderTokenUnavailable) {
	// Ask the user to sign in with GitHub again.
}
if err != nil {
	return err
}
req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/repos", nil)
if err != nil {
	return err
}
req.Header.Set("Authorization", "Bearer "+token.AccessToken)
```

`ctx` names the user: the caller of a request signed in with a session, or `identity.System(name).For(userID)` for a job. See [Callers and permissions](callers.md).

The plugin keeps the tokens as follows:

- **Sealed, in the user's account.** The access token, the refresh token, and the ID token are encrypted in the `accounts` table, with when they expire and the scope the provider granted. No hook payload and no audit event carries them.
- **Replaced at each sign-in and link.** A refresh token the provider does not issue again is kept, with its expiry. A sign-in with a provider that is not in `StoreTokens` removes the tokens its account holds.
- **Refreshed when you ask for one.** `AccessToken` refreshes an access token that expires within a minute, with the refresh token, and stores the new tokens. Calls for one account that find its token expired share one refresh, within one process.
- **Revoked by an unlink.** `Unlink` revokes the tokens at the provider once the account is removed. A revocation that fails is logged, and the unlink stands.
- **On no route.** No route returns a provider's token. It belongs on your server, not in your pages.

`AccessToken` returns these errors:

| When | Category | Code |
| --- | --- | --- |
| no session, or one that is pending, revoked, or expired | session | `session_missing`, `session_pending`, `session_revoked`, `session_expired`, `session_invalid` |
| a provider the plugin doesn't have | validation | `provider_invalid_input` |
| a provider that is not in `StoreTokens` | configuration | |
| the user has no account at the provider | conflict | `provider_not_linked` |
| no usable token: none was stored, or the access token expired and there is no refresh token, the refresh token expired, the provider can't refresh, or the provider refused the refresh token | conflict | `provider_token_unavailable` |
| the provider failed | a failure of the system | |

After `provider_token_unavailable`, the user signs in with the provider again, which stores new tokens. A refresh token the provider refused is removed with the rest of the tokens, so the next call does not ask the provider again.

Each provider issues and revokes tokens its own way:

- **Google** issues a refresh token only to a client that asks for offline access, which a Google provider in `StoreTokens` does, and only when the user consents. A user who granted your application access before its tokens were stored gets no refresh token until they grant it again, for example after an unlink, which revokes the grant.
- **Another OpenID Connect provider** usually issues a refresh token for the scope `offline_access` in `ProviderConfig.Scopes`. It revokes at the `revocation_endpoint` of its discovery document, and a provider that names none revokes nothing.
- **GitHub** gives an OAuth App a token that does not expire, and gives a GitHub App whose user tokens expire a refresh token too. An unlink deletes the application's grant, which needs an access token that is still valid.

## Routes

Paths are below the router's base path, `/api/auth` by default.

| Route | Body | Result |
| --- | --- | --- |
| `POST /sign-in/oauth` (`oauth.PathBegin`) | `provider`, and optionally `redirect_url`. Other fields go to handlers on `auth.oauth.beforeBegin`. | `200` with `{"authorization_url": "..."}`, and `binding` for a client without cookies. |
| `POST /oauth/link` (`oauth.PathLink`) | as for `POST /sign-in/oauth`. Needs a fresh session. | as for `POST /sign-in/oauth` |
| `POST /oauth/complete` (`oauth.PathComplete`) | `provider`, `code`, `state`, `iss` when the provider sent it, and `binding` for a client without cookies. A link also needs the session that started it. | For a sign-in, `200` with `{"user": {...}, "redirect_url": "..."}`, or `{"status": "requires_second_factor", "redirect_url": "..."}` when a second factor is pending. The session token arrives by the configured delivery. See [Sessions](sessions.md#the-sign-in-response). For a link, `200` with `{"status": "linked", "account": {"provider": "...", "created_at": "..."}, "redirect_url": "..."}`, and no session token. |
| `POST /oauth/unlink` (`oauth.PathUnlink`) | `provider`. Needs a fresh session. | `200` with `{"status": "unlinked"}` |
| `GET /oauth/accounts` (`oauth.PathAccounts`) | none. Needs a session. | `200` with `{"accounts": [{"provider": "...", "created_at": "..."}]}` |

Every `200` answer is sent with `Cache-Control: no-store`.

`redirect_url` is checked like the magic link's. It is a path on your own site, or an absolute URL whose origin is in `RouterConfig.TrustedOrigins`. See [The redirect URL](magiclink.md#the-redirect-url). Your page does the redirect.

### Error responses

| Route | When | Status | Code |
| --- | --- | --- | --- |
| all but accounts | the body is not valid JSON | `400` | `request_validation_error` |
| begin, link, unlink | `provider` is missing, not a string, or not a configured provider | `400` | `provider_invalid_input` |
| begin, link | `redirect_url` is not a string, or not allowed | `400` | `redirect_url_invalid_input` |
| link, unlink, accounts, and complete for a link | no session, or a session that is pending, revoked, expired, or unknown | `401` | `session_missing`, `session_pending`, `session_revoked`, `session_expired`, `session_invalid` |
| link, unlink | the session signed in more than `SessionConfig.FreshAge` ago | `401` | `session_not_fresh` |
| link, and complete for a link | the user has an account at the provider already | `409` | `provider_already_linked` |
| complete | `provider`, `code`, or `state` is missing or not a string | `400` | `provider_invalid_input`, `code_invalid_input`, `state_invalid_input` |
| complete | `binding` or `iss` is not a string | `400` | `binding_invalid_input`, `iss_invalid_input` |
| complete | the state is unknown, expired, or already used; the binding value is missing or wrong; the provider is not the one the sign-in was started with; a link is completed in another session than the one that started it; the provider refused the code; the ID token or the `iss` parameter failed a check | `401` | `invalid_oauth_callback` |
| complete for a sign-in | a user with the provider's email exists and has no account at this provider, and the plugin does not link the two | `409` | `account_not_linked` |
| complete for a sign-in | the provider gave no usable email for a first sign-in | `403` | `email_not_provided` |
| complete for a link | the provider account is linked to another user | `409` | `provider_account_taken` |
| unlink | the user has no account at the provider | `409` | `provider_not_linked` |
| unlink | the account is the user's last way to sign in | `409` | `last_sign_in_method` |
| begin, link, complete | too many requests | `429` with `Retry-After` | `rate_limited` |
| all | a hook handler rejected the request with a typed error | the error's status | the error's code |
| begin, link, complete | the provider failed | `500` | |

A refused callback that fails before the code is redeemed leaves the sign-in or the link usable, so a request with a wrong binding value, or from another session, does not end it. From the redeem on, the state is used up, also when a later step refuses the sign-in or the link. The user starts again.

## What a sign-in does

The plugin finds the user by the provider and the identity's `Subject`. The email matters only for a subject that has no account:

| The plugin finds | Result |
| --- | --- |
| an account for the provider and the subject | The account's user is signed in. Nothing of the provider's profile is written to the user. |
| no account, and no user with the identity's email | A user and the account are created, and the user is signed in. |
| no account, and a user with the identity's email | `409` `account_not_linked`. The user signs in the way they did before, and links the provider from there. With the provider in `LinkByVerifiedEmail` and the email verified on both sides, the account is linked to that user instead, who is signed in. See [Link by a verified email](#link-by-a-verified-email). |
| no account, and no usable email | `403` `email_not_provided`. |

A new user is built from the identity:

- **The email is normalized** (trimmed and lowercased), and `email_verified` is the identity's `EmailVerified`. With the email verification plugin installed, an unverified user gets a verification link.
- **The name** is `GivenName` and `FamilyName`, or `Name` as the first name when the provider gives no parts. Each is cut to 255 characters.
- **The picture** is kept only when it is an absolute `http` or `https` URL.
- **The provider's tokens are stored** only for a provider in `StoreTokens`.

## Call the flows from code

The flows are exported for a CLI, a test, or a transport other than HTTP.

```go
begun, err := plugin.Begin(ctx, oauth.BeginRequest{Provider: "google", RedirectURL: "/dashboard"})
// begun.AuthorizationURL, begun.State, begun.Binding, begun.ExpiresAt

signIn, err := plugin.Complete(ctx, oauth.CompleteRequest{
	Provider: "google", Code: code, State: state, Issuer: iss, Binding: begun.Binding,
})
// signIn.User, signIn.Session, signIn.RawToken, signIn.RedirectURL
```

The flows on a user's provider accounts act for the caller on the context, as every self-service flow does. See [Callers and permissions](callers.md).

```go
begun, err := plugin.BeginLink(ctx, oauth.BeginRequest{Provider: "github"})

done, err := plugin.Complete(ctx, oauth.CompleteRequest{
	Provider: "github", Code: code, State: state, Binding: begun.Binding,
})
// done.Link.UserID, done.Link.Provider, done.Link.CreatedAt, done.RedirectURL

accounts, err := plugin.Accounts(ctx) // []oauth.ProviderAccount
token, err := plugin.AccessToken(ctx, oauth.AccessTokenRequest{Provider: "github"})
_, err = plugin.Unlink(ctx, oauth.UnlinkRequest{Provider: "github"})
```

The flows behave as follows:

- **You keep the binding value.** `Begin` and `BeginLink` return it, and `Complete` needs it. Keep it with the client that started the sign-in, and never in a URL. A caller that stores it on its own server, looked up by the state, removes the protection it gives.
- **`Complete` delivers nothing.** For a sign-in it returns the raw session token. From a route of your own, hand `signIn.SignInResult` to `httpapi.WriteSignIn`, which is what the plugin's route does.
- **`Complete` returns a sign-in or a link.** For a state of `BeginLink`, `Link` is set and the embedded `SignInResult` is nil. Check which one you got before you read the user.
- **A link needs a session on the context.** `BeginLink`, and `Complete` for a link, read it from the caller. A system caller acting for a user (`identity.System(name).For(userID)`) has none, and `BeginLink` refuses it with `session_missing`. `Unlink`, `Accounts`, and `AccessToken` work for it.
- **Pass the context of the request being handled** when there is one, so the session records the client's address and handlers get the request.
- **Called before `Boot`,** every flow returns a configuration error.

## Hooks

`Complete` fires the core sign-in points for a sign-in, and the sign-up points for a user it creates:

| Point | What the handler gets |
| --- | --- |
| `auth.signIn.before` | payload `method` (`"oauth"`) and `provider`. Veto only. The code, the state, and the binding value are not in the payload. |
| `auth.signIn.credentialsVerified` | `HookValueUserID`, and `HookValueFactor` with the value `oauth`. Set `HookValueRequireStepUp` to make the session pending. |
| `auth.signIn.after` | a `*core.SignInResult` with `Method` set to `"oauth"` |
| `auth.signIn.failed` | codes `invalid_oauth_state`, `oauth_provider_mismatch`, `oauth_binding_mismatch`, `oauth_exchange_refused`, `user_not_found`, `email_not_provided`, `account_not_linked`, `rejected_by_hook`. The metadata has the provider. |
| `auth.signUp.before` | payload `method`, `provider`, `email`, `email_verified`, `firstname`, `lastname`, and `image_url`. Veto only. |
| `auth.signUp.after` | the created `*models.User` |
| `auth.signUp.failed` | code `rejected_by_hook` |

A refused sign-up fires `auth.signUp.failed` and then `auth.signIn.failed`. A link by a verified email that a handler refuses fires `auth.oauth.linkFailed` and then `auth.signIn.failed`.

`hctx.Values[oauth.HookValueProvider]` holds the provider's id on every one of these points. When the subject is new and the identity has a usable email, `hctx.Values[hooks.HookValueEmail]` holds that email from the redeem on.

The plugin also declares nine points of its own. Three are around starting a sign-in or a link:

| Point | Phase | What the handler gets |
| --- | --- | --- |
| `auth.oauth.beforeBegin` (`oauth.HookBeginBefore`) | before | payload `provider`, `redirect_url`, `intent` (`sign_in` or `link`), and the request's other fields. The returned payload becomes the input, except for `intent`, which is the flow's. |
| `auth.oauth.afterBegin` (`oauth.HookBeginAfter`) | after | a `*oauth.BeginResult` with the provider, the intent, the sign-in's id, and its expiry. Not the authorization URL, the state, or the binding value. |
| `auth.oauth.beginFailed` (`oauth.HookBeginFailed`) | failed | code `rejected_by_hook` |

None of the three is audited. For a link, `hctx.Values[hooks.HookValueUserID]` holds the user.

Three are around a link, and three around an unlink. Their after and failed points are audited, with the user as the subject:

| Point | Phase | What the handler gets |
| --- | --- | --- |
| `auth.oauth.beforeLink` (`oauth.HookLinkBefore`) | before | payload `userID`, `provider`, and `by`: `user` for a link the user started, `verified_email` for a link a sign-in makes (see [Link by a verified email](#link-by-a-verified-email)). Veto only. For a link the user started, it fires before the callback is checked. |
| `auth.oauth.afterLink` (`oauth.HookLinkAfter`) | after | a `*oauth.LinkResult`: the user, the provider, when the account was created, and `By` |
| `auth.oauth.linkFailed` (`oauth.HookLinkFailed`) | failed | codes `invalid_oauth_state`, `oauth_provider_mismatch`, `oauth_binding_mismatch`, `oauth_session_mismatch`, `oauth_exchange_refused`, `provider_already_linked`, `provider_account_taken`, `rejected_by_hook`. The metadata has the provider. |
| `auth.oauth.beforeUnlink` (`oauth.HookUnlinkBefore`) | before | payload `userID` and `provider`. Veto only. |
| `auth.oauth.afterUnlink` (`oauth.HookUnlinkAfter`) | after | a `*oauth.UnlinkResult`: the user and the provider |
| `auth.oauth.unlinkFailed` (`oauth.HookUnlinkFailed`) | failed | codes `provider_not_linked`, `last_sign_in_method`, `rejected_by_hook`. The metadata has the provider. |

These points behave as follows:

- **`hctx.Values` holds the user and the provider:** `hooks.HookValueUserID` and `oauth.HookValueProvider`. A link that a sign-in makes runs inside the sign-in, and its points also see the sign-in's values.
- **A state that is unknown or expired can't be told from a sign-in's.** Its refusal fires `auth.signIn.failed` with `invalid_oauth_state`, also when it was a link's.
- **The audit event of an after point names the user, not the provider.** A failure's event has the provider in its metadata.

Starting and completing a sign-in or a link also fire the token manager's points (`token.beforeIssue`, `token.afterIssue`, `token.consumed`, `token.failed`) with the kind `oauth_state`.

## Permissions

The flows on a user's provider accounts require these self-service permissions. A user signed in with a session holds all three. See [Callers and permissions](callers.md).

| Permission | Required by |
| --- | --- |
| `oauth_account:read` (`oauth.PermissionRead`) | `Accounts`, `GET /oauth/accounts` |
| `oauth_account:manage` (`oauth.PermissionManage`) | `BeginLink` and `Unlink`, with a fresh session, and `Complete` for a link |
| `oauth_token:read` (`oauth.PermissionAccessToken`) | `AccessToken` |

## Rate limits

The plugin declares one rule.

| Rule | Limits | Default |
| --- | --- | --- |
| `oauth.RuleBeginAddress` | sign-ins and links started per client address | 10 a minute |

Completing a sign-in is a sign-in, which core limits to 10 attempts a minute per client address and sign-in method (`engine.RuleSignInAddress`), and the sign-up of a new user is limited the same way (`engine.RuleSignUpAddress`). Both count the `oauth` method apart from the others. The rules are on the flows' before points, so they hold for `Begin`, `BeginLink`, and `Complete` called from your own code too. Change a limit, or turn a rule off, by its name with `RateLimitConfig`; see [Hooks](hooks.md#change-a-rule-by-its-name).

Completing a link, unlinking, listing, and `AccessToken` have no rule of their own. Each acts for a user who is signed in, or for a user your own code names, and a link completes at most once for each link started.

## Limits

The plugin has these limits:

- **Three kinds built in.** Another provider is a `KindOIDC` one when it follows OpenID Connect, or a `Provider` you write. A Microsoft endpoint for several tenants names a placeholder as its issuer and does not work as `KindOIDC`. Use the endpoint of one tenant.
- **One account per provider.** A user can't link two accounts at the same provider.
- **Unlink counts accounts only.** It keeps a password, or an account at another configured provider. A magic link or a code sent to a phone number signs a user in without either, so a user who signs in that way still can't unlink their last provider account.
- **Two unlinks at once.** On databases other than SQLite, two unlinks of one user at the same moment can each remove one of the user's last two accounts.
- **One refresh per process.** Calls for one account share a refresh within a process, not across replicas. Two replicas can refresh one account's token at the same moment. A provider that rotates refresh tokens then refuses one of the two, and the caller gets the token the other stored, unless the provider also revokes the tokens of a refresh token used twice.
- **Renewing the tokens takes a sign-in.** A user whose tokens can't be used any more signs in with the provider again, which also starts a new session. A link of a provider the user has an account at is refused.
- **Deleting a user revokes nothing.** Its accounts go, with their tokens, and the grant stays at the provider.
- **No form posts.** A provider that posts the callback (`response_mode=form_post`) posts it to your application. Your endpoint has to accept the form and hand the code and the state to your page.
