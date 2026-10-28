# Session token delivery

This document explains how a session token reaches the client over HTTP and comes back: the four deliveries, the split between the token carrier, the session manager and the route that answers a sign-in, how a sign-out takes the token back, how the cookie follows a session whose expiry rolls, the defaults of the settings, and the JSON a sign-in answers with.

The code lives in:

- `httpapi/token_carrier.go`: `TokenDelivery`, `SessionTokenHeader`, `TokenCarrierConfig` (`RouterConfig.Carrier`), `TokenCarrier`
- `core/session.go`: the `SessionManager` interface
- `httpapi/session.go`: `RequireSession`, `RequireFreshSession`
- `httpapi/authenticate.go`: `AuthenticateRequests`, the stage the router puts in front of every route (`Router.ApplyAuthentication`)
- `auth/authenticator.go`: `DefaultAuthenticator`, which resolves the token and applies the rolling expiration
- `core/auth-options.go`: `SessionConfig`, `WithDefaults`, `Validate`
- `core/signin.go`: `SignInResult`
- `httpapi/signin.go`: `WriteSignIn`
- `auth/session.go`: `DefaultSessionManager.Touch`
- `plugins/emailpassword/email-and-password.go`: `handleSignOut`, the one route that ends its caller's session
- `models/user.go`: the user's JSON

---

## Two directions, one setting

`RouterConfig.Carrier.Delivery` decides both how a sign-in delivers the token and where a request is expected to carry it. It also decides what a sign-out has to undo, and what has to follow when a session is extended.

| Delivery | `Write` | `Extract` reads | `Clear` | `Extend` |
| --- | --- | --- | --- | --- |
| `cookie` | `Set-Cookie` with `CookieName` | the cookie | `Set-Cookie` with `CookieName`, empty and expired, if the request carried the cookie | `Set-Cookie` with the same token and the new expiry |
| `header` | the `Set-Auth-Token` response header | `Authorization: Bearer` | nothing | nothing |
| `body` | nothing; returns `true` | `Authorization: Bearer` | nothing | nothing |
| `both` | the cookie and the header | the header, then the cookie | the expired cookie, if the request carried the token in it | the cookie, if the request carried the token in it |

`header` and `body` differ in delivery only. A client of either sends a bearer token, so `Extract` treats them alike.

## The carrier and the session manager

A session has two halves. The session manager (`SessionManager`) creates, resolves, extends, and revokes sessions, and takes a `context.Context`, never a request. The token carrier (`httpapi.TokenCarrier`) is the HTTP half: it moves the raw token between the response, the client, and the next request, and knows nothing of sessions beyond a token and an expiry.

| Step | Session manager | Token carrier |
| --- | --- | --- |
| sign-in | `Create` returns the session and its raw token | `Write` hands the token to the client |
| each request | `Get` resolves the token; `Touch` extends a session that is due | `Extract` reads the token; `Extend` writes the cookie again |
| sign-out | `Revoke` ends the session | `Clear` takes the token back |

The authentication stage and the sign-in and sign-out routes join the two. A transport other than HTTP would carry the token its own way and call the same session manager, through the authenticator.

The router builds the carrier from `RouterConfig.Carrier`, with its defaults applied, and hands it to every handler as `RequestContext.TokenCarrier`, as it hands over the `AuthContext`. `Boot` validates the settings first. It is a struct and not an interface: what it does follows from its settings alone, and it holds no state besides them.

## Who writes what

The carrier can set headers on the response, and a cookie is a header. It can't write the body: the route decides what the body is. That is why `Write` returns a value.

```go
Write(rctx *RequestContext, rawToken string, expiresAt time.Time) (inBody bool)
```

`WriteSignIn` is the one caller in core, and the one function a sign-in route calls:

1. Copies the plugin's extra fields into the body, minus the keys it owns (`user`, `token`, `status`).
2. Calls `Write` with the session's expiry. If it returns `true`, sets `body["token"]`.
3. Sets `status: "requires_second_factor"` for a pending session, and `user` otherwise.
4. Sets `Cache-Control: no-store` and writes the JSON with status `200`.

| Route | Calls |
| --- | --- |
| `POST /sign-in/email` | `httpapi.WriteSignIn(rctx, result, nil)` |
| `POST /magic-link/verify` | `httpapi.WriteSignIn(rctx, result.SignInResult, behemoth.M{"redirect_url": ...})` |
| `POST /oauth/complete` | `httpapi.WriteSignIn(rctx, result.SignInResult, behemoth.M{"redirect_url": ...})` |

