# Request routing

This document explains how a request to one of behemoth's HTTP endpoints reaches a plugin's handler: how plugins declare routes and middlewares, how `Boot` turns them into one route table, how that table is checked for conflicts, how every handler is wrapped, and how the table is handed to the application's HTTP framework.

The code lives in:

- `httpapi/router.go`: `Router`, `RouterConfig`, `FrameworkDriver`, the wrapping pipeline
- `httpapi/authenticate.go`: `AuthenticateRequests` and `Router.ApplyAuthentication`, the stage that turns a request's session token into its caller
- `httpapi/token_carrier.go`: `TokenCarrier`, which reads the token from a request (`RouterConfig.Carrier`)
- `httpapi/plugin.go`: `Plugin`, `RouteRulesPlugin`
- `httpapi/request.go`: `Route`, `Middleware`, `RequestContext`, `ResponseRecorder`
- `engine/init.go`: `Boot`, which drives everything below
- `plugins/adapters/{gin,echo,chi,fiber}`: one `FrameworkDriver` per framework, each in its own module
- `plugins/adapters/nethttp`: the `FrameworkDriver` for the standard library's `*http.ServeMux`, a package of the root module

Two guideline documents apply to anyone who adds or changes a route: [`../guidelines/http_contract.md`](../guidelines/http_contract.md) for what a client depends on, and [`../guidelines/transport_neutral_flows.md`](../guidelines/transport_neutral_flows.md) for what belongs in a handler and what belongs in the flow behind it.

## Ownership boundary

`[Convention, important]` Behemoth never owns the HTTP server. The application creates its framework instance (`*gin.Engine`, `*echo.Echo`, `chi.Router`, `*http.ServeMux`, `*fiber.App`), registers its own routes on it, and hands it to `Boot` wrapped in an adapter:

```go
engine := gin.New()
engine.GET("/", ...) // the application's own routes

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	Router: httpapi.RouterConfig{BasePath: "/api/auth"}, // optional; defaults shown
	HTTP:   ginadapter.New(engine),
})
engine.Run(addr)
```

