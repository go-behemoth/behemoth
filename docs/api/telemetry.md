# Telemetry

Behemoth reports what it does in four ways: log lines, audit events, metrics, and trace spans. Each goes to a sink you provide. The logger, the metrics sink and the tracer are optional and silent when you leave them out. Audit is on by default and writes to the `audit_log` table.

For stdout logging you need nothing beyond this package. For metrics and traces, the [OpenTelemetry adapter](#opentelemetry) connects Behemoth to a collector, Jaeger, or any other OTLP destination.

## Setup

Build a `Telemetry` with `telemetry.New` and pass it to `Boot`:

```go
import (
	"log/slog"
	"os"

	"github.com/go-behemoth/behemoth/telemetry"
)

tel := telemetry.New(
	telemetry.NewJSONLogger(os.Stdout, slog.LevelInfo), // logger
	nil, // audit recorder: the default, the audit_log table
	nil, // metrics: none
)

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto:    cryptoCfg,
	Telemetry: tel,
})
```

A nil logger or metrics sink means none. A nil audit recorder means the default. See [Audit](#audit). `BootConfig.Telemetry` itself may be nil.

## Choose a logger

| You want | Use |
| --- | --- |
| readable lines on stdout | `telemetry.NewTextLogger(os.Stdout, slog.LevelInfo)` |
| JSON lines on stdout | `telemetry.NewJSONLogger(os.Stdout, slog.LevelInfo)` |
| your existing `*slog.Logger` | `telemetry.NewSlogLogger(logger)` |
| zap or zerolog | their slog handler, through `telemetry.NewSlogLogger(slog.New(handler))` |
| anything else | implement `telemetry.Logger` |

The interface has four methods:

```go
type Logger interface {
	Debug(ctx context.Context, msg string, fields behemoth.M)
	Info(ctx context.Context, msg string, fields behemoth.M)
	Warn(ctx context.Context, msg string, fields behemoth.M)
	Error(ctx context.Context, msg string, fields behemoth.M)
}
```

`fields` may be nil. The map you receive is yours to keep, and Behemoth does not reuse it.

## What Behemoth does to every line

Whichever logger you pass, `telemetry.New` wraps it. Your backend receives fields that are already:

- **Redacted.** The value of a field whose key is, or ends with, `password`, `token`, `secret`, `hash`, `cookie`, `authorization`, or `email` is replaced by `[REDACTED]`. Matching ignores case, so `newPassword` and `refresh_token` are covered. `tokenID` is not: it is an identifier.
- **Correlated.** A line written while handling a request has a `request_id` field.

Two options change redaction:

```go
tel := telemetry.New(logger, nil, nil,
	telemetry.WithEmailsInLogs(),               // let email addresses through
	telemetry.WithRedactedKeys("api_key", "otp"), // redact more keys
)
```

By default logs identify a user by ID only. Use `WithEmailsInLogs` if your log storage is allowed to hold email addresses.

Redaction works on field keys. Text inside a log message is not inspected.

## What Behemoth logs

| Level | When | Examples |
| --- | --- | --- |
| Error | Behemoth failed at something it should have done | a request answered with a 5xx other than a busy server's `503`, a hook handler that returned an error or panicked, a rejected secret rotation |
| Warn | Behemoth continued, with something degraded | a session cache write failed, the rate-limit store is unavailable, a request refused with `503` because the password hasher was busy, a configuration that is probably a mistake |
| Info | lifecycle | the boot summary, a secret rotation applied |
| Debug | per-request detail | a request that was rejected (wrong password, rate limit), SQL statements |

A rejected request is not an error. A wrong password or an expired token is logged at Debug only, so the Error level stays reserved for problems on your side.

Every line has a `component` field naming its source: `boot`, `router`, `hooks`, `ratelimit`, `session`, `token`, `crypto`, `migration`, a plugin's name such as `emailpassword`, or a storage adapter such as `storage.postgres`.

### Failed requests

A request that ends in a 5xx produces one Error line from the router:

```json
{"level":"ERROR","msg":"request failed","component":"router",
 "method":"POST","route":"/api/auth/sign-up/email","status":500,
 "error":"dial tcp 10.0.0.5:5432: connection refused",
 "error_code":"database_error","error_category":"database","op":"Create",
 "request_id":"5f2c8e0d9a7b4c1e8f3a6d2b9c4e7a10"}
```

The client receives only `{"error": "internal server error"}` and the `X-Request-ID` header, whose value matches `request_id`. `route` is the route's pattern, such as `/users/{id}`, not the path that was requested.

### Boot summary

When `Boot` succeeds it writes one Info line, `behemoth booted`, with the plugins in boot order (`plugins`), the number of routes (`routes`), whether they were mounted on an HTTP framework (`routes_mounted`), and whether a key-value store is configured (`kv`).

It also warns about configurations that work but are probably not what you meant. One such case is `RouterConfig.ClientIPHeader` set without `TrustedProxies`, in which case the header is ignored.

### SQL statements

The `sqlite`, `postgres`, `mysql`, and `sqlserver` adapters can log each statement they run. This is off unless you give the adapter a logger:

```go
tel := telemetry.New(telemetry.NewTextLogger(os.Stdout, slog.LevelDebug), nil, nil)

db := pgAdapter.NewPostgresAdapter(conn, app.Resolver).WithLogger(tel.Logger)
```

Statements are logged at Debug, with the SQL text in the `statement` field. Argument values are never logged, so a line shows `WHERE email = $1` and not the address.

Pass `tel.Logger`, the logger `telemetry.New` returned, and not your raw logger, so statement lines get the request ID like every other line.

## Field names

Behemoth uses the same keys in every line. They are constants in the `telemetry` package.

| Constant | Key | Meaning |
| --- | --- | --- |
| `FieldComponent` | `component` | the part of Behemoth that wrote the line |
| `FieldOp` | `op` | the operation that failed |
| `FieldRequestID` | `request_id` | the request being handled |
| `FieldUserID` | `user_id` | |
| `FieldSessionID` | `session_id` | |
| `FieldPlugin` | `plugin` | the plugin a hook handler belongs to |
| `FieldPoint` | `point` | a hook point |
| `FieldError` | `error` | the error's text |
| `FieldErrorCode` | `error_code` | the error's code, or `unknown_error` |
| `FieldErrorCategory` | `error_category` | the error's category |
| `FieldMethod` | `method` | HTTP method |
| `FieldRoute` | `route` | a route's pattern |
| `FieldStatus` | `status` | HTTP status code |
| `FieldStatement` | `statement` | SQL text without values |
| `FieldTraceID`, `FieldSpanID` | `trace_id`, `span_id` | the trace a line belongs to; added by the OpenTelemetry adapter's `WithTraceIDs` |

## Request IDs

Every request to a Behemoth route has an ID. Behemoth takes it from, in order:

1. The request's context, if your own middleware stored one there with `telemetry.ContextWithRequestID`.
2. The `X-Request-ID` request header, if its value is 1 to 128 visible ASCII characters.
3. A newly generated ID.

The ID is returned in the same header of the response, including error responses, so a client can quote it in a bug report. Change the header with `RouterConfig.RequestIDHeader`:

```go
Router: httpapi.RouterConfig{RequestIDHeader: "X-Correlation-ID"},
```

Read the ID in a route handler or a hook handler:

```go
id := telemetry.RequestIDFrom(rctx.Ctx) // in a route handler; use hctx.Ctx in a hook handler
```

Outside a request, the ID is the empty string.

## Log from a plugin

A plugin takes the logger from the `AuthContext` and names itself, so its lines can be told apart:

```go
func (p *Plugin) Init(ac *core.AuthContext) error {
	p.log = telemetry.Named(ac.Telemetry.Logger, "invites")
	return nil
}

func (p *Plugin) accept(rctx *httpapi.RequestContext) error {
	if err := p.redeem(rctx.Ctx, code); err != nil {
		p.log.Warn(rctx.Ctx, "invite could not be redeemed", telemetry.ErrorFields(err))
		return err
	}
	return nil
}
```

`ac.Telemetry` and its fields are never nil, so no checks are needed.

`telemetry.ErrorFields(err)` returns `error`, `error_code`, `error_category`, and `op` for a Behemoth error, and `error` with the code `unknown_error` for any other. Pass your own fields as a second argument to have them in the same line:

```go
p.log.Warn(ctx, "invite could not be redeemed", telemetry.ErrorFields(err, behemoth.M{"invite_id": id}))
```

An error your route handler returns is logged by the router, so log only the errors you handle yourself.

## Audit

The audit log records actions on accounts: who signed up and signed in, which sign-ins failed and for which account, which sessions were created and revoked, which users were created or changed. A log line can be dropped. An audit event is stored in your database and can be queried.

### What is recorded

| Event type | When | Outcome |
| --- | --- | --- |
| `user.created`, `user.updated`, `user.deleted` | a user row is inserted, updated, or deleted, by any flow | success |
| `auth.signUp.after`, `auth.signUp.failed` | a sign-up finished, or was refused | success, failure |
| `auth.signIn.after`, `auth.signIn.failed` | a sign-in finished, or was refused | success, failure |
| `auth.signOut.after` | a sign-out finished | success |
| `auth.session.afterCreate`, `auth.session.afterRevoke` | a session was created or revoked. A session that ended because its user was deleted is recorded as revoked. | success |
| `token.afterIssue`, `token.consumed`, `token.failed` | a token was issued, used, or refused | success, failure |
| `ratelimit.exceeded` | a rate limit stopped a request, the first time in its window | denied |
| `migration.applied` | a migration was applied | success |
| `crypto.secret.rotated` | a secret rotation took effect | success |

Each event has:

| Field | Meaning |
| --- | --- |
| `ID` | assigned when the event is stored |
| `Type`, `Outcome` | see the table above |
| `ActorType`, `ActorID` | who did it: a `user` with its id, a `service` with its id, `anonymous` for a request without a signed-in user, or `system` for work outside a request, with the name the code gave itself (`identity.System("purge-sessions")`) |
| `DelegateType`, `DelegateID` | who acted for the actor, when someone did: the user who impersonated them, or the code that ran a flow for them (`identity.System("support-tool").For(userID)`). Empty otherwise. |
| `SubjectType`, `SubjectID` | what it was done to: a table name such as `users` or `sessions`, and the row's id |
| `SessionID`, `RequestID` | the session and request involved, when there is one |
| `IPAddress`, `UserAgent` | of the request, when there is one, unless `BootConfig.Audit.OmitIPAndAgent` is set |
| `Metadata` | details of the event type: the failure `code`, with `cause` and `causeCode` (the text and the error code of the error behind it, when there is one), a token's `kind`, the `email` tried in a sign-in for an unknown address |
| `Timestamp` | when it was recorded |

Things worth knowing:

- **A failed sign-in names the account, not the person.** A wrong password for an existing user has that user as subject and an anonymous actor. Behemoth does not know who typed it.
- **Email addresses are kept** in audit metadata, where logs redact them. Passwords, tokens, and other secrets are redacted in both.
- **IP address and user agent are recorded unless you turn them off.** `SessionConfig.CaptureIPAndAgent` covers session rows only. Audit events have a setting of their own:

  ```go
  engine.BootConfig{
  	Audit: core.AuditConfig{OmitIPAndAgent: true}, // audit events keep neither
  }
  ```

  A `ratelimit.exceeded` event then also leaves out the counter key from its metadata: for a limit per client address the key is the address.

- **An impersonated action names both users.** The impersonated user is the actor, so a query by their id finds it, and the impersonator is the delegate.
- **A rate limit records one event per window.** A client that keeps sending requests past its limit adds one `ratelimit.exceeded` event, for the first refused request, and none for the rest of the window. To see how many requests were refused, use the `behemoth.ratelimit.checks` metric with `result="limited"`.

### Read events

```go
page, err := ac.Store.QueryAuditEvents(ctx, telemetry.AuditFilter{
	SubjectType: "users",
	SubjectID:   userID,
	From:        time.Now().Add(-30 * 24 * time.Hour),
	Limit:       50,
})
for _, e := range page.Events {
	fmt.Println(e.Timestamp, e.Type, e.Outcome, e.IPAddress)
}
```

Events come newest first. A filter field you leave empty matches everything. The ones you set must all match.

| Filter field | Selects |
| --- | --- |
| `Types` | events of any of these types |
| `Outcome` | `telemetry.OutcomeSuccess`, `OutcomeFailure`, or `OutcomeDenied` |
| `ActorID` | events done by this user |
| `SubjectType`, `SubjectID` | events about this row |
| `SessionID`, `RequestID` | events of one session or one request |
| `From`, `To` | events recorded at or after `From` and before `To` |
| `Limit` | page size, 50 by default and at most 500 |
| `Cursor` | the next page |

To read on, pass the page's `NextCursor` back as `Cursor` and keep the other fields the same. An empty `NextCursor` means the page was the last. Events recorded while you page do not shift the pages.

```go
filter := telemetry.AuditFilter{Outcome: telemetry.OutcomeFailure}
for {
	page, err := ac.Store.QueryAuditEvents(ctx, filter)
	if err != nil {
		return err
	}
	handle(page.Events)
	if page.NextCursor == "" {
		break
	}
	filter.Cursor = page.NextCursor
}
```

Behemoth has no HTTP route for the audit log. Put `QueryAuditEvents` behind a route of your own, with your own access check.

### How reliable it is

- **User creation and updates** are written in the same database transaction as the user row. The event exists exactly when the change does. If the audit row cannot be written, the change fails too.
- **Everything else** is recorded after the action, best effort. If the write fails, the action has still happened, and Behemoth logs the failure at Error (`audit event could not be recorded`).

### The table

`audit_log` is one of Behemoth's own tables and is created by your migrations like `users` and `sessions`. Because user changes write to it in their transaction, it has to exist before your application serves requests. After upgrading from a version without it, run your migrations first. The same holds for the table's `delegate_type` and `delegate_id` columns: generate and apply the migration that adds them before the new version writes events.

Behemoth never deletes audit events by itself. Remove old ones from a scheduled job of your own, with the `AuditEventsBefore` cutoff of `Store.Purge` (see [Remove old rows](core-tables.md#remove-old-rows)) or on their own:

```go
err := ac.Store.PurgeAuditEvents(ctx, time.Now().AddDate(-1, 0, 0)) // keep one year
```

There is no way to change or delete a single event through Behemoth.

### Record your own events

```go
ac.Telemetry.RecordAudit(ctx, telemetry.AuditEvent{
	Type:        "billing.plan.changed",
	ActorID:     adminID,
	SubjectType: "users",
	SubjectID:   userID,
	Metadata:    behemoth.M{"from": "free", "to": "pro"},
})
```

`RecordAudit` fills in the time, the request ID, and the outcome (`success`) when you leave them out, and redacts the metadata. It is best effort and returns the error after logging it.

A plugin that declares its own hook points can have them audited without recording anything itself. See [Hooks](hooks.md#audit).

### Send events somewhere else

The second argument of `telemetry.New` chooses the recorder.

| You want | Pass |
| --- | --- |
| the `audit_log` table | `nil` (the default) |
| the table and another destination | `telemetry.MultiRecorder(store.NewAuditRecorder(st), yours)` |
| only your own destination | your `telemetry.AuditRecorder` |
| no auditing | `telemetry.NoOpAuditRecorder{}` |

A recorder is one method:

```go
type AuditRecorder interface {
	Record(ctx context.Context, event AuditEvent) error
}
```

For the table and another destination, build the database recorder over a store on your database:

```go
st := store.New(db, store.WithSchema(app.Resolver))
tel := telemetry.New(logger, telemetry.MultiRecorder(store.NewAuditRecorder(st), siem), nil)
```

User creation and updates are still written to the table in their transaction. Your recorder is called for them once the transaction has committed, and never for a change that was rolled back.

`QueryAuditEvents` and `PurgeAuditEvents` always work on the `audit_log` table. With only your own recorder, or with auditing off, the table stays empty.

## Metrics

Behemoth measures its own work and hands the numbers to a sink you provide, the third argument of `telemetry.New`. Without one nothing is measured.

```go
type Metrics interface {
	Counter(ctx context.Context, name string, delta int64, attrs behemoth.M)
	Histogram(ctx context.Context, name string, value float64, attrs behemoth.M)
}
```

The [OpenTelemetry adapter](#opentelemetry) is a ready-made sink. For another metrics library, implement the two methods yourself. A sketch over the Prometheus client:

```go
type promMetrics struct {
	requests *prometheus.CounterVec   // labels: method, route, status
	// ... one vector per metric you want
}

func (p *promMetrics) Counter(_ context.Context, name string, delta int64, attrs behemoth.M) {
	if name == telemetry.MetricHTTPRequests {
		p.requests.WithLabelValues(
			fmt.Sprint(attrs[telemetry.AttrMethod]), fmt.Sprint(attrs[telemetry.AttrRoute]), fmt.Sprint(attrs[telemetry.AttrStatus]),
		).Add(float64(delta))
	}
}
```

Durations are in seconds. Attribute values come from fixed sets (a route pattern, a table name, a failure code) and never hold a user id, an IP address or an email, so every attribute is safe to use as a label.

### What is measured

| Metric | Type | Attributes | Counts or times |
| --- | --- | --- | --- |
| `behemoth.http.requests` | counter | `method`, `route`, `status` | requests to Behemoth's routes |
| `behemoth.http.duration` | histogram | `method`, `route`, `status` | how long each took |
| `behemoth.http.origin_refused` | counter | `reason` | requests the origin check refused; see [Cross-site requests](sessions.md#cross-site-requests) |
| `behemoth.auth.sign_in` | counter | `outcome`, `reason` | sign-ins that finished or were refused |
| `behemoth.auth.sign_up` | counter | `outcome`, `reason` | sign-ups that finished or were refused |
| `behemoth.session.created` | counter | | sessions created |
| `behemoth.session.revoked` | counter | | sessions revoked |
| `behemoth.session.validated` | counter | `cache`, `outcome`, `reason` | session lookups by token, one per authenticated request |
| `behemoth.token.issued` | counter | `kind` | tokens issued |
| `behemoth.token.consumed` | counter | `kind` | tokens used |
| `behemoth.token.failed` | counter | `reason` | tokens refused |
| `behemoth.ratelimit.checks` | counter | `rule`, `result` | rate-limit rules evaluated |
| `behemoth.hook.duration` | histogram | `point`, `phase`, `plugin` | each hook handler call |
| `behemoth.hook.errors` | counter | `point`, `phase`, `plugin` | handler calls that returned an error or panicked |
| `behemoth.store.duration` | histogram | `op`, `entity` | each database operation Behemoth makes |
| `behemoth.store.errors` | counter | `op`, `entity`, `error_category` | database operations that returned an error |
| `behemoth.audit.record_failures` | counter | `type` | audit events that could not be stored |
| `behemoth.mail.sent` | counter | `kind`, `result` | messages handed to your mail sender; see [Mail](mail.md) |
| `behemoth.sms.sent` | counter | `kind`, `result` | messages handed to your SMS sender; see [SMS](sms.md) |
| `behemoth.password.wait` | histogram | | how long each password hash or check waited for its turn under `crypto.PasswordLimit`, zero when it did not wait |
| `behemoth.password.busy` | counter | | password hashes refused with `503` and `server_busy` after waiting `PasswordLimit.MaxWait`; see [Email and password](emailpassword.md#bound-the-password-hashes-that-run-at-once) |

The names and attribute keys are constants in the `telemetry` package (`telemetry.MetricSignIn`, `telemetry.AttrOutcome`).

Attribute values:

| Attribute | Values |
| --- | --- |
| `route` | the route's pattern, such as `/api/auth/sign-in/email` or `/users/{id}` |
| `status` | the HTTP status the client received |
| `outcome` | `success` or `failure` |
| `reason` | on failure only: the failure's code, such as `invalid_credentials`, `user_not_found`, `session_expired` |
| `cache` | `hit` or `miss` |
| `result` | rate limits: `allowed`, `limited`, `error` when the rate-limit store could not be reached, or `skipped` when a hook rule did not apply to the call. Mail: `sent`, `error`, or `dropped` when the background queue was full |
| `phase` | `before`, `after`, or `failed` |
| `plugin` | the owner of the hook handler: a plugin's name, or your application's |
| `op` | `create`, `find_one`, `find_many`, `count`, `update`, `update_one`, `update_many`, `delete`, `delete_one`, `delete_many`, `delete_all`, `transaction` |
| `entity` | the table, such as `users`; empty for `transaction` |
| `error_category` | the error's category, such as `not_found`, `duplicate_key`, `database` |

### Read the metrics

These queries are a starting point:

- **Failed sign-ins:** `behemoth.auth.sign_in` with `outcome="failure"`, split by `reason`. A rise in `invalid_credentials` for a steady `success` rate is what a password-guessing run looks like.
- **Session cache hit ratio:** `behemoth.session.validated` with `cache="hit"` over all of them.
- **Database health:** alert on `behemoth.store.errors` with `error_category="database"`. `not_found` is counted too and is a normal answer: sign-up looks an address up and expects to find nothing.
- **A slow hook handler:** `behemoth.hook.duration` by `plugin`. Handlers on data points run inside a database transaction, so a slow one also holds locks.
- **Hook errors:** with `phase="before"` they are refusals, which is how a handler stops an operation. With `phase="after"` or `"failed"` a handler did not do its work.
- **Rate limiting:** `behemoth.ratelimit.checks` with `result="limited"` by `rule`. `result="error"` means the counter store is down. A hook rule that is `skipped` on every call reads a value its point does not publish.
- **Password hashing capacity:** a rising `behemoth.password.wait` means sign-ins queue for the hasher, and any `behemoth.password.busy` means some were refused with `503`. Both are per process, so they are a signal to add replicas.

### Measure your own code

A plugin uses the same sink:

```go
ac.Telemetry.Count(ctx, "invites.redeemed", behemoth.M{"plan": plan})
ac.Telemetry.ObserveSince(ctx, "invites.lookup.duration", start, nil)
```

Keep attribute values to a small fixed set, as Behemoth does.

What your plugin does through `ac.Store`, and through `ac.Store.DB()` for its own tables, is already timed in `behemoth.store.duration`. With a metrics sink configured, `ac.Store.DB()` returns a wrapper around your database adapter; `ac.DB` is the adapter itself.

## Tracing

With a tracer, Behemoth records a span for each piece of its work, so a trace of one request shows where the time went.

Set the tracer with an option:

```go
tel := telemetry.New(logger, nil, metrics, telemetry.WithTracer(tracer))
```

### Spans

| Span | Attributes | Covers |
| --- | --- | --- |
| `behemoth.request` | `method`, `route`, `request_id`, `status` | one request to a Behemoth route |
| `behemoth.hook.chain` | `point`, `phase` | the handlers of one hook point |
| `behemoth.hook.handler` | `point`, `plugin` | one handler call |
| `behemoth.store.<op>` | `entity` | one database operation, such as `behemoth.store.find_one` on `users` |
| `behemoth.store.transaction` | | a transaction, with the operations inside as children |
| `behemoth.session.create`, `.validate`, `.revoke` | | the session manager |
| `behemoth.token.issue`, `.consume` | `kind` | the token manager |
| `behemoth.password.hash`, `.verify` | | one password hash or check by the password hasher, including its wait for a turn |
| `behemoth.ratelimit.check` | `rule`, `result` | one rate-limit rule |
| `behemoth.migration.apply` | `id`, `baseline` | one applied migration |
| `behemoth.outbound.request` | `method`, `host`, `status` | one call to another service, until its answer is read; see [Outbound calls](outbound.md) |

A span of an operation that returned an error also has `error_code`, and `error_category` when the error is one of Behemoth's.

Things worth knowing:

- **Behemoth joins your trace.** It does not read `traceparent` or any other header. Your HTTP framework's tracing middleware starts the server span, and `behemoth.request` becomes its child. Without such middleware, each request to a Behemoth route is a trace of its own.
- **Only real failures mark a span as failed.** A database error or a panic does. A wrong password, an expired session, a lookup that finds nothing, a hook handler that refuses an operation, or a password hasher too busy to take a hash does not. The span carries the `error_code` and stays unfailed.
- **Hook points without handlers have no chain span.**
- **`request_id`** on the request span is the ID in the `X-Request-ID` response header and in the log lines.

### Trace your own code

Start spans from the context Behemoth gives you, and they appear in the right place:

```go
func (p *Plugin) onUserCreated(hctx *core.HookContext, result any) error {
	ctx, span := hctx.Auth.Telemetry.StartSpan(hctx.Ctx, "invites.attach", nil)
	err := p.attach(ctx, result.(*models.User))
	telemetry.FinishSpan(span, err)
	return err
}
```

`telemetry.FinishSpan` ends the span and describes the error on it, with the rule above for what counts as a failure. A hook handler already runs inside a `behemoth.hook.handler` span, so calls you make with `hctx.Ctx` are traced under it without any code of yours. The same goes for `rctx.Ctx` in a route handler.

## OpenTelemetry

The adapter is a separate module, so applications that do not use OpenTelemetry do not download it:

```
go get github.com/go-behemoth/behemoth/telemetry/adapters/otel
```

It provides three things:

| Function | Gives Behemoth |
| --- | --- |
| `behemothotel.NewTracer(tracerProvider)` | a tracer |
| `behemothotel.NewMetrics(meterProvider)` | a metrics sink |
| `behemothotel.WithTraceIDs(logger)` | a logger that adds `trace_id` and `span_id` to its lines |

You build the OpenTelemetry SDK, its providers and exporters, and shut them down. The adapter only needs the providers. Passing nil uses the global ones (`otel.GetTracerProvider`, `otel.GetMeterProvider`).

```go
import (
	"github.com/go-behemoth/behemoth/telemetry"
	behemothotel "github.com/go-behemoth/behemoth/telemetry/adapters/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The exporters read OTEL_EXPORTER_OTLP_ENDPOINT, for example http://localhost:4318.
traceExporter, err := otlptracehttp.New(ctx)
metricExporter, err := otlpmetrichttp.New(ctx)

res := resource.NewSchemaless(attribute.String("service.name", "my-app"))
tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
meterProvider := sdkmetric.NewMeterProvider(
	sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)), sdkmetric.WithResource(res))

tel := telemetry.New(
	behemothotel.WithTraceIDs(telemetry.NewJSONLogger(os.Stdout, slog.LevelInfo)),
	nil, // audit: the audit_log table
	behemothotel.NewMetrics(meterProvider),
	telemetry.WithTracer(behemothotel.NewTracer(tracerProvider)),
)

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{Crypto: cryptoCfg, Telemetry: tel})

// Before the process exits:
tracerProvider.Shutdown(ctx)
meterProvider.Shutdown(ctx)
```

- **Shut the providers down before exiting.** They export in batches, so a process that stops without it loses its last few seconds of traces and metrics. For a server, that means handling SIGINT and SIGTERM. For a short command, call `Shutdown` before it returns.
- **Jaeger** needs nothing special. Jaeger accepts OTLP, so point the trace exporter at it: `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces`. Jaeger takes traces and no metrics, so set the traces variable and not the general one, or the metric exporter logs errors.
- **Incoming traces** are picked up by your framework's middleware, such as `otelgin`, `otelecho`, or `otelhttp`, not by Behemoth. Add it to your router as usual.
- **SQL statements** as spans come from your driver's instrumentation, such as `otelsql`. They appear under Behemoth's `behemoth.store.*` spans.
- **Metrics** arrive under the names in the [Metrics](#metrics) table, in the instrumentation scope `github.com/go-behemoth/behemoth`. Durations have the unit `s`.
- **Logs** still go where your logger writes them. `WithTraceIDs` adds the two IDs so a line can be matched to its trace. To export logs through OpenTelemetry as well, build a slog logger over the `otelslog` bridge and pass it to `telemetry.NewSlogLogger`.

## A complete example

`examples/init` wires everything on this page into one application:

| File | Shows |
| --- | --- |
| `telemetry.go` | building the `Telemetry`: a text logger, the default audit table, and OTLP traces and metrics when an endpoint is set |
| `serve.go` | passing it to `Boot`, giving the same logger to the database adapter, adding the framework's tracing middleware, and flushing on shutdown |
| `audit.go` | reading the audit log with `QueryAuditEvents` |

```
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/jaeger

cd examples/init
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces LOG_LEVEL=debug go run . serve

curl -X POST localhost:8080/api/auth/sign-up/email \
  -d '{"email":"ada@example.com","password":"correct horse","inviteCode":"WELCOME2026"}'

go run . audit -email ada@example.com
```

The trace of the sign-up is at http://localhost:16686 under the service `behemoth-example`. The example needs PostgreSQL and its migration applied first. The comment at the top of `main.go` has the steps.

## Testing

`telemetry/telemetrytest` records what was emitted:

```go
tel, rec := telemetrytest.New()
// ... boot with tel, exercise the code ...

for _, line := range rec.Logger.At(slog.LevelError) {
	t.Errorf("unexpected error line: %s %v", line.Message, line.Fields)
}
events := rec.Audit.OfType("auth.signIn.failed")
failed := rec.Metrics.Count(telemetry.MetricSignIn, behemoth.M{telemetry.AttrOutcome: "failure"})
spans := rec.Tracer.Named(telemetry.SpanSessionCreate)
```

Passing `tel` replaces the default recorder, so in such a test events go to `rec.Audit` and not to the `audit_log` table.

Recorded lines are redacted and correlated like those a real backend receives.