A route that hands the client a value of its own reads `rctx.TokenCarrier.Config()`, the carrier's settings with their defaults, to deliver it the same way. The OAuth plugin's binding value is the one case ([`../plugins/oauth.md`](../plugins/oauth.md)).

A pending session's token is delivered like an active one's. The client needs it to present the second factor, and `SessionManager.Get` resolves a pending session by its token.

When the session passes its second factor, `SessionManager.Promote` gives it a new token ([ADR-0008](../decisions/0008-renewing-the-session-token-on-promotion.md)). The flow returns the token in a `SignInResult`, and its route answers with `WriteSignIn`. That delivers the token the configured way and, under the cookie deliveries, sets the cookie again with the active session's expiry: the pending cookie was set to expire with the pending session.

The pending token stops working at once. A request that still carries it finds no session, and the authentication stage takes the token back as it does for a session that is gone. Under the cookie deliveries, if that answer reaches the browser after the answer to the second factor, its `Set-Cookie` removes the new cookie, since both have the cookie's name. A client therefore sends the second factor once its other requests with the pending token have returned.

A flow called from code (`Plugin.SignIn`, `Plugin.Verify`) returns the raw token in its `SignInResult` and writes nothing. There is no response to write to.

## Taking the token back

Revoking a session ends it on the server. The client still holds the token. A client of the header or body delivery keeps the token in its own storage and discards it when it signs out. A browser with the cookie can't do that: the cookie is `HttpOnly`, so no script can remove it, and only a response can. Without one the browser sends the token of a revoked session with every request until the cookie expires.

```go
Clear(rctx *RequestContext)
```

`Clear` is the counterpart of `Write`. For the cookie deliveries it sets the session cookie again with no value, `Max-Age=0`, and an `Expires` in 1970:

```
Set-Cookie: session_token=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; HttpOnly; Secure; SameSite=Lax
```

- A browser replaces a cookie only with one of the same name, path, and domain. `Write`, `Extend`, and `Clear` therefore build the cookie in one function, `TokenCarrier.sessionCookie`, and differ in the value and the expiry.
- `HttpOnly` and `Path=/` are fixed. `Domain`, `SameSite`, and `Secure` come from `RouterConfig.Carrier.CookieDomain`, `CookieSameSite`, and `CookieInsecure`; the line above shows the defaults. `TokenCarrierConfig.Validate` refuses an unknown mode, a domain that is not a host name, and `SameSiteNone` without `Secure`. `Boot` checks the two that depend on `RouterConfig.BaseURL`: see [`../http/origin_check.md`](../http/origin_check.md), "The cookie and the base URL".
- Invariant: a changed `CookieDomain` does not reach cookies already in browsers. `Clear` then emits a cookie of the new domain, and the old one stays until it expires. Its token is refused once the session is gone.
- `Max-Age=0` is what current browsers act on. `Expires` is for a client that reads only that.
- It writes to the response and nothing else. It does not revoke.
- It removes the cookie for the request that presented its token in it (`TokenCarrier.cookieCarries`, which `Extend` uses too). Under `DeliveryBoth` the token is read from the `Authorization` header first, and the cookie next to it may be another session's, which is live. A request without the cookie gets no `Set-Cookie`.

It has two callers: a route that ended its caller's own session, and the authentication stage when the session a request carries is gone.

`[Convention, important]` **A route calls it only after the session has ended.** `POST /sign-out` is the one route that does:

1. The authentication stage resolves the caller's session.
2. `emailpassword.Plugin.SignOut` requires a signed-in caller (`Access.RequireUser`), fires the sign-out points, and revokes the caller's session.
3. `handleSignOut` calls `TokenCarrier.Clear` and answers `200`.

A sign-out that did not go through returns at step 2: a hook handler on `auth.signOut.before` refused it, or the revoke failed. The session is then still live, so the cookie stays. Clearing it would leave a live session that no client holds the token of.

`SignOut` does not call `Clear` itself: it is a flow, which takes a context and writes no response, and it also runs from code or another transport, where there is no cookie to remove.

**A session that ends in another request** leaves its cookie in the browser at first: one revoked from another device, by `RevokeAllForUser` (the email change revert), by eviction over `MaxConcurrent`, expired, or deleted with its user. No response went to that browser when it ended. The authentication stage takes the cookie back on the next request that carries it, whatever the route: see *What the session check answers* below. Until that request the cookie is a dead token that grants nothing.