Behemoth adds its routes to that instance and touches nothing else on it: no `engine.Use()`, no 404 handler, no server settings. `BootConfig.HTTP == nil` builds and conflict-checks the route table without mounting it (workers, CLIs that boot but don't serve), so a routing misconfiguration fails `Boot` the same way in every process.

`[Question, resolved]` "Should adapters register themselves through a blank import (`_ ".../adapters/gin"`) and a global driver registry, like `database/sql`?" No. A `database/sql` driver is a stateless factory and the caller still names it (`sql.Open("postgres", ...)`); a framework driver needs the application's own instance, which a self-registering `init()` cannot reach. A registry would force either an adapter-owned engine handed back as `any`, or a `(name string, engine any)` pair. That is no shorter for the user, it is untyped, and it adds global state shared by every app in the process (including parallel tests). Passing the driver value in `BootConfig.HTTP` is the same one line, type-checked.

---

## Declaration: what plugins provide

### Routes

A plugin serves routes by implementing `httpapi.Plugin`, which adds two methods to `core.Plugin`. A plugin without routes implements `core.Plugin` alone, and `Boot` mounts nothing for it.

```go
type Plugin interface { // httpapi
	core.Plugin               // Meta().MountPath: optional prefix for this plugin's routes
	Routes() []Route
	Middlewares() []Middleware
}

type Route struct {
	Method      string       // "GET", "POST", ...
	Path        string       // relative to BasePath + MountPath, for example "/sign-in/email"
	Handler     HandlerFunc  // func(*RequestContext) error
	Middlewares []Middleware // this route only; Middlewares[0] runs first
}

type Middleware func(next HandlerFunc) HandlerFunc
```

- A route's full path is `path.Join(RouterConfig.BasePath, PluginMeta.MountPath, Route.Path)`. `BasePath` defaults to `/api/auth`; `MountPath` defaults to empty, mounting the plugin flat under `BasePath`. E.g. `BasePath "/api/auth"`, `MountPath "/audit"`, `Path "/status"` → `GET /api/auth/audit/status`.
- **Path parameters** use `{name}` (`/users/{id}`), read in the handler with `rctx.Param("id")`. Adapters translate to their framework's syntax where it differs (gin, echo, and fiber use `:id`; chi and `http.ServeMux` use `{id}` natively).
- `Route.Middlewares` apply to that route only. `Plugin.Middlewares()` apply to **every** behemoth route. See Global middleware below.
- `[Convention]` `Boot` calls every plugin's `Init` **before** it calls `Routes()` or `Middlewares()`, so a plugin may build handlers from state it sets up in `Init` (services, `WithLifecycle`-wrapped flows).
- `[Implementation Detail]` `Boot` visits plugins in **dependency order** (`ResolvePluginOrder`) for both `Mount` and middleware collection. Route order within the table has no routing significance (frameworks match by pattern) but it fixes which plugin is reported as the second claimant of a conflict, and it fixes global middleware order.

### Absolute routes

`Router.MountAbsolute(owner, routes...)` registers routes at their exact `Path`, bypassing `BasePath` and `MountPath`, for standards-fixed locations that must not move with the developer's API prefix (for example a JWKS endpoint at `/.well-known/jwks.json`). They are conflict-checked and wrapped exactly like mounted routes. `Boot` mounts the key set route with it when session tokens are on (`httpapi.KeySetRoutes`).

### Handlers and the request context

```go
type HandlerFunc func(ctx *RequestContext) error

type RequestContext struct {
	Ctx      context.Context
	Request  *http.Request     // the original request
	Response *ResponseRecorder // buffered; flushed by the adapter after the handler returns
	Auth     *AuthContext      // set by the router, see Wrapping
	Values   behemoth.M        // per-request scratch space shared by middlewares and the handler
	Params   map[string]string // path parameters, filled in by the adapter
}
```

- One `RequestContext` is built per request, by the adapter, and the **same instance** flows through every middleware and the handler, so `Values` set by a middleware are visible to the handler.
- Handlers never touch the framework's response writer. They write into `ResponseRecorder` (`JSON`, `Text`, `Status`, `Cookie`, `Redirect`, `SetHeader`); nothing reaches the client until the adapter calls `Flush` after the whole chain returns. This is what lets a middleware short-circuit with a response without knowing the framework.
- **Two ways to end a request:** write a response and return `nil`, or return an error and let the router map it (see Error Mapping).
- `[Convention, important]` Never do both. `ResponseRecorder.JSON` appends to the buffered body, so a handler that writes a body and then returns an error produces the handler's body followed by the error body, under the error's status.

---

## Route table construction: `Boot`

Two fields of `RouterConfig` are read before the router exists. `Boot` builds `AuthContext.Origins` from `TrustedOrigins` when it builds the `AuthContext`, so that a plugin's `Init` and flows can check redirect URLs against it. See [`../plugins/magiclink.md`](../plugins/magiclink.md#trusted-origins). It also validates `Carrier`. The router builds its token carrier from it and hands it to every handler as `RequestContext.TokenCarrier`, which sign-in and sign-out routes use to hand over and take back the session token. See [`../sessions/token_delivery.md`](../sessions/token_delivery.md).

After the AuthContext is fully built and every plugin's `Init` has run, `Boot`:

1. Creates the router: `httpapi.NewRouter(cfg.Router, ac)`. The `AuthContext` is captured here, once.
2. For each plugin that implements `httpapi.Plugin`, in dependency order: `router.Mount(name, meta.MountPath, p.Routes())`, and appends `p.Middlewares()` to the global middleware list. With session tokens on, it mounts `httpapi.SessionTokenRoutes(ac.SessionTokens)` below the base path, and `httpapi.KeySetRoutes` at the key set's path with `MountAbsolute`.
3. Builds the client-IP configuration from `RouterConfig.TrustedProxies` / `ClientIPHeader`, and calls `router.ApplyAuthentication(ac.Authenticator, ipCfg)`, then `router.ApplyRateLimiting(rateLimiter, app.RouteRules, ipCfg)`: the route rules `Prepare` collected from the plugins that implement `httpapi.RouteRulesPlugin`.
4. If `cfg.HTTP != nil`: `router.Build(cfg.HTTP, globalMiddleware...)`.

Any error aborts `Boot`. Steps 1–3 run whether or not a driver is configured.

---

## Duplicate path detection

Conflicts are checked at two levels: between behemoth's own routes, inside the router, and against the application's routes, inside each adapter.

### Level 1: within behemoth (`Router.claim`)

`Router.claim` works as follows:

- The router keeps `routeOwners map[routeKey]string`, `routeKey{method, fullPath}` → owning plugin name. Every route passing through `Mount` or `MountAbsolute` is claimed under its **full** path, so two plugins may use the same relative path under different `MountPath`s, and an absolute route conflicts with a mounted route at the same full path.
- A route whose key is already claimed is **not added**. `Mount` keeps going through the rest of that plugin's routes and returns one `ConfigurationError` listing every conflict in the call:

  ```
  route conflicts detected:
    GET /api/auth/status: claimed by both "sessions" and "emailpassword"
  ```

A conflict is reported as follows:

- The first claimant wins and the second is reported, so with dependency ordering a plugin conflicting with one it depends on is always the one named second.
- `[Implementation Detail]` Detection is per `Mount` call: `Boot` stops at the first plugin whose `Mount` fails, so conflicts belonging to later plugins surface on the next run.
- `[Convention]` Keys are compared literally. `GET /users/{id}` and `GET /users/{user_id}` are different keys to the router even though they match the same requests. Whether the framework then rejects the second depends on the framework (Level 2).

### Level 2: against the application's routes (adapters)

Behemoth's routes share the framework instance with the application's, so a behemoth route can collide with one the developer registered. Each framework reacts differently; each adapter turns the reaction into an error from `FrameworkDriver.Mount`, which fails `Boot`:

| Framework | Native reaction to a duplicate | Adapter |
|---|---|---|
| gin | panics at registration | recovers the panic into an error |
| echo v5 | `AddRoute` returns an error (unless `AllowOverwritingRoute`) | uses `AddRoute`, returns the error |
| chi | **silently replaces** the existing handler | walks the existing routes first (`chi.Walk`, which lists a route registered for all methods once per method) and rejects an exact method + pattern match; also recovers chi's panics for invalid patterns |
| `http.ServeMux` | panics at registration, for a pattern it cannot parse and for one that conflicts with a registered pattern. A pattern without a method is **silently accepted** next to a route with one, and loses that method to it | recovers the panic into an error; before registering, asks the mux which pattern would answer the route (`ServeMux.Handler`) and rejects an application pattern without a method, unless it is a subtree |
| fiber | **silently keeps both**, and the route registered first answers, so behemoth's would never run | lists the existing routes first (`App.GetRoutes(true)`, which leaves out `Use` middleware) and rejects a match on method and path; also recovers fiber's panics for invalid routes |

`[Convention]` The chi and fiber pre-checks compare patterns as text, so the same caveat as Level 1 applies: a differently spelled but equivalent pattern (`/users/{id}` against `/users/{user_id}`) is not caught there. The fiber check does follow the app's matching rules: it ignores case unless `Config.CaseSensitive` is set and a trailing slash unless `Config.StrictRouting` is set, because fiber would send `/API/Auth/Session/` and `/api/auth/session` to the same route. `http.ServeMux` compares what two patterns match and not how they are spelled, so it catches the renamed parameter too.

`[Convention]` The pre-checks of chi, fiber, and `http.ServeMux` see the routes that exist when `Boot` runs. A route the application registers afterwards is not checked: on chi it replaces behemoth's route, on fiber behemoth's route keeps answering and the application's is never reached, and on the mux a pattern without a method gets every method but behemoth's.

`[Convention]` A rejected mount must never disturb the application's existing route. In chi's case, the check runs **before** registering, precisely because registering would already have replaced it.

### A pattern without a method is a conflict on `http.ServeMux`

**Context:** A mux pattern may leave the method out, and then it answers every method. To the mux, `/api/auth/session` and behemoth's `POST /api/auth/session` do not conflict: it gives a request to the most specific pattern, so behemoth's route takes `POST` and the application's handler keeps the other methods. Nothing reports that the handler lost `POST`. The chi and fiber adapters fail `Boot` in the same situation (a route registered for all methods on the same path). Code written for the mux before Go 1.22 has no other way to register a route than without a method, and much code still does so, which makes this the mux's usual spelling of "the same route".

**Options considered:**

- Leave it to the mux. The precedence is documented behaviour of `ServeMux` and needs no code. A collision passes silently, on this router only.
- Probe before mounting, and reject an application pattern without a method that would answer the route. The mux cannot list its patterns, so this is the only way to look. It refuses a setup the standard library accepts, and it has the limits listed below.
- The same check behind an option (`nethttp.New(mux, nethttp.Strict())`). The default would stay silent, and the people who hit the collision are the ones who do not know to ask for the check.

**Decision:** probe, always. `Boot` failing on a collision is what every other adapter does, and the developer who meant it has a way out: a pattern that names its methods is never rejected by this check.

**Revisit if:** applications turn out to rely on a pattern without a method as the fallback for the methods behemoth does not use on a path.

`[Implementation Detail]` How the probe works (`ServeMuxDriver.methodlessPattern`): `ServeMux.Handler(req)` returns the pattern the mux would give a request to. Before registering a route, the driver asks about a request that route would match: the route's method, and its path with `x` in place of each `{param}`. The answer is the single best match, which is the pattern the route would take its method from. It is judged by how it is written:

| The mux answers | Example, for `POST /api/auth/session` | Verdict |
|---|---|---|
| nothing (`""`) | no pattern, or only patterns for other methods | mount |
| a pattern with a method | `POST /api/auth/{action}` | mount: the mux compares the two itself, and panics if they conflict |
| a pattern without a method that ends in `/` or `...}` | `/`, `/api/`, `/api/auth/{rest...}` | mount: a subtree is the application's catch-all |
| a pattern without a method that ends in `{$}`, when the route's path has no trailing slash | `/api/auth/session/{$}` | mount: that pattern answers another path, and came back because the mux would redirect to it |
| any other pattern without a method | `/api/auth/session`, `/api/auth/{action}`, `/api/{section}/session` | reject |

Behemoth's own patterns always have a method (the driver refuses a route without one), so a route never trips over an earlier route of the same `Mount`.

`[Convention, important]` The probe has limits, and the mux reports none of these cases. `docs/api/routers.md` carries them as a warning to the developer:

- **Patterns registered after `Boot`** are not seen (see the convention above).
- **Patterns with a host** (`auth.example.com/api/auth/session`) are not seen, because the made-up request has no host. The mux prefers a pattern with a host, so on that host the application's handler answers every method and behemoth's route is not reached. `TestPatternWithAHostIsNotSeen` pins this.
- **A second mux is not seen.** When behemoth mounts on a mux of its own that the application registers under `/api/auth/` on its main mux, the probe looks at behemoth's mux. A more specific pattern on the main mux takes its requests before they reach behemoth's.
- **A pattern without a method that reaches the route through a wildcard is rejected too** (`/api/auth/{action}`). It is not the same route, but it would lose the method the same way, and the probe does not compare paths segment by segment.

---

## Wrapping: the request pipeline

Every route reaches the framework as a single `HandlerFunc` built from layers added at three points: `Mount`, `ApplyRateLimiting`, and `Build`.

### Layer order

From outermost (runs first) to innermost:

```
adapter: build RequestContext (Request, Params, empty Response, Values)
  │
  ├─ withRequestScope         sets rctx.Auth, assigns the request ID,         (Build)
  │                           puts both the ID and rctx on rctx.Ctx
  ├─ withTracing              one span per request; rctx.Ctx carries it         (Build)
  ├─ withMetrics              counts and times the request, by route pattern    (Build)
  ├─ wrapWithErrorMapping     returned error → mapped JSON response             (Build)
  ├─ origin check             OriginCheck.Middleware, cookie deliveries only    (Build)
  ├─ form content types       RefuseFormContentTypes, cookie deliveries only    (Build)
  ├─ global middleware[0]                                                      (Build)
  ├─ ...
  ├─ global middleware[n]     every plugin's Middlewares(), dependency order    (Build)
  ├─ route rate limit         most specific matching RouteRateLimitRule, if any (ApplyRateLimiting)
  ├─ authentication           AuthenticateRequests: the caller on rctx.Ctx      (ApplyAuthentication)
  ├─ route middleware[0]                                                       (Mount)
  ├─ ...
  ├─ route middleware[n]      Route.Middlewares                                (Mount)
  └─ handler                  Route.Handler
  │
adapter: Response.Flush(writer)  — or 500 if the chain returned an error
```

How each step builds it:

- **`Mount`:** `h = route.Middlewares[0](... route.Middlewares[n](route.Handler))`, iterated in reverse so `Middlewares[0]` ends up outermost and runs first.
- **`ApplyAuthentication`:** for each route, `h = AuthenticateRequests(authn, carrier, ipCfg)(h)`, with the router's carrier. Every route gets it, and a public one too: the stage reads the token where `RouterConfig.Carrier` says, has the authenticator build the caller, puts the caller on `rctx.Ctx` in place of any caller already there, and has the carrier remove the cookie of a session that is gone or extend the cookie of a session that rolled. It refuses nothing. `RequireSession`, a route middleware, refuses a request whose caller is not signed in. It is applied before `ApplyRateLimiting`, so a rate-limited request is refused before it costs a session lookup. See [`../sessions/token_delivery.md`](../sessions/token_delivery.md).
- **`ApplyRateLimiting`:** for each route, `BestRouteMatch` picks the single most specific `RouteRateLimitRule` for its method and **match path** (below). No match, or a `Disabled` best match, leaves the route untouched; otherwise `h = rateLimitWrap(h, rule, ...)`, which resolves the client's address, computes the rule's key from the request, and calls `RateLimiter.CheckRule` with the rule's name. It returns the error instead of calling `h` when the limit is exceeded. Resolved once at boot, not per request.

  `[Convention, important]` **Rule paths are relative to `BasePath`.** Core and plugins declare rules without knowing the developer's `BasePath` (core's sign-in rule is `POST /sign-in/email`) so the router records, for every route, the path rules are matched against:

  | Route kind | Match path | Example (`BasePath "/api/auth"`) |
  |---|---|---|
  | mounted (`Mount`) | `MountPath + Route.Path`, that is, the full path minus `BasePath` | `/api/auth/audit/status` → `/audit/status` |
  | absolute (`MountAbsolute`) | the full path, since it lives outside `BasePath` | `/.well-known/jwks.json` → `/.well-known/jwks.json` |

  A rule written with the full path of a mounted route (`/api/auth/audit/status`) never matches. Changing `BasePath` never requires touching a rule.
