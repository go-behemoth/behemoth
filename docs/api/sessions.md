# Sessions

A sign-in creates a session and gives the client a token for it. The client sends the token with later requests, and Behemoth resolves it back to the session. This page covers the session settings, how the token travels, and what a sign-in route answers with.

## Configuration

Two settings shape sessions. `BootConfig.Session` says how long a session lasts. `BootConfig.Router.Carrier` says how its token travels over HTTP, and sets the cookie's attributes. Every field of both is optional.

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Session: core.SessionConfig{ExpiresIn: 24 * time.Hour},
	Router: httpapi.RouterConfig{
		Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader},
	},
	// ...
})
```

`BootConfig.Session`, a `core.SessionConfig`:

| Field | Default | Meaning |
| --- | --- | --- |
| `ExpiresIn` | 7 days | How long a session lasts. |
| `PendingExpiresIn` | 10 minutes | How long a session lasts while it waits for a second factor. |
| `UpdateAge` | 0, off | Rolling expiration: a session in use is extended by `ExpiresIn` once less than `UpdateAge` is left of it. At zero a session ends `ExpiresIn` after its sign-in, however much it is used. Keep it well below `ExpiresIn`: a value that is not smaller extends the session, with a database write, on every request. See [Rolling expiration](#rolling-expiration). |
| `FreshAge` | 15 minutes | How long after a sign-in a session counts as fresh. See [Fresh sessions](#fresh-sessions). |
| `MaxConcurrent` | 0, no limit | Sessions a user can use at once: active sessions that have not expired. A session that waits for a second factor doesn't count. |
| `EvictOldestOnLimit` | false | At the limit, end the user's oldest active session to make room. A sign-in that waits for a second factor makes room once the second factor passes. When false, a sign-in at the limit is refused with `401` and the code `session_limit_reached`, before its second factor when it needs one. |
| `CaptureIPAndAgent` | false | Record the client's IP address and user agent on the session. |
| `UnversionedCacheTTL` | 1 minute | How long a session stays in the session cache when the key-value storage can't order the cache's writes. See [The session cache](#the-session-cache). |

`BootConfig.Router.Carrier`, a `httpapi.TokenCarrierConfig`:

| Field | Default | Meaning |
| --- | --- | --- |
| `Delivery` | `httpapi.DeliveryCookie` | Where the token travels. See [Token delivery](#token-delivery). |
| `CookieName` | `session_token` | The cookie's name, for the deliveries that use one. |
| `CookieDomain` | empty | The cookie's `Domain` attribute, a host name such as `example.com`. Empty sends the cookie only to the host that set it. See [Cookie attributes](#cookie-attributes). |
| `CookieSameSite` | `httpapi.SameSiteLax` | The cookie's `SameSite` attribute: `httpapi.SameSiteLax`, `httpapi.SameSiteStrict`, or `httpapi.SameSiteNone`. |
| `CookieInsecure` | false | Leaves `Secure` off the cookie, for development over plain HTTP. |

A negative duration, a `Delivery` that is not one of the four values, and a cookie setting that can't work are configuration errors from `Boot`. See [Cookie attributes](#cookie-attributes) for the cookie settings `Boot` refuses.

### The session cache

With a key-value storage in `BootConfig.KV`, the session manager keeps a copy of each session there, and checks a request's token against the copy without a database query. Each change of the session goes to the copy too: an extension, a second factor, a revoke, and the deletion of the session's user.

Two requests can write copies of one session at nearly the same time. One of them may be a request that read the session just before another request revoked it. Its copy is older, and must not replace the revoked one. Behemoth orders the copies by the session's `version` column, which every update of the session raises by one:

- **A storage that implements `core.VersionedCache` keeps the latest copy.** It writes a copy only when the copy's version is later than the one it holds, whatever order the copies arrive in. A revoked session stays revoked in the cache, and a session stays in the cache for its whole life. The Redis adapter implements `core.VersionedCache`.
- **A storage that implements only `behemoth.KeyValueStorage` keeps the last copy written.** The session manager then keeps each copy for `UnversionedCacheTTL` at most. A revoke that races a request's cache write can leave the session valid from the cache for that long.

If you bring your own key-value storage, implement `core.VersionedCache` on it. `SetIfNewer` must compare the versions and write the value as one atomic step, as the Redis adapter does with a Lua script. A comparison followed by a separate write leaves the race open.

The `version` column is new. Generate and apply a migration before you deploy a version of Behemoth that has it. The sessions that exist get version 0. On MongoDB, a session stored without the field reads as version 0 as well, and needs no migration.

## Token delivery

The delivery names where the token travels in both directions: how a sign-in hands it to the client, and where the client's requests carry it.

| `Delivery` | The sign-in response has the token in | A request carries it in |
| --- | --- | --- |
| `httpapi.DeliveryCookie` | a `Set-Cookie` header | the cookie |
| `httpapi.DeliveryHeader` | the `Set-Auth-Token` header | `Authorization: Bearer <token>` |
| `httpapi.DeliveryBody` | the JSON body, under `token` | `Authorization: Bearer <token>` |
| `httpapi.DeliveryBoth` | the cookie and `Set-Auth-Token` | either |

**Cookie.** The browser stores the token and sends it by itself. By default the cookie is `HttpOnly`, `Secure`, `SameSite=Lax`, with the path `/` and no `Domain`, and expires with the session. See [Cookie attributes](#cookie-attributes) to change the domain, `SameSite`, or `Secure`. When [rolling expiration](#rolling-expiration) extends the session, the cookie is set again with the new date. Scripts on the page can't read it, and can't delete it either: the sign-out response removes it. See [Sign out](#sign-out). This is the choice for a browser application served from the same site as the API. Behemoth checks where each state-changing request comes from under this delivery. See [Cross-site requests](#cross-site-requests).

**Header.** The client reads the token from `Set-Auth-Token`, keeps it, and sends it as a bearer token. This suits mobile apps, command-line tools, and other servers.

```bash
curl -i -X POST localhost:8080/api/auth/sign-in/email -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct horse"}'
# HTTP/1.1 200 OK
# Set-Auth-Token: r8_x6fau4053kWPOukmnuUFxoY8iz...
# {"user":{"id":"8b41...","email":"ada@example.com", ...}}

