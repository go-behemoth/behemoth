# Telemetry plan: logging, audit, metrics, and tracing

This document is the record of how behemoth's telemetry was planned and built. All six phases are done. It keeps what the per-topic documents do not: the overall shape, where the result differs from the first plan, the decisions made before the work started, and what was left out on purpose.

To learn how the code works, start at [`telemetry.md`](telemetry.md). This document is for the question "why is it built this way".

## Phases

| Phase | Content | Described in |
| --- | --- | --- |
| 1. Foundations | the `telemetry` package, field keys, `ErrorFields`, redaction, the request ID, the slog logger, `telemetrytest` | `foundations.md` |
| 2. Logging | named loggers per component, request failures logged by the router, statement logging in the SQL adapters, the boot summary | `logging.md` |
| 3. Audit | the event model, the `audit_log` core table, the database recorder with querying and purging, in-transaction recording for user writes, actor and subject, audited core points | `audit.md` |
| 4. Metrics | the counter and histogram interface, the metric catalog, measurements in the router, the store, the dispatcher, the session manager and the rate limiter | `metrics.md` |
| 5. Tracing | the `Tracer` interface, spans around requests, hooks, store operations, sessions, tokens, password hashing, rate-limit checks and migrations, the OpenTelemetry adapter module | `tracing.md` |
| 6. Docs and example | `docs/api/telemetry.md`, the index `telemetry.md`, and `examples/init` wired for stdout logs, the audit table and OTLP | `telemetry.md`, the example's `telemetry.go` |

## Shape: taxonomy in core, backends in adapter modules

Behemoth's part is a thin layer: interfaces, names, and conventions. A real backend does the work. This is the same split as the error taxonomy (`errors/` defines categories and codes, `ErrorMapper` adapts them to HTTP) and the storage layer (`storage.go` defines `Database`, each adapter lives in its own module).

| Piece | Location | Depends on |
| --- | --- | --- |
| Interfaces, no-ops, field keys, audit event types, metric names, span names, request-ID helpers, redaction | `telemetry/` in the root module | standard library |
| Logger backed by `log/slog` | `telemetry/` in the root module | standard library |
| In-memory recorder for tests | `telemetry/telemetrytest/` | standard library |
| Database audit recorder | `store/` | the store, like other core tables |
| OpenTelemetry tracer and metrics, trace IDs on log lines | `telemetry/adapters/otel/`, with its own `go.mod` | the OpenTelemetry API |

Notes on backends:

- **Stdout** is the slog logger with a text or JSON handler.
- **Zap and zerolog** plug in through their slog handlers. They need no adapter of their own.
- **Jaeger** has no adapter of its own. Jaeger's client libraries are deprecated and Jaeger ingests OTLP, so it is the OpenTelemetry adapter with an OTLP exporter pointed at a Jaeger endpoint.
- **Exporter lifecycle** belongs to the application. It creates the providers and exporters, passes them to the adapter, and flushes and shuts them down. This matches the HTTP ownership boundary described in `docs/internal/http/request_routing.md`.

## Where the result differs from the first plan

### Logging

Logging differs from the plan in these ways:

- The store logs nothing, and the token manager logs one line. On inspection neither swallows errors: they return them, and the router logs what reaches it.
- `PluginContext` was deleted. A plugin names its own logger from `AuthContext.Telemetry`.

- The boot summary reports whether a key-value store is configured, not the names of the session and rate-limit backends, and has one configuration warning.

### Audit

Audit differs from the plan in these ways:

- `ActorID` and `SubjectID` are strings, not `any`. Every id behemoth stores is a string, and a typed column can be indexed and filtered.
- Audit ids are UUIDv7, and pages are ordered by id. The plan did not say how paging would stay stable.
- In-transaction recording goes through an optional interface, `TxAuditRecorder`, so an application's own recorder is still honored for the data points: it is called after the commit.
- The data events are named `user.created` and `user.updated`, not after their hook points.
- There is no "password changed" event, because no flow changes a password yet.
- A failed write is logged at Error and, since phase 4, counted in `behemoth.audit.record_failures`.

Still deferred: an HTTP route for querying (needs an authorization model), tamper evidence, and a fail-closed mode for best-effort events.

### Metrics

Metrics differ from the plan in these ways:

- The sign-in and sign-up counters have no `plugin` attribute. They are counted in the dispatcher, which does not know which plugin fired a core point.
- The hook metrics have a `phase` attribute, so a before handler's veto can be told apart from a failing after handler.
- `behemoth.session.validated` and the two failure-carrying flow counters have a `reason` attribute with the failure's code.
- The store metrics name the operation after the `Database` method (`find_one`), because they are taken by wrapping the store's adapter.

### Tracing

Tracing differs from the plan in these ways:

- The tracer is set with the option `telemetry.WithTracer`, not as a fourth argument of `New`.
- The password spans were first started in the email/password plugin, not in `crypto`: `PasswordHasher` took no context, so the hasher could not start a span with a parent. Since 2026-10-24 its methods take a context and the hasher starts the spans, as planned.
- A hook point without handlers gets no chain span.
- A rejection (not found, an expired session, a hook's veto) does not fail its span. It sets `error_code` and `error_category` only.
- The adapter has no log exporter. `WithTraceIDs` adds the trace and span IDs to the lines of any logger.
- The adapter's package is named `behemothotel`, since `otel` is the name of the package it imports.

### Related areas

The related areas stand as follows:

- **Health.** `AuthContext.Health(ctx)` was listed and is not built. See "Left out" below.
- **Request correlation, test helpers and the catalogs as a contract** are built as planned.

## Left out

Each of these was considered and deferred. None is started.

| Item | Why it waits |
| --- | --- |
| `AuthContext.Health(ctx)`, a check of the database, the key-value store and the key manager | it is not telemetry, and no phase needed it. It belongs with a readiness story for the whole library. |
| An HTTP route to query audit events | needs an authorization model behemoth does not have |
| Tamper evidence for the audit log (a hash chain over rows) | no requirement yet |
| A fail-closed mode for best-effort audit events | no requirement yet |
| A "password changed" audit event | no flow changes a password yet |
| Metrics for hook points declared by plugins | the dispatcher counts core points from a table; plugins count their own |
| A log exporter in the OpenTelemetry adapter | `otelslog` covers it; see the decision in `tracing.md` |
| A level check on `Logger` | recorded in `docs/ongoing.md` |

Open questions that came up during the work are in `docs/ongoing.md`, each marked with the phase that raised it.

## Design decisions

### Telemetry is a thin taxonomy over external backends

**Context:** Behemoth needs logs, audit events, metrics, and traces, and applications already run a logging and observability stack of their own.

**Options considered:**

- *Depend on the OpenTelemetry API in the root module.* One standard interface for all three signals. Every user of behemoth pulls in the OpenTelemetry dependency tree, including those who only want stdout logs.
- *Take a `*slog.Logger` directly and define nothing for metrics and traces.* No interface to maintain for logging. Leaves metrics and tracing without a seam, and call sites pick field names freely.
- *Define small interfaces and a naming taxonomy in core, with backends as adapters.* More code to maintain. The root module stays on the standard library, and the names (field keys, event types, metric names) are owned by behemoth and stay the same across backends.

**Decision:** The third option, the same split as the error taxonomy and the storage adapters. Core ships the slog-backed logger; OpenTelemetry lives in `telemetry/adapters/otel` with its own `go.mod`. Jaeger is reached through the OpenTelemetry adapter over OTLP and has no adapter of its own.

**Revisit if:** the interfaces start to mirror the OpenTelemetry API method for method, at which point depending on it directly is simpler.

### Audit is recorded by default

**Context:** `BootConfig.Telemetry == nil` currently means a no-op audit recorder, so an application that configures nothing has no audit trail, and `audit_log` is not declared by any schema.

**Options considered:**

- *Opt-in.* Keeps today's default and adds no writes. An application finds out it has no audit trail when it first needs one.
- *On by default, with an opt-out.* `audit_log` is a core table and the database recorder is the default. Each audited action costs one extra insert, and the table grows until the application purges it.

**Decision:** On by default. `audit_log` is declared by core and created by the migration engine like the other core tables. When `Telemetry.Audit` is nil, `Boot` uses the database recorder. An application opts out, or sends events elsewhere, by setting `Telemetry.Audit` to another recorder (a no-op recorder is exported for this).

**Revisit if:** the extra insert shows up as a measurable cost on the sign-in path.

### Audit rows for data writes commit with the write

**Context:** An audit event recorded after commit is lost if the process stops between the commit and the callback. An event recorded before commit through a separate connection can describe a row that was rolled back. This was raised in `docs/ongoing.md` as "Should data points be audited, and how reliably".

**Options considered:**

- *Best effort everywhere.* One code path. A crash at the wrong moment loses the record of a user being created or changed.
- *In the transaction for data writes, best effort elsewhere.* The dispatcher records through the transaction-bound store (`hctx.Tx`) when there is one. Needs the recorder to accept a transaction-bound store, and `RunAfterTx` to record where today it deliberately does not.
- *An outbox table drained by a worker.* Reliable for every event. Needs a background worker, which behemoth does not run.

**Decision:** The second option. Events for data points are written inside the write's transaction and commit or roll back with the row. Events with no transaction (a failed sign-in, a rate-limit rejection, an applied migration) are best effort: a failed write is logged at Error and counted, and the request proceeds.

**Revisit if:** a compliance requirement needs every event to be durable, which would call for the outbox or a fail-closed mode.

### Email addresses in logs

**Context:** Logs are often shipped to third-party services and kept under looser access control than the database. An email address identifies a person; a user ID does not without database access.

**Options considered:**

- *Log emails.* Easiest to debug from logs alone. Spreads personal data into every log sink.
- *Log user IDs only.* Logs hold no personal data by default. Debugging a sign-in problem for a known email needs a lookup of the ID first, or the audit log.

**Decision:** Logs carry user IDs only by default. Audit rows keep the email, because the audit log is in the application's database and is the record of who did what. A logging option lets an application allow emails in logs.

**Revisit if:** other personal fields (IP address, user agent) need the same treatment in logs.