The wrapping works as follows:

- **`Build`:** `h = global[0](... global[n](h))`, then `h = withRequestScope(withTracing(withMetrics(wrapWithErrorMapping(h))))`. `withTracing` and `withMetrics` each return `h` itself when no tracer or metrics sink is configured. `Build` works on a copy of the table, so the router's own routes stay unwrapped by this step.

### Why this order

The layers are ordered for these reasons:

- **Request scope outermost.** `withRequestScope` sets `rctx.Auth` and `rctx.TokenCarrier` before anything else runs, so every middleware (global or per-route) and the handler can rely on them. The router captured the `AuthContext` in `NewRouter` and built the carrier there; adapters build the `RequestContext` without ever knowing about either. It also puts the `RequestContext` on `rctx.Ctx` (`httpapi.ContextWithRequest`), so code that only receives a `context.Context` still knows which request it serves: a hook handler reads it back with `httpapi.RequestFrom(hctx.Ctx)`. `[Convention]` Only the request travels through the context, because it is request-scoped by definition; everything else is passed explicitly. A `context.Context` without one (CLI, background job) is a call made outside any request.
- **Request ID with the scope.** `withRequestScope` also assigns the request ID: the one already on the context, else a usable value of the `RouterConfig.RequestIDHeader` request header (default `X-Request-ID`), else a generated one. It stores the ID on `rctx.Ctx` (`telemetry.ContextWithRequestID`) and sets it on the response header. Doing this in the outermost wrapper means every log line written for the request carries the ID, and an error response has the header too. See `docs/internal/telemetry/foundations.md`.
- **Tracing inside the request scope.** `withTracing` needs the request ID for the span, and replaces `rctx.Ctx` with the span's context so middleware and the handler nest under it. See `docs/internal/telemetry/tracing.md`.
- **Metrics outside error mapping.** `withMetrics` reads the status after the mapper has run, so `behemoth.http.requests` reports what the client received. See `docs/internal/telemetry/metrics.md`.
- **Error mapping inside that, outside everything that can fail.** An error from a global middleware, the rate limiter, a route middleware or the handler all become a response through the same mapper. Before this order, only handler errors were mapped; a rate-limit rejection reached the adapter as a raw error.
- **The origin check first among the global middleware.** `Boot` passes `OriginCheck.Middleware` to `Build` ahead of the plugins' middleware when the session token travels in a cookie. `RefuseFormContentTypes` follows it. A cross-site request is refused before any plugin code or rate limit sees it. See [`origin_check.md`](origin_check.md).
- **Global middleware outside route-specific layers.** Cross-cutting concerns (request IDs, security headers) see every request first, including ones the rate limiter is about to reject.
- **Route middleware innermost.** It is the route's own concern and sees the request last, after everything general has passed.