## Following a rolling session

With `SessionConfig.UpdateAge` set, a session in use is extended: `Touch` moves `ExpiresAt` forward by `ExpiresIn` once less than `UpdateAge` is left. The cookie was written at sign-in with `Expires` set to the session's first expiry, and a browser drops a cookie at that date whatever the server thinks. Unless the cookie is written again, a cookie client is signed out `ExpiresIn` after its sign-in while its session is still live.

```go
Touch(ctx context.Context, session *models.Session) (*models.Session, error) // SessionManager
Extend(rctx *RequestContext, rawToken string, expiresAt time.Time) (written bool) // TokenCarrier
```

The authentication stage is where both are called, for every request that carries a token, whatever its route:

1. `Extract` reads the token, and `DefaultAuthenticator` resolves it with `Get`. A request without a usable session goes on as an anonymous caller: see *What the session check answers* below.
2. `Touch` is handed the validated session and decides whether an extension is due. It returns the session with its new expiry, or nil when it wrote nothing.
3. On an extension, the authenticator reports the extended session in `AuthOutcome.Extended`, and `Extend` sets the cookie again with the same token and the new `Expires`, and reports that it did. The extended session is the caller's session, so the handler reads the expiry the client now has.
4. The handler runs.
5. If the cookie was written, the stage sets `Cache-Control: no-store`.

- **Best effort.** A failed `Touch` is not an error of the request. The session is used as it was validated, and nothing is written.
- **A request that is not due costs nothing.** Steps 1 to 3 run on every authenticated request. `Touch` reads whether an extension is due off the session it is handed (`DefaultSessionManager.extensionDue`: less than `UpdateAge` is left, and never with `UpdateAge` at zero), and returns without touching the database when it is not. With a session cache, where `Get` is answered from the cache, such a request runs no statement at all. Without one it runs the read that validates the token.
- **When it is due, the row decides.** `Touch` then reads the row and checks again before it writes. The session it was handed may be a cached copy, and another request may have extended or revoked the session a moment earlier. A row that is no longer due, or is revoked, is left alone and `Touch` returns nil. The write is guarded on the version read ([ADR-0011](../decisions/0011-ordering-session-cache-writes-by-row-version.md)): a request that changed the row between the read and the write wins, and `Touch` returns nil too. The read and the write happen once per extension.
- **Only the cookie's own token.** `Extend` writes the cookie when the request carried the token in it. Under `DeliveryBoth` a request may present the token in the `Authorization` header, which `Extract` reads first, while its cookie holds another session's token. Writing the cookie then would replace that other session's token in the browser.
- **The header and body deliveries need nothing.** Their clients keep the token without a date, and the server alone decides when it ends. `Extend` returns false, and no `Set-Auth-Token` is sent: that header is for a sign-in.
- **`no-store` is set after the handler, and overrides it.** The response carries the session token. A handler that marks its responses cacheable does so for the content it wrote and does not know the stage added a credential. A shared cache that stored such a response would hand the token to its next reader. `WriteSignIn` marks its response the same way. This affects the one response per extension.

`[Implementation Detail]` **One `Set-Cookie` per response.** The sign-out route is authenticated like every route. A sign-out of a session that is due would queue two cookies: the extended one from step 3 and the expired one from `Clear`. `TokenCarrier.setCookie`, which all three writers use, removes a session cookie already queued on the recorder before it adds the new one, so the last write is the only one sent. A response should not set one cookie twice (RFC 6265, section 4.1.1), and here the two would contradict each other.

## What the session check answers

The authentication stage resolves the token of every request, and a request without a usable session goes on as an anonymous caller that keeps the reason. `RequireSession` writes no response. It returns that reason, and the router maps it like a handler's (`wrapWithErrorMapping`, see [`../http/request_routing.md`](../http/request_routing.md)): the status from the error's category, the public message and the code in the body, one log line.

| What happened | The error `RequireSession` returns | The router answers | `Clear` |
| --- | --- | --- | --- |
| `Extract` found no token | a session error, `session_missing` | `401 {"error": "missing session token", "code": "session_missing"}` | not called |
| `Get` found no session for the token (not found) | a session error, `session_invalid`, wrapping the not-found error | `401 {"error": "invalid session", "code": "session_invalid"}` | called |
| the session is revoked or expired | `Get`'s session error, unchanged | `401` with `session_revoked` or `session_expired` | called |
| the session waits for a second factor | a session error, `session_pending` | `401` with `session_pending` | not called |
| anything else went wrong in `Get`: the database, a key | that error, unchanged | `500` with a generic message; logged at Error | not called |

