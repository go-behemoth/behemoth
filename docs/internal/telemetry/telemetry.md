# Telemetry

This folder explains how behemoth reports what it does: log lines, audit events, metrics, and trace spans. The four share one package, one configuration value and one set of names, and each has a document of its own.

For how to configure and read them from an application, see [`../../api/telemetry.md`](../../api/telemetry.md). These documents describe the code behind that API.

| Document | Covers |
| --- | --- |
| [foundations.md](foundations.md) | the `telemetry` package, `Telemetry` and `New`, the logger wrapper, redaction, field keys, `ErrorFields`, the request ID, the slog backend, `telemetrytest` |
| [logging.md](logging.md) | which component logs what, the rule for where an error is logged, request failures, SQL statement logging, the boot summary |
| [audit.md](audit.md) | the event model, how the dispatcher builds an event, actor and subject, best-effort and in-transaction recording, the `audit_log` table, paging |
| [metrics.md](metrics.md) | the interface, the catalog, where each measurement is taken, the store wrapper |
| [tracing.md](tracing.md) | the tracer interface, span sites, when a span is marked as failed, the OpenTelemetry adapter |
| [plan.md](plan.md) | the phases the work was done in, the decisions taken before it started, what differs from the first plan, what was left out |

The code lives in:

- `telemetry/`: the interfaces, the names (field keys, audit event types, metric names, span names), and the helpers every component uses. Standard library only.
- `telemetry/telemetrytest/`: recording sinks for tests.
- `telemetry/adapters/otel/`: the OpenTelemetry backend, in its own module.
- `store/audit.go`, `store/instrument.go`, `models/audit.go`: the audit log's storage, and the wrapper that measures and traces database operations.
- `httpapi/router.go`, `engine/init.go`, `auth/`: the call sites.

---

## The shape

Behemoth's part is thin. It defines four small interfaces and the names used through them, and decides where a line is written, an event recorded, a counter incremented and a span started. A backend does the rest.

```
 components                     telemetry package                         backends
 ──────────                     ─────────────────                         ────────
 router, dispatcher,   ───►  Telemetry
 store, managers,              ├─ Logger   (redaction, request ID)  ───►  slog: stdout, zap, zerolog
 rate limiter, crypto,         ├─ Audit    (normalize, log failures) ───►  store.AuditRecorder: audit_log table
 migration runner,             ├─ Metrics                            ───►  behemothotel.NewMetrics: OTLP
 plugins                       └─ Tracer                             ───►  behemothotel.NewTracer:  OTLP
```

This is the split the error taxonomy and the storage layer use. `errors/` defines categories and an `ErrorMapper` adapts them to HTTP. `storage.go` defines `Database` and each adapter lives in its own module. Here `telemetry/` defines the interfaces and names, and the OpenTelemetry adapter lives in its own module, so the root module depends on the standard library only.

Every component receives the same `*telemetry.Telemetry`, built once by the application with `telemetry.New` and passed in `BootConfig.Telemetry`. Each field left out is a no-op, except audit, where `Boot` uses the database recorder.

## What each signal is for

| Signal | Answers | Kept | May be dropped |
| --- | --- | --- | --- |
| Log line | what happened to this request, for whoever operates the system | wherever the logger writes | yes |
| Audit event | who did what to which account, and how it ended | the application's database | no, for user writes; best effort otherwise |
| Metric | how many and how long, over all requests | the metrics backend | aggregated by nature |
| Span | where one request spent its time | the tracing backend | often sampled |

The same occurrence usually produces more than one. A wrong password is a Debug log line from the router, an `auth.signIn.failed` audit event, one `behemoth.auth.sign_in{outcome="failure"}`, and a `behemoth.request` span with status 400 that is not marked as failed.

## One rule in four places

A refused request is not an error of the system. Each signal applies that the same way:

| Signal | A rejection (wrong password, expired session, a veto) | A failure (the database is down, a panic) |
| --- | --- | --- |
| Log | Debug, `request rejected` | Error, `request failed` |
| Audit | outcome `failure` or `denied` | nothing: there is no action to record |
| Metric | counted with `outcome="failure"` and a `reason` | counted in `behemoth.store.errors{error_category="database"}`, and as a 5xx in `behemoth.http.requests` |
| Span | `error_code` attribute, span not failed | `RecordError`, span failed |

`telemetry.IsSystemFailure(err)` is the test the span uses. The router uses the mapped status (5xx) for its log level, which gives the same split for errors that reach it.

A refusal for lack of capacity, an error of category `unavailable` such as a busy password hasher, is neither. The router answers it with `503` and logs it at Warn as `request refused: no capacity`, and its span is not failed. The process works as it is set up to, and a burst of these at Error would bury real failures. `behemoth.password.busy` counts the hasher's refusals.

## How the signals are tied together

| Identifier | Carried by | Set by |
| --- | --- | --- |
| request ID | log lines (`request_id`), audit events (`RequestID`), the `behemoth.request` span, the `X-Request-ID` response header | `Router.withRequestScope` |
| trace ID and span ID | spans, and log lines when the logger is wrapped with `behemothotel.WithTraceIDs` | the tracing backend |
| component | log lines | `telemetry.Named` |
| route pattern, hook point, table, error category | log fields, metric attributes and span attributes, under the same keys | the call sites, from constants in `telemetry` |

A client quotes the request ID from the response header. That finds the log lines and audit events of the request, the line gives the trace ID, and the trace shows where the time went.

## Names are a contract

Field keys, audit event types, metric names with their attributes, and span names are constants in `telemetry`. Dashboards, alerts, and queries are built on them, so renaming one is a breaking change for applications even though no Go code breaks. They are listed for users in `docs/api/telemetry.md`.

New call sites take their names from the constants or add one there. An attribute value has to come from a fixed set; see "Attributes are bounded" in [metrics.md](metrics.md).

## Seen working

`examples/init` wires all four: text logs on stdout, the audit table, and OTLP export of traces and metrics when an endpoint is set (`telemetry.go` in the example). Run against PostgreSQL and Jaeger, one sign-up through the route produces this trace. The first span is gin's own tracing middleware; the rest are behemoth's.

```
POST /api/auth/sign-up/email                                  (otelgin)
└─ behemoth.request                    route=/api/auth/sign-up/email status=201
   ├─ behemoth.hook.chain              auth.signUp.before
   │  ├─ behemoth.hook.handler         plugin=activity
   │  └─ behemoth.hook.handler         plugin=app
   ├─ behemoth.store.find_one          users          error_code=users_not_found, not failed
   ├─ behemoth.password.hash
   ├─ behemoth.store.transaction
   │  ├─ behemoth.store.create         users
   │  ├─ behemoth.hook.chain           data.user.afterCreate
   │  │  ├─ behemoth.hook.handler      plugin=activity
   │  │  │  └─ behemoth.store.create   activity_events   the plugin's own table, through hctx.Tx.DB()
   │  │  ├─ behemoth.hook.handler      plugin=app
   │  │  │  └─ behemoth.store.create   todos
   │  │  └─ behemoth.store.create      audit_log      user.created, in the transaction
   │  └─ behemoth.store.create         accounts
   ├─ behemoth.hook.chain              data.user.created
   │  └─ behemoth.hook.handler         plugin=app
   └─ behemoth.hook.chain              auth.signUp.after
      ├─ behemoth.hook.handler         plugin=activity
      ├─ behemoth.hook.handler         plugin=app
      └─ behemoth.store.create         audit_log      auth.signUp.after, best effort
```

The same run leaves four audit events about the new user after a failed and a successful sign-in (`user.created`, `auth.signUp.after`, `auth.signIn.failed`, `auth.signIn.after`), which the example's `audit` command prints.