`[Question, open]` Rate limiting currently runs *after* global middleware, so a rate-limited request still pays for every global middleware. Moving it to just inside error mapping would make rejection cheaper, at the cost of global middleware (for example a request logger) no longer seeing rejected requests.

### Error mapping

`wrapWithErrorMapping` is the only place a returned error becomes a response, and the place it is logged:

- `RouterConfig.ErrorMapper.Map(err)` returns `(status, body)`. The default, `httpapi.DefaultErrorMapper`, maps a `*DomainError` through its category (`categoryToStatus`) to a status and `{"error": PublicMessage, "code": Code}`. Anything else (a plain error, or a `DomainError` whose category has no status, such as configuration or internal errors) becomes `500 {"error": "internal server error"}`, so raw error text never reaches a client.
- A `*DomainError` with `RetryAfter > 0` (a rate limit, a busy password hasher) also sets the `Retry-After` header, in whole seconds, rounded up.
- The mapped body is written with `rctx.Response.JSON`, and the wrapper returns `nil`.
- The error is logged here, once: at Warn for an error of category `unavailable` (a `503`, such as a busy password hasher), at Error for any other 5xx, and at Debug otherwise, with the route's method and pattern, the status and the fields of `telemetry.ErrorFields`. Components below the router return their errors without logging them. See `docs/internal/telemetry/logging.md`.