The error is the caller's `AuthError`, which the authenticator set; for a request without a token it is nil, and `RequireSession` names `session_missing` itself. The stage calls `Clear` before the route runs, whatever the route, when the authenticator reports `AuthOutcome.Ended`.

- **Not found is translated, once.** To `SessionManager.Get`, a token that resolves to nothing is a not-found error, and the router answers that category with `404`. To a route that needs a session the same token is a request without one. The authenticator is the place that knows which of the two it is, so it wraps the error there and `Get` keeps answering a lookup as a lookup.
- **A failure is not a refusal.** A `401` tells a client that its session is gone, and a client acts on that by signing the user out. When the database is down the session may be perfectly good, so the error passes through and becomes a `500`, which also puts it in the log at Error and marks the request's span as failed.
- **A session that is gone loses its cookie.** Invalid, revoked, and expired are final (`sessionIsGone` in `auth/authenticator.go`), so the authenticator reports `Ended` and the stage calls `Clear` before the route runs. The `Set-Cookie` is on the recorder by then, and the route's answer, or the router's mapping of `RequireSession`'s error, goes out in the same response. A pending session is still live and needs its cookie to present the second factor. An error that is not one of the three says the lookup failed, not what became of the session, so nothing is taken from the client.
- **`RequireFreshSession`** adds one more refusal after these, `session_not_fresh`, also a session error. The session is live, so the cookie stays.

## Defaults

The settings are in two places. `SessionConfig` (`BootConfig.Session`) is the session manager's: how long a session lasts and when it is extended. `TokenCarrierConfig` (`RouterConfig.Carrier`) is the carrier's: where the token travels over HTTP, and the cookie. Each has a `WithDefaults` that fills the fields below when they are left at zero.

`NewSessionManager` applies `SessionConfig.WithDefaults`, and `NewTokenCarrier` applies `TokenCarrierConfig.WithDefaults`. `AuthContext.SessionConfig` and `RequestContext.TokenCarrier.Config()` hold the values with their defaults. `BootConfig`, read anywhere else, may still hold zeros.

| Field | Default |
| --- | --- |
| `SessionConfig.ExpiresIn` | 7 days |
| `SessionConfig.PendingExpiresIn` | 10 minutes |
| `SessionConfig.FreshAge` | 15 minutes |
| `TokenCarrierConfig.Delivery` | `cookie` |
| `TokenCarrierConfig.CookieName` | `session_token` |
| `TokenCarrierConfig.CookieSameSite` | `lax` |

`UpdateAge` is not in the table. Its comment used to name a default of one day, which was never applied, and zero has a working meaning: `Touch` then never extends a session. Applying a day would change that for every application that left it unset, and with an `ExpiresIn` of a day or less it would extend the session on every request. `docs/ongoing.md` has the entry.

Each `Validate` reports what no default can repair, and `Boot` returns its error as a configuration error:

- `SessionConfig.Validate`: a negative duration.
- `TokenCarrierConfig.Validate`: a `Delivery` that is not one of the four values, a `CookieSameSite` that is not one of the three modes, `SameSiteNone` with `CookieInsecure`, and a `CookieDomain` that is not a host name. Its messages name the field as `RouterConfig.Carrier.<Field>`.

Before the defaults were applied, the comments on the fields named defaults that nothing applied: a zero delivery delivered and accepted no token, a zero `ExpiresIn` or `PendingExpiresIn` made a session expire the moment it was created, and the cookie name was not read.

## The user as JSON

A response carries a user as `AuthContext.Public.Of(user)`: a flat map of the user's public columns, keyed by column name, so it says `email_verified` and `image_url` as a sign-up request does. A column a plugin contributed is in it only if the plugin declared it public. [`../models/models.md`](../models/models.md) has the mechanism under *What a client sees: public columns*.

`models.User` also has `json` tags equal to its column names, for code that decodes a response into the model. Encoding the model directly is not what the routes do: it would carry every contributed column under `extra`.

---

## Tests

