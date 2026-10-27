# OAuth plugin

This document explains how `plugins/oauth` works: the provider contract, the options and their checks, `Begin` and `BeginLink`, the flows that start a sign-in and a link, `Complete`, the flow that finishes both, `Unlink` and `Accounts`, the stored provider tokens and `AccessToken`, the routes, and the three built-in providers.

[`../plans/oauth.md`](../plans/oauth.md) has the whole design: the components, the threats and their controls, the decisions, and the steps that remain. Read it first. This document covers how the built part works and the choices made while building it.

The code lives in:

- `plugins/oauth/oauth.go`: the plugin, its options, its declarations, and `Init`
- `plugins/oauth/provider.go`: `Provider`, `Identity`, `Tokens`, the request types, and the optional `Refresher` and `Revoker`
- `plugins/oauth/begin.go`: `Begin` and `BeginLink`
- `plugins/oauth/complete.go`: `Complete`, the checks and the code exchange a sign-in and a link share, the lookup of the user, and the sign-up of a new one
- `plugins/oauth/link.go`: the completion of a link, the link by a verified email, `Unlink`, and `Accounts`
- `plugins/oauth/tokens.go`: `AccessToken`, the refresh, and how an account's tokens are written, kept, and revoked
- `plugins/oauth/routes.go`: the routes and the binding cookie
- `plugins/oauth/builtin.go`: `ProviderConfig`, the kinds, and the checks of a configuration
- `plugins/oauth/protocol.go`: what the built-in providers share: the HTTP client, the code exchange, the refresh, the revocation, client authentication
- `plugins/oauth/oidc.go`: the OpenID Connect provider: discovery, the key set, the ID token
- `plugins/oauth/github.go`: the GitHub provider
- `httpapi/token_carrier.go`: `TokenCarrier.Config`, which the routes read through `RequestContext.TokenCarrier`
- `store/seal.go`: `Store.Seal` and `Store.Open`, which `Begin` uses for the PKCE verifier

`examples/init` wires the plugin from its environment (`oauth.go`), serves a page that signs in, links, lists, and unlinks, and the callback page (`pages.go`), and shows `AccessToken` with its `oauth-token` command.

## What the plugin contributes

| `core.Plugin` method | Result |
| --- | --- |
| `Meta` | name `oauth`, no dependencies, no mount path |
| `Declare` | the `oauth_state` token kind, three permissions, nine hook points, and one rate-limit rule |
| `Register` | nothing |
| `Middlewares` | nothing |
| `Init` | checks `Options`, indexes the providers by id, and wraps `Complete`, its sign-up, both ways to link, and `Unlink` with their points |
| `Routes` | `POST /sign-in/oauth`, `POST /oauth/link`, `POST /oauth/complete`, `POST /oauth/unlink`, `GET /oauth/accounts` |

| Declared | Value |
| --- | --- |
| token kind | `core.TokenKindOAuthState`, single use, `Options.StateTTL` (10 minutes by default), database backend |
| permissions | `oauth_account:read` (self) for `Accounts`, `oauth_account:manage` (self) for `BeginLink`, `Unlink`, and the completion of a link, `oauth_token:read` (self) for `AccessToken` |
| hook points around a start | `auth.oauth.beforeBegin` (before), `.afterBegin` (after), `.beginFailed` (failed). None is audited. `Begin` and `BeginLink` fire them. |
| hook points around a link | `auth.oauth.beforeLink` (before), `.afterLink` (after, audited), `.linkFailed` (failed, audited) |
| hook points around an unlink | `auth.oauth.beforeUnlink` (before), `.afterUnlink` (after, audited), `.unlinkFailed` (failed, audited) |
| hook rule per address | `oauth.begin.address` on `auth.oauth.beforeBegin`: 10 a minute per client address (`core.KeyByClientIP`), for a sign-in and a link started. A completion of a sign-in is a sign-in, which core's `core.signin.address` counts per address and method; `Complete` publishes `HookValueMethod` for it, and the sign-up of a new user inherits it |

For a sign-in, `Complete` fires core's `auth.signIn.*` points, and `auth.signUp.*` for a user it creates. For a link, it fires the plugin's `auth.oauth.*Link` points instead.

Compare with the magic link plugin, which audits its request points. A link request is about an account. `Begin` needs no session and names no account, so an audit event for each call would record clicks on a button. A start of a link names an account, and is not audited either: it changes nothing, and its completion is audited. The link and unlink points are audited because each changes how a user signs in.

## The provider contract

`Provider` is the interface the flows call. It is exported, so an application can pass an implementation of its own.

| Method | Called by | Receives | Returns |
| --- | --- | --- | --- |
| `ID` | `Init`, `Begin` | | the provider's id |
| `AuthorizationURL` | `Begin` | the state, the redirect URI, the S256 challenge, the nonce | the URL to send the browser to |
| `Exchange` | `Complete` | the code, the redirect URI, the verifier, the nonce | an `ExchangeResult` with the `Identity` and the `Tokens` |

Two more interfaces are optional. The plugin asks a provider for them with a type assertion:

| Interface | Method | Called by | Receives | Returns |
| --- | --- | --- | --- | --- |
| `Refresher` | `Refresh` | `AccessToken` | the refresh token | the new `Tokens` |
| `Revoker` | `Revoke` | `Unlink` | the tokens the account held | an error |

- **The id is stored.** It is the `provider_id` of the accounts the provider signs in, and the last segment of its redirect URI. Changing it separates users from their accounts.
- **The plugin can't check what a provider does with the challenge and the nonce.** A provider that leaves the challenge out of its URL has no PKCE, and nothing reports it. The built-in providers use both. An application's own provider is responsible for itself.
- **A provider tells a refusal from an outage.** `Exchange` wraps `ErrExchangeRefused` when the code was refused or its own checks failed. `Complete` then refuses the sign-in. `Refresh` wraps it for a refresh token the provider refused. Any other error is a failure of the system.
- **`ExchangeResult` grew a field, and the interface no method.** The tokens joined the result as `Tokens`, and refresh and revocation are interfaces of their own, so a provider written before them still compiles. Its `Exchange` gives no tokens, so the plugin stores none and has nothing to refresh or revoke.

## What `Init` checks

Each failure is a configuration error from `Boot`.

| Check | Why |
| --- | --- |
| `StateTTL` is not negative | a token that expired before it was issued |
| `CallbackURL` is an absolute `http` or `https` URL without credentials, a query, or a fragment | the provider's id is appended as a path segment, and a provider refuses a redirect URI with a fragment |
| at least one provider, and none is nil | the plugin does nothing without one |
| an id is 1 to 64 characters of `a-z`, `0-9`, `-`, and `_`, and starts with a letter or a digit | it is a path segment and a `provider_id`, a column of 64 characters |
| an id is not `credential` | `models.ProviderCredential` is the `provider_id` of the account that holds a password |
| no two providers share an id | a request names a provider by its id |

## Starting a sign-in

`Plugin.Begin` and `Plugin.BeginLink` share `begin`, which builds the operation's `HookContext` and runs these steps:

| Step | What | On failure |
| --- | --- | --- |
| 1 | `RunBefore(auth.oauth.beforeBegin)` on `provider`, `redirect_url`, `intent`, and the extra fields | `FailIfRejected`: `auth.oauth.beginFailed` with `rejected_by_hook` for a rejection |
| 2 | find the provider by its id | a validation error, code `provider_invalid_input` |
| 3 | check `RedirectURL` with `core.TrustedRedirect(ac.Origins, url)`, if one was given | a validation error, code `redirect_url_invalid_input` |
| 3a | a link only: the user has no account at the provider (`accountAt`) | a conflict, code `provider_already_linked` |
| 4 | create the PKCE verifier, the nonce, and the binding value: 32 random bytes each | a security error |
| 5 | `Store.Seal(verifier, verifierBinding(bindingHash))`: bound to the hash of the binding value | the error is returned |
| 6 | `TokenManager.Issue(oauth_state, subject, metadata)`: no subject for a sign-in, the user for a link | `FailIfRejected`, for a refusal on `token.beforeIssue` |
| 7 | `Provider.AuthorizationURL` with the state, the redirect URI, the S256 challenge, and the nonce. Then check that the URL is absolute `http` or `https`. | revoke the token, and return the error wrapped |
| 8 | `RunAfter(auth.oauth.afterBegin)` with a copy of the result that has no URL, no state, and no binding value | |

Steps 2 to 3a come before anything is written, so a refused request leaves no token.

`BeginLink` checks the caller before `begin`, as every self-service flow does: `Access.RequireFreshUser(oauth_account:manage)`, then a session on the caller. A system caller acting for a user passes the first check and has no session, and gets `session_missing`: a link is bound to the session that started it, and there would be nothing to compare at the completion. `begin` then publishes the user under `HookValueUserID`, and runs step 3a.

The token's metadata is the authorization transaction:

| Key | Value | Why in this form |
| --- | --- | --- |
| `provider` | the provider's id | |
| `intent` | `sign_in` or `link` | A token without one counts as a sign-in's: one written by the version before links existed, during a rolling upgrade. |
| `session_id` | for a link, the session that started it. Absent for a sign-in. | A session id is no credential, so it is kept as it is. |
| `redirect_url` | the checked redirect target. Absent when none was given. | |
| `nonce` | the nonce, as it is | The provider puts it into the ID token, so it is not a secret. |
| `verifier_secret` | the PKCE verifier, sealed with the purpose `oauth.pkce_verifier` and the binding hash as the owner | Handlers on the token points and readers of the database see the metadata. The verifier is sealed before the token exists, so the token's id can't be the owner. |
| `binding_hash` | the SHA-256 of the binding value, in hex | The raw value is only in `BeginResult`. Finishing the sign-in compares hashes. |

Both key names end in a word the telemetry redactor knows (`secret`, `hash`).

Invariants:

- **The raw token is the state.** `Begin` hands it to the provider and returns it in `BeginResult.State`, and keeps no copy. Finishing a sign-in finds the transaction by consuming the state, which is also what makes a state single use.
- **The binding value is in no URL and in no row.** The authorization code and the state pass through the callback page's URL. If the binding value were there too, or in the database, whoever reads a URL or a row could finish somebody else's sign-in.
- **A sign-in's token has no subject, and a link's names its user.** Nobody is signed in when a sign-in starts, so `Store.DeleteUser` and `RevokeAllForSubject` never touch its token. A link's token goes with its user.
- **The intent is the flow's.** `Begin` starts a sign-in and `BeginLink` a link, whatever the payload says: `BeginRequest.FromMap` drops `intent`, from a body and from a rewritten payload. A handler that could turn a sign-in into a link would start a link no caller check let through, and one that turned a link into a sign-in would have it completed as whoever the provider names.
- **A provider's error does not quote the URL.** The URL holds the state, and the error ends up in a log line.

The limits per client address are hook rules, so they hold for `Begin` and `Complete` called from any transport whose caller has an address. Code in the process has none and is not counted.

## Completing a sign-in

`Plugin.Complete` first refuses a request without a provider, a code, or a state, as a validation error. Nothing fires for it.

