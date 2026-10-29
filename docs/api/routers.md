# Router adapters

Behemoth does not run an HTTP server. You create your router, register your own routes on it, and hand it to `Boot` wrapped in an adapter. `Boot` adds Behemoth's routes to it under `RouterConfig.BasePath` (default `/api/auth`). You start the server as you always do.

This page lists the adapters and covers what is specific to each router.

## Adapters

| Router | Import path | Installed with | Constructor takes |
| --- | --- | --- | --- |
| `net/http` (`http.ServeMux`) | `github.com/go-behemoth/behemoth/plugins/adapters/nethttp` | Behemoth itself | `*http.ServeMux` |
| Gin | `github.com/go-behemoth/behemoth/plugins/adapters/gin` | its own `go get` | `*gin.Engine` or a `*gin.RouterGroup` |
| Echo v5 | `github.com/go-behemoth/behemoth/plugins/adapters/echo` | its own `go get` | `*echo.Echo` |
| chi v5 | `github.com/go-behemoth/behemoth/plugins/adapters/chi` | its own `go get` | `chi.Router`: a mux, a `Group`, or a `Route` |
| Fiber v3 | `github.com/go-behemoth/behemoth/plugins/adapters/fiber` | its own `go get` | `*fiber.App` |

The framework adapters are separate modules, so your application downloads Gin only if it uses Gin. The `net/http` adapter needs nothing outside the standard library and is part of Behemoth's module.

Every adapter has one function, `New`, and its result goes into `BootConfig.HTTP`:

```go
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	Router: httpapi.RouterConfig{BasePath: "/api/auth"}, // optional; the default is shown
	HTTP:   nethttp.New(mux),
})
```

These hold for all of them:

- Behemoth adds routes and changes nothing else on your router: no middleware, no error handler, no server settings.
- Register your own routes before `Boot`. If one of them is the same route as one of Behemoth's, `Boot` returns an error and your route is left as it was.
- With `HTTP` left nil, `Boot` builds and checks the routes without serving them. Use that in a worker or a CLI.

## net/http

```go
import "github.com/go-behemoth/behemoth/plugins/adapters/nethttp"

mux := http.NewServeMux()
mux.HandleFunc("GET /{$}", home) // your own routes

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	HTTP:   nethttp.New(mux),
})
if err != nil {
	log.Fatal(err)
}

log.Fatal(http.ListenAndServe(":8080", mux))
```

Behemoth registers each route as a method and a path, for example `POST /api/auth/sign-in/email`. Register your own patterns before `Boot`.

### Conflicts with your patterns

`Boot` returns an error when one of your patterns already answers a route of Behemoth's. Your pattern is left as it was.

| Your pattern | Behemoth's route | Result |
| --- | --- | --- |
| `POST /api/auth/sign-in/email` | `POST /api/auth/sign-in/email` | Error. It is the same route. |
| `/api/auth/sign-in/email` (no method, so every method) | `POST /api/auth/sign-in/email` | Error. The mux alone would accept both and give `POST` to Behemoth, and your handler would lose it without notice. |
| `/api/auth/{action}` (no method, a wildcard) | `POST /api/auth/sign-in/email` | Error, for the same reason. |
| `GET /api/auth/sign-in/email` | `POST /api/auth/sign-in/email` | No error. The methods differ. |
| `POST /api/auth/{action}` | `POST /api/auth/sign-in/email` | No error. Your pattern names its method, so the mux's own rule applies: the more specific pattern answers, which is Behemoth's. |
| `/api/`, `/`, or `/api/auth/{rest...}` (a subtree) | `POST /api/auth/sign-in/email` | No error. A subtree is a catch-all: your handler answers what no more specific pattern claims. |

To fix an error, give your pattern its methods (`GET /api/auth/{action}`), move it to another path, or move Behemoth with `RouterConfig.BasePath`.

> [!WARNING]
> The conflict check looks at the mux you pass to `nethttp.New`, once, when `Boot` runs. It cannot see the cases below, and the mux reports none of them. In each one a request reaches a different handler than you expect.
>
> | Case | What happens | What to do |
> | --- | --- | --- |
> | You register a pattern without a method on one of Behemoth's paths **after** `Boot`. | No error. Behemoth answers its method and your handler gets the others. | Register all your patterns before `Boot`. |
> | Your pattern has a host, such as `auth.example.com/api/auth/sign-out`. | No error. On that host your handler answers every method and Behemoth's route is not reached. | Keep host patterns off Behemoth's paths. |
> | Behemoth has a mux of its own (see below), and your main mux has a pattern below the one you registered Behemoth's mux under, such as `POST /api/auth/sign-out` next to `/api/auth/`. | No error. Your main mux sends those requests to your handler and they never reach Behemoth. | Keep the main mux free of patterns under Behemoth's base path. |
>
> After `Boot`, send one request to each of Behemoth's routes you rely on, for example in a test of your server, to confirm that Behemoth answers it.

