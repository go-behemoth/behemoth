# OAuth sign-in design

This document describes the design for signing a user in through an OAuth 2.0 or OpenID Connect provider. It covers the components, the threats and their controls, how a provider account is linked to a user, and the decisions taken so far.

**Status:** built, as of 2026-10-26. Every step is done. The package is `plugins/oauth`, and [`../plugins/oauth.md`](../plugins/oauth.md) describes what is built. [Implementation steps](#implementation-steps) lists the work, and [`../../api/oauth.md`](../../api/oauth.md) describes the result for an application.

The documents it builds on are:

- [`../plugins/magiclink.md`](../plugins/magiclink.md): the closest existing plugin. It issues a single-use token, fires core's sign-in points, and takes a redirect target from a request.
- [`../models/models.md`](../models/models.md): the `accounts` table and its sealed token columns.
- [`../plugins/oauth.md`](../plugins/oauth.md): how the built part of the plugin works.
- [`../http/origin_check.md`](../http/origin_check.md): the origin check and the refusal of form bodies, which cover the plugin's routes.
- [`../guidelines/http_contract.md`](../guidelines/http_contract.md), [`../guidelines/transport_neutral_flows.md`](../guidelines/transport_neutral_flows.md), and [`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md): the guidelines for routes, flows, and options.

## Terms

| Term | Meaning |
| --- | --- |
| Provider | The external service that authenticates the user, such as Google, GitHub, or an OpenID Connect issuer. |
| Subject | The provider's stable id for the user. It is stored in `accounts.account_id`. |
| Provider account | The row in `accounts` for one `(provider_id, account_id)`. It ties a subject to a user. |
| Callback page | The page of the application that the provider sends the browser back to. It posts the authorization code and the state to Behemoth. |
| Authorization code | The short-lived code the provider puts in the callback page's URL. Behemoth exchanges it with the provider for tokens. |
| State | A random value that Behemoth creates when a sign-in starts. The provider returns it with the authorization code. |
| PKCE | Proof Key for Code Exchange. Behemoth keeps a secret verifier and sends its hash with the authorization request, so only Behemoth can redeem the code. |

## Context

The earlier provider code is removed: the `providers` package, `behemoth.Provider`, and `behemoth.UserInfo`. Nothing called it. It had no flow, route, state, PKCE, or ID token check, and its profile type carried the provider's tokens.

Core already has the pieces a sign-in plugin needs:

- **Accounts.** `accounts` is unique on `(provider_id, account_id)`. The store seals `access_token`, `refresh_token`, and `id_token` (`store/seal.go`).
- **Single-use tokens.** `TokenManager.Consume` checks and invalidates a token in one step.
- **Redirect checks.** `core.TrustedRedirect` accepts a path on the application's site, or a URL whose origin is in `RouterConfig.TrustedOrigins`.
- **One sign-in answer.** `httpapi.WriteSignIn` answers every sign-in route, and delivers the session token by whichever delivery is configured.
- **Sign-in points.** Core declares `auth.signIn.*` and `auth.signUp.*`, and `SignInResult.Method` names the plugin that signed the user in.

`core.TokenKindOAuthState` exists as a constant only. No plugin declares the kind yet.

## Scope of the first version

The first version includes:

- sign-in and sign-up through a generic OpenID Connect provider, Google, and GitHub
- every token delivery, because the sign-in is answered through `httpapi.WriteSignIn`
- explicit linking of a provider account to an existing user

The first version leaves out:

- `Deferred:` Apple. It posts the callback, so the application needs an endpoint that accepts a form post and hands the code and the state to its page.

Each of these choices has an entry under [Design decisions](#design-decisions).

## Key components

### Provider definition

`Provider` is the interface the flows call, and it is exported. It has an id and two operations:

| Method | What it does |
| --- | --- |
| `AuthorizationURL` | Returns the URL the browser is sent to for one transaction. It receives the state, the redirect URI, the PKCE challenge, and the nonce. |
| `Exchange` | Redeems the authorization code and returns an `Identity`. It receives the code, the redirect URI, the PKCE verifier, and the nonce. |

An `Identity` holds the subject, the email, whether the provider verified the email, the name, and the picture. It holds no tokens.

The built-in providers share one implementation of the protocol: the code exchange with PKCE, the nonce, and the checks on an ID token. A definition adds to it:

| Provider | Protocol | What the definition needs |
| --- | --- | --- |
| Generic OpenID Connect | OpenID Connect | Data only: an id, a client id, scopes, and an issuer. The issuer's discovery document gives the endpoints and keys, and the ID token gives the identity. A configuration file can declare it. |
| Google | OpenID Connect | The same path as the generic provider. |
| GitHub | OAuth 2.0 without an ID token | Code that calls GitHub's profile and email endpoints. |

An application can also pass a `Provider` of its own. That provider is then responsible for its side of the protocol: the flows hand it the challenge, the verifier, and the nonce, and can't check that it uses them. [Integration with goth](#integration-with-goth) describes one such case.

Two rules apply to every built-in definition:

- The client secret is not a field of the options. The plugin takes it as a value of its own or through a secret source, as "A secret is not an option" in `service_ready_components.md` asks.
- `golang.org/x/oauth2` types stay out of the interface. The root module no longer requires that module.

### Authorization transaction

An authorization transaction is the record of one sign-in in progress, from `Begin` to `Complete`. It holds:

- the state
- the PKCE verifier
- the nonce, for an OpenID Connect provider
- the provider's id
- the intent: sign in, or link, and for a link the user and the session
- the redirect target, already checked with `core.TrustedRedirect`

Behemoth stores it as a token of kind `oauth_state`: single use, with a lifetime of about ten minutes.

The verifier is a secret for as long as the transaction lives. It is sealed, and not stored as plain token metadata.

### `Begin` and `Complete`

The plugin has two flows:

| Flow | Takes | Returns |
| --- | --- | --- |
| `Begin` | the provider, the intent, and the redirect target | the provider's authorization URL and a binding value |
| `Complete` | the provider, the authorization code, the state, and the binding value | a `core.SignInResult` and the redirect target |

Both follow `transport_neutral_flows.md`. They take a context and typed input, and they set no cookie and no header.

Each flow has one route, and both routes are JSON calls. The route of `Complete` answers through `httpapi.WriteSignIn`, with the redirect target as an extra key, as the magic link plugin's verify route does.

The binding value ties the two calls to one client. How the client keeps it depends on the token delivery:

- **Under the cookie deliveries,** the route of `Begin` sets it in a cookie, and the route of `Complete` reads it from there.
- **A client without cookies** gets the value in the answer of `Begin`, keeps it, and sends it with `Complete`.

`Complete` fires core's `auth.signIn.*` points, as the magic link plugin does. A hook handler and the audit log then see one kind of sign-in, whichever plugin made it. The points include `auth.signIn.credentialsVerified`, so a second-factor plugin can still make the session pending.

### Callback page

The provider sends the browser back to a page of the application, not to a route of Behemoth. The page reads the authorization code and the state from its URL and posts them to `Complete`. The magic link plugin works the same way: its link opens a page of the application, which posts the token.

The callback follows these rules:

- **The redirect URI is an option of the plugin.** It names the application's page, as `LinkURL` does for the magic link plugin. `Init` checks it, and the same value is registered at the provider.
- **The session arrives like any sign-in.** The session token travels by whichever delivery is configured. No route of Behemoth redirects a browser.
- **A failure is a typed error.** The route returns the flow's error, and the router answers with a status and an exported code, as for every other route.
- **The code is in the page's URL.** It stays there until the page replaces the URL. PKCE makes a leaked code useless without the verifier, which never leaves Behemoth.

### Callers that are not a browser

The two flows are plain request and response calls. A gRPC wrapper is two unary methods, and it needs no HTTP route of Behemoth. Two things stay outside such a wrapper:

- **Something that speaks HTTP still receives the provider's redirect.** The provider sends a browser to a URL. That is the application's page or backend, which then calls `Complete`.
- **The caller keeps the binding value.** Behemoth can only compare it with the state. It has to live in the user's browser or client. A caller that stores it on its own server, keyed by the state, loses the protection against a forged callback.

### Identity resolution

Identity resolution decides what an `Identity` means for the `users` and `accounts` tables. Most of the risk is here.

| What `Complete` finds | Outcome |
| --- | --- |
| A provider account for `(provider, subject)` | Sign in its user. |
| No provider account, and no user with the email | Create the user and the provider account in one transaction. |
| No provider account, and a user with the email | Refuse with `account_not_linked`. See [Account linking](#account-linking). |
| No provider account, and no usable email from the provider | Refuse with `email_not_provided`. |
| The intent is link | Attach the provider account to the signed-in user. |

A new user's `email_verified` is set only from the provider's own verified flag.

### Token custody

By default, the plugin discards the provider's tokens once it has the identity.

With storage turned on, the tokens go into the sealed columns of `accounts`, and the plugin has three more jobs:

- refresh an access token that expired
- hand the application a valid access token through a flow
- revoke the tokens at the provider when the account is unlinked

[`../plugins/oauth.md`](../plugins/oauth.md#provider-tokens) describes how step 6 built them. The option lists provider ids, the access token is refreshed when the flow asks for it, the flow has no route, and an unlink revokes the tokens once the account is removed.

### Account routes

Three routes let a signed-in user manage provider accounts: link, unlink, and list. Each requires a session. Unlink refuses to remove the last way the user can sign in.

[`../plugins/oauth.md`](../plugins/oauth.md#linking) describes how step 5 built them. A link starts at its own route and completes through `Complete`, like a sign-in. A user has one account per provider, and unlink counts a password and the accounts at the configured providers as ways to sign in.

## Sign-in sequence

A sign-in runs in these steps:

1. The application's page calls `Begin`. Behemoth stores the authorization transaction, and returns the provider's authorization URL and the binding value.
2. The page sends the browser to the provider, and the user approves.
3. The provider redirects the browser to the callback page, with the authorization code and the state in the URL.
4. The callback page posts the code and the state to `Complete`, together with the binding value.
5. `Complete` compares the binding value with the state and consumes the state. It then exchanges the code with the provider, server to server, and resolves the identity.
6. The route answers through `WriteSignIn`, and the page goes on to the redirect target.

Each value in the sequence has its own path:

| Value | Path | Who can see it |
| --- | --- | --- |
| Authorization code and state | from the provider to the callback page, in the URL, then to `Complete`, in the body | the browser and the page's scripts |
| PKCE verifier | none. It stays in the authorization transaction. | Behemoth |
| Binding value | from `Begin` to the client, and back with `Complete` | the client that called `Begin` |
| Session token | from `Complete` to the client, by the configured delivery | the client, as after any sign-in |

The session token never appears in a URL.

## Security

### Rules the design depends on

Three rules shape the design. Each one states what breaks without it.

**The state is bound to the client that started the sign-in.** A check that the state exists in the database is not enough. An attacker can start a sign-in, and then send a victim's browser to the callback page with the attacker's own state and authorization code. The page posts them, and the victim is signed in as the attacker. What the victim enters then lands in the attacker's account. `Complete` compares the state with the binding value, which only the client that called `Begin` holds. Under the cookie deliveries, the binding value is in a cookie whose name starts with `__Host-`, which a browser accepts only from the exact host over HTTPS.

**A user is found by `(provider, subject)`, never by email.** An email from a provider is a claim. It can change, and the provider may not have verified it. A lookup by email turns every enabled provider into a way into every account. [Account linking](#account-linking) has the conditions under which an email may link two accounts.

**A link carries the session.** The state of a link records the user and the session, and `Complete` checks that the request carries the same session. Without the check, a forged callback attaches the attacker's provider account to the victim's user. The attacker then signs in as the victim through the provider.

### Threats and controls

| Threat | Control |
| --- | --- |
| A forged callback signs the victim in as the attacker | Bind the state to the client with the binding value, a `__Host-` cookie under the cookie deliveries. The state is single use and short-lived. |
| Account takeover through a matching email | Find the user by `(provider, subject)`. Link explicitly by default. Auto-link only when both emails are verified. |
| A forged callback links the attacker's provider account | Record the intent, the user, and the session in the state. `Complete` requires the same session. |
| A forged request from another site to `Begin` or `Complete` | Both routes take a JSON `POST`. Under the cookie deliveries, the origin check and the refusal of form content types cover them. |
| Code interception or injection | The code passes through the callback page's URL. `Begin` creates a PKCE verifier and a nonce for every transaction. Every built-in provider sends the S256 challenge, and an OpenID Connect provider checks the nonce. |
| Provider mix-up | Use one redirect URI per provider, record the provider in the state, and check the `iss` parameter when the provider sends it. |
| Forged or replayed ID token | Verify the signature against the provider's keys with an algorithm allowlist. Then check `iss`, `aud`, `exp`, and `nonce`. |
| Callback replay | Consume the state before the code exchange, as the magic link plugin consumes its token first. |
| Redirect URI tampering | The redirect URI is a fixed option that `Init` checks. It is never built from a request. |
| Open redirect, or a token leak through a URL | Check the target with `TrustedRedirect` at `Begin` and store it in the transaction. The session token never appears in a URL, and `WriteSignIn` marks its answer `Cache-Control: no-store`. |
| Second factor bypass | Fire `auth.signIn.credentialsVerified`, so a step-up still applies. |
| Stored token theft | Store no provider tokens by default. When they are stored, seal them, ask for minimal scopes, and keep them out of hook payloads and audit events. |
| A slow or hostile provider endpoint | Use HTTPS only, set timeouts, and cap the response size. The discovered issuer must equal the configured one. |
| Flooding the state store | Limit `Begin` per client address. It needs no session and writes a row for each call. |
| Locking a user out | Refuse to unlink the last way to sign in. |

## Account linking

Linking attaches a provider account to a user who already exists.

### What auto-link assumes

With auto-link, `Complete` finds no provider account for `(provider, subject)`, finds a user with the same email, attaches the provider account to that user, and signs in.

Auto-link treats "same email" as proof of "same person". That proof is only as strong as the weaker of the two email checks.

### The two takeovers

Each side of the match can be the unverified one:

| Unverified side | Attack |
| --- | --- |
| The provider's email | The attacker creates an account at a provider that lets them claim the victim's address without proving it, and signs in. Auto-link attaches them to the victim's user. Microsoft's `email` claim, a self-hosted issuer, and a GitHub address the user has not verified are real cases. |
| The local email | The attacker signs up first with the victim's address and a password. Later the victim signs in with a provider, and auto-link attaches the victim's provider account to the attacker's user. Both can sign in, and the attacker reads what the victim stores. |

A verified email on both sides closes both attacks.

### What remains with both emails verified

Two smaller risks remain:

- **Every auto-linked provider is a way into any account with that email.** An account is then as secure as the weakest provider the application enabled.
- **Addresses get reassigned.** A lapsed company domain or a recycled mailbox passes the provider's check for the new holder.

With both sides verified, auto-link is about as safe as password reset by email, which already trusts the mailbox.

### How explicit linking works

Explicit linking asks the user to prove control of the existing account:

1. `Complete` finds no provider account and a user with the email. It refuses with `account_not_linked`.
2. The user signs in the way they signed in before.
3. The user links the provider from a signed-in page. The state carries the session, as the third rule in [Security](#security) requires.

The cost is the extra step. A user who signed up with a password and later chooses a provider gets a refusal first.

## Design decisions

### Linking is explicit by default

**Context:** A user who signed up with a password later signs in with a provider that reports the same email. `Complete` has to decide whether that is the same person.

**Options considered:**

- *Auto-link on a matching email.* No extra step for the user. An unverified email on either side is an account takeover.
- *Auto-link when both emails are verified.* Closes both takeovers. Every enabled provider can then open any account with a matching address, and a reassigned address passes the check.
- *Explicit linking only.* The user proves control of the existing account by signing in to it. A user who forgot how they signed up gets a refusal first, and an application that trusts its provider has no way to skip the step.
- *Explicit by default, with auto-link as an option per provider.* The application decides which providers it trusts that far. The option needs a clear description of what it trades away.

**Decision:** explicit by default. Auto-link is an option per provider, off unless the application turns it on, and it requires a verified email on both sides.

**Revisit if:** applications report that the `account_not_linked` refusal turns users away, and most of them turn auto-link on for the same providers.

### Provider tokens are not stored by default

**Context:** `accounts` has sealed columns for an access token, a refresh token, and an ID token. Signing in needs only the identity. An application that calls the provider's API on the user's behalf needs the tokens later.

**Options considered:**

- *Always store them.* The application can call the provider at any time. Every application then holds credentials for its users' provider accounts, including those that only wanted sign-in.
- *Never store them.* Nothing to steal, refresh, or revoke. An application that needs the provider's API has to build its own storage around the plugin.
- *An option, off by default.* An application that only wants sign-in holds nothing worth stealing. One that needs the tokens opts in, and takes on refresh and revocation with it.

**Decision:** an option, off by default.

**Revisit if:** most applications turn the option on. The cost of the default is that tokens of earlier sign-ins are gone: users have to sign in again before the application can call the provider for them.

### The provider redirects to a page of the application

**Context:** The provider sends the browser to a redirect URI with the authorization code. Something has to receive that request and finish the sign-in. `http_contract.md` says that every flow is a JSON call, that no route redirects a browser, and that a sign-in route answers through `WriteSignIn`.

**Options considered:**

- *A callback route of Behemoth that redirects.* The application builds no page. The route is a `GET` that signs the user in, and it can deliver a session only by cookie. The header and body deliveries then need a single-use handoff code that the client redeems, with a verifier of its own. A failure reaches the application as a query parameter and not as a typed error. A wrapper for another transport still depends on this HTTP route.
- *A page of the application that posts the code and the state.* The route answers through `WriteSignIn` under every delivery, a failure is a typed error, and a gRPC wrapper can expose both flows. Each application builds the page, and the redirect URI becomes an option. The code passes through the page's scripts, and a provider that posts the callback reaches the application and not Behemoth.

**Decision:** the page of the application. An earlier plan to support only the cookie delivery in the first version is dropped with it. That limit came from the handoff code, which this design does not need.

**Revisit if:** applications without a front end of their own need the flow. A callback route that redirects can then be added as a second HTTP wrapper over the same `Complete`.

### A provider that returns no email is refused

**Context:** Some providers return no email for some users. A GitHub user can keep every address private, for example. `users.email` is unique and not nullable, and core writes an empty email as `""`, so the unique index allows one such user at most. The magic link, email verification, and password reset flows all assume an address.

**Options considered:**

- *Refuse the sign-in with a typed error.* No change to core. A user without a public address at the provider can't sign in until they share one.
- *Create a user without an email.* Nobody is turned away. Core needs a nullable email and a rule for every flow that sends mail.

**Decision:** refuse, with an error code of its own.

**Revisit if:** core supports a user without an email. "No sign-up with a phone number alone" in `docs/ongoing.md` raises the same need from the phone number plugin.

### The first providers are generic OpenID Connect, Google, and GitHub

**Context:** The provider interface has to cover two kinds of provider: one that returns an ID token, and one that needs a profile call.

**Options considered:**

- *The generic OpenID Connect provider alone.* Data only, and any issuer that follows the standard works. GitHub does not issue ID tokens, so the interface for a provider with code stays untested.
- *Presets for many providers.* More applications are covered on the first day. Each preset is code to maintain against a provider's changes.
- *The generic provider, Google, and GitHub.* One provider per kind, plus the generic one. Other providers wait.

**Decision:** the generic provider, Google, and GitHub. Apple is deferred: it posts the callback, so the application needs an endpoint that accepts a form post. Facebook, which the removed code supported, is not in the first set.

**Revisit if:** applications ask for a provider that neither the generic provider nor the two presets cover.

## Implementation steps

The plugin is built in this order. Each step ends with its tests and its docs.

| Step | Content | Status |
| --- | --- | --- |
| 1 | The `Provider` contract, the plugin with its options and its checks at `Init`, and `Begin` with the authorization transaction | Done |
| 2 | `Complete` for a sign-in: the binding check, the exchange, identity resolution, and the session | Done |
| 3 | The two routes, their rate-limit rules, and the delivery of the binding value | Done |
| 4 | The built-in providers: the shared protocol implementation, generic OpenID Connect, Google, and GitHub | Done |
| 5 | Linking: the link intent in `Begin` and `Complete`, the option that links by a verified email, and the link, unlink, and list routes | Done |
| 6 | Stored provider tokens: the option, refresh, a flow that returns a valid access token, and revocation | Done |
| 7 | The example application, the README, and the API page's move out of experimental. The API page exists since step 3. | Done |

## Related entries

These entries in `docs/ongoing.md` affect the design:

- **"No route can accept a form body under the cookie deliveries".** The entry names an OAuth callback as a route that needs the exemption. With the callback on a page of the application, the plugin has no such route. A provider's form post goes to the application.
- **"Nothing removes old sessions, tokens, or rate-limit counters".** Every `Begin` writes a state row, and nothing purges them. The key-value backend suits the `oauth_state` kind, because an entry there expires by itself.

## Integration with goth

`Deferred:` no step above includes it. This section records what an adapter for the providers of [goth](https://github.com/markbates/goth) needs and what it can't offer. The facts were checked against goth v1.82.0, which has 74 provider packages.

An application that uses goth has a list of configured `goth.Provider` values. An adapter lets it keep that list and sign its users in through Behemoth. The adapter is a module of its own, so the root module does not gain goth's dependencies. No decision in this document changes for it.

### How an adapter works

The adapter wraps one `goth.Provider` as a `Provider` of this plugin. It uses goth's providers only. The `gothic` package, with its HTTP handlers and its cookie store, is replaced by the plugin's routes.

| `Provider` method | goth calls |
| --- | --- |
| `AuthorizationURL` | `BeginAuth(state)`, then `Session.GetAuthURL()` |
| `Exchange` | `Session.Authorize` with the code, then `FetchUser`. `goth.User.UserID` becomes the subject. |

The application builds each goth provider with the redirect URI of that provider as its callback URL.

### What stays the same

Everything outside the provider is the plugin's: the state and the binding value, identity resolution by `(provider, subject)`, explicit linking, the refusal of an identity without an email, sessions, hook points, and rate limits.

An application that moves from goth keeps its users. goth's provider name and `UserID` are the pair that `accounts` stores as `provider_id` and `account_id`.

### What an adapted provider can't offer

| Limit | Consequence |
| --- | --- |
| Only three of the 74 providers (`openidConnect`, `zoom`, and `fitbit`) forward a PKCE verifier. | For the others, a leaked authorization code is not protected by PKCE. The code sits in the callback page's URL, so this matters. |
| `goth.User` has no field for a verified email. | The email counts as unverified. A new user is unverified, and auto-link can't be turned on for the provider, unless the application supplies a function that reads `goth.User.RawData`. |
| goth's methods take no `context.Context`, and a provider without a client of its own uses `http.DefaultClient`. | A call to the provider has no timeout and can't be cancelled, unless the application sets a client on the goth provider. |
| The checks on a token are goth's own. | Its `openidConnect` provider validates the claims of an ID token. No signature check was found in it. |
| Four OAuth 1.0a providers (`twitter`, `twitterv2`, `tumblr`, and `xero`) keep a request token in goth's session between the two calls. | They need a place in the authorization transaction for a provider's own data, which the design does not have. The adapter leaves them out. |
| A goth provider is a Go value that holds the client secret. | The adapter works in library mode only. A configuration file can't declare such a provider. |
| goth can refresh a token and has no call to revoke one. | With stored provider tokens, refresh works and revocation at the provider does not. |
