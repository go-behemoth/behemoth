# Logging

This document describes how behemoth's components log, as built: which component writes which line, where a request's failure is logged, how the SQL adapters log statements, and what `Boot` reports. It is phase 2 of `plan.md` and builds on `foundations.md`, which covers the `Logger` interface, redaction, and the request ID.

## The rule: log where an error stops

A function either returns an error or logs it. It does not do both, so one failure produces one line.

| Situation | Who logs |
| --- | --- |
| an error returned up to a route | the router, in `wrapWithErrorMapping` |
| an error a component swallows to keep going (a cache write, an after-hook handler) | that component, where it swallows it |
| an error returned to a caller that is not a route (a CLI, a job, `Plugin.SignIn` called from code) | the caller; behemoth returns it and logs nothing |

This is why the store logs almost nothing. Every store method returns its error. The one failure it swallows is a panic in an after-commit callback, which happens after the write is durable and has no caller to return to. The same holds for most of the token manager and the email/password flows.

## Components and their loggers

Each component takes a named logger once, at construction, so its lines carry a `component` field.

| Component | `component` | Named by | Lines |
| --- | --- | --- | --- |
| `Boot` | `boot` | `logBootSummary` | the boot summary, configuration warnings |
| `Router` | `router` | `NewRouter`, from `AuthContext.Telemetry` | request failed (Error), request refused: no capacity (Warn), request rejected (Debug) |
| `DefaultDispatcher`, `dataHooks` | `hooks` | `NewDefaultDispatcher` | handler errors and panics, failed audit writes, undispatchable after-commit points |
| `Store` | `store` | `runCommitted`, on use | after-commit callback panicked (Error) |
| `DefaultRateLimiter` | `ratelimit` | `Boot` | store unavailable (Warn) |
| `DefaultSessionManager` | `session` | `NewSessionManager` | cache write and invalidation failures (Warn) |
| `DefaultTokenManager` | `token` | `log()`, on use | undecodable key-value entry (Warn) |
| `DefaultKeyManager` | `crypto` | `NewDefaultKeyManager` | rotation applied, rotation rejected, watch not started |
| `DefaultMigrationRunner`, managed path | `migration` | `NewMigrationRunner`, `warn` | atomicity warning, script render and write failures |
| email/password plugin | `emailpassword` | `Plugin.Init` | sign-in failed, sign-out failed (Error) |
| SQL adapters | `storage.sqlite`, `storage.postgres`, `storage.mysql`, `storage.sqlserver` | `adapters.LogQueries` | sql statement (Debug) |

Constructors use `telemetry.OrDefault(tel).Named(name)`. `Telemetry.Named` returns a copy of the `Telemetry` with a named logger and the same audit recorder and metrics sink, so the component keeps writing `tel.Logger.Warn(...)` as before.

The token manager is the exception: `Boot` hands it the `AuthContext` before every field is set, so it reads the logger from `auth.Telemetry` when it logs, in `DefaultTokenManager.log`. Its one line is rare, so building the named logger per call costs nothing that matters.

Error lines are built with `telemetry.ErrorFields(err, extra)`. No call site writes the `error` key by hand any more.

## Request failures

`Router.wrapWithErrorMapping` is where an error from a route's chain becomes a response, and it is also where it is logged:

```go
status, body := r.errorMapper.Map(err)
fields := telemetry.ErrorFields(err, behemoth.M{
	telemetry.FieldMethod: route.Method,
	telemetry.FieldRoute:  route.Path,
	telemetry.FieldStatus: status,
})
switch {
case errs.IsCategory(err, errs.CategoryUnavailable):
	r.log.Warn(rctx.Ctx, "request refused: no capacity", fields)
case status >= 500:
	telemetry.SpanFrom(rctx.Ctx).RecordError(err)
	r.log.Error(rctx.Ctx, "request failed", fields)
default:
	r.log.Debug(rctx.Ctx, "request rejected", fields)
}
```

- **Level by status.** A 5xx means behemoth failed at something it owed and is logged at Error. Any other status is a rejection of the request (a wrong password, a rate limit, a missing row) and is logged at Debug. A rejection is not an error of the system; phase 3 records it as an audit event and phase 4 counts it.
- **A refusal for lack of capacity is a Warn.** An error of category `unavailable`, such as a busy password hasher, is a `503`, but the process refused work it had no room for, as it is set up to. Under a burst there is one per refused request, and at Error they would bury real failures.
- **`route` is the pattern.** `route.Path` is the mounted path with its `{name}` parameters, not the request's path. It has a bounded set of values and holds no identifier.
- **`error` is the internal message.** The response gets `PublicMessage` from the mapper; the log gets `err.Error()`. The two never mix.
- **The request ID** is added by the guarded logger from `rctx.Ctx`, and it is the same value the response header carries, so a client's report can be matched to the line.

`wrapWithErrorMapping` takes the `Route` for this, where it used to take only the next handler.

`[Limit]` The router logs only what is returned to it. A handler that writes its own error response returns nil and is not seen.

## The email/password plugin