### Middleware around Behemoth's routes

Give Behemoth a mux of its own and register that mux, wrapped, on yours:

```go
authMux := http.NewServeMux()

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	HTTP:   nethttp.New(authMux),
})

mux.Handle("/api/auth/", logRequests(authMux))
```

Do not strip the prefix: Behemoth's routes carry the full path. The conflict check then covers `authMux` only, which is the third case of the warning above.

### `HEAD` requests

The mux sends a `HEAD` request to the matching `GET` route.

## Fiber

```go
import (
	fiberadapter "github.com/go-behemoth/behemoth/plugins/adapters/fiber"
	"github.com/gofiber/fiber/v3"
)

fiberApp := fiber.New()
fiberApp.Get("/", home) // your own routes

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg,
	HTTP:   fiberadapter.New(fiberApp),
})
if err != nil {
	log.Fatal(err)
}

log.Fatal(fiberApp.Listen(":8080"))
```

The adapter is for Fiber v3. `New` takes the app itself and not a group.

**Register your routes and middleware before `Boot`.** Fiber accepts the same route twice and runs the one registered first, so order decides what happens:

| You register | When | Result |
| --- | --- | --- |
| a route with the method and path of one of Behemoth's | before `Boot` | `Boot` returns an error |
| the same | after `Boot` | No error. Behemoth's route answers and yours is never reached. |
| middleware with `Use` on Behemoth's path | before `Boot` | It runs around Behemoth's routes |
| the same | after `Boot` | It does not run for Behemoth's routes |

Paths are compared the way your app matches them: without case unless you set `Config.CaseSensitive`, and without a trailing slash unless you set `Config.StrictRouting`.

**To run your middleware around Behemoth's routes**, register it on their path:

```go
fiberApp.Use("/api/auth", logRequests)
// then Boot
```

Fiber differs from the `net/http` routers in a few ways that reach your hooks and plugins:

| Topic | On Fiber |
| --- | --- |
| The request | Handlers and hooks get a `*http.Request` like on any other router. The adapter builds it from Fiber's request and copies it, so it stays valid after the handler returns. You do not need `Config.Immutable` for Behemoth. |
| The context | It is `c.Context()`. To pass a context to Behemoth, such as the span of a tracing middleware, set it in your middleware with `c.SetContext`. It is not cancelled when the client disconnects. |
| The client's address | Behemoth reads the connection's address and `RouterConfig.TrustedProxies`, as on every router. Fiber's `TrustProxy` setting does not apply to Behemoth's routes. |
| Path parameters | A plugin reads them decoded (`a b` for `a%20b`), as on the other routers, whether or not you set `Config.UnescapePath`. |
| A request URI Go cannot parse | The route answers `400`. One example is a path with a broken percent escape, such as `%zz`. |
| `HEAD` requests | Fiber answers them with the matching `GET` route unless you set `Config.DisableHeadAutoRegister`. |

## Gin, Echo, and chi

```go
import ginadapter "github.com/go-behemoth/behemoth/plugins/adapters/gin"

engine := gin.New()
// HTTP: ginadapter.New(engine)
```

```go
import echoadapter "github.com/go-behemoth/behemoth/plugins/adapters/echo"

e := echo.New()
// HTTP: echoadapter.New(e)
```

```go
import chiadapter "github.com/go-behemoth/behemoth/plugins/adapters/chi"

r := chi.NewRouter()
// HTTP: chiadapter.New(r)
```

The Echo adapter returns an error for a route that your Echo instance already has with the same method and path. It checks this itself, so the result does not depend on Echo's `AllowOverwritingRoute` setting, which `echo.New` turns on from v5.4.

The Gin and chi adapters also take a group. Behemoth's routes then sit behind the group's prefix and its middleware:

```go
auth := engine.Group("", logRequests)
// HTTP: ginadapter.New(auth)
```

## Path parameters in a plugin

A plugin writes a path parameter as `{name}` and reads it with `rctx.Param("name")`, whatever the router:

```go
func (p *Plugin) Routes() []httpapi.Route {
	return []httpapi.Route{{
		Method: http.MethodGet,
		Path:   "/users/{id}",
		Handler: func(rctx *httpapi.RequestContext) error {
			return rctx.Response.JSON(http.StatusOK, map[string]string{"id": rctx.Param("id")})
		},
	}}
}
```

Each adapter translates the syntax for its router.