It then reads the state's token without consuming it (`TokenManager.Verify`). The checks before the consume need the token's metadata, and the result needs the redirect target. A token whose intent is `link` goes to the link path ([Completing a link](#completing-a-link)). Every other state goes to the sign-in path, one that does not check out included, so its refusal fires `auth.signIn.failed` like any other. A state that is unknown or expired can't say whether it was a link's.

`completeBody`, wrapped with core's three sign-in points. Steps 1 to 3 are `check`, and steps 4 to 6 are `redeem`, which the link path shares:

| Step | What | On failure |
| --- | --- | --- |
| 1 | the state's token was found, and names a provider the plugin has | `Fail` with `invalid_oauth_state` |
| 2 | the request's provider is the token's | `Fail` with `oauth_provider_mismatch` |
| 3 | the SHA-256 of the binding value equals `binding_hash`, compared in constant time | `Fail` with `oauth_binding_mismatch` |
| 4 | `TokenManager.Consume(oauth_state, state)` | `Fail` with `invalid_oauth_state` |
| 5 | `Store.Open(verifier_secret)`, then `Provider.Exchange` with the code, the redirect URI, the verifier, and the nonce | `Fail` with `oauth_exchange_refused` for `ErrExchangeRefused`. Any other error is returned. |
| 6 | the identity has a subject | an error of the system: the provider's mistake, not the user's |
| 7 | `resolveUser`: find or create the user | see below |
| 8 | `RunBefore(auth.signIn.credentialsVerified)` with the factor `oauth`; `requireStepUp` makes the session pending | `FailIfRejected`: `Fail` with `rejected_by_hook` for a rejection |
| 9 | `SessionManager.Create` | the error is returned |
| 10 | return a `*core.SignInResult` with `Method: "oauth"` | |

Every `Fail` of steps 1 to 5 answers with one typed unauthorized error, code `invalid_oauth_callback`. The audit event of `auth.signIn.failed` has the real code, and the provider in its metadata.

The payload on `auth.signIn.before` is `method` and `provider`, and it is veto only. The code, the state, and the binding value are not in it.

### Finding the user

`resolveUser` decides what an identity means for `users` and `accounts`:

| What it finds | Outcome | Code on `auth.signIn.failed` | Error |
| --- | --- | --- | --- |
| an account for `(provider, subject)` | sign in the account's user | | |
| that account, and its user is gone | refuse | `user_not_found` | `invalid_oauth_callback` (401) |
| no account, and no usable email in the identity | refuse | `email_not_provided` | `email_not_provided` (403) |
| no account, and a user with the email that `linkByVerifiedEmail` links the subject to | sign that user in | | |
| no account, and a user with the email, not linked | refuse | `account_not_linked`, with that user as the subject | `account_not_linked` (409) |
| no account, and no user with the email | sign the user up | | |

A usable email is one that `utils.IsValidEmail` accepts and that fits the column.

`linkByVerifiedEmail` links only when the provider is in `Options.LinkByVerifiedEmail`, `Identity.EmailVerified` is true, the user's `email_verified` is true, and the user has no account at the provider. The link is an operation of its own, nested under the sign-in like the sign-up: `autoLinkBody` is wrapped with the plugin's link points, with `by` set to `verified_email`, and its `Values` start as a copy of the sign-in's with the user's id added. A handler's veto fires `auth.oauth.linkFailed` and then `auth.signIn.failed`, both with `rejected_by_hook`. A link that loses the race for the provider account is resolved once more, as a sign-up is: the second lookup finds the account the winner wrote.

The sign-up is an operation of its own, nested under the sign-in:

- **It fires `auth.signUp.before`, `.after`, and `.failed`.** The payload is `method`, `provider`, `email`, `email_verified`, `firstname`, `lastname`, and `image_url`, and it is veto only.
- **The user and the account are one transaction.** `auth.signUp.after` fires once it has committed, and before the session exists.
- **`email_verified` is the provider's statement.** `Identity.EmailVerified` sets it, and nothing else does.
- **The profile is taken as far as it goes.** A full name stands in when the provider gives no parts. A name is cut to 255 characters. A picture that is not an `http` or `https` URL is dropped.
- **The account holds the provider's tokens only for a provider in `StoreTokens`.** Otherwise it has the provider's id and the subject. See [Provider tokens](#provider-tokens).

Invariants:

- **Steps 1 to 3 run before the consume.** A request that fails one of them leaves the state usable. If the consume came first, whoever read a state in a URL could end the sign-in it belongs to.
- **The consume runs before the exchange.** Of two requests with one state, one loses before either reaches the provider. A state is not given back when a later step fails.
- **A user is found by the subject.** The user `FindUserByEmail` returns is signed in only once `linkByVerifiedEmail` has linked the subject to it, which needs a listed provider and the email verified on both sides. Any other path from that lookup to a session would let every provider open every account with a matching email.
- **A sign-in writes nothing to an existing user.** The provider's email, name, and picture are read at sign-up only.
- **`oauth_provider` is in `Values` from the start, and the email once it is known.** `oauth.HookValueProvider` is published before the first dispatch. `hooks.HookValueEmail` is published for a new subject, after the exchange, so a handler on `auth.signUp.before` and the failed points have it.

`[Limit]` `account_not_linked` tells whoever signed in at the provider that an account with the email exists. When the provider verified the email, that is the mailbox's holder. When it did not, it is anyone who can claim the address at that provider. The email/password sign-up already answers `email_taken` to anyone, so this adds no new way to learn it.

## Linking

### Completing a link

`finishLink` runs for a state whose intent is `link`. It checks the caller first, before any point fires, as every self-service flow does: `Access.RequireUser(oauth_account:manage)`. A caller who is not signed in gets the session error of its credential, and a link's points don't fire for it. It then publishes the state's user under `HookValueUserID`, and calls `completeLinkBody`, wrapped with the plugin's three link points:

| Step | What | On failure |
| --- | --- | --- |
| 1 to 3 | `check`, as for a sign-in | `Fail(auth.oauth.linkFailed)` with the sign-in's codes |
| 4 | the caller's session is the token's `session_id`, and the caller is the token's subject | `Fail` with `oauth_session_mismatch` |
| 5 to 7 | `redeem`: the consume, the exchange, and the subject, as for a sign-in | `Fail` with `invalid_oauth_state` or `oauth_exchange_refused`. A failure of the system is returned. |
| 8 | `attach`: refuse a provider account that has a user, and a provider the user has an account at; then create the account | `Fail` with `provider_account_taken` or `provider_already_linked`, and a conflict with that code |
| 9 | return a `*LinkResult` with `By: "user"` | |

Every `Fail` of steps 1 to 7 answers with `invalid_oauth_callback`, and names the state's user as the subject. The payload on `auth.oauth.beforeLink` is `userID`, `provider`, and `by`, and it is veto only.

`attach` creates the account after its two lookups. A duplicate key on `(provider_id, account_id)` means a concurrent link or sign-in wrote the provider account between them. `attach` then runs once more, and its lookups say which conflict it lost to.

Invariants:

- **The link attaches to the state's user, never to the caller.** The caller has to be that user, in the session that started the link. If a link attached to whoever completes it, a callback forged with the attacker's state and code would attach the attacker's provider account to a victim who is signed in.
- **Steps 1 to 4 come before the consume.** A request from another session leaves the state usable, as a wrong binding value does.
- **Revoking a session stops the links started in it.** The caller check and step 4 need that session to be valid at the completion. Somebody who held a stolen fresh session long enough to start a link loses it when the user signs out everywhere.
- **Freshness is checked when a link starts, not when it completes.** The user may spend longer at the provider than `SessionConfig.FreshAge` leaves, and the state ties the completion to the fresh session that started it.
- **A link creates no session and writes nothing to the user.** The provider's email and profile are not read.

### Unlinking

`Plugin.Unlink` checks the caller (`Access.RequireFreshUser(oauth_account:manage)`) and the provider's id, publishes the user and the provider in `Values`, and calls `unlinkBody`, wrapped with the three unlink points:

| Step | What | On failure |
| --- | --- | --- |
| 1 | find the user's account at the provider (`accountAt`) | `Fail(auth.oauth.unlinkFailed)` with `provider_not_linked`, and a conflict with that code |
| 2 | in one transaction, delete the account, then list the user's accounts left | a `NotFound` from the delete is `provider_not_linked`: a concurrent unlink removed it |
| 3 | `canSignInWith` on what is left: a credential account with a password hash, or an account at a configured provider | roll back, and `Fail` with `last_sign_in_method` |
| 4 | return an `*UnlinkResult` | |

The delete comes before the check, so that the transaction holds a write from its first statement. On SQLite that takes the write lock: a second unlink of the same user waits for the first, and then counts without the account the first removed. A transaction that read first and wrote second would instead fail at its upgrade with "database is locked", as the entry "A data hook that reads before the write can fail with "database is locked" on SQLite" in `docs/ongoing.md` describes.

`[Limit]` The other databases don't make two deletes of different rows wait on each other. Two unlinks of one user at the same moment can each count the other's account as the one that remains, and both commit. `docs/ongoing.md` has the entry, with the write to the user's row that would close it.

`[Limit]` `canSignInWith` sees accounts only. A magic link signs in any user by email, and the phone number plugin signs in by a code to a verified number, without an account row, so a user who can sign in that way is still refused the unlink of their last provider account.

### Listing

`Plugin.Accounts` checks the caller (`Access.RequireUser(oauth_account:read)`) and returns the user's accounts at the configured providers, oldest first, as `ProviderAccount` values: the provider's id and `created_at`. The subject stays out of the result and of the route's answer. A client has no use for the provider's id of its user, and a field added later breaks nothing.

## Provider tokens

For a provider in `Options.StoreTokens`, the tokens of an exchange go into the account's columns: `access_token`, `refresh_token`, and `id_token`, which the store seals, `access_token_expires_at`, `refresh_token_expires_at`, and `scope`. For any other provider they are dropped. `p.storeTokens` holds the set.

### When they are written

| Moment | Write | Function |
| --- | --- | --- |
| sign-up of a new subject | the new account, with the tokens | `signUpBody` (`newAccount`) |
| sign-in of a known subject | the account's tokens become the new ones (`issuedChanges`) | `keepTokens` |
| sign-in with a provider that is not in the set | the account's tokens are removed, when it holds any (`noTokens`) | `keepTokens` |
| link, and link by a verified email | the new account, with the tokens | `attach`, `autoLinkBody` (`newAccount`) |
| refresh | the refreshed tokens (`issuedChanges`) | `refresh` |
| refresh token the provider refused | every token removed (`noTokens`) | `forgetTokens` |

`issuedChanges` replaces the access token and its expiry, and keeps what the provider did not issue again:

- **A refresh token, with its expiry.** Google issues one at the first consent only, and most providers keep the refresh token at a refresh.
- **The ID token.** A refresh issues none that the plugin checked, and the built-in providers drop the one a refresh answers with.
- **The scope.** An answer without one leaves the granted scope as it was (RFC 6749, section 5.1). The built-in providers fill in the requested scope when the answer to a code exchange names none.

A provider whose `Exchange` gives no access token leaves the stored tokens as they are at a sign-in. Expiries are stored in UTC.

### Handing out an access token

`Plugin.AccessToken` runs these steps:

| Step | What | On failure |
| --- | --- | --- |
| 1 | `Access.RequireUser(oauth_token:read)`. A system caller acting for the user passes. | the error of `Access` |
| 2 | the provider is configured, and in the set | a validation error, or a configuration error |
| 3 | `accountAt`: the user's account at the provider | `provider_not_linked` |
| 4 | the access token is usable: it is there, and expires more than `accessTokenLeeway` (a minute) from now, or has no expiry | |
| 5 | otherwise, `refresh`, through `p.refreshes`, a `singleflight.Group` keyed by the account's id | see below |

`refresh` reads the account again first, since a refresh may have finished while the call waited. It then needs a refresh token that has not expired and a provider that implements `Refresher`, or it answers `provider_token_unavailable`. A refusal of the provider (`ErrExchangeRefused`) goes to `forgetTokens`, which reads the account once more: another replica may have refreshed it with a refresh token the provider rotated. When the account still has no usable access token, its tokens are removed, so the next call answers at once without asking the provider. A failure of the system changes nothing.

Invariants:

- **One refresh per account at a time, within a process.** A provider that rotates refresh tokens refuses the second of two refreshes that use the same one. The shared call runs with `context.WithoutCancel`, so a caller that gives up does not fail the others. The HTTP client's timeout bounds it.
- **A token is never in a hook payload, an audit event, or a log line.** The account rows that data hooks see are sealed by the store, and no result the plugin hands to a point carries a token.
- **No route hands out a token.** `AccessToken` is a flow for the application's server.

`[Limit]` Two replicas can refresh one account at the same moment. With a provider that rotates refresh tokens, one of them is refused, and `forgetTokens` finds the other's tokens. A provider that also revokes the token family when a refresh token is used twice ends both. `docs/ongoing.md` has the entry.

### Revoking at an unlink

`unlinkBody` calls `revokeTokens` with the account it removed, once the transaction has committed. `revokeTokens` does nothing for an account without tokens, or a provider that is not a `Revoker`. It calls `Revoke` with `context.WithoutCancel`, so a client that disconnects does not leave the grant behind, and logs a failure as a warning: the account is gone, and the unlink stands.

`[Limit]` `Store.DeleteUser` deletes the user's accounts in its transaction, and revokes nothing at the providers.

## The routes

The routes that change something are JSON `POST`s, so the origin check and the refusal of form content types cover them under the cookie deliveries. Each handler decodes the body, calls one flow, and encodes the result. Every answer of a handler is sent with `Cache-Control: no-store`, by `answer` or by `WriteSignIn`.

| Route | Flow | Answer |
| --- | --- | --- |
| `POST /sign-in/oauth` (`PathBegin`) | `Begin` | `{"authorization_url": ...}`, plus `binding` for a client without cookies (`writeBegun`) |
| `POST /oauth/link` (`PathLink`) | `BeginLink` | as for `Begin` |
| `POST /oauth/complete` (`PathComplete`) | `Complete` | for a sign-in, `httpapi.WriteSignIn` with `redirect_url` as the extra key. For a link, `{"status": "linked", "account": {...}, "redirect_url": ...}` |
| `POST /oauth/unlink` (`PathUnlink`) | `Unlink` | `{"status": "unlinked"}` |
| `GET /oauth/accounts` (`PathAccounts`) | `Accounts` | `{"accounts": [...]}`, each `{"provider": ..., "created_at": ...}` |

No route has a session middleware. The flows read the caller, so they refuse the same callers however they are reached.

`Begin` and `BeginLink` are limited per client address on their before point, 10 a minute: `oauth.begin.address`. A completion of a sign-in counts on core's `core.signin.address`. The completion of a link, `Unlink`, and `Accounts` have no rule: each needs a session, and a link completes at most once for each start.

### Where the binding value goes

The handlers read `rctx.TokenCarrier.Config()`: `RouterConfig.Carrier` with its defaults applied. The binding value follows the session token:

| `RouterConfig.Carrier.Delivery` | The answer of `Begin` and `BeginLink` | Complete reads it from |
| --- | --- | --- |
| `DeliveryCookie` | a cookie only | the body, else the cookie |
| `DeliveryBoth` | a cookie and the body | the body, else the cookie |
| `DeliveryHeader`, `DeliveryBody` | the body only | the body |

Accepting a value from the body under `DeliveryCookie` costs nothing: a page there has no value to send, and a value somebody guessed does not match.

The cookie is built in `bindingCookie`:

| Attribute | Value | Why |
| --- | --- | --- |
| name | `__Host-behemoth_oauth_` and 16 hex characters of the state's SHA-256 | one cookie per sign-in, so two tabs don't overwrite each other's. The state itself stays out of the name. |
| `HttpOnly`, `Path=/`, no `Domain` | always | a script can't read it, and it goes only to Behemoth's host |
| `Secure` | unless `CookieInsecure` | |
| `SameSite` | the session cookie's | it has to reach the complete route on the requests the session cookie reaches. A front end on another site needs `SameSiteNone` for both. |
| `Max-Age` | until the sign-in expires | `Complete` sets `Max-Age=0` once the sign-in is done |

Invariants:

- **The `__Host-` prefix is what keeps a sibling subdomain out.** Without it, a page on `evil.example.com` could set a cookie for the parent domain with its own binding value, and send the victim's browser to the callback page with its own code and state. With `CookieInsecure` set the prefix can't be used, because a browser accepts it only on a `Secure` cookie, so that development setting loses this protection.
- **The binding value is in the body only for a delivery that already puts the session token where a script reads it.** Under `DeliveryCookie`, a script on the application's page can neither read the session token nor the binding value.

`[Limit]` A refused completion leaves its cookie until the sign-in expires. Only a successful one removes it.

## The built-in providers

`Options.Providers` is a list of `ProviderConfig`, which is data. `Init` turns each into a `Provider` with `newBuiltinProvider`, and checks the result like an application's provider in `Options.CustomProviders`: the ids of both lists share one namespace.

| Kind | Type | Protocol | User from |
| --- | --- | --- | --- |
| `oidc` | `oidcProvider` | OpenID Connect, endpoints from discovery | the ID token, then the userinfo endpoint when the token has no email |
| `google` | `oidcProvider` with Google's issuer | OpenID Connect | the ID token. `iss` may be `https://accounts.google.com` or `accounts.google.com`. |
| `github` | `githubProvider` | OAuth 2.0, fixed endpoints | `GET /user` and `GET /user/emails` |

What `newBuiltinProvider` refuses, each as a configuration error from `Boot`:

- a kind it does not know, a missing client id, and a `KindOIDC` provider without an id or an issuer
- a missing client secret for Google and GitHub, and a source whose current secret can't be read
- an issuer that is not `https` (or `http` on a loopback address), or has a query, and an issuer set for Google or GitHub
- scopes without `openid` for an OpenID Connect kind

`Init` calls no provider. Discovery and the key set are fetched at the first sign-in, so a provider that is down fails sign-ins and not the boot.

### The calls to a provider

`providerHTTP` makes every call, with `Options.HTTPClient`, or else `AuthContext.HTTPClient`, the client `Boot` builds from `BootConfig.Outbound`. The rules are the ones `outbound` gives every outbound call (ADR-0003, [`../http/outbound.md`](../http/outbound.md)):

| Rule | Where | Why |
| --- | --- | --- |
| 10-second timeout, by default | `outbound.NewClient` | a provider that hangs holds a sign-in request |
| no redirect followed, by default | `outbound.NewClient` | the code exchange sends the code, the verifier, and the client secret, and they go only to the configured endpoint |
| at most 1 MiB read of an answer | `outbound.GetJSON`, `outbound.PostForm` | a hostile or broken endpoint can't make the plugin read without end |
| `User-Agent: behemoth`, unless configured | `outbound.NewClient` | GitHub's API refuses a request without one |
| a span per call, `behemoth.outbound.request` | `outbound.NewClient` | a slow provider shows in the trace of the sign-in |
| the request's context | every call | a client that disconnects cancels the call |

A client in `Options.HTTPClient` keeps the rules it was built with. The option's comment asks for one built with `outbound.NewClient`, or one that follows no redirect. `Init` builds a client of its own only for an `AuthContext` that `Boot` did not build, which has no `HTTPClient`.

`requestTokens` is the one request to a token endpoint, for the code exchange (`redeemCode`) and for a refresh (`refreshTokens`). It refuses with `ErrExchangeRefused` an answer that names an OAuth error, whatever its status: GitHub answers a refused code with `200`. Any other answer without an access token is a failure of the system. `authenticate` adds the client's credentials, to a token request and to a revocation: HTTP Basic (id and secret form-encoded first, RFC 6749 section 2.3.1), the form, or the id alone for a public client. `authMethod` picks Basic unless the discovery document lists only `client_secret_post`. GitHub always gets the form. The client secret is read from its source for every request.

`tokenResponse.tokens` turns an answer into `Tokens`, with the lifetimes counted from the provider's clock. A lifetime is a JSON number or a string of digits (`seconds`), and anything else, or one past an int32 of seconds, reads as unknown and does not fail the answer: a provider that writes `expires_in` oddly still signs users in.

`revokeToken` posts a token to a revocation endpoint (RFC 7009), with its type as `token_type_hint`, and takes any `2xx` as a success: the endpoint answers `200` for a token it does not know as well. `revocable` picks the token: the refresh token, whose revocation ends the grant at most providers, else the access token.

### The OpenID Connect provider

`oidcProvider` keeps the discovery document on the value, under its mutex, and a `crypto.CachedKeySet` for the document's `jwks_uri`, which has a mutex of its own:

| Cache | Fetched | Refetched |
| --- | --- | --- |
| discovery document | at the first sign-in | after `discoveryTTL`, 24 hours |
| key set | at the first ID token | after `keysTTL`, one hour, or when a token names a key the set lacks and the last fetch is older than `keyRefetchInterval`, one minute. While the set can't be fetched, its keys are used for `crypto.DefaultKeySetMaxStale`, one hour, past `keysTTL`. |

`document` builds the key set when the document is fetched, and keeps it, with its keys, while a new document names the same `jwks_uri`. The cache rules are those of [`../crypto/key_sets.md`](../crypto/key_sets.md#the-cached-key-set).

A replica keeps its own caches. That is fine: they hold what the issuer publishes, and a replica that fetches later sees the same.

The document is refused when its `issuer` is not exactly the configured one (OpenID Connect Discovery, section 4.3), or when the authorization, token, key set, userinfo, or revocation endpoint fails `outbound.CheckURL`. A failure here is a failure of the system.

`Refresh` posts the refresh token to the document's token endpoint, authenticated like the code exchange, and drops an ID token in the answer: the plugin keeps the one of the sign-in, which it checked. `Revoke` posts to the document's `revocation_endpoint` (RFC 8414), authenticated by `revocation_endpoint_auth_methods_supported`, and does nothing for a document that names none.

`authParams` are parameters of the authorization URL besides the protocol's, and never replace one of the protocol's. A Google provider whose tokens are stored has `access_type=offline` there: Google issues a refresh token only to a client that asks for offline access, and ignores the scope `offline_access`.

`Exchange` checks, in this order:

| Check | Refusal |
| --- | --- |
| the `iss` parameter is the issuer, when one was sent; and one was sent, when the document says the issuer sends it (RFC 9207) | `ErrExchangeRefused` |
| the code is redeemed (`redeemCode`) | `ErrExchangeRefused` for an OAuth error |
| the answer has an ID token | a failure of the system |
| `verifyIDToken`, through `crypto.VerifyJWT`: the signature with an algorithm of `signingAlgs` and a key of the set; `aud` has the client id; `exp` is required, and `exp` and `iat` are checked with one minute of leeway; `iss` is one of the provider's issuers. Then the rules of OpenID Connect: `azp` is the client id when there is one or several audiences; `nonce` is the sign-in's; `sub` is not empty. | `ErrExchangeRefused`, wrapping the `CategoryToken` error of `VerifyJWT`. Any other error of `VerifyJWT`, such as a key set that can't be fetched or has no usable key, is a failure of the system. |
| without an email in the token: the userinfo endpoint's `sub` is the token's | `ErrExchangeRefused` |

`signingAlgs` holds the RSA, RSA-PSS, and ECDSA algorithms, and `VerifyJWT` refuses every symmetric one anyway. A key's own algorithm, and its type, have to fit the token's (`fits`), so an HMAC token would fail twice: no HMAC algorithm is allowed, and no symmetric key is ever in the set.

The set comes through a `keysource.JWKSet` on the plugin's client, and `crypto.ParseJWKSet` decides which keys are kept. An Ed25519 key can be in the set, but no ID token is accepted with `EdDSA`, since `signingAlgs` does not list it. A set without a usable key is `keysource.ErrNoUsableKey`, a failure of the system, and not a refused token: no ID token could be checked with it.

An ID token without a `kid` is checked against every key of the set that fits its algorithm (`AllowMissingKID`). The plugin's rule before `VerifyJWT` accepted one only when exactly one key fitted, which refused every such token while a provider published two keys of one type.

`email_verified` counts as true when it is the boolean `true` or the string `"true"`. Some providers send the string.

### The GitHub provider

`githubProvider` redeems the code at `https://github.com/login/oauth/access_token`, then calls `GET /user` and `GET /user/emails` with the access token:

- **The subject is the numeric `id`,** as a string. A login can be renamed, and the old one taken by somebody else.
- **The email is the primary address, and only when GitHub verified it.** The other addresses are ignored. Without the `user:email` scope, `/user/emails` answers `404`, and the identity has no email.
- **GitHub names no issuer,** so an `iss` parameter is not checked.
- **GitHub separates scopes with commas.** `githubTokens` turns them into the spaces of OAuth 2.0.
- **Only a GitHub App's user tokens expire.** They come with a refresh token, which `Refresh` redeems at the same token endpoint, and which GitHub replaces at every refresh. An OAuth App's tokens never expire and have none.
- **`Revoke` deletes the application's grant** (`DELETE /applications/{client_id}/grant`), with the client id and secret as Basic credentials and an access token of the user in the body. That ends every token GitHub issued the application for the user. GitHub refuses an access token that expired, so a GitHub App's grant can stay behind when its access token expired before the unlink.

`[Limit]` The ID token's `at_hash` is not checked. In the code flow the ID token comes from the token endpoint over TLS, and OpenID Connect Core makes the check optional there.

`[Limit]` A Microsoft endpoint for several tenants publishes a discovery document whose `issuer` holds a `{tenantid}` placeholder. The exact comparison refuses it, so such a provider works only with the endpoint of one tenant.

## Design decisions

### One redirect URI per provider, derived from `CallbackURL`

**Context:** A redirect URI per provider is the control against provider mix-up: finishing a sign-in can compare the page a code arrived at with the provider the state names. The application also needs a page for the provider to send the browser to.

**Options considered:**

- *A redirect URI in each provider's own configuration.* Any shape of URL works. The plugin can't know the URI of a provider an application implements, and nothing stops two providers from sharing one.
- *One `CallbackURL`, with the provider's id as a query parameter.* One page for the application. The id then shares the query with the `code` and the `state` the provider adds, and `CallbackURL` could no longer be required to have no query.
- *One `CallbackURL`, with the provider's id appended as a path segment.* One option, one route with a parameter in the application, and the URIs can't collide because the ids can't. The application's route has to have that shape.

**Decision:** the path segment. `oauth.RedirectURI` returns the URI, so an application registers the same string the plugin sends.

**Revisit if:** an application can't serve `<CallbackURL>/<id>`, for example because a provider's console accepts one fixed URI that is already in use.

### The state token is kept in the database

**Context:** The plan notes that the key-value backend suits `oauth_state`, because an entry there expires by itself. A token kind names one backend when it is declared, before `Boot` knows whether a key-value storage exists.

**Options considered:**

- *The key-value backend.* No row outlives its sign-in. `Boot` fails for an application without a key-value storage.
- *The database backend.* The plugin works wherever core does. Used and expired rows stay, since nothing purges tokens.
- *An option.* Each application picks. One more setting, for a problem that a purge of tokens solves for every kind.

**Decision:** the database backend.

**Revisit if:** the rows become a problem although the application runs `Store.Purge`. Since the decision, `Store.PurgeExpiredTokens` removes the used and expired state tokens, and `Purge` runs it when `TokensBefore` is set.

### Handlers on the after point do not get the URL or the binding value

**Context:** `core.WithLifecycle` hands the flow's result to the after point as it is. `Begin`'s result holds the authorization URL, which holds the state, and the binding value. Hook handlers are planned to be reachable in other processes, as `service_ready_components.md` describes.

**Options considered:**

- *`WithLifecycle`, with the full result.* The shortest code, and what the sign-in points do with a session token. Every handler and every forwarder then holds what finishes the sign-in.
- *Write the three dispatches out in `Begin`, and give the after point a copy without the two fields.* About fifteen lines that repeat `WithLifecycle`. A handler learns the provider, the transaction's id, and the expiry.

**Decision:** the copy. `Begin` calls `AsOperation`, `RunBefore`, `FailIfRejected`, and `RunAfter` itself.

**Revisit if:** a second flow needs the same, and `WithLifecycle` gets a way to say what the after point receives.

### `Store.Seal` and `Store.Open` are exported

**Context:** The verifier has to be sealed before it goes into the token's metadata. The store's sealing was private, and its form (`v<key version>:<base64>`) is what a re-encryption sweep selects on.

**Options considered:**

- *Repeat the form in the plugin, over `ac.Crypto.AtRest`.* No change to the store. Two places then define what a sealed value looks like.
- *Package functions that take an `Encryptor`.* Usable without a store. A plugin has the store at hand, and would have to pass the same encryptor the store already holds.
- *Two methods on `Store`.* One form, one encryptor, and nothing new to configure.

**Decision:** `Store.Seal` and `Store.Open`. A plugin with a secret column of its own uses the same two methods.

**Revisit if:** code without a store needs to seal a value.

### The checks that need no write run before the consume

**Context:** A state has to be single use, and the consume is what makes it so. The state travels in two URLs, so more people can read it than can finish the sign-in.

**Options considered:**

- *Consume first, then check.* One read of the token. Any request with a valid state ends the sign-in, whether or not it has the binding value.
- *Read, check, then consume.* Two reads. A request without the binding value changes nothing. The checks run on a token that may be consumed a moment later, which the consume itself catches.

**Decision:** read, check, then consume. `Complete` hands the token it read to the flow, so the flow does not read it a second time.

**Revisit if:** the token's metadata can change after it is issued. The checks would then have to run on what the consume returns.

### One error code for a refused callback

**Context:** A callback can be wrong in several ways: the state, the binding value, the provider, or the code. A client does the same thing for all of them: it starts the sign-in again.

**Options considered:**

- *A code per cause.* A developer sees at once what failed. The answer also tells a caller without the binding value that the state it tried is valid.
- *One code.* The response says nothing about which part was wrong. The cause is in the audit event and the log.

**Decision:** one code, `invalid_oauth_callback`, as the magic link plugin answers every refused link with `invalid_magic_link`. `account_not_linked` and `email_not_provided` have their own, because the user has to do something different for each.

**Revisit if:** client developers can't find the cause of a refusal in practice.

### A new user's sign-up is a nested operation with a veto-only payload

**Context:** A first sign-in creates a user. Handlers and counters for sign-ups are attached to `auth.signUp.*`, which the email/password plugin fires.

**Options considered:**

- *Create the user inside the sign-in and fire no sign-up point.* Less code. A handler that keeps a domain out, a welcome email, and the sign-up counter would all miss these users.
- *Fire `auth.signUp.*` with a payload a handler can rewrite, as for an email sign-up.* One contract for both. A handler could then change the email the provider reported.
- *Fire `auth.signUp.*` with a veto-only payload.* A handler can refuse and can read the profile. The payload has `method` and no `password`, so a handler written for email sign-up has to allow for that.

**Decision:** the veto-only payload, in an operation nested under the sign-in. A refused sign-up fires `auth.signUp.failed` and then `auth.signIn.failed`: both operations failed.

**Revisit if:** applications need to complete a new user's profile at this point. A data hook on `data.user.beforeCreate` already can.

### A sign-up that loses a race is resolved once more

**Context:** Two callbacks of one new person can both find no account and no user, for example from two tabs. The second insert fails on a unique index.

**Options considered:**

- *Return the store's error.* No code. The loser gets a `409` with the store's code for a sign-in that should have worked.
- *Look again, once.* By then the winner has committed. The loser signs in the winner's user, or is refused with `account_not_linked` when the winner was a sign-up with the same email by another way.

**Decision:** look again, once. A second duplicate is returned as it is.

**Revisit if:** the store gains a retry for writes that lose a race.

### The binding value follows the session token

**Context:** A started sign-in hands the client a binding value that it shows again at completion. A browser client of the cookie deliveries should not be able to read it from a script, and a client without cookies has nowhere else to keep it.

**Options considered:**

- *Always a cookie.* One behavior. A native application, whose callback runs in the system browser, does not have the cookie when it completes the sign-in.
- *Always the body.* One behavior. A script on the page can read a value that an `HttpOnly` cookie would keep from it, which the cookie deliveries exist to avoid.
- *The way the session token travels.* A client that already handles the session token one way handles the binding value the same way. The plugin needs to know the delivery.

**Decision:** the way the session token travels. `Boot` puts the session settings on `AuthContext` as `SessionConfig`. The settings and not only the transport, because the cookie also needs the session cookie's `SameSite` and `Secure`.

The settings have since moved with the HTTP side of the session manager to the token carrier, as this decision expected. The handlers read them from `rctx.TokenCarrier.Config()`, and the transport is now called the delivery. See *The token travels with a carrier of its own* in [`../sessions/token_delivery.md`](../sessions/token_delivery.md).

### One binding cookie per sign-in

**Context:** A user can start two sign-ins in one browser, from two tabs or with a second click.

**Options considered:**

- *One cookie for all sign-ins.* One name. The second sign-in overwrites the first one's value, and completing the first fails.
- *A cookie per sign-in, named after the state.* Each sign-in finds its own. A browser keeps a cookie for every sign-in that was started and not completed, until it expires.

**Decision:** a cookie per sign-in. The name has 16 hex characters of the state's SHA-256, which the complete route computes from the state in the body.

**Revisit if:** applications report hitting a browser's limit on cookies per host.

### Built-in providers are data, and the application's providers are a second list

**Context:** `service_ready_components.md` asks for a data form next to an option that is an interface ("The OAuth plugin's providers are Go values" in `docs/ongoing.md` until this step). `Options.Providers` was the list of `Provider` values.

**Options considered:**

- *Constructors, such as `oauth.Google(cfg)`, that return a `Provider`.* Short in Go. A configuration file still can't name a provider, and a service would have to map each kind to a constructor itself.
- *A second option for the data, next to `Providers`.* No rename. The common case gets the longer name.
- *`Providers` for the data, and `CustomProviders` for the application's own.* The common case reads naturally. The plugin's earlier `Options.Providers` changes type, which an experimental plugin can still do.

**Decision:** `Providers []ProviderConfig` and `CustomProviders []Provider`.

**Revisit if:** a kind needs a setting that is code, such as a function. It would get a data form of its own.

### The client secret is a `SecretSource`, read for every exchange

**Context:** A client secret is a secret, which `service_ready_components.md` keeps out of the options. The core already has `SecretSource`, built for versioned keys: a map of secrets and the current version.

**Options considered:**

- *A string field.* Simple. It ends up in a configuration file and is printed with it.
- *A source of its own, with one value.* A new interface for one use.
- *`SecretSource`, using its current secret.* The type that a file or environment source implements anyway. A source with one entry is a little verbose.

**Decision:** `SecretSource`. `Init` reads it once to fail early, and every code exchange reads it again, so a rotated secret takes effect at once.

**Revisit if:** sources that are slow to read appear. The exchange would then cache the secret for a short time.

### Discovery happens at the first sign-in, not at `Boot`

**Context:** An OpenID Connect provider is described by its issuer, and its endpoints come from a document on the network.

**Options considered:**

- *Fetch at `Init`.* A wrong issuer fails the boot, where `service_ready_components.md` wants a wrong value to fail. A provider that is down also fails the boot, and with it every other way to sign in.
- *Fetch at the first use, and keep it.* The boot does not depend on the provider. A wrong issuer shows up at the first sign-in, as a failure of the system.

**Decision:** at the first use. `Init` checks everything it can without the network: the issuer's form, the client id, the secret.

**Revisit if:** applications ask for a check at start. A method that fetches every provider's document could be called by the application after `Boot`.

### An unknown key is fetched at most once a minute

**Context:** An issuer rotates its keys, and the key set is cached. A token that names a key the cache lacks is either newly signed or forged.

**Options considered:**

- *Fetch the set for every unknown key.* A rotation is picked up at once. Anybody can make the plugin call the issuer by sending tokens with made-up key ids.
- *Fetch on a schedule only.* No call can be caused from outside. A rotation fails sign-ins until the next fetch.
- *Fetch for an unknown key, at most once a minute, and every hour anyway.* At most one call a minute can be caused, and a rotation fails sign-ins for a minute at most.

**Decision:** the third. Issuers publish a new key before they sign with it, so in practice the hourly fetch already has it.

**Revisit if:** an issuer rotates without publishing ahead, and the minute shows up as failed sign-ins.

### GitHub's subject is its numeric id, and its email is the primary verified address

**Context:** GitHub has no ID token. Its API returns a login, a numeric id, and a list of addresses.

**Options considered:**

- *The login as the subject.* Readable. A renamed login can be taken by somebody else, who would then sign in to the first user's account.
- *The numeric id as the subject.* It never changes.
- *For the email, the public address on the profile.* One call less. Whether GitHub verified it is not part of that answer.
- *For the email, the primary address from `/user/emails`, when it is verified.* One more call, and the `user:email` scope. The address is one GitHub verified.

**Decision:** the numeric id, and the primary verified address. A user whose primary address is not verified has no email, and a first sign-in is refused with `email_not_provided`.

**Revisit if:** users with a verified address that is not primary are refused in practice. Another verified address could stand in.

### A link completes through `Complete`

**Context:** The provider sends the browser to the same redirect URI for a sign-in and for a link, and the callback page posts what it got. Nothing in the URL tells the page which one it is.

**Options considered:**

- *A complete route and flow for each.* Each answers in one shape. The callback page has to remember which one it started, in storage of its own, and a state posted to the wrong route needs a refusal of its own.
- *One flow that reads the intent from the state.* The callback page stays as it is, and the answer says which it was. `CompleteResult` carries a sign-in or a link, and a caller in code checks which.

**Decision:** one flow. The route answers a link with `{"status": "linked", ...}`, the shape of an action, and a sign-in with `WriteSignIn`. `CompleteResult` keeps its embedded `*core.SignInResult`, nil for a link, so the callers that only sign in did not change.

**Revisit if:** callers in code trip over the nil `SignInResult`. A result with two named fields and no embedding makes the choice explicit.

### A link is bound to the session that started it

**Context:** The plan's third rule says that a link carries the session. The binding value already ties the completion to the client that started the link, and the state names the user.

**Options considered:**

- *The user only.* Any session of the user completes the link. A stolen session that started a link keeps it after the user signs out everywhere, for as long as the state lives.
- *The session.* Only the session that started the link completes it, and only while it is valid. A user who signs in again in another tab starts the link again.
- *The session, and fresh at the completion too.* A link that takes longer at the provider than `FreshAge` leaves fails, although the session was fresh when the user asked for it.

**Decision:** the session, fresh at the start only.

**Revisit if:** links completed in another session than the one that started them are refused in practice.

### A user has one account per provider

**Context:** `accounts` is unique on `(provider_id, account_id)`, so two users can't share a provider account. Nothing stopped one user from having two accounts at one provider, such as a personal and a work GitHub account.

**Options considered:**

- *Several per provider.* A user links every account they have. An account row has no email or name of the provider's, so a list can't tell two of them apart, and an unlink has to name the account by an id the client has no use for. A flow that returns a valid access token for a provider, the plan's next step, would have to pick one.
- *One per provider.* An unlink and an access token name the provider. A user who wants the other account unlinks the first.

**Decision:** one per provider, refused with `provider_already_linked` when a link starts and when it completes. The rule is the plugin's and not a unique index, so two links of different subjects at one provider, completed at the same moment, can both pass.

**Revisit if:** applications need several accounts at one provider. A unique index on `(user_id, provider_id)` for the provider accounts would also close the race.

### Unlink counts the accounts it can see

**Context:** The plan says that an unlink keeps the user's last way to sign in. A password and a provider account are rows in `accounts`. A magic link and a code to a phone number sign a user in through other plugins, without a row.

**Options considered:**

- *Count the accounts.* No new contract. A user who signs in by magic link is refused the unlink of their last provider account, although they would not be locked out.
- *Ask each sign-in plugin, through a new core contract* that says whether a user can sign in with it. Exact. A contract between plugins for one check, and a sign-in plugin written later has to implement it or count as no way in.
- *An option that says every user has another way,* for an application with the magic link plugin. Data, and simple. It is right only when the other way is open to every user, which a phone number is not.

**Decision:** count the accounts. The refusal errs on the safe side: it never leaves a user locked out.

**Revisit if:** users who sign in by magic link or phone number ask to unlink their last provider. `docs/ongoing.md` has the entry.

### A link by a verified email fires the link points

**Context:** A sign-in that links by a verified email attaches a provider account to an existing user, the same change as a link the user makes.

**Options considered:**

- *Write the account inside the sign-in, and fire only the sign-in points.* Less code. The audit log has a sign-in and no link, and a handler that watches links misses these.
- *A nested link operation with the link points.* The audit log records the link, and a handler can refuse it, for an administrator's account for example.

**Decision:** the nested operation, like the sign-up of a new user. A refused link fails the sign-in with the handler's error.

**Revisit if:** applications want a refused link to fall back to `account_not_linked`.

### Tokens are stored per provider

**Context:** The plan makes storing the provider's tokens an option, off by default. An application that calls one provider's API often signs users in with others too.

**Options considered:**

- *One switch for every provider.* Simple. An application that needs GitHub's API also holds its users' Google tokens, which the default exists to avoid.
- *A field of `ProviderConfig`.* Next to the provider's other settings. A provider in `CustomProviders` has no config to carry it.
- *A list of provider ids in `Options`, like `LinkByVerifiedEmail`.* One form for both kinds of provider, and data.

**Decision:** the list, `StoreTokens`.

**Revisit if:** a provider needs settings of its own about its tokens. Both lists would then move into one per-provider form.

### `AccessToken` has no route

**Context:** The plan asks for a flow that hands the application a valid access token. A route would hand it to the application's pages.

**Options considered:**

- *A route for the signed-in user.* A page can call the provider from the browser. A script on the page then holds a credential of the user's provider account, the way the cookie deliveries keep the session token from scripts.
- *A flow only.* The application's server calls the provider, and a page asks that server. A later standalone service exposes the flow to other servers, not to browsers.

**Decision:** a flow only.

**Revisit if:** applications need to call a provider from the browser with the user's token.

### An access token is refreshed when it is asked for

**Context:** An access token expires, an hour after it was issued at many providers.

**Options considered:**

- *A job that refreshes tokens before they expire.* A token is always valid when asked for. It refreshes tokens nobody uses, needs a schedule, and runs on every replica, which `service_ready_components.md` asks a component to leave to its owner.
- *A refresh inside `AccessToken`.* Only tokens in use are refreshed, and nothing runs in the background. The first call after an expiry waits for the provider.

**Decision:** inside `AccessToken`, a minute before the expiry.

**Revisit if:** the wait for the provider shows in the application's requests.

### One refresh per account within a process, and a second look across replicas

**Context:** Calls that find one account's token expired at the same moment would each refresh it. A provider that rotates refresh tokens accepts the first refresh and refuses the others.

**Options considered:**

- *No coordination.* Every caller but one fails when the provider rotates.
- *`singleflight` in the process, and a second read after a refusal.* Calls in one replica share a refresh. A call refused across replicas finds the tokens the other replica stored. A provider that revokes the token family when a refresh token is used twice still ends both.
- *A lock in the database.* One refresh across replicas. It needs a lease on the account, a column or a table core does not have, and a wait for its holder.

**Decision:** the second.

**Revisit if:** applications run several replicas with a provider that detects a reused refresh token. `docs/ongoing.md` has the entry.

### Refresh and revocation are optional interfaces

**Context:** The built-in providers can refresh and revoke tokens. An application's provider may or may not.

**Options considered:**

- *Methods of `Provider`.* Every provider has them. A provider written before them stops compiling, and one that can't revoke writes an empty method.
- *Optional interfaces, found with a type assertion.* Nothing breaks. A provider without them is neither refreshed nor revoked, and `AccessToken` answers `provider_token_unavailable` once its token expires.

**Decision:** optional interfaces, `Refresher` and `Revoker`.

**Revisit if:** a provider's author misses them, because a method with the wrong signature compiles and is never called.

### The tokens are revoked after the unlink commits

**Context:** An unlink removes the account and its tokens. The grant at the provider stays unless the tokens are revoked there.

**Options considered:**

- *Revoke before the delete, and refuse the unlink when the revocation fails.* The grant never outlives the account. A provider that is down keeps users from unlinking, and a delete that is then refused, as the last way to sign in, leaves an account whose tokens are revoked.
- *Revoke after the commit, best effort.* The unlink works whatever the provider does. A failed revocation leaves the grant at the provider, with nothing left in the database to revoke it with later.

**Decision:** after the commit, with a failure logged as a warning.

**Revisit if:** applications need to know that a revocation failed, to send the user to the provider's settings. The result could say so.

## Deferred

The items that remain:

- `Deferred:` two unlinks of one user at the same moment, on a database other than SQLite. See [Unlinking](#unlinking).
- `Deferred:` one refresh per account across replicas. See [Handing out an access token](#handing-out-an-access-token).
- `Deferred:` revoking the tokens of a user that `Store.DeleteUser` deletes. See [Revoking at an unlink](#revoking-at-an-unlink).
- `Deferred:` renewing the tokens of a linked provider without a sign-in. A user whose tokens can't be used any more signs in with the provider again, and a link of a provider the user has an account at is refused.
