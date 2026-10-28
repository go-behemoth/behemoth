# Changelog

All notable changes to Behemoth are listed here. Each release has one section, and the section at the top, "Unreleased", collects the changes that are not released yet.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Under each version, changes are grouped as Added, Changed, Deprecated, Removed, Fixed, and Security. A version heading has the form `## [0.2.0] - 2026-11-01`.

## Versioning policy

Behemoth uses [semantic versioning](https://semver.org/) with the `v` prefix that Go modules require, such as `v0.2.0`.

### Before v1.0.0

- A `v0.x` release can change the API between minor versions. For example, `v0.3.0` can remove or rename something that exists in `v0.2.0`.
- Each such change is listed in this file under "Changed" or "Removed", so that you can read it before you upgrade.
- A patch release, such as `v0.2.1`, fixes bugs and doesn't change the API.
- Pin the version in your `go.mod` and read this file before you raise it.

### From v1.0.0

`v1.0.0` starts the compatibility promise. From then on, a change that breaks the API needs a new major version.

### Modules and lockstep versions

Behemoth is one repository with several Go modules. These modules are published, and each has its own `go get` path:

| Module | Path |
| --- | --- |
| Core | `github.com/go-behemoth/behemoth` |
| Storage adapters | `github.com/go-behemoth/behemoth/storage/adapters/NAME` |
| Router adapters | `github.com/go-behemoth/behemoth/plugins/adapters/NAME` |
| Telemetry adapters | `github.com/go-behemoth/behemoth/telemetry/adapters/NAME` |

NAME is the adapter's directory name, such as `postgres`, `gin`, or `otel`.

Every release gives all published modules the same version. If you use the `postgres` storage adapter at `v0.2.0`, the matching core module is also `v0.2.0`. An adapter that has no change in a release still gets the new version number. Use one version for every Behemoth module in your `go.mod`.

The `tests/` and `examples/` modules in the repository are not published. They have no versions, and you can't install them.

### Tags

Releases are tagged on the `dev` branch until `v1.0.0`, and on `main` after it. Go resolves a tag on any branch, so you don't need to select a branch to install a version.

A tag names a version of one module:

- The core module is tagged `v0.2.0`.
- An adapter is tagged with its directory in front of the version, such as `storage/adapters/postgres/v0.2.0`.

Tags are never moved or deleted after they are pushed.

## Unreleased

### Added

- [Supported databases](docs/api/supported-databases.md) lists the minimum release of each database and the releases the tests run on: PostgreSQL 15, MySQL 8.4, SQL Server 2019, MongoDB 7.0, Redis 6.2 and SQLite 3.53.
- `core.VersionedCache`, a capability of a key-value storage that keeps the latest version of a value whatever order the writes arrive in. The session cache uses it when `BootConfig.KV` has it, and the Redis adapter does. See [The session cache](docs/api/sessions.md#the-session-cache).
- `SessionConfig.UnversionedCacheTTL`, one minute by default: how long a session stays in a session cache whose storage doesn't implement `core.VersionedCache`.

### Security

- Every module requires dependency releases that have no known vulnerabilities. The Gin, chi, GORM and Echo adapters needed newer releases of `golang.org/x/net`, `github.com/quic-go/quic-go`, `github.com/go-chi/chi/v5`, `golang.org/x/text` and `github.com/labstack/echo/v5`.
- The SQLite adapter requires `github.com/mattn/go-sqlite3` v1.14.52, which compiles in SQLite 3.53.4. The earlier requirement, v1.14.22, compiled in SQLite 3.45.1, which lacks the fixes for several memory-corruption vulnerabilities reported in 2025 and 2026.

### Changed

- `Boot` fails on a SQLite database that does not enforce foreign keys, which includes the Bun and GORM adapters on SQLite. Open the database with `_foreign_keys=on` in the DSN of `github.com/mattn/go-sqlite3`, or set `BootConfig.AllowUnenforcedForeignKeys` to start anyway, with a warning. Without foreign keys, a sign-in that races a user's deletion can leave a session for a user that no longer exists.
- The Bun adapter requires Bun v1.3.0. Use v1.3.0 of Bun's dialect modules (`pgdialect`, `mysqldialect`, `mssqldialect` and `sqlitedialect`) as well, because Bun stops at startup when a dialect module has another version.
- The Echo adapter checks for a route that the Echo instance already has with the same method and path, and returns an error for it. Echo v5.4 lets `echo.New` overwrite a duplicate route without an error, so the adapter no longer relies on Echo for this.
- The module path changes from `github.com/MastewalB/behemoth` to `github.com/go-behemoth/behemoth`, and so does the path of every adapter module. Replace the old path with the new one in your imports and in your `go.mod`.
- The OpenTelemetry instrumentation scope of the `otel` adapter is now `github.com/go-behemoth/behemoth`. Update a dashboard or a filter that selects spans or metrics by the old scope name.
- The `go` line of every module is `1.26.0`, the oldest Go release Behemoth supports. Earlier versions named a patch release, which made `go` fetch that exact toolchain. Behemoth supports the two newest Go releases, and CI tests with the newest patch of each. Build your application with the newest patch release of your Go version, because the standard library receives security fixes in patch releases.
- Each adapter module requires the matching release of the core module, so `go get` installs an adapter on its own. Earlier adapter modules required a version that doesn't exist.
- The `sessions` table has a new column, `version`, which every update of a session raises by one. Generate and apply a migration before you deploy this version. Existing sessions get version 0. On MongoDB, sessions stored without the field read as version 0 and need nothing.
- With a key-value storage that doesn't implement `core.VersionedCache`, a session stays in the session cache for `SessionConfig.UnversionedCacheTTL` at most, instead of until it expires. A session in use is then read from the database about once a minute.

### Removed

- `Store.UpdateSession` and `Store.UpdateSessionIn`. Use `Store.UpdateSessionAt`, which writes a change only if the session is still at the version you read, and raises the version.

### Fixed

- A session revoked while another request was writing it to the session cache could stay valid from the cache until it expired. This happened when the revoke landed between a lookup's read of the session and its cache write, or between an extension's write and its cache write. With the Redis adapter, a revoked session now stays revoked in the cache.

## Earlier versions

`v0.1.0` to `v0.1.3` were published under `github.com/MastewalB/behemoth`. That path is deprecated, and `v0.1.3` carries the deprecation notice. They have no changelog entries.
