# Metrics

This document describes behemoth's metrics as built: the interface, the catalog, and where each measurement is taken. It is phase 4 of `plan.md`.

Logs say what happened to one request and audit events say what happened to one account. Metrics answer questions about all of them at once: how many sign-ins fail, how slow the database is, which hook handler takes the time.

The code lives in:

- `telemetry/metrics.go`: `Metrics`, `NoOpMetrics`, the metric names and attribute keys, `Telemetry.MetricsEnabled`, `Count`, `ObserveSince`
- `httpapi/router.go`: `Router.withMetrics`
- `store/instrument.go`: `WithTelemetry`, `instrumentedDB`
- `dispatch/dispatcher.go`: `DefaultDispatcher.observeHandler`, `countPoint`, `corePointMetrics`
- `ratelimit/limiter.go`: `DefaultRateLimiter.countCheck`
- `auth/session.go`: `DefaultSessionManager.countValidated`
- `telemetry/audit.go`: `Telemetry.AuditFailed`
- `telemetry/telemetrytest`: the recording sink

## The interface

```go
type Metrics interface {
	Counter(ctx context.Context, name string, delta int64, attrs behemoth.M)
	Histogram(ctx context.Context, name string, value float64, attrs behemoth.M)
}
```

- **Two instruments.** A counter for things that happen, a histogram for how long they take. Durations are in seconds.
- **The context** is that of the work being measured, so a backend can attach the trace it belongs to (an exemplar).
- **`Gauge` is gone.** Nothing in the library reports a level, and the old method had no call site.

A backend maps the two onto its own instruments. The OpenTelemetry adapter (`telemetry/adapters/otel`, see `tracing.md`) is the first; an application can implement the interface over anything else.

### Attributes are bounded

Every attribute value comes from a fixed set: a route's pattern, a hook point, a table name, an error category, a failure code. No attribute holds a user id, a session id, an IP address, an email or a request path. A backend can use every attribute as a label and the number of series stays fixed by the code and the configuration, not by traffic.

This is a rule for new call sites too. An identifier belongs in a log line or an audit event.

### The cost without a sink

`Telemetry.MetricsEnabled()` is false for `NoOpMetrics`. Call sites on a hot path check it before they build an attribute map:

| Call site | Without a sink |
| --- | --- |
| `Router.withMetrics` | returns the handler unwrapped, at `Build` |
| `store.WithTelemetry` | leaves the adapter unwrapped, in `store.New`, unless a tracer is set |
| dispatcher, session manager, rate limiter | one type check per call, no allocation |

The dispatcher still reads the clock before each handler. That is two clock reads per handler call and no allocation.

`Telemetry.Count(ctx, name, attrs)` and `ObserveSince(ctx, name, start, attrs)` are the two helpers call sites use.

## The catalog

Names and attribute keys are constants in `telemetry/metrics.go` (`MetricSignIn`, `AttrOutcome`, ...).

| Metric | Type | Attributes | Taken in |
| --- | --- | --- | --- |
| `behemoth.http.requests` | counter | `method`, `route`, `status` | `Router.withMetrics` |
| `behemoth.http.duration` | histogram | `method`, `route`, `status` | `Router.withMetrics` |
| `behemoth.http.origin_refused` | counter | `reason` | `OriginCheck.Middleware` |
| `behemoth.auth.sign_in` | counter | `outcome`, `reason` on failure | dispatcher, `auth.signIn.after` and `.failed` |
| `behemoth.auth.sign_up` | counter | `outcome`, `reason` on failure | dispatcher, `auth.signUp.after` and `.failed` |
| `behemoth.session.created` | counter | | dispatcher, `auth.session.afterCreate` |
| `behemoth.session.revoked` | counter | | dispatcher, `auth.session.afterRevoke` |
| `behemoth.session.validated` | counter | `cache`, `outcome`, `reason` on failure | `DefaultSessionManager.Get` |
| `behemoth.token.issued` | counter | `kind` | dispatcher, `token.afterIssue` |
| `behemoth.token.consumed` | counter | `kind` | dispatcher, `token.consumed` |
| `behemoth.token.failed` | counter | `reason` | dispatcher, `token.failed` |
| `behemoth.ratelimit.checks` | counter | `rule`, `result` | `DefaultRateLimiter.evaluate` |
| `behemoth.hook.duration` | histogram | `point`, `phase`, `plugin` | dispatcher, per handler call |
| `behemoth.hook.errors` | counter | `point`, `phase`, `plugin` | dispatcher, per handler call |
| `behemoth.store.duration` | histogram | `op`, `entity` | `instrumentedDB` |
| `behemoth.store.errors` | counter | `op`, `entity`, `error_category` | `instrumentedDB` |
| `behemoth.audit.record_failures` | counter | `type` | `Telemetry.AuditFailed` |
| `behemoth.password.wait` | histogram | | `Argon2idHasher.acquire`, per hash or verify; zero when there was room at once |
| `behemoth.password.busy` | counter | | `Argon2idHasher.acquire`, per call refused with `server_busy`; see [`../crypto/passwords.md`](../crypto/passwords.md) |

