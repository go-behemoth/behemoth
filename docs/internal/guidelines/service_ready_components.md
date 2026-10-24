# Components that run without application code

This document explains what a component needs so that behemoth can also run as a standalone service, and what follows for anyone who adds an option, a constructor, an extension point, background work or a schema change. Behemoth is a library today: the application's code sets it up, schedules its upkeep and shuts it down. A service configured by a YAML or TOML file and run in Docker or Kubernetes is planned, for stacks that can't import a Go library.

The guidelines are suggestions for new code and for changes. They do not describe a finished state: *Where the code differs today* lists the places that do not follow them yet. Nearly all of them also make a library application easier to run, so none waits for the service.

The code it refers to lives in:

- `engine/init.go`: `Prepare`, `Boot`, `BootConfig`, `PrepareConfig`
- `plugins/*`: each plugin's `Options`, `New`, and `Init`
- `core/mail.go`: `MailSender`, `MailMessage`
- `crypto/`: `SecretSource`, `WatchableSecretSource`, `PollingWatcher`
- `messaging/mailer.go`: `DefaultMailer`, the one component with background workers
- `store/rate_limits.go`, `store/audit.go`: the purges

The other guideline documents are [`http_contract.md`](http_contract.md), about what a client of the routes depends on, and [`transport_neutral_flows.md`](transport_neutral_flows.md), about flows and their wrappers.

## What a service takes away

In library mode the application is in the same process, and it does several jobs in code. A service has a file and a network, and no application code:

| The application's code does this today | A service has only |
| --- | --- |
| builds plugins and options as Go values | values a file can carry |
| registers hook handlers, writes and sends mail | calls to another process |
| holds the secrets and hands them over | files and variables mounted by the platform |
| schedules the purges and closes the mail queue | its own start and stop, which the platform triggers |
| is one process | several replicas, replaced one at a time during an upgrade |

Two questions cover most small decisions. Could this be set up from a file? Would it still be right with three replicas, one of them killed at any moment?

## Guidelines

### An option is data, and code is the exception

A file carries text, numbers, booleans, durations, lists, and maps. An option of one of those types works in both modes. `magiclink.Options` is all data:

```go
type Options struct {
	WaitForSend          bool
	LinkURL              string
	TTL                  time.Duration
	RequestLimit         core.Limit
	LeaveEmailUnverified bool
}
```

`emailpassword.Options.ValidatePassword` is a function, for a check such as a breached-password lookup. A file can't carry it. When an option has to be a function or an interface, the common cases get a data form next to it (the length limits exist as `MinPasswordLength` and `MaxPasswordLength`), and the function stays for what data can't express.

### The zero value is the default, and a wrong value fails at start

A file leaves most settings out, so the zero value of every option means its documented default. `New` or `WithDefaults` fills it in, as `SessionConfig.WithDefaults` does.

A value that can't work is reported by `Init` or `Boot` as a configuration error, not by the first request that reaches it. `NewTrustedOrigins` refuses a malformed origin "so a typo is found at Boot and not by a refused redirect". For a service this is the difference between a deployment that fails where the platform shows it and one that looks healthy and refuses its users.

### A secret is not an option

A password, an API key or a signing key is mounted by the platform separately from the configuration, and it may change while the process runs. `crypto.Config.Secrets` is a `SecretSource` for this reason, and a `WatchableSecretSource` reports a rotation.

A component that needs a secret takes it as a value of its own or through a source. A `Password string` field among the options ends up written in a file and printed with it.

### An extension point passes data

`MailSender.Send` gets a `MailMessage`: a kind, an address, a URL, an expiry, the user and two maps. Nothing in it is a live object, so the sender can be a function in the application or a process somewhere else.

An interface the application implements is easiest to carry over a network when it takes data and returns data or an error. Handing over the store, a transaction or the request ties the implementation to this process. Some points need that and are in-process by nature: a data hook gets `HookContext.Tx` so that its writes commit with the row.

### A hook payload can leave the process

Hook handlers are planned to be reachable as HTTP callouts and events. A point's payload and result are then sent to another system.

A payload and a result follow these rules:

- A payload is a `behemoth.M` of plain values, and a result is a model or a small struct. Avoid a payload that holds a function, a channel or a handle.
- A credential sits under a key the telemetry redactor knows: `password`, `token`, `secret`, `hash`, `cookie`, `authorization`, or a key that ends in one of them. The sign-in payload holds the password under `password`. A key such as `pw` would pass a forwarder unredacted.
- A before handler answers with data: a changed payload, or a typed error with a code. A point whose handler is expected to change a shared object in place can't be answered from another process.
- A new point's payload keys are listed in `docs/api/hooks.md`.

### State lives on a value, not in the package