| Test | File | Covers |
| --- | --- | --- |
| `TestSessionConfigDefaults`, `TestSessionConfigValidate` | `core/auth_options_test.go` | every default, a set field kept, what `Validate` refuses |
| `TestTokenCarrierConfigDefaults`, `TestTokenCarrierConfigValidate` | `httpapi/token_carrier_test.go` | the same for the carrier's settings, and that `NewTokenCarrier` applies the defaults |
| `TestTokenCarrierDeliveries` | same | per delivery: what `Write` sets and returns, the cookie's name and expiry, what `Extract` accepts, and that `Clear` expires that same cookie for the request that carried it, and writes nothing for a request without the cookie or with another session's |
| `TestTokenCarrierCookieAttributes` | same | `Domain`, `SameSite`, and `Secure` from the settings, on the cookie and on its removal; `HttpOnly` and the path fixed |
| `TestTokenCarrierExtend` | same | the cookie written again with the same token and the new date; nothing for the header and body deliveries, a bearer token next to another session's cookie, or a request without the cookie |
| `TestTokenCarrierSetsTheCookieOnce` | same | an extension and a removal in one request send one session cookie, the removal, and leave other cookies alone |
| `TestSessionManagerWithAZeroConfig` | `tests/store/managers_test.go` | a zero config: session lifetimes, a session that validates |
| `TestSignInDeliversTheTokenByDelivery` | `tests/plugins/signin_response_test.go` | through the routes, per delivery: where the token arrives and that it arrives nowhere else, the body's shape, the user's keys, `no-store`, sign-out with the token where the client sends it, and that the sign-out response removes the cookie |
| `TestRefusedSignOutKeepsTheCookie` | same | a sign-out a hook handler refused sets no cookie, and the session still validates |
| `TestSessionTouchExtendsExpiry` | `tests/store/managers_test.go` | `Touch` returns the session it extended, and nil when no extension was due |
| `TestSessionTouchDecidesFromTheSessionItIsGiven` | same | a session that is not due is not read (one that was never stored shows it); a copy that says due is checked against the row, which may be extended already or revoked |
| `TestRequireSessionReadsNoMoreThanItNeeds` | same | statements run by one request through `RequireSession`, counted from the adapter's debug log: none with a session cache, one without, with rolling expiration off and on |
| `TestRequireSessionExtendsTheCookieOfARollingSession` | same | through `RequireSession`: the cookie comes back with the extended session's expiry and the same token, the handler sees the extended session, `no-store` overrides the handler's `Cache-Control`; nothing is written when the session is not due, for the header delivery, or when a bearer token's cookie belongs to another session |
| `TestSignOutOfARollingSessionSetsTheCookieOnce` | `tests/plugins/signin_response_test.go` | a sign-out of a session that is due answers with one `Set-Cookie`, the removal |
| `TestRequireSessionTakesBackTheCookieOfASessionThatIsGone` | `tests/store/managers_test.go` | the refusal removes the cookie of a revoked, an expired and a never-issued token; it does not for a pending session, a missing token, a session that is only not fresh, a dead bearer token next to a live cookie, or a failed lookup |
| `TestASessionEndedElsewhereLosesItsCookie` | `tests/plugins/signin_response_test.go` | through the router: after `RevokeAllForUser`, the next request with the cookie gets `401 session_revoked` and the removal; the one after is `session_missing` |
| `TestRequireSessionRefusesWithTypedErrors` | `tests/store/managers_test.go` | each refusal is a session error with its code and public message and writes no response; a database failure is passed on as a database error and maps to `500` without its text |
| `TestSessionCheckThroughTheRouter` | `tests/plugins/emailpassword_test.go` | through the router: `401` with message and code for a missing and an unknown token, nothing logged at Error; with the database closed, `500` without the error's text, logged once as a failed request |
| `TestSignInPendingSecondFactorCarriesTheToken` | `tests/plugins/signin_response_test.go` | the pending body, and its token by header and by body |
| `TestBootRefusesAnUnknownTokenDelivery` | same | the configuration error, which names `RouterConfig.Carrier.Delivery` |
| `TestMagicLinkRoutes` | `tests/plugins/magiclink_test.go` | the verify route delivers the token like the password route |

---

## Design decisions

The decisions written before the token carrier existed use the names of that time. `SessionConfig.Transport` and its values `TransportCookie`, `TransportHeader`, `TransportBody`, and `TransportBoth` are now `RouterConfig.Carrier.Delivery` and `DeliveryCookie`, `DeliveryHeader`, `DeliveryBody`, and `DeliveryBoth`. The session manager's `WriteToken`, `ExtractToken`, `ClearToken`, and `ExtendToken` are now `TokenCarrier.Write`, `Extract`, `Clear`, and `Extend`. The first two decisions below explain the move.