`[Convention]` Because the mapper always handles the error, **a route reaching an adapter never returns a mapped error**. An error returned to the adapter therefore means the response itself could not be built (for example, JSON marshalling failed), and every adapter answers it with a bare `500` instead of flushing. Adapters take no error mapper of their own.

---

## Handoff: `FrameworkDriver`

```go
type FrameworkDriver interface {
	Mount(routes ...Route) error
}
```

The only seam between behemoth and an HTTP framework. Each adapter, for every route:

1. Translates the path syntax if needed (`{id}` → `:id` for gin, echo, and fiber).
2. Registers a native handler on the application's instance that builds a `RequestContext` from the native request (`Ctx`, `Request`, `Params`, a fresh `ResponseRecorder`, empty `Values`), calls the route's handler, and then flushes the recorder to the native writer, or writes `500` if the handler returned an error.
3. Returns an error, never panics, when the framework rejects the route (Level 2 above).

| | gin | echo | chi | `net/http` | fiber |
|---|---|---|---|---|---|
| Package | `plugins/adapters/gin` | `plugins/adapters/echo` | `plugins/adapters/chi` | `plugins/adapters/nethttp` | `plugins/adapters/fiber` |
| Module | its own | its own | its own | the root module | its own |
| Constructor | `New(gin.IRoutes)`: engine or `*RouterGroup` | `New(*echo.Echo)` | `New(chi.Router)`: mux, `Group`, or `Route` | `New(*http.ServeMux)` | `New(*fiber.App)` |
| Path params | `{id}` → `:id` | `{id}` → `:id` | unchanged | unchanged | `{id}` → `:id` |
| Params from | `c.Params` | `c.PathValues()` | `chi.RouteContext(r.Context()).URLParams` | `r.PathValue(name)` | `c.Params(name)` for each of `c.Route().Params` |
| `*http.Request` | the framework's | the framework's | the framework's | the mux's | built by the driver |

