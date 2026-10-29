# Supported databases

This page lists the database releases Behemoth supports, the releases its tests run against, and what the adapters need from each database.

## Supported releases

Behemoth supports a release while its maker publishes security fixes for it. When the maker ends support for a release, Behemoth can stop supporting it in the next minor version, and the [changelog](../../CHANGELOG.md) says so.

| Database | Minimum release | Tested on | Notes |
| --- | --- | --- | --- |
| PostgreSQL | 15 | 15, 18 | The migration driver and the adapter also pass the tests on 13 and 14. Releases before 13 are not supported: UUID columns default to `gen_random_uuid()`, which is built in from 13. |
| MySQL | 8.4 | 8.4, 9 | The tests also pass on 8.0, whose maker ended support in April 2026. MariaDB is not supported. |
| SQL Server | 2019 | 2019, 2022 | The adapter uses nothing newer than SQL Server 2016. Releases before 2019 are not tested. |
| MongoDB | 7.0 | 7.0 | The tests also pass on 6.0. A replica set is required. See [MongoDB](./mongodb.md). |
| Redis | 6.2 | 6.2, 7.2, 8 | The adapter uses `GET`, `SET` with an expiry, `DEL`, and `HMGET`, and runs Lua scripts for the rate-limit counters and the session cache. A Redis user that an ACL restricts needs the scripting commands. |
| SQLite | 3.53 | 3.53 | The SQLite library comes with the driver. See [SQLite](#sqlite). |

"Tested on" lists the releases the full suite of storage and store tests ran on without a failure. The releases between a minimum and a tested release are supported but not tested one by one.

## SQLite

SQLite has no server to install. The SQLite adapter uses the `github.com/mattn/go-sqlite3` driver, which compiles the SQLite library into your program. The adapter requires a driver release whose library has the security fixes of SQLite 3.53, so the SQLite version your program runs is the one that driver release carries, or a later one if your `go.mod` requires it.

The driver needs cgo. Build with a C compiler installed and `CGO_ENABLED=1`.

### Foreign keys

SQLite starts every connection with foreign keys off, and Behemoth's tables declare them. Without them, a sign-in that races the deletion of its user can leave a session for a user that no longer exists. `Boot` therefore refuses a SQLite database that does not enforce foreign keys, and the error says how to switch them on. This applies to the SQLite adapter, and to the Bun and GORM adapters on SQLite.

Open the database with foreign keys on, in the DSN, so that every connection of the pool has them:

```go
db, err := sql.Open("sqlite3", "file:app.db?_foreign_keys=on") // github.com/mattn/go-sqlite3
```

With `modernc.org/sqlite`, the DSN parameter is `_pragma=foreign_keys(1)`. A `PRAGMA foreign_keys = ON` that you run yourself covers only the connection it ran on.

To start without them, set `BootConfig.AllowUnenforcedForeignKeys`. `Boot` then logs a warning. Use it for a throwaway database, not for one that holds users.

## Run the tests against another release

The container suites in `tests/` start the minimum release of each database. Set an environment variable to start another release. These are the newest releases:

```bash
BEHEMOTH_TEST_POSTGRES_IMAGE=postgres:18 \
BEHEMOTH_TEST_MYSQL_IMAGE=mysql:9 \
BEHEMOTH_TEST_MONGO_IMAGE=mongo:8 \
BEHEMOTH_TEST_MSSQL_IMAGE=mcr.microsoft.com/mssql/server:2025-latest \
BEHEMOTH_TEST_REDIS_IMAGE=redis:8-alpine \
  go test ./storage/... ./store/...
```

Each variable takes a container image name and tag. A variable you leave out keeps the minimum release. MongoDB 8 does not start on Linux 6.19 and later ([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)).