Core has no `init()` function and no registry in a package variable. Plugins are passed to `Prepare`, and a router adapter is passed in `BootConfig.HTTP`; `docs/internal/http/request_routing.md` has the reasoning for adapters. The list of plugin factories a service needs will be a value its `main` builds.

A component keeps its state on the value its constructor returns. Two of them can then exist in one process: in parallel tests today, and for two tenants if that is ever wanted.

### Background work is a method first

`Store.Purge(ctx, opts)` runs each of the store's purges once and returns. It can be run by a ticker, by an application's scheduler or by a Kubernetes CronJob, and from several replicas at once, since a second pass finds nothing to delete.

A component that needs periodic work offers it this way, and leaves the schedule to whoever owns the process. A constructor that starts its own ticker runs once per replica, can't be turned off, and keeps running in tests.

### What is started can be stopped

A goroutine a component does start ends with a context or has a `Close`. The goroutine `PollingWatcher.Watch` starts ends when its context is done. `DefaultMailer` starts its workers with the first background send, and `Close` waits for the queued messages.

A platform stops a process with `SIGTERM` and kills it some seconds later. Work that has no stop is cut off there.

### Nothing that matters lives only in memory

With several replicas, a request reaches any of them, and a replica can disappear between two requests. Rate-limit counters are in Redis or in the `rate_limits` table for this reason, and sessions and tokens are rows.

Memory is fine for what may be lost or may differ between replicas, such as a cache with a short life. It does not suit a count that a limit depends on, or work that was accepted and not yet done.

A bound on the process's own resources is the exception, and belongs in memory. The password hasher's bound on hashes in flight (`crypto.PasswordLimit`) protects the memory of one replica, which no other replica shares. A count in Redis could not tell which replica holds the memory, and would add a round trip to every sign-in. See [ADR-0010](../decisions/0010-bounding-concurrent-password-hashes.md).

### Core reads no environment, file, or flag

Settings arrive as values. `Boot` and the plugins read no environment variable. The composition root does: the application's `main` today, the service's `main` later. The migration code reads the migration folder, whose path it was given.

A component that reads `os.Getenv` itself has a setting that the file does not show and a test can't set per case.

### Report through the logger and the telemetry interfaces

Core writes nothing to standard output and calls no `log.Printf`. A plugin takes its logger from the `AuthContext` (`telemetry.Named(ac.Telemetry.Logger, PluginName)`). A service's logs are structured lines that the platform collects, and one stray print breaks the format.

### A schema change keeps the previous version working

During a rolling upgrade the old and the new version run against one database for a while. A column a plugin adds is nullable or has a default, so the old version's inserts still succeed. A column or a table that a version stops using is removed in a later version.

### What the application does in code needs a flow

An application manages users and sessions by calling `Store.DeleteUser` or `SessionManager.RevokeAllForUser`. A service's callers are other processes, so the same operations will be exposed to them. An operation meant for the application is an exported method with a context and typed input, as [`transport_neutral_flows.md`](transport_neutral_flows.md) describes for flows, so that a wrapper for other services is a few lines.

### A new dependency gets a module of its own

The framework and storage adapters are modules of their own, and the root `go.mod` stays small. A package that needs a YAML decoder, an SMTP client or a cloud SDK is packaged the same way. The service is such a module: it wires what the library offers.

## Where the code differs today

Not models to copy. Each has an entry in `docs/ongoing.md` under the quoted title, unless noted:

- `emailpassword.Options.ValidatePassword` and `ValidateEmail` are functions. `BootConfig.Hooks` and `PrepareConfig.Schema` are functions too. The application's `MailSender` is the only way to send, and it writes the message. `StaticSecretSource` is the only secret source. `outbound.Options.Transport` is an interface with no plain-value form for a proxy or a CA bundle, and the default transport takes the proxy from the environment. "An application configures behemoth with Go values only".
- The mail queue is in memory, and `Boot` returns no handle that stops everything it started. "The background mail queue is in memory and does not retry".
- No call reports whether the database and the key-value store answer. No entry.

## Design decisions

### The service is a second composition root, not a second code base

**Context:** A standalone service needs things a library application writes itself: reading a file, choosing a database by name, sending mail, calling out for hooks, scheduling upkeep.

**Options considered:**

- *A separate server with its own logic.* It could be shaped for a service from the start. Two code bases would then implement sign-in, and a fix in one would have to be repeated in the other.
- *Service-only code inside the library, switched on by a mode.* One code base. Every component would carry a second path, and the library would pull in the service's dependencies.
- *A `main` in a module of its own that builds `Prepare` and `Boot` from a file.* What the service needs (a file secret source, a posting mail sender, callouts for hooks) is added to the library as ordinary packages, which a library application can use too. The service module only wires them.

**Decision:** the third. The guidelines above are what keep it possible: a component that takes data, holds its state on a value and can be stopped needs no second path.

**Revisit if:** the service needs behavior a library application should not have, such as tenants sharing one process.