curl -X POST localhost:8080/api/auth/sign-out -H 'Authorization: Bearer r8_x6fau4053kWPOukmnuUFxoY8iz...'
```

A browser script on another origin can read a response header only when your CORS configuration exposes it. With the header delivery and a browser client, add `Set-Auth-Token` to `Access-Control-Expose-Headers`. Without it the sign-in succeeds and the script sees no token.

**Body.** The token is a field of the sign-in response's JSON. The client sends it back the same way as with the header delivery, as a bearer token. Only the way the sign-in hands it over differs. Choose it when reading a response header is awkward for your client, for example a cross-origin browser script.

```json
{"user": {"id": "8b41...", "email": "ada@example.com"}, "token": "r8_x6fau4053kWPOukmnuUFxoY8iz..."}
```

**Both.** A sign-in sets the cookie and the header, and a request may carry either. It is for an application with browser and non-browser clients, or one moving from one to the other.

There is no combination with the body. A token is never read from a request's body.

### Cookie attributes

`HttpOnly` and the path `/` are fixed. Three fields of `RouterConfig.Carrier` set the rest:

| Your setup | Set |
| --- | --- |
| The front end and Behemoth share one host | nothing |
| A server-rendered front end on a sibling subdomain, such as `app.example.com` next to `auth.example.com`, has to receive the cookie | `CookieDomain: "example.com"` |
| The front end is on another site, such as `app.example.org` | `CookieSameSite: httpapi.SameSiteNone`, with `RouterConfig.BaseURL` set and the front end in `RouterConfig.TrustedOrigins` |
| Development over plain HTTP on a host other than `localhost` | `CookieInsecure: true` |

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Router: httpapi.RouterConfig{
		BaseURL: "https://auth.example.com",
		Carrier: httpapi.TokenCarrierConfig{
			CookieDomain:   "example.com",
			CookieSameSite: httpapi.SameSiteStrict,
		},
	},
	// ...
})
```

`Boot` returns a configuration error for these settings:

- A `CookieSameSite` that is not one of the three values.
- A `CookieDomain` that is not a host name. A leading dot is accepted and ignored.
- `httpapi.SameSiteNone` with `CookieInsecure`. Browsers accept `SameSite=None` on a `Secure` cookie only.
- `httpapi.SameSiteNone` under a cookie delivery without `RouterConfig.BaseURL`. The browser then sends the cookie with cross-site requests, and the [origin check](#cross-site-requests) is what refuses a forged one.
- A `CookieDomain` that is neither the host of `RouterConfig.BaseURL` nor a parent of it. A browser drops such a cookie.

With `httpapi.SameSiteStrict`, a user who follows a link from another site to your application arrives without the cookie, and appears signed out until the next navigation.

Changing `CookieDomain` on a running deployment does not replace the cookies browsers already hold. A browser keeps the old cookie next to the new one until it expires, and users may have to sign in again.

## The sign-in response

Every route that signs a user in answers with the same JSON, whichever plugin it belongs to:

| Case | Body |
| --- | --- |
| signed in | `{"user": {...}}` |
| a second factor is pending | `{"status": "requires_second_factor"}` |
| either, with `httpapi.DeliveryBody` | the same, plus `"token": "..."` |

The response follows these rules:

- A plugin can add fields of its own next to these. The magic link plugin adds `redirect_url`.
- A pending session's token is delivered like any other. The client presents it together with the second factor, which the [two-factor plugin](twofactor.md#sign-in-with-a-second-factor) checks.
- When the second factor passes, the answer carries a new token, delivered the same way, and the pending token stops working. Send the second factor once the client's other requests with the pending token have returned: under the cookie deliveries, a late answer to such a request would remove the new cookie.
- The response has `Cache-Control: no-store`.
- Sign-up answers `{"user": {...}}` too. It creates no session, so it has no token.

### The user

A user is keyed by its column names, the same names a request uses:

```json
{
  "id": "8b41bcc4-81f3-46a4-920a-9326d9cb01a1",
  "email": "ada@example.com",
  "firstname": "Ada",
  "lastname": "",
  "email_verified": false,
  "image_url": "",
  "created_at": "2026-10-09T09:21:25.186568934Z",
  "updated_at": "2026-10-09T09:21:25.186568934Z"
}
```

These are the public columns of `users`. A column a plugin added to the table appears next to them, under its own name, if the plugin declared it public, and not at all otherwise. See [What a client sees of a user](core-tables.md#what-a-client-sees-of-a-user).

## Rolling expiration

By default a session ends `ExpiresIn` after its sign-in, however much it is used. Set `UpdateAge` to keep an active user signed in: a session that is used when less than `UpdateAge` is left of it is extended by `ExpiresIn` from that moment.

```go
Session: core.SessionConfig{
	ExpiresIn: 7 * 24 * time.Hour,
	UpdateAge: 24 * time.Hour, // used in its last day, a session gets seven more
},
```

A session is "used" by any request to one of Behemoth's routes that carries its token: the router authenticates every request before the route runs, and that is where the extension happens. A request that carries no token extends nothing.

Only the request that crosses the threshold writes to the database. The others cost nothing extra: with a session cache (`BootConfig.KV`), a request whose session is not due is checked without a database query.

What the client sees depends on the delivery:

| `Delivery` | The response of the request that extended the session | What the client does |
| --- | --- | --- |
| `httpapi.DeliveryCookie` | sets the cookie again, with the same token and the new expiry | nothing |
| `httpapi.DeliveryHeader`, `httpapi.DeliveryBody` | has nothing to add | nothing. The client keeps the token without a date, and the server decides when it ends. |
| `httpapi.DeliveryBoth` | sets the cookie again if the request carried the token in the cookie | nothing |

For a route of yours, here `/me`, and a session that is due:

```bash
curl -i localhost:8080/api/auth/me --cookie 'session_token=r8_x6fau4053kWPOukmnuUFxoY8iz...'
# HTTP/1.1 200 OK
# Cache-Control: no-store
# Set-Cookie: session_token=r8_x6fau4053kWPOukmnuUFxoY8iz...; Path=/; Expires=Fri, 16 Oct 2026 18:16:16 GMT; HttpOnly; Secure; SameSite=Lax
```

The extension behaves as follows:

- **The response that sets the cookie is marked `Cache-Control: no-store`**, also when your handler set another value. It carries the session token, and a cache that stored it could hand the token to someone else. This affects one response per extension, not every response of the route.
- The token does not change. Only the cookie's date does.
- `FreshAge` is not affected. An extended session is no fresher than before. See [Fresh sessions](#fresh-sessions).

## Sign out

`POST /sign-out` of the `emailpassword` plugin revokes the session the request carries. What happens to the token on the client depends on the delivery:

| `Delivery` | The sign-out response | What the client does |
| --- | --- | --- |
| `httpapi.DeliveryCookie` | replaces the cookie with an empty one that has expired, so the browser drops it | nothing |
| `httpapi.DeliveryHeader`, `httpapi.DeliveryBody` | has nothing to remove | discards the token it kept |
| `httpapi.DeliveryBoth` | removes the cookie, if the request carried its token in the cookie | discards the token if it kept one from the header |

```bash
curl -i -X POST localhost:8080/api/auth/sign-out --cookie 'session_token=r8_x6fau4053kWPOukmnuUFxoY8iz...'
# HTTP/1.1 200 OK
# Set-Cookie: session_token=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; HttpOnly; Secure; SameSite=Lax
# {"status":"signed_out"}
```

The cookie is removed when the sign-out went through. If a hook handler refuses the sign-out, the session is still live and the cookie stays.

A session can also end without a sign-out from that browser: it was revoked from another device, ended with all the user's sessions, or expired. The browser still holds the cookie. The next request it sends to a route behind `httpapi.RequireSession` is refused with `401`, and that response removes the cookie. See [Protect your own routes](#protect-your-own-routes).

## Cross-site requests

A browser attaches the session cookie to every request to your API, including one that a page of another site caused. Under `httpapi.DeliveryCookie` and `httpapi.DeliveryBoth`, Behemoth therefore refuses a `POST`, `PUT`, `PATCH`, or `DELETE` to one of its routes that a browser sent from an origin it does not trust. The check also covers sign-in and sign-up, which need no cookie. Under `httpapi.DeliveryHeader` and `httpapi.DeliveryBody` there is no check.

Configure the check with two fields of `BootConfig.Router`:

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Router: httpapi.RouterConfig{
		BaseURL:        "https://auth.example.com",
		TrustedOrigins: []string{"https://app.example.com"},
	},
	// ...
})
```

| Field | Default | Meaning |
| --- | --- | --- |
| `BaseURL` | empty | The origin a browser reaches Behemoth at: a scheme, a host, and an optional port, without the base path. Requests from it pass. |
| `TrustedOrigins` | empty | Other origins whose pages may call Behemoth, such as a front end on another host. Each entry is matched exactly. The same list decides where a [redirect URL](magiclink.md#the-redirect-url) may point. |

A value that is not an `http` or `https` origin is a configuration error from `Boot`.

A request passes in these cases:

- Its method is `GET`, `HEAD`, or `OPTIONS`.
- Its `Origin` header is `BaseURL` or one of `TrustedOrigins`.
- It has no `Origin` header, and its `Sec-Fetch-Site` header is absent, `same-origin`, or `none`. A client that is not a browser, such as a mobile app, a command-line tool, or another server, sends neither header and needs no change.

Every other request is refused with `403` and the code `untrusted_origin`, before any rate limit or handler runs. That includes a request with `Origin: null`, which a browser sends from a sandboxed frame or a local file.

```bash
curl -i -X POST https://auth.example.com/api/auth/sign-in/email \
  -H 'Origin: https://other.example' -d '{"email":"ada@example.com","password":"correct horse"}'
# HTTP/1.1 403 Forbidden
# {"code":"untrusted_origin","error":"request from an untrusted origin"}
```

### Form content types

Under the same two deliveries, Behemoth also refuses a state-changing request whose `Content-Type` is `text/plain`, `application/x-www-form-urlencoded`, or `multipart/form-data`. These are the types an HTML form can send to another origin. The answer is `415` with the code `unsupported_content_type`. A request with another `Content-Type`, or with none, passes.

Send JSON bodies as `application/json`. `curl -d` sends `application/x-www-form-urlencoded` unless you name the type:

```bash
curl -i -X POST https://auth.example.com/api/auth/sign-in/email \
  -H 'Content-Type: application/json' -d '{"email":"ada@example.com","password":"correct horse"}'
```

### The base URL

Set `BaseURL` in production. When it is empty and `Boot` mounts routes, `Boot` logs a warning, and Behemoth compares the host and port of `Origin` with the request's `Host` header. The scheme is not compared. Behind a reverse proxy that replaces `Host` with an internal name, that comparison fails and every request from a browser is refused.

The check does not replace CORS. Your server's CORS configuration still decides whether a script on another origin may read a response.

Refusals are counted in the `behemoth.http.origin_refused` metric by `reason`. See [Telemetry](telemetry.md).

## Protect your own routes

The router runs the routes that plugins declare, and mounts them under `RouterConfig.BasePath` and the plugin's `PluginMeta.MountPath`. To protect a route of your own, declare it in a plugin of your own, and put `httpapi.RequireSession` in front of its handler:

```go
func (p *Plugin) Routes() []httpapi.Route {
	return []httpapi.Route{{
		Method:      http.MethodGet,
		Path:        "/me",
		Handler:     me,
		Middlewares: []httpapi.Middleware{httpapi.RequireSession()},
	}}
}

func me(rctx *httpapi.RequestContext) error {
	session, _ := identity.CallerFrom(rctx.Ctx).Session()
	return rctx.Response.JSON(http.StatusOK, map[string]string{"user_id": session.UserID})
}
```

The router authenticates every request before its route runs: it reads the token where the delivery says, and puts the caller on the request's context. `httpapi.RequireSession` answers `401` unless the caller signed in with a valid active session. Your handler reads the session from the caller with `identity.CallerFrom(rctx.Ctx).Session()`, and the user is `session.UserID`. [Callers and permissions](callers.md) describes the caller in full. With `UpdateAge` set, a session that is due has been extended by then, and the session your handler reads has the new expiry. See [Rolling expiration](#rolling-expiration).

A request it refuses does not reach your handler. The response is JSON with a message and a code your client can act on:

| The request | Status | Body | The session cookie |
| --- | --- | --- | --- |
| has no session token | `401` | `{"error": "missing session token", "code": "session_missing"}` | there is none |
| has a token that belongs to no session | `401` | `{"error": "invalid session", "code": "session_invalid"}` | removed |
| has the token of a revoked session | `401` | `{"error": "session has been revoked", "code": "session_revoked"}` | removed |
| has the token of an expired session | `401` | `{"error": "session expired", "code": "session_expired"}` | removed |
| has the token of a session that waits for a second factor | `401` | `{"error": "additional verification required", "code": "session_pending"}` | kept: the client needs it to present the second factor |

A revoked or expired session is told apart only while its row exists. After a purge removes the row, its token gets `session_invalid`. See [Remove old rows](core-tables.md#remove-old-rows).

A session that is invalid, revoked, or expired does not come back, so with a cookie delivery the response also removes the cookie. The router does this for any request that carries such a token, whatever its route. A browser can't delete it itself, and would send the dead token with every request until the cookie expired. With `httpapi.DeliveryBoth` this applies when the token came in the cookie. A request that sent a bearer token keeps its cookie. Clients of the header and body deliveries hold the token themselves and discard it when they get one of these codes.

A `401` always means the client has no usable session. If Behemoth could not check the session, because the database is down for example, the answer is `500` with a generic message, and the error is logged. No cookie is removed, and a client should not sign its user out on a `500`.

`RequireSession` returns these as typed errors and the router writes the response, so your `RouterConfig.ErrorMapper` shapes them like any other error.

### Fresh sessions

A session is fresh for `FreshAge` after its sign-in, or after its second factor. `httpapi.RequireFreshSession` is `RequireSession` plus that check, for a route that changes something sensitive: the account's email, a password, a deletion. A session that is valid and too old gets `401` with the code `session_not_fresh`, and the client asks the user to sign in again.

```go
func (p *Plugin) Routes() []httpapi.Route {
	return []httpapi.Route{{
		Method:      http.MethodPost,
		Path:        "/account/delete",
		Handler:     deleteAccount,
		Middlewares: []httpapi.Middleware{httpapi.RequireFreshSession(p.ac.SessionManager)},
	}}
}
```

Nothing refreshes a session in place. Signing in again is how a user gets a fresh one.

To tell another service who a request is from, exchange the session for a short-lived JWT. See [Session tokens](session-tokens.md).

### What a session proved

A session records the factor its sign-in checked, in `Session.Factors`, and a session that passes a second factor records its name after the first. A flow reads them on the caller's session, `identity.CallerFrom(ctx).Session()`.

| Factor | What the sign-in checked |
| --- | --- |
| `password` | a password, whether the user was named by email, username, or phone number |
| `email` | control of a mailbox: a magic link |
| `sms` | control of a phone number: a code sent by text message |
| `oauth` | the answer of an OAuth or OpenID Connect provider |

The names are constants in `core`, such as `core.FactorPassword`. Each names a channel, whichever plugin checked it. A plugin may record a name of its own, in lowercase snake case. A session that code created without naming a factor has none.

## Write a sign-in route

A sign-in flow passes core's checkpoint, `auth.signIn.credentialsVerified`, once the credential checks out and before it creates the session. A handler there can refuse the sign-in, or ask for a second factor, and the session is then pending. Name the factor your flow checked, in the payload and in `SessionMeta`:

```go
checkpoint, err := ac.Dispatcher.RunBefore(hctx, hooks.HookSignInCredentialsVerified,
	behemoth.M{hooks.HookValueUserID: user.ID, hooks.HookValueFactor: core.FactorEmail})
if err != nil {
	return nil, err // a handler refused the sign-in
}
state := models.SessionActive
if stepUp, _ := checkpoint[hooks.HookValueRequireStepUp].(bool); stepUp {
	state = models.SessionPending
}
session, rawToken, err := ac.SessionManager.Create(hctx.Ctx, user.ID, core.SessionMeta{State: state, Factor: core.FactorEmail})
if err != nil {
	return nil, err
}
return &core.SignInResult{User: user, Session: session, RawToken: rawToken, Method: "myplugin"}, nil
```

`hctx` is the `core.HookContext` of your sign-in, and `core.FactorEmail` stands for what your flow checked (see [What a session proved](#what-a-session-proved)). Behemoth's sign-in plugins also fire `auth.signIn.before`, `.after`, and `.failed` around the flow with `core.WithLifecycle`, so that rate limits, hook handlers, and the audit log see every sign-in.

The route hands the result to `httpapi.WriteSignIn`:

```go
result, err := p.SignIn(rctx.Ctx, creds) // the flow above
if err != nil {
	return err
}
return httpapi.WriteSignIn(rctx, result, nil) // the body above, and the token by the configured delivery
```

`WriteSignIn` is what makes the token arrive. If you write the response yourself, call `rctx.TokenCarrier.Write(rctx, rawToken, session.ExpiresAt)`. It sets the cookie or the header. It returns `true` when the delivery is the body, and then putting the token in your JSON is up to you.

A flow called from code, such as `emailpassword.Plugin.SignIn`, returns the raw token in its result and delivers nothing. The caller decides what to do with it.

A route that gives the client a value of its own, to be sent back later, can deliver it the way the session token travels. `rctx.TokenCarrier.Config()` returns the carrier's settings with their defaults applied: `Delivery`, and the cookie's `CookieSameSite` and `CookieInsecure`. The OAuth plugin does this with its binding value. See [OAuth](oauth.md#build-the-callback-page).

## Write a route that ends the session

A route that ends its caller's own session takes the token back afterwards with `rctx.TokenCarrier.Clear`, the counterpart of `Write`. With a cookie delivery it removes the cookie the request presented its token in, and it does nothing for the other deliveries.

```go
func signOutEverywhere(rctx *httpapi.RequestContext) error {
	session, _ := identity.CallerFrom(rctx.Ctx).Session() // behind RequireSession
	if err := rctx.Auth.SessionManager.RevokeAllForUser(rctx.Ctx, session.UserID, "signed_out_everywhere", ""); err != nil {
		return err
	}
	rctx.TokenCarrier.Clear(rctx) // after the revoke succeeded
	return rctx.Response.JSON(http.StatusOK, behemoth.M{"status": "signed_out"})
}
```

Call it only after the session has ended. A response that removes the cookie of a session that is still live leaves that session without a client. Do not call it when the session you ended is someone else's, as in an administrator's route: the cookie on the response is the caller's.