Passing a group (gin `RouterGroup`, chi `Group`/`Route`) puts behemoth's routes behind that group's prefix and the group's own middleware, which is the developer's way to wrap behemoth's routes in application middleware, without behemoth installing anything. The two routers without a group to pass have their own way to the same result:

- **`http.ServeMux`:** give behemoth a mux of its own and register it, wrapped, on the application's: `mux.Handle("/api/auth/", logRequests(authMux))`. Behemoth's patterns carry the full path, so the inner mux needs no `http.StripPrefix`.
- **fiber:** register the middleware on the path before `Boot`: `app.Use("/api/auth", mw)`. Fiber runs handlers in the order they were registered, so one registered after `Boot` does not run for behemoth's routes.

`[Convention]` Adapters stay thin: no error mapping, no auth, no middleware composition. Everything above the native handler is done once in the router, so a new adapter only needs the three steps above.

### The `net/http` driver

`plugins/adapters/nethttp` registers each route as the pattern `METHOD /path` on a `*http.ServeMux`, the method-and-wildcard syntax the mux has had since Go 1.22. It differs from the other adapters in these ways:

- **Parameter names are read at mount.** A request gives a parameter's value by name (`r.PathValue("id")`) and has no list of them. The driver takes the names out of the route's path once, in `paramNames`, and the handler looks each one up.
- **A trailing slash gets `{$}`.** The mux treats a pattern ending in `/` as a prefix: `GET /` matches every `GET` request no other pattern claims. A behemoth route is one path on every other framework, so `toMuxPattern` turns `/` into `/{$}`, which matches that path only. `Router.Mount` joins paths with `path.Join`, which removes a trailing slash, so this concerns a route at `/` and absolute routes.
- **A route without a method is refused.** A mux pattern without a method matches every method. gin, chi, and fiber reject an empty method on their own; here the driver returns the error.
- **The application's patterns without a method are checked.** The mux accepts one next to a behemoth route on the same path and quietly splits the methods between them. The driver looks for it before registering each route. See *A pattern without a method is a conflict on `http.ServeMux`* under Level 2.

`[Implementation Detail]` The mux sends a `HEAD` request to a `GET` pattern. The route's handler runs and the server drops the body. Metrics and rate limits count the request under the `GET` route. Fiber does the same by registering a `HEAD` route for each `GET` route when it starts, unless `Config.DisableHeadAutoRegister` is set.

### The `net/http` driver is part of the root module

**Context:** Each framework adapter is a module of its own, so that an application downloads gin only if it uses gin. The driver for `http.ServeMux` imports the standard library and behemoth, nothing else.

**Options considered:**