## Where each measurement is taken

### Requests

`Router.Build` wraps each route as `withRequestScope(withMetrics(wrapWithErrorMapping(...)))`. Metrics sit outside error mapping, so `status` is the one the client receives, mapped errors included. When the chain returns an error to the adapter (the response could not be built), the adapter answers 500 and so does the metric.

`route` is `Route.Path`, the mounted pattern with its `{name}` parameters.

### Flows, sessions, and tokens

These are counted in the dispatcher, not in the flows. `corePointMetrics` maps a core after or failed point to a counter and an outcome:

```go
hooks.HookSignInAfter:  {telemetry.MetricSignIn, telemetry.OutcomeSuccess},
hooks.HookSignInFailed: {telemetry.MetricSignIn, telemetry.OutcomeFailure},
```

`countPoint` runs at the end of `RunAfter` and `Fail`, next to `recordAudit`. A failure adds `FailureReason.Code` as `reason`, and a `*models.Token` result adds its `kind`.

The sign-in flow, the session manager and the token manager report nothing themselves. Any flow that fires `auth.signIn.after` is counted as a sign-in, which also covers an OAuth sign-in once it fires the same points.

`[Limit]` A point declared by a plugin has no entry. The plugin counts what it needs through `ac.Telemetry.Metrics`.

`[Limit]` The plan listed a `plugin` attribute on the sign-in and sign-up counters. The dispatcher does not know which plugin fired a core point, so the attribute is not there.

### Counting flows in the dispatcher

**Context:** Sign-ins, sign-ups, sessions, and tokens have to be counted with their outcome. The code that produces them is spread over a plugin and two managers.

**Options considered:**

- *Count at each site.* The site knows everything, including which plugin it is. Every flow and every future plugin has to remember to count, in the same names, on every exit path.
- *A metric declaration on `HookPointDef`, like `AuditSpec`.* Plugins could have their own points counted. It adds to the hook declaration API for a need no plugin has yet.
- *A table in core from point to counter.* Every dispatch of a core point is counted once, whoever fired it, with no new API. Only core points are covered.

**Decision:** The table. The after and failed points already mark exactly the moments to count, and the audit events are produced at the same place from the same data.

**Revisit if:** plugins want their points in the catalog, which would turn the table into a declaration on the point.

### Session lookups

`DefaultSessionManager.Get` counts each lookup by token in `countValidated`: `cache` is `hit` or `miss`, `outcome` is `success` or `failure`, and a failure carries the error's code as `reason` (`session_expired`, `session_revoked`, `session_not_found`, ...). `Validate` goes through `Get`, so every authenticated request is one count. The hit ratio of the session cache is `cache="hit"` over the total.

### Rate limits

`DefaultRateLimiter.evaluate` counts each rule it evaluates: `allowed`, `limited`, or `error` when the counter store could not be reached. `CheckHookLimit` counts `skipped` for a hook rule whose `KeyFunc` said the rule does not apply to the call; `evaluate` does not run for it. What happens to the request on `error` is `RateLimitConfig.FailureMode`'s decision and is not part of the attribute.