### The token travels with a carrier of its own, not the session manager

**Context:** `SessionManager` had four methods that take HTTP types: `WriteToken`, `ExtractToken`, `ClearToken`, and `ExtendToken`. `SessionConfig` held the delivery and the cookie's attributes next to the session lifetimes. A job or another transport that uses the session manager got an interface tied to HTTP, against the guideline that a method on a core interface takes a context ([`../guidelines/transport_neutral_flows.md`](../guidelines/transport_neutral_flows.md)). The router's origin check had to read the session settings to learn whether a cookie is in use.

**Options considered:**

- *Keep the methods on the session manager.* No change. The interface stays tied to HTTP, and package `auth` keeps importing `net/http` for them.
- *A `TokenCarrier` interface, implemented in `auth`.* An application could replace it. What it does follows from its settings alone, and nothing calls for a second implementation.
- *A `TokenCarrier` struct in the HTTP layer, built from a section of `RouterConfig`.* The settings sit with the router, which also reads them for the origin check. The type moves with the HTTP types when they get a package of their own ([`../plans/package_layout.md`](../plans/package_layout.md)).

**Decision:** the struct, `httpapi.TokenCarrier`, built from `RouterConfig.Carrier`. It was put on `AuthContext.TokenCarrier` at first; when the HTTP types left `core`, the router took it over and hands it to every handler as `RequestContext.TokenCarrier`. The delivery and the cookie's settings moved from `SessionConfig` to `TokenCarrierConfig`. The methods take the session's expiry instead of a `*models.Session`, which was all they read of it. `SessionManager` has no method that takes an HTTP type.

**Revisit if:** an application needs to carry the token in a way the four deliveries and the cookie's settings can't express, such as another header name. An interface would then be the place to extend.

### `TokenTransport` is `TokenDelivery`

**Context:** In the rest of the code a transport is HTTP, gRPC, or a WebSocket: `identity.Peer.Transport` says which one a call came over, and the guideline on flows is about transports in that sense. The setting chose among four places in an HTTP message.

**Options considered:**

- *Keep `TokenTransport`.* No rename. One word with two meanings in code that sits side by side: the authentication stage sets `Peer.Transport` to `"http"` and reads the token per the "transport".
- *`TokenDelivery`.* It names what the setting chooses: where the token is delivered, and where it comes back.

**Decision:** `TokenDelivery`, with `DeliveryCookie`, `DeliveryHeader`, `DeliveryBody`, and `DeliveryBoth`, set in `RouterConfig.Carrier.Delivery`. The values are the same strings as before.

### `TransportHeader` delivers the token in a response header

**Context:** With the header transport, `WriteToken` put the token in `rctx.Values["token"]` and nothing read it. A client signed in and never received its token.

**Options considered:**

- *The routes add the token to their JSON body.* It is where API clients look first. The setting says "header", and the application that chose it would get something else.
- *A response header.* It is what the setting names, and it mirrors the cookie: the server sets a credential in a header and the client stores it. A cross-origin browser script reads it only if CORS exposes it.
- *The router merges the token into JSON bodies.* No route has to remember. The body is already serialized by then, the merge works only for JSON objects, and for a route that answered with a bare user it would put `token` among the user's columns.

**Decision:** The header, named `Set-Auth-Token`. Whether a header is the right place for a given client is the application's decision, and it made it by choosing the transport. The transport now names both directions, where before it described only how the client sends the token.

**Revisit if:** applications need another header name. It is a constant, `httpapi.SessionTokenHeader`.

### The body is a transport of its own

**Context:** Delivering the token in the JSON body is still what some clients want.

**Options considered:**

- *Always put it in the body as well.* Convenient. Under the cookie transport it would hand scripts the token an `HttpOnly` cookie keeps from them.
- *A fourth transport, `TransportBody`.* The application asks for it. It needs the routes' cooperation, because the session manager does not write bodies.

**Decision:** `TransportBody`. `WriteToken` returns whether the token belongs in the body, and `WriteSignIn` acts on it. A returned value is visible in the signature; the value parked in `rctx.Values` that this replaces was not, and nothing read it.

**Revisit if:** an application wants the body together with the cookie or the header. `Transport` would then become a set of places and not one of four names.

### The sign-out route takes the cookie back, through the session manager

**Context:** `POST /sign-out` revoked the session and answered without a `Set-Cookie` header. The browser kept an `HttpOnly` cookie it has no way to drop, and sent a revoked token with every request until the cookie expired.