- A module of its own, like the others. Every adapter is then installed the same way. It costs a `go.mod` with a `replace` to maintain and a second `go get` for the application, to isolate a dependency that does not exist.
- A package of the root module. An application that uses the standard library's router adds no module, and the root `go.mod` does not change. `go test ./...` at the root runs its tests, which the other adapters' tests need a step of their own for.

**Decision:** a package of the root module, at `plugins/adapters/nethttp` next to the other adapters.

**Revisit if:** the driver needs a dependency outside the standard library.

### The fiber driver

`plugins/adapters/fiber` targets fiber v3. Fiber is built on fasthttp and not on `net/http`, so the driver does more than the others: it builds the request, picks the context and writes the response itself.

**The request.** `RequestContext.Request` is a `*http.Request`, and fiber has none, so `newRequest` builds one for every request: method, URL, protocol, headers, body, `Host`, `RemoteAddr`, `RequestURI`, and `TLS`. It follows what a `net/http` server does where the two differ: the `Host` header is in `Request.Host` only, and a header sent twice (`X-Forwarded-For` through two proxies) keeps both values in order. `RemoteAddr` is the peer's address, so `RouterConfig.TrustedProxies` works as on the other frameworks; fiber's own `TrustProxy` setting is not read. A request URI that `net/url` refuses, such as one with a broken percent escape, gets a `400`: fiber routes it, but no `*http.Request` can describe it.

**The context.** `RequestContext.Ctx` and the request's context are `c.Context()`: what the application's middleware set with `c.SetContext` (a tracing middleware's span, a deadline), or `context.Background()`. Fiber has no context that ends when the client disconnects, so on fiber a request's work is not cancelled when the client goes away. The `fiber.Ctx` itself also implements `context.Context` but is pooled and reused, so the driver never hands it out.

**The response.** `ResponseRecorder` exposes its body only through `Flush(http.ResponseWriter)`. The driver passes it a `responseWriter` that puts the headers, the status and the body on fiber's response. Headers are added with fasthttp's `Add`, which keeps several `Set-Cookie` lines apart.

**Path parameters.** Fiber hands out a parameter as it was sent (`a%20b`) unless `Config.UnescapePath` is set; the `net/http` routers hand out the decoded value (`a b`). The driver decodes when the app does not, so `rctx.Param` returns the same value on every framework.

### The fiber driver builds its own request and copies every string

**Context:** Fiber reuses a request's memory for the next request on the connection, and the strings it hands out point into that memory (fiber's docs: "returned value is only valid within the handler"). Behemoth holds on to what it reads for longer. `Mailer.SendAsync` keeps the request's context for a send that runs after the response, and that context carries the `RequestContext` and the request ID read from the `X-Request-ID` header. The same ID is an attribute of the request's span, which a tracer exports later. A hook handler of the application may keep the `RequestContext` it was given. A string that changes after the handler returned would corrupt these silently.

**Options considered:**

- Fiber's `adaptor` package (`adaptor.ConvertRequest`, or `adaptor.HTTPHandler` around a `net/http` handler). The least code. Its request shares fiber's memory: with it, the test `TestRequestStaysValidAfterTheHandler` saw the first request's path, query, user agent and `X-Forwarded-For` turn into the second request's. `HTTPHandler` also gives the handler no `fiber.Ctx`, so no path parameters and no `c.Context()`.
- Require `fiber.Config{Immutable: true}`, and refuse to mount without it. The setting makes fiber's own getters return copies, at a cost on every route of the application. The adaptor's conversion does not honour it, so the driver would still build the request itself, from those getters.
- Build the request in the driver and copy each string. More code in the adapter, and one allocation per header on behemoth's routes only.

**Decision:** build and copy. The body is the exception: it is read while the handler runs, as with `net/http`, so a reader over fiber's buffer is enough.

**Revisit if:** fiber's adaptor gains a conversion that copies.

### The fiber driver takes the app, not a router

**Context:** The gin and chi drivers take an interface that an engine and a group both satisfy. Fiber's equivalent is `fiber.Router`. The driver has to list the existing routes, because fiber does not reject a duplicate (Level 2), and only `*fiber.App` can list them (`GetRoutes`). A `fiber.Router` exposes neither the routes nor its prefix.

**Options considered:**

- `New(fiber.Router)`, without a conflict check. A behemoth route that repeats one of the application's would then never run, and nothing would say so.
- `New(fiber.Router)`, checking only when the router is the app. The behaviour would depend on which type the application passed.
- `New(*fiber.App)`. The check always runs. Mounting behind a group is not possible; `app.Use(prefix, mw)` covers the middleware a group would add, and `RouterConfig.BasePath` covers the prefix.

**Decision:** `New(*fiber.App)`.