A rejection is still also an audit event; see the entry "Every rate-limit rejection writes an audit row" in `docs/ongoing.md`.

### Mail

`DefaultSMS` counts `behemoth.sms.sent` the same way, from the queue code the two share. `DefaultMailer` counts `behemoth.mail.sent` by `kind` and `result`: `sent` or `error` where the application's sender returns, for a send the caller waited for and for a background one, and `dropped` when `SendAsync` found the queue full. See [`../mail/mailer.md`](../mail/mailer.md).

### Hook handlers

`observeHandler` runs after every handler call in all four dispatcher methods. It records the duration, and one `behemoth.hook.errors` when the handler returned an error or panicked.

`[Convention]` On a before point a returned error is a veto, the way a handler refuses an operation. It is counted like any other handler error, and the `phase` attribute separates the two: `phase="before"` are refusals, `phase="after"`, and `phase="failed"` are handlers that did not do their work.

### The database

`store.WithTelemetry(tel)` makes `store.New` wrap the store's adapter in an `instrumentedDB`. It implements `behemoth.Database`, times each call, and passes it on. The same wrapper starts the store's spans; see `tracing.md`.

| `op` | `Database` method |
| --- | --- |
| `create`, `find_one`, `find_many`, `count` | the same |
| `update`, `update_one`, `update_many` | the same |
| `delete`, `delete_one`, `delete_many`, `delete_all` | the same |
| `transaction` | `Transaction`, callback included, with an empty `entity` |

- `entity` is the model's `SchemaName()`, the canonical table name.
- `Transaction` hands the callback a wrapped adapter, so operations inside a transaction are measured one by one, and the whole transaction once more as `transaction`.
- An error adds one `behemoth.store.errors` with the `DomainError`'s category, or `unknown`. A lookup that finds nothing is counted with `error_category="not_found"`. That is a normal answer and not a failure of the database; alerts belong on `database` and `transaction`.

`Boot` passes its `Telemetry` to the three stores it builds: the one plugins use, the rate-limit counter store and the audit recorder's.

`[Consequence]` `Store.DB()` returns the wrapped adapter. What a plugin does through it is measured too. It can no longer be type-asserted to the adapter the application built; nothing in the repository did.

### Measuring the store by wrapping its adapter

**Context:** The store has about forty call sites of `s.db`. Each needs a duration and an error count.

**Options considered:**

- *Measure in each store method.* The operation can have a business name (`FindUserByEmail`). Every method and every future one needs the same four lines.
- *Wrap the adapter.* One implementation covers every method, transactions included, and plugins' own tables through `Store.DB()`. The operation is named after the `Database` method, not the store method.
- *Measure in the storage adapters.* Covers code that bypasses the store. It would have to be written once per adapter, in separate modules.

**Decision:** Wrap the adapter in the store. The table and the kind of operation are what a dashboard groups by.

**Revisit if:** a dashboard or a trace needs the store method's name (`FindUserByEmail`). Tracing uses the same wrapper and the same operation names.

The metric is named `behemoth.store.*` because it measures what the store asks of the database. It is not a per-statement metric; drivers have their own instrumentation for that.

### Audit failures

`Telemetry.AuditFailed(ctx, type, err)` logs the Error line of phase 3 and counts one `behemoth.audit.record_failures`. `RecordAudit` calls it for best-effort events, and the dispatcher calls it for the two transaction paths: a failed `RecordTx`, and a failed after-commit record.

## Testing

`telemetrytest.Metrics` keeps every measurement.

| Method | Returns |
| --- | --- |
| `Count(name, attrs)` | the total of a counter over the increments whose attributes include `attrs` |
| `Observations(name, attrs)` | the matching observations of a histogram |
| `Counters()`, `Histograms()` | everything, as `Sample{Name, Value, Attrs}` |

Matching is by subset, so a test names only the attributes it cares about.