**Options considered:**

- *The route writes the expired cookie itself.* No change to an interface. The route would need the cookie's name and attributes, and the only copy of `SessionConfig` with its defaults applied is the session manager's. Every later route that ends a session would repeat it.
- *`SignOut` clears it.* One place, and every caller of the flow gets it. `SignOut` ends a session by id, which need not be the caller's, and it runs without a request.
- *`RequireSession` clears the token whenever `Validate` fails.* It would also cover sessions that ended elsewhere. `Validate` also fails for a session waiting for a second factor, and for every session while the database is down; both would lose a cookie they still need. With `TransportBoth` the refused token may have come from the `Authorization` header while the cookie holds another, live one.
- *`SessionManager.ClearToken(rctx)`, called by the route.* The manager knows the transport and the cookie, as it does for `WriteToken`. The route knows that the session is the caller's and that it has ended. It adds a method to the `SessionManager` interface, whose only implementation is the one `Boot` builds.

**Decision:** `ClearToken` on the session manager, called by `handleSignOut` after `SignOut` returned without an error.

**Revisit if:** a route ends sessions other than its caller's and wants to remove a cookie anyway. `ClearToken` acts on the token the request presented, and would need to be told otherwise.

### The session check removes the cookie of a session that is gone

**Context:** Sign-out removes the cookie because the response goes to the browser that signed out. A session that ends in another request (revoked elsewhere, expired, deleted with its user) has no response to that browser. Its `HttpOnly` cookie stayed until it expired or a sign-in replaced it, and went out with every request in between.

**Options considered:**

- *Leave it.* The token is dead and grants nothing. The browser keeps sending it, and a sign-out from that browser is refused and cannot remove it either.
- *Remove the cookie where the session ends.* `Revoke` and `RevokeAllForUser` run in the request of whoever ended the session, a different client or no client at all, so there is nothing to write to.
- *Remove it on any error of `Validate`.* A pending session and a failed lookup are errors too, and both clients still need their cookie.
- *Remove it in `RequireSession` when the error says the session is gone.* The next request that carries the dead cookie cleans it up. It needs the errors to be told apart, which the typed refusals now allow, and it needs to know that the dead token is the one in the cookie.

**Decision:** the last. `RequireSession` calls `ClearToken` for `session_invalid`, `session_revoked`, and `session_expired`. `ClearToken` removes the cookie only for the request that presented its token in it, which also made the sign-out route exact under `TransportBoth`: a sign-out with a bearer token no longer removes a cookie that belongs to another session.

**Revisit if:** sessions are read from a store that can lag behind its writes, such as a read replica. A session created a moment ago could then be reported as not found, and the refusal would remove a cookie that is good. `session_invalid` would have to leave the cookie alone there; revoked and expired are read off a row and are not affected.

### The cookie is written again when the session is extended

**Context:** `Touch` extended a rolling session in the database and the cache. It takes a context and has no response, so the cookie kept the date of its sign-in. For a cookie client `UpdateAge` had no effect: the browser dropped the cookie `ExpiresIn` after the sign-in.

**Options considered:**

- *Document that rolling expiration does not apply to the cookie transport.* No code. The cookie is the default transport, so the setting would not work where most applications use it.
- *A session cookie without a date*, which the browser keeps until it closes, with the server alone deciding the expiry. It removes the mismatch for good. It changes how long a sign-in survives for every application: closing the browser would sign the user out, and browsers that restore sessions keep such cookies indefinitely.
- *Write the cookie on every request through `RequireSession`.* No coupling to `Touch`. Every response of every protected route would carry the token and would have to be kept out of caches.
- *Have `Touch` write it.* One call for the middleware. `Touch` would need the request and the raw token, and persistence and response writing are separate elsewhere in the manager (`Create` and `WriteToken`, `Revoke`, and `ClearToken`).
- *Call `WriteToken` again.* No new method. It also sets `Set-Auth-Token` under the header transports and reports a token for the body, which are answers to a sign-in.
- *`Touch` reports the extension, and a new `ExtendToken` writes the cookie.* Two changes to the interface. Each method keeps one job, and the cookie is written only on the request that extended the session.

**Decision:** the last. `Touch` returns the extended session or nil, and `RequireSession` passes it to `ExtendToken`.

**Revisit if:** something other than `RequireSession` has to extend sessions. `Touch` and `ExtendToken` are always called together today, and a second caller would be the reason to fold them into one call.

### `Touch` decides from the session it is given