**Revisit if:** fiber lets a `Router` list its routes or report its prefix.

---

## Global middleware

### What exists: middleware across behemoth's routes

`Plugin.Middlewares()` is behemoth-wide: every plugin's list is composed, in dependency order, around **every behemoth route** (mounted and absolute). It is applied by the router during `Build`, never with the framework's `Use()`. Cross-cutting concerns that apply to every auth endpoint (request IDs, security headers) belong here; without it every plugin would repeat them per route. Core's origin check uses the same mechanism: `Boot` puts it first in the list (see [`origin_check.md`](origin_check.md)).

`[Convention, important]` Behemoth-wide does **not** mean server-wide. A plugin's global middleware never runs on the application's own routes. Registering it with `engine.Use()` would have done exactly that, which is why the router composes it instead.

Known gaps:

- **Ordering between plugins is implicit.** Global middleware runs in plugin dependency order, a by-product of plugin resolution, not a decision anyone makes. Hooks already have explicit ordering (`HookPriority`, `HookOptions.Before/After`); middleware should get the same model, so "the request logger runs before the audit logger" is declared rather than inherited. Core's origin check does not depend on this: `Boot` places it ahead of every plugin's middleware.
- **Scope is all-or-nothing.** A plugin wanting middleware on its own routes only already has `Route.Middlewares`. For anything in between, a declared path pattern could reuse the matcher route rate-limit rules already use (`MatchesMethod` / `MatchesPath` / `BestRouteMatch`). `[Deferred]` until a plugin needs it.

### What does not exist: plugins intercepting every request to the server

`[Question, resolved]` "Should a plugin be able to intercept every request to the application, not only behemoth's routes?" **Not automatically.** Three reasons:

1. **Behemoth doesn't own the server.** A middleware installed on the application's instance changes the application's own routes without the developer having written that line: it can add latency, reject requests, consume request bodies, and see every request the application receives. That is surprising, hard to debug, and a security concern for third-party plugins. A developer adopts a plugin for an auth feature, not to grant it all of their traffic.
2. **It is fragile in practice.** In gin, `engine.Use()` only affects routes registered *after* it (each route copies the handler chain at registration), so whether a plugin's middleware covered the application's routes would depend on whether `Boot` ran before or after the application registered them. Each framework has its own variant of this, so the adapters could not promise one behaviour.
3. **It contradicts the ownership boundary.** `BootConfig.HTTP` exists so that the developer's wiring is explicit; behemoth installing middleware on the developer's instance would bring back the hidden setup that decision avoided.

### Future: exported middleware the developer opts into

The real needs behind "intercept every request" are things like *make the current user available in my own handlers* or *protect my `/dashboard` routes*. Those are met by plugins **exporting** `httpapi.Middleware` values and the developer applying them where they choose:

```go
// a plugin exports httpapi.Middleware; the adapter bridges it to the framework
engine.Use(ginadapter.Middleware(ac, sessions.LoadSession))              // whole app — the developer's call
admin := engine.Group("/admin", ginadapter.Middleware(ac, sessions.RequireAuth))

func handler(c *gin.Context) {
	user, ok := ginadapter.CurrentUser(c) // values set by the middleware, through typed accessors
}
```

Implementation outline:

1. **A bridge per adapter**: `Middleware(ac *core.AuthContext, mw httpapi.Middleware)` returning the framework's native middleware (`gin.HandlerFunc`, `echo.MiddlewareFunc`, `func(http.Handler) http.Handler` for chi). It builds a `RequestContext` with `Auth` set, runs `mw` with a `next` that continues the native chain, and stores the `RequestContext` on the native context (`c.Set`, `c.Set`, `context.WithValue`). If `mw` returns without calling `next`, the bridge flushes the middleware's `Response` and aborts the native chain; if it returns an error, the bridge maps it with the same `ErrorMapper` the router uses, so an application route rejected by a behemoth middleware answers exactly like a behemoth route would.
2. **Typed accessors per adapter**: `CurrentUser(c)`, `Session(c)`, ... read what the middleware stored, so application handlers never handle `RequestContext` or untyped `Values`.
3. **A `net/http` form in core**: `func(http.Handler) http.Handler` built on the same bridge logic, covering chi and the standard library's `ServeMux` without a framework-specific adapter; framework adapters can wrap it where their native type differs. `plugins/adapters/nethttp`, which is already in the root module, is the place for it.
4. **Shared `RequestContext` with behemoth's own routes is not required.** The bridge serves application routes; behemoth's routes keep building their context in the adapter's route handler. Anything both need (session lookup) lives in the plugin's service, called from both.

`[Convention]` Plugins provide the middleware; only the developer decides where it runs.