The flows (`signUpBody`, `signInBody`) return their errors and log nothing. So do the three handlers: `handleSignIn` and `handleSignOut` return every error to the router, and `handleSignUp` returns the typed ones. A failed sign-in or sign-out is therefore logged once, by the router: as `request refused: no capacity` at Warn when the password hasher was busy, as `request failed` at Error for another 5xx, and as `request rejected` at Debug otherwise.

The plugin still takes a logger in `Init` (`p.log`) and writes nothing to it at present. `handleSignUp` answers sign-up's untyped refusals itself with a `400`; those are rejections, so nothing is lost by not logging them.

`PluginContext` was deleted. It was declared with a `Logger` field, but no plugin ever received one. A plugin takes its logger from the `AuthContext` in `Init`:

```go
p.log = telemetry.Named(ac.Telemetry.Logger, PluginName)
```

### Named loggers for plugins come from the AuthContext

**Context:** A plugin needs a logger whose lines can be told apart from core's. `PluginContext` existed for this and was never wired.

**Options considered:**

- *Pass a `PluginContext` to `Init`.* The logger arrives already named. It changes the `Plugin` interface, which every plugin implements, and adds a second context type next to `AuthContext` that carries one useful field.
- *Take the logger from `AuthContext.Telemetry` and name it.* One line in `Init`. The plugin chooses the name, so nothing stops it from using another plugin's.

**Decision:** The second option. `AuthContext` already carries every other service a plugin uses, and the interface stays as it is.

**Revisit if:** plugins need more per-plugin state from core than a name, which would justify a context of their own.

## SQL statements

The four SQL adapters (`sqlite`, `postgres`, `mysql`, `sqlserver`) used to print some statements with their arguments to stdout. They now log every statement through an optional logger:

```go
db := sqliteAdapter.NewSQLiteAdapter(conn, resolver).WithLogger(tel.Logger)
```

How it is built:

- Each adapter has a `Logger` field and a `WithLogger` method that sets it and returns the adapter.
- Each adapter runs its statements on `q()` and not on `DB` directly. `q()` returns `adapters.LogQueries(DB, Logger, component)`.
- `LogQueries` (`storage/adapters/logging.go`) wraps a `Querier` in one that writes a Debug line with `component` and `statement` before each `ExecContext`, `QueryContext`, and `QueryRowContext`. For a nil or no-op logger it returns the `Querier` itself.
- `Transaction` builds the adapter bound to the transaction with the same logger, so statements inside a transaction are logged too.

`DB` stays the raw `*sql.DB` or `*sql.Tx`. `Transaction` switches on its type to decide whether to begin a transaction, and a wrapped value would hide that.

Argument values are never logged. They hold password hashes, token hashes and personal data, and key-based redaction cannot help with a positional argument list. The statement text has placeholders in their place.

The logger is opt-in per adapter and separate from `BootConfig.Telemetry`, because the application builds the adapter before `Boot`. Passing the same `tel.Logger` to both gives statement lines the same redaction and request ID as every other line.

`[Cost]` `Logger` has no level check, so with a logger set, each statement builds a small field map even when the backend discards Debug lines. An application that does not want statement logs leaves `WithLogger` out.

`[Not covered]` The `gorm`, `bun`, and `mongo` adapters have no statement logging of their own; GORM and Bun have their own query logging. The migration drivers and introspectors do not log their statements.

### Statement logging wraps the connection

**Context:** The adapters printed statements from a few methods, with arguments. Logging had to cover every statement and stay out of each method's body.

**Options considered:**

- *A log call in each method.* Explicit, and each line can name its operation (`FindOne`). About fifty call sites across four adapters, and a new method can forget its line.
- *Wrap the `Querier`.* One implementation for all four adapters, and no statement can be missed. The line does not know which adapter method ran it.

**Decision:** Wrap the `Querier`. The statement text identifies the operation well enough for a Debug line.

**Revisit if:** statement lines need the name of the adapter method. Tracing did not need a hook per method: the store's spans are started one layer up, around the adapter (`tracing.md`).

## Removed prints

These prints were removed:

- `providers/google.go` and `providers/facebook.go` printed the provider's user-info response body with `log.Println`. The calls are removed and not replaced: the body is personal data. The `providers` package was removed later, ahead of the OAuth redesign.
- `storage/utils.go` printed unmatched columns and scan errors. The scan error is returned to the caller with the same detail, so the prints are removed.

## Boot summary

`logBootSummary` runs as `Boot`'s last step, so its line means `Boot` succeeded.

One Info line, `behemoth booted`:

| Field | Meaning |
| --- | --- |
| `plugins` | plugin names in boot order |
| `routes` | number of routes in the table |
| `routes_mounted` | false when `BootConfig.HTTP` is nil: the table was built and checked but is not served |
| `kv` | false when `BootConfig.KV` is nil: sessions and rate-limit counters use the database only |

One Warn line per configuration that is valid but probably a mistake. There is one check so far:

- `RouterConfig.ClientIPHeader` is set and `TrustedProxies` is empty. `httpapi.ClientIP` ignores the header unless the direct peer is a trusted proxy, so every request is attributed to its direct peer. Behind a load balancer that means one address for all clients, and per-IP rate limits apply to everyone together.

New checks go in `logBootSummary`. A configuration that cannot work is an error from `Boot`, not a warning.