**Context:** `Touch(ctx, sessionID)` began by reading the session row to learn its expiry, and only then checked whether an extension was due. `RequireSession` calls it on every authenticated request, so every such request read the row: also with a session cache, which exists to avoid that read, and with `UpdateAge` at zero, where nothing is ever due. The caller already held the session, from `Validate`.

**Options considered:**

- *Return early when `UpdateAge` is zero.* One line. An application that turns rolling expiration on gets the read back on every request.
- *Take the session and decide from it alone, writing without a read.* The fewest statements. A cached copy can be older than the row: two requests at once would both write, and a session revoked a moment earlier would have its expiry written again.
- *Take the session, decide from it, and read the row only when it says an extension is due.* A request that is not due costs nothing. The rare request that is due pays the read the old code paid every time, and keeps its checks against the row.

**Decision:** the last. The signature becomes `Touch(ctx, session)`.

**Revisit if:** the store gains a conditional update (extend where the row is still due and not revoked), which would make the read in the due case unnecessary.

### `RequireSession` returns its refusals

**Context:** The middleware wrote its own response for every error of `Validate`: `401` with `err.Error()` as the message. For a typed error that is the internal message. With the database closed, a route behind it answered `401 {"error":"SessionManager.Get: sql: database is closed"}`. Internal text reached the client, a failure of the system looked like a session that had ended, and nothing was logged at Error.

**Options considered:**

- *Keep writing the response, with the public message and a status chosen in the middleware.* The middleware would repeat what the error mapper does, and an application's `RouterConfig.ErrorMapper` would not apply to these responses.
- *Return every error of `Validate` unchanged.* The least code. An unknown token is a not-found error, which the mapper answers with `404`: a protected route would answer `404` to a bad token.
- *Have `SessionManager.Get` return a session error for an unknown token.* One place for every caller. `Get` is a lookup, and its other callers (a second-factor plugin resolving a pending session, the metrics that count lookups by reason) rely on not found meaning not found.
- *Return the errors, and translate not found in the middleware.* Refusals become session errors, failures pass through, and the router does the rest.

**Decision:** the last. Two codes were added for the cases the middleware names itself: `session_missing` and `session_invalid`.

**Revisit if:** a route needs to tell an unauthenticated visitor from a failed lookup without the router, such as a middleware that lets anonymous requests through. It would then want a helper that returns the session or the reason, and `RequireSession` would be built on it.

### One body for every sign-in route

**Context:** The email/password sign-in answered with the bare user, and the magic link verify with `{"user": ..., "redirect_url": ...}`. A token in the body needs an object to sit in.

**Options considered:**

- *Wrap only under the body transport.* No change for existing clients. The shape of a response would depend on a server setting.
- *Always `{"user": ...}`.* One shape for every plugin and transport. It changes the email/password responses.

**Decision:** Always the wrapper, written by `WriteSignIn` so that a new sign-in plugin gets it without repeating it. Sign-up answers `{"user": ...}` too, so a client parses a user the same way wherever it gets one.

### The session manager applies the defaults

**Context:** `SessionConfig` named defaults in comments, and each use of a field read the raw value.

**Options considered:**

- *A default at each use.* Local. Easy to miss one, which is what had happened for every field.
- *Normalize once, where the manager is built.* One function to read and test.
- *Require every field.* No hidden values. A zero-value config, which the rest of `BootConfig` supports, would stop working.

**Decision:** `WithDefaults`, called by `NewSessionManager`, so a manager built outside `Boot` (a test, a worker) gets the same values. It covers the fields whose zero value did not work. `UpdateAge` is left alone, because its zero value does. `Boot` calls `Validate` first, because a misspelled transport should stop the process and not quietly become the cookie.

### Users are keyed by column name in JSON

**Context:** The routes encode `models.User` directly. Without `json` tags the keys were Go field names (`EmailVerified`, `ImageUrl`), while requests use column names.

**Options considered:**

- *`json` tags equal to the column names.* Small, and symmetric: a test or a client can decode a response into `models.User`.
- *Answer with `user.ToMap()`.* Flat, with contributed columns next to core ones. Decoding back into the model would need a custom unmarshaller for the timestamps.

**Decision:** The tags, at first. The routes have since moved to the public view (see `models.md`), which is `ToMap` reduced to the public columns, with values normalized for JSON. It needs no unmarshaller, because nothing decodes it back on the server. The tags remain so that a client or a test written in Go can decode a response into `models.User`.
