# Performance testing

This document describes how Behemoth's performance is measured. It records the first measurements and what they showed. It then proposes three layers of tests that keep measuring: micro-benchmarks, flow benchmarks with budgets, and load tests. It also covers the checks on what Behemoth adds to an application's binary and dependency graph, and the order of the work.

**Status:** proposed, as of 2026-10-23. Nothing in it is built. The first measurements come from programs outside the repository, and [step 2](#implementation-steps) brings their harness in.

The documents it builds on are:

- [`../guidelines/service_ready_components.md`](../guidelines/service_ready_components.md): a component stays right with several replicas. The load tests check that under load.
- [`../guidelines/transport_neutral_flows.md`](../guidelines/transport_neutral_flows.md): rules and protections live in the flow. The flow benchmarks call flows in the process as well as through routes. The bound on password hashes (`crypto.PasswordLimit`, [ADR-0010](../decisions/0010-bounding-concurrent-password-hashes.md)) is in the hasher, below every flow, so it holds for both.
- [`../ratelimit/rate_limiter.md`](../ratelimit/rate_limiter.md): the counters that every limited request reads and writes.
- [`../telemetry/metrics.md`](../telemetry/metrics.md): the metrics that the budgets and the load tests read.
- [`package_layout.md`](package_layout.md) and [ADR-0007](../decisions/0007-layering-the-packages.md): the layering that `layout_test.go` checks. The dependency allowlist goes next to it.

## Terms

| Term | Meaning |
| --- | --- |
| Micro-benchmark | A `testing.B` benchmark of one function or type, in the package that defines it. It needs no database. |
| Flow benchmark | A benchmark of a whole flow on a booted application with a real database. It calls the flow in the process, or a route through a router adapter with `httptest`. |
| Budget | A count that a test checks against a fixed limit: database calls per flow, allocations per operation, binary size, or third-party imports. A budget gives the same answer on every run, so it can fail CI. |
| Load test | Requests over the network to a running server, sent by a load generator in another process. |
| In flight | Started and not yet finished. A password hash in flight holds its memory. |
| P | A processor of the Go scheduler. `GOMAXPROCS`, or the `-cpu` flag of `go test`, sets how many there are. `b.RunParallel` starts one goroutine per P. |
| Peak RSS | The most physical memory the process held, as `/usr/bin/time` reports it (maximum resident set size). |
| Open model | Load generation that starts requests at a set rate, whether the earlier ones have finished or not. A closed model waits for each response before it sends the next request. |

## Context

Before this plan, Behemoth had three benchmarks and no other performance test:

| Benchmark | Where | What it measures |
| --- | --- | --- |
| `BenchmarkDefaultKeyManager_Current` | `crypto/key_manager_test.go` | It builds a new key manager on every iteration, so it times construction and not the lookup (L55). |
| `BenchmarkDefaultKeyManager_GetByVersion` | `crypto/key_manager_test.go` | The same as `BenchmarkDefaultKeyManager_Current`. |
| `BenchmarkBuildSQLiteWhereClause` | `tests/storage/sqladapter_test.go` | It builds a `WHERE` clause from a nested expression. It is named for SQLite but builds with the PostgreSQL clause options. |

CI (`.github/workflows/go.yml`) runs `go test` without `-bench`, so no benchmark runs there. No test counts database calls or allocations, and nothing records the size Behemoth adds to a program.

Behemoth is a library, so its performance is what an application gets through it:

- the time, memory, allocations, and database calls of each flow
- how these change with concurrency, with the database, and with a key-value store
- what Behemoth adds to the application's binary and dependency graph

## Goals

- Know the cost of each flow in time, memory, allocations, and database calls, on each supported database, with and without a key-value store.
- Catch a regression in a pull request with checks that give the same answer on every run.
- Know the capacity of one process: password hashes per second and the memory each needs, and session checks per second.
- Check under load that the flows stay correct with several replicas.
- Keep what Behemoth adds to a binary and to the dependency graph small, and see it when it grows.

## Non-goals

- Comparing Behemoth with other authentication libraries.
- Tuning the databases or their drivers. The tests use their defaults, except the connection pool (see [Size the connection pool](#size-the-connection-pool)).
- Publishing performance numbers in the API docs. The numbers here are for maintainers, and they depend on the machine.
- Fixing what the measurements found. L55 and L56 in `docs/ongoing.md` track it, and the tests here measure the fixes. M28, the unbounded hashes, was fixed on 2026-10-24 ([ADR-0010](../decisions/0010-bounding-concurrent-password-hashes.md)).

## First measurements

### Setup

The measurements ran in a throwaway module outside the repository. Its `go.mod` pointed at the checkout with `replace` directives, so it built against the working tree. There were two runs:

| | Run 1 | Run 2 |
| --- | --- | --- |
| Date | 2026-10-22 | 2026-10-23 |
| Commit | `4582efe` | `4582efe`, with `utils/password.go` removed |
| Machine | Intel Core i7-14650HX, 16 cores, 24 threads, Linux 7.0 | the same |
| Power | not recorded | on battery, `powersave` governor |
| Go | 1.26.4, linux/amd64 | the same |

The flow benchmarks booted Behemoth with this configuration:

- The email and password plugin alone, through `engine.Prepare` and `engine.Boot`.
- SQLite, in a file in a temporary directory, in WAL mode (`_journal_mode=WAL`). The tables were created the way `tests/testutils.MigrateApp` creates them.
- The `net/http` adapter on an `http.ServeMux`, with the session token in a header (`httpapi.DeliveryHeader`).
- One user, signed up and signed in by calling the plugin in the process. That sign-in's token is the one the session check uses.
- No key-value store, so sessions and rate-limit counters are in the database.
- No telemetry, except where database calls were counted.
- Core's per-address limits on sign-in and sign-up turned off (`RateLimitConfig.Disabled`) in the timed benchmarks, because one address signs in thousands of times. They stayed on where database calls were counted.

Every benchmark used `b.Loop` and reported allocations. The time per operation of a `b.RunParallel` benchmark is the wall time divided by the operations of all goroutines. A lower number means more throughput in total, not a faster single call.

### Time and memory per operation

Run 1, on one goroutine unless the row says otherwise:

| Operation | Time | Memory | Allocations |
| --- | --- | --- | --- |
| `Argon2idHasher.Hash` with `DefaultParams` | 33.0 ms | 64.0 MiB | 101 |
| Sign-in over HTTP, right password | 30.7 ms | 64.1 MiB | 657 |
| Sign-in over HTTP, wrong password | 30.3 ms | 64.0 MiB | 452 |
| Sign-in over HTTP, unknown email | 29.1 ms | 64.0 MiB | 313 |
| Session check (`Authenticator.Authenticate`) | 19.1 µs | 8.9 KiB | 137 |
| Session token check (`SecretHasher.Verify`) | 1.08 µs | 656 B | 9 |
| `DefaultKeyManager.Current`, from the cache | 37.9 ns | 0 | 0 |
| `DefaultKeyManager.Current`, 24 goroutines | 74.5 ns | 0 | 0 |
| `BenchmarkDefaultKeyManager_Current` as written | 9.6 µs | 6.7 KiB | 84 |

The table shows these points:

- A password flow costs one Argon2id hash, and the hash is almost all of its time and memory.
- A session check takes about 19 µs on SQLite in the same process. [Where a session check spends its time](#where-a-session-check-spends-its-time) shows that Behemoth's own code is a small part of it. Over a network, the database round trip dominates.
- A key manager lookup takes 38 ns. The existing benchmark reports 9.6 µs for it, 250 times more, because it builds the manager on every iteration (L55).
- With 24 goroutines, a lookup takes 74 ns instead of 38 ns, so the total rate halves as goroutines are added. Each lookup takes the key manager's read lock twice (L56). It is under 1% of a session check.

### Password hashes in flight

A parallel benchmark hashed on every P at once, so the number of hashes in flight equalled P. Each run went under `/usr/bin/time` for its peak RSS. Run 1:

| Hashes in flight (P) | Time per hash, all goroutines | Hashes per second | Live hash memory | Peak RSS |
| --- | --- | --- | --- | --- |
| 1 | 80.2 ms | 12.5 | 64 MiB | 140 MiB |
| 4 | 24.2 ms | 41 | 256 MiB | 558 MiB |
| 24 | 14.8 ms | 68 | 1.5 GiB | 3.27 GiB |

The table shows these points:

- With one P, a hash takes 80 ms. `DefaultParams` asks for four lanes (`Parallelism: 4`), and one P runs them one after another. The 33 ms of the previous table spread the lanes over 24 Ps.
- Throughput grows much less than the number of Ps: 24 hashes in flight give 68 hashes a second, against 41 with 4. The machine's 24 threads include hyper-threads and efficient cores, and Argon2id is designed to be bound by memory bandwidth. Which of these limits it here was not measured.
- Peak RSS is about 2.2 times the live hash memory at every level. That matches Go's default `GOGC=100`, which lets the heap grow to twice the live data before a collection. Each hash's 64 MiB becomes garbage when the hash returns, and stays in the heap until the next collection.
- Nothing bounded the hashes in flight when this was measured. Core's limits count per client address, so requests from many addresses started as many hashes as there were requests. 200 sign-ins in flight would hold 12.5 GiB of live hash memory, and about twice that in RSS at the default `GOGC`. Since 2026-10-24, `crypto.PasswordLimit` bounds them per process, at `GOMAXPROCS` hashes by default ([ADR-0010](../decisions/0010-bounding-concurrent-password-hashes.md)).
- `GOMEMLIMIT` makes the collector run sooner as the heap nears the limit. It trades CPU for memory, and it does not bound the live memory.

### Sign-in timing parity

Sign-in answers a wrong password and an unknown email the same way. It hashes the password in both cases, so that the time does not tell them apart (the timing mitigation in `signInBody`). Run 1, ten runs of 20 sign-ins each:

| Case | Mean | Standard deviation |
| --- | --- | --- |
| Right password | 30.32 ms | 0.81 ms |
| Unknown email | 30.74 ms | 1.33 ms |
| Wrong password | 31.26 ms | 1.14 ms |

The differences are within one standard deviation. In the process, on SQLite, the three cases can't be told apart at this resolution. A single run had shown a gap of 1.2 ms between a wrong password and an unknown email, and ten runs did not confirm it.

The cases still differ in database calls, as the next section shows. A wrong password makes one call more than an unknown email: the account lookup. In the process that call takes microseconds. Over a network it takes a round trip. An attacker averages many requests, so a gap smaller than the noise of one request can still show. The comparison is repeated against PostgreSQL in [layer 2](#timing-parity) and in the load tests.

### Database calls per flow

The store reports every call to the `behemoth.Database` interface as an observation of `behemoth.store.duration` (`telemetry.MetricStoreDuration`), with the operation and the table. A `telemetrytest` recorder collected them, with core's limits on. Both runs gave the same counts:

| Flow | Calls | What they are |
| --- | --- | --- |
| Sign-up over HTTP | 6 | The rate-limit counter's read and write, the user lookup, and a transaction that creates the user and the account. The transaction counts as one call, and each create in it as one more. |
| Sign-in, right password | 5 | The counter's read and write, the user lookup, the account lookup, and the session's create. |
| Sign-in, wrong password | 4 | The counter's read and write, the user lookup, and the account lookup. |
| Sign-in, unknown email | 3 | The counter's read and write, and the user lookup. |
| Session check | 1 | The session lookup by its lookup hash. |

The counter's write is a create when the key has no row yet, and an update otherwise.

The counts show these points:

- Without a key-value store, every request that a rule limits costs the counter a read and a write. `Store.IncrementRateLimit` writes only if the count is still the one it read, and reads again if another call wrote first, up to 32 times. Under many concurrent requests on one key, each lost race is another read and write. After 32 lost races the call fails, and the limiter's failure mode decides, which is fail open by default. [Load scenario 4](#scenarios) measures it.
- The session check makes no write, because rolling expiration is off while `SessionConfig.UpdateAge` is zero (L23). With it on, a check that extends the session adds a read by ID and an update.
- With a key-value store, a session found in the cache costs no database call: a `GET` and a JSON decode replace the lookup. The counters move to the store's atomic increment (the Redis adapter). This comes from reading the code. It was not measured.
- The counts are of calls to the `Database` interface, so they are the same on every adapter. A call that an adapter turns into several statements still counts once.

### Where a session check spends its time

Run 2 profiled the session check, on one goroutine and on 24.

#### Size the connection pool

With the default pool, the parallel session check got slower past 4 goroutines: 10.0 µs per check at 4, and 27.9 µs at 24. At 24 goroutines, 62% of the CPU samples were in `database/sql` opening connections (`DB.conn`, then the go-sqlite3 driver's `Open`). `database/sql` keeps two idle connections by default. With more goroutines than that, most queries closed a connection and opened a new one. The cause was the harness, not Behemoth.

Every flow benchmark and load test sets the pool, and an application should too: the maximum of idle connections (`SetMaxIdleConns`) has to cover the queries that run at once.

#### Scaling

With `db.SetMaxIdleConns(64)`, run 2:

| Goroutines (P) | Time per check, all goroutines | Checks per second |
| --- | --- | --- |
| 1 | 19.9 µs | 50,000 |
| 2 | 10.3 µs | 97,000 |
| 4 | 6.8 µs | 148,000 |
| 8 | 5.3 µs | 187,000 |
| 24 | 34.3 µs | 29,000 |

Throughput grows up to 8 goroutines and falls sharply at 24. The cause is not settled. The CPU profile at 24 goroutines is mostly cgo calls into SQLite and runtime spinning, and Behemoth's own functions are 1.6% of it. The flow benchmark on PostgreSQL shows whether the drop belongs to SQLite (see [Open questions](#open-questions)).

#### CPU

The CPU profile at 24 goroutines, as shares of all samples:

| Where | Share | Counted |
| --- | --- | --- |
| The SQLite adapter's `FindOne` and everything below it | 75% | cumulative |
| cgo calls (`runtime.cgocall`) | 34% | cumulative |
| `database/sql`'s `Row.Scan`, which includes parsing the times SQLite stores as text | 32% | cumulative |
| Allocation (`runtime.mallocgc`) | 19.5% | cumulative |
| The background garbage collector (`runtime.gcBgMarkWorker`) | 8.4% | cumulative |
| Behemoth's own functions | 1.6% | flat |

A cumulative share includes the functions a function calls, so the cumulative rows overlap. A flat share counts only the time spent in the function itself.

#### Allocations

A memory profile of 20,000 checks on one goroutine recorded every allocation (`-memprofilerate 1`). It places about 130 of the 137 allocations of a check:

| Source | Allocations per check |
| --- | --- |
| The go-sqlite3 driver: row values, column types, and strings copied from C | 74 |
| Behemoth's SQL adapter code: the `WHERE` clause, the query, and the scan targets | 17 |
| The HMAC, SHA-256, and hex encoding of the lookup hash and the check | 10 |
| Turning the row into a model (`models.GenerateModelFromRows`) | 9 |
| Other Behemoth code | 7 |
| `database/sql` | 7 |
| `fmt` | 3 |

More than half of the allocations belong to the driver, and they change with the driver. An allocation budget on this path measures the driver too (see [Allocations per operation](#allocations-per-operation)).

#### Noise between runs

The same benchmarks gave different numbers on the two days:

| Benchmark | Run 1 | Run 2 |
| --- | --- | --- |
| Session check, one goroutine | 19.1 µs | 32.7 µs to 33.5 µs, three runs |
| `SecretHasher.Verify` | 1.08 µs | 0.94 µs |
| `DefaultKeyManager.Current`, 24 goroutines | 74.5 ns | 101 ns |

In run 2, the session check took 36.8 µs as a loop on one P and 19.9 µs as a parallel benchmark on one P. That is the same work in the same process. On battery with the `powersave` governor, the clock speed changes during a run. Timings compare only within one run, on one machine, in one power state. This is why [CI](#ci) reports timings and does not gate on them.

### Binary size and dependencies

Three programs measured what Behemoth adds to an application. Each has the same application dependencies: `net/http`, `database/sql`, and the `lib/pq` driver.

- `base` serves one route and opens a database, without Behemoth.
- `min` adds `engine.Prepare` and `engine.Boot` with the email and password plugin, the `net/http` adapter, and the PostgreSQL adapter.
- `all` adds the other six plugins: email verification, magic link, OAuth, phone number, two-factor, and username.

Each was built with `go build -trimpath`, with and without `-ldflags="-s -w"`. Run 2:

| Program | Size | Stripped | Packages linked | Behemoth packages |
| --- | --- | --- | --- | --- |
| `base` | 9.11 MiB | 6.32 MiB | 195 | 0 |
| `min` | 11.56 MiB | 8.09 MiB (+1.77) | 231 | 24 |
| `all` | 12.88 MiB | 9.03 MiB (+0.94) | 240 | 32 |

Run 1 gave the same stripped sizes. Its programs linked two more packages each, `x/crypto/bcrypt` and `blowfish`, through `utils/password.go`. For comparison, run 1 also built these programs:

| Program | Stripped |
| --- | --- |
| `examples/init`: Gin, OpenTelemetry, three plugins, PostgreSQL | 28.95 MiB |
| `cmd/behemoth` | 2.89 MiB |

The size of each package's symbols in `all`, from `go tool nm -size` without the uninitialized data (BSS), run 1:

| Package | Symbols |
| --- | --- |
| `plugins/twofactor` | 76.9 KiB |
| `crypto` | 58.7 KiB |
| `auth` | 56.5 KiB |
| `plugins/oauth` | 53.1 KiB |
| `dispatch` | 50.1 KiB |
| `store` | 49.2 KiB |
| `plugins/emailverification` | 46.4 KiB |
| `engine` | 44.9 KiB |
| `plugins/emailpassword` | 37.8 KiB |
| `plugins/phonenumber` | 36.1 KiB |
| `models` | 35.4 KiB |
| `schema` | 30.3 KiB |
| `httpapi` | 27.0 KiB |
| Every Behemoth package | 821 KiB |
| Third-party: `blake2b` 23.1, `argon2` 21.5, `jwt/v5` 21.5, `uuid` 4.4, `hkdf` 1.5 | 72 KiB |

Besides its own packages, `min` links these packages that `base` does not: `encoding/json`, `log/slog`, `hash/fnv`, `github.com/golang-jwt/jwt/v5`, `github.com/google/uuid`, and `golang.org/x/crypto/argon2`, `blake2b`, and `hkdf`, with `golang.org/x/sys/cpu`.

The numbers show these points:

- Behemoth with every plugin adds 2.7 MiB to a stripped program. About 821 KiB of it is Behemoth's own symbols, and about 72 KiB its third-party packages. The rest is the standard packages it brings, such as `encoding/json` and `log/slog`, and the runtime's tables for the added functions and types.
- An application's framework and telemetry weigh more. Behemoth is under a tenth of `examples/init`.
- The layout already limits the cost to what an application uses. Each router, storage, and telemetry adapter is a module of its own, so an application downloads and links only those it imports. Each plugin is a package of its own, so one plugin links 24 Behemoth packages and all of them link 32. The root module imports four third-party packages.
- The linker drops the functions that nothing reaches. The unused `utils.GeneratePasswordHash` linked in at 296 bytes, though its package was imported. A package is linked with its `init` work, its package-level variables, and the methods of its types that reach an interface, whether the application uses them or not.
- `jwt/v5` registers its RSA, ECDSA, and Ed25519 signing methods at `init`. `crypto/tls`, which any HTTP server links, already brings those packages, so they cost nothing more.
- No package of the root module calls `reflect.Value.Method` or `MethodByName`, or uses `text/template` or `html/template`. Any of them would make the linker keep every exported method of every type it reaches.
- `-s -w` removes the symbol table and the DWARF data: about 30% of the program, 11.56 MiB to 8.09 MiB. It is the application's choice.

### What the measurements say

- Behemoth's cost has two parts. A password flow is bound by Argon2id, in CPU and in memory. Every other flow is bound by its database calls. Behemoth's own code is a small share of either.
- The largest risk was memory under concurrent password flows, not speed. `crypto.PasswordLimit` bounds it since 2026-10-24.
- The database calls of a flow are few and can be counted exactly, which makes them the best regression check.
- Sign-in timing parity holds in the process. It needs a check over a network.
- Size has no large reduction to offer. The checks keep it where it is and show growth.
- Three things can spoil a measurement: the connection pool, the rate limits on one client address, and the machine's power state.

## Layer 1: micro-benchmarks

A micro-benchmark measures one function: its time, its allocations, and how it behaves when many goroutines call it. It runs in seconds and needs no database.

### Where they live

A micro-benchmark lives next to the code it measures, in the package's `_test.go` files, so that it can reach unexported helpers. A storage adapter's benchmarks live in the adapter's module. `BenchmarkBuildSQLiteWhereClause` moves from `tests/storage` to `storage/adapters`, which already has tests. It builds with `?` placeholders, the default of `adapters.ClauseOptions` and what SQLite uses.

### Conventions

Follow these conventions in every micro-benchmark:

- Build what the benchmark needs before the loop. Only the operation goes inside `b.Loop()`.
- Call `b.ReportAllocs()`.
- When goroutines share state, such as a lock, a cache, or a pool, add a `b.RunParallel` case and run it with `-cpu 1,4,8` and the machine's thread count. The parallel case of the key manager is what showed L56.
- Name the benchmark after what it measures, such as `BenchmarkArgon2idHasher_Hash` or `BenchmarkTokenCarrier_Extract`. Use sub-benchmarks for the cases, such as `serial`, `parallel`, or `wrong_password`.
- Use realistic inputs: a token of the length the session manager issues, a password of a usual length, and a `WHERE` clause of the shape the store builds.
- Check the result and call `b.Fatal` when it is wrong, so that a fast wrong answer can't pass for a speed-up.

### The benchmarks

| Package | What to benchmark | What it answers |
| --- | --- | --- |
| `crypto` | `Argon2idHasher.Hash` and `Verify`, serial and parallel | The cost of `Params` on a machine, and the memory of each hash in flight. Run it on the deployment's hardware before you change `Params`. |
| `crypto` | The secret hasher's `Hash` and `Verify` (`NewSecretHasher`) | The cost of checking a session token or another token. |
| `crypto` | `DefaultKeyManager.Current` and `GetByVersion`, serial and parallel | Replaces the two existing benchmarks (L55), and measures L56. |
| `crypto` | `JWTProviderImpl.Sign` and `Verify`, and `VerifyJWT` over a `CachedKeySet` | The cost of issuing a session token, and of a service checking one. |
| `crypto` | `AESGCMEncryptor.Encrypt` and `Decrypt` | The cost of a sealed value (`Store.Seal`). |
| `crypto` | `HMACSigner.Sign` and `Verify` | The cost of a signed payload. |
| `utils` | `LookupHashOf` | Part of every session and token lookup. |
| `httpapi` | `TokenCarrier.Extract`, for each delivery | Runs on every request. |
| `httpapi` | `ClientIP`, with trusted proxies and a long `X-Forwarded-For` | Runs on every request and every limited call. |
| `httpapi` | `DefaultErrorMapper.Map` | Runs on every refused request. |
| `plugins/adapters/nethttp` | A route whose handler returns at once, through `Router.Build` and the adapter, with `AuthenticateRequests` over a stub authenticator | What the request pipeline adds to every request: the request ID, the scope, error mapping, and authentication. It lives in the adapter's package, because `httpapi` can't import an adapter. |
| `dispatch` | `DefaultDispatcher.RunBefore` and `RunAfter`, with 0, 1, and 5 handlers, on an audited and an unaudited point | What hooks add to a flow. |
| `ratelimit` | `FixedWindow.Allow` over a counter in memory, in parallel on one key and on many keys | The limiter's own cost, apart from where the counter is stored. |
| `storage/adapters` | `BuildSQLWhereClause` | 17 of a session check's allocations are the SQL adapter code's. |
| `models` | `GenerateModelFromRows` | 9 of a session check's allocations. |
| `auth` | `DefaultAuthenticator.Authenticate` over a session manager stub | The authenticator's own allocations, apart from the database. A stub embeds `core.SessionManager` and overrides `Get` and `Touch`. |

`BestRouteMatch` is not in the list: the router resolves each route's rule once at boot, not per request.

### Run and compare

Run a package's benchmarks ten times, before and after a change, and compare the two with `benchstat`:

```sh
go test ./crypto/ -run '^$' -bench . -benchmem -count 10 > old.txt
# make the change
go test ./crypto/ -run '^$' -bench . -benchmem -count 10 > new.txt
benchstat old.txt new.txt
```

`benchstat` (`golang.org/x/perf/cmd/benchstat`) reports each change and whether it is statistically significant. Run both sides in the same session, on the same machine, in the same power state. One run can mislead, as the sign-in timing parity showed.

## Layer 2: flow benchmarks and budgets

A flow benchmark measures a whole flow on a booted application with a real database. It shows what layer 1 can't: the database calls, the store, the hooks, the rate limits, and the router, together. It runs in one process, so its results repeat and it needs no deployment.

### Where it lives

Layer 2 is a new package, `tests/perf`, in the `tests` module. That module already has the database containers (testcontainers) and `testutils`. The budget tests run in the "other" group of the integration job, on every pull request, with no change to the workflow. The benchmarks run only when `go test` is given `-bench`.

### The harness

`tests/perf` gets a boot helper with the shape of the first measurements' harness. It runs these steps:

1. Call `engine.Prepare` with the plugins the flow needs.
2. Open the database and size its pool. `SetMaxIdleConns` covers the benchmark's goroutines, and `SetMaxOpenConns` is what a deployment would set.
3. Create the tables with `testutils.MigrateApp`. It takes a `testing.TB` instead of a `*testing.T`, so that a benchmark can call it.
4. Call `engine.Boot` with the `net/http` adapter, the session token in a header, and the telemetry the test needs.
5. Create the users and the sessions the flow needs in the process, before the timer starts.

The helper takes the database to use. SQLite is the default. PostgreSQL, MySQL, SQL Server, and MongoDB come from the containers when an environment variable names them, such as `BEHEMOTH_PERF_DB=postgres`. The key-value store is either none or Redis from a container.

The timed benchmarks of password flows turn off `engine.RuleSignInAddress` and `engine.RuleSignUpAddress`, or send each request from another client address: one address signs in thousands of times. The budget tests keep every limit on, because the counter's calls are part of a flow's cost.

### The flows

| Flow | How it is called | Cases | Step |
| --- | --- | --- | --- |
| Session check | `Authenticator.Authenticate` | Serial and parallel. Without and with a session cache. With `UpdateAge` set (L23). | 2 |
| Sign-in by password | `POST /sign-in/email` | Right password, wrong password, and unknown email. Serial and parallel. | 2 |
| Sign-up | `POST /sign-up/email` | Without hook handlers, and with a `data.user.created` handler that waits (L27). | 2 |
| A protected request | A route of a test plugin that requires a session (`RequireSession`) and returns at once | The whole pipeline with authentication. | 5 |
| Session token | `POST /session/token` | Issuing a JWT for a session. | 5 |
| Magic link | The request route, then the sign-in with the link's token | Known and unknown email (M1). | 5 |
| Password reset | The request route, then the confirm route | Known and unknown email (M1). The confirm hashes a password. | 5 |
| Email verification | The send route, then the confirm route | Known and unknown email (M1). | 5 |
| Phone number | `send-otp`, then the sign-in by code | Known and unknown number (M1). | 5 |
| Two-factor | The challenge, then `Verify` with a TOTP code | A pending session from a password sign-in. | 5 |
| OAuth | The callback, against a provider served by `httptest` | A new user and a returning one. | 5 |

Each flow benchmark runs on SQLite in every run that asks for benchmarks. It runs on PostgreSQL, MySQL, SQL Server, and MongoDB, and with Redis next to PostgreSQL, in the nightly run.

Parallel benchmarks of writing flows on SQLite can fail with "database is locked" (M12), as concurrent sign-ups with the username plugin do. They open the database with `_txlock=immediate`, as the username tests do.

### Budgets

A budget test fails when a count passes its limit. A pull request that changes the count on purpose updates the limit in the same change, and says why.

#### Database calls per flow

The test boots with a `telemetrytest` recorder as `BootConfig.Telemetry`. It runs the flow once, counts the observations of `telemetry.MetricStoreDuration` that the flow added, and compares them by operation and table with the budget. On failure it prints the calls, so that the new one has a name.

The first budgets are today's counts:

| Flow | Budget |
| --- | --- |
| Sign-up over HTTP | 6 |
| Sign-in, right password | 5 |
| Sign-in, wrong password | 4 |
| Sign-in, unknown email | 3 |
| Session check | 1 |

The other flows of the table above get their budgets as their benchmarks arrive, from the counts they have then.

#### Allocations per operation

The test runs the operation with `testing.AllocsPerRun` and compares the result with the budget. It boots without telemetry, because the recorder allocates too. There are two kinds of budget:

- On paths in Behemoth's code alone, such as the request pipeline with a handler that returns at once, the token carrier, the dispatcher, and the authenticator over a stub, the budget is today's count. It changes only when Behemoth's code changes.
- On paths through a driver, such as the session check on SQLite, the budget is today's count plus headroom, 15% for a start. More than half of a session check's allocations belong to the driver, and a driver update moves them.

#### Timing parity

Timing parity is not a budget, because it is a comparison of timings. Each flow that answers a known and an unknown account the same way runs both cases with `-count 20`, and `benchstat` reports whether the difference is significant. It runs on SQLite and on PostgreSQL in the nightly run. On PostgreSQL the extra database calls of a known account take a network round trip each.

The flows to compare are the ones M1 lists, and sign-in by password:

- sign-in by password: wrong password and unknown email
- the magic link request
- the password reset request
- the email verification send
- the phone number plugin's `send-otp` and `request-password-reset`

M1 already says that all but sign-in have a gap. The check shows its size, and then confirms the fix.

## Layer 3: load tests

A load test measures what one process can't show: network round trips, connection pools under load, several replicas that share a database and Redis, memory under concurrency, and behavior over hours.

### The target

The load target is a small server in `tests/perf/server`, in the `tests` module, which already depends on the OpenTelemetry adapter. It has these parts:

- every plugin whose flow a scenario uses
- PostgreSQL and Redis, from a Compose file next to the server
- `net/http/pprof` on a port of its own, which the load generator doesn't reach
- the OpenTelemetry adapter (`telemetry/adapters/otel`), exporting to a collector
- its settings from environment variables: the connection pool, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, `UpdateAge`, the trusted proxies, and the key-value store

A scenario runs on one replica first, then on two or three behind a reverse proxy, sharing PostgreSQL and Redis.

### The load generator

The load generator is k6, with its `constant-arrival-rate` and `ramping-arrival-rate` executors, which are open-model. Its scripts live in `tests/perf/load`, one per scenario. k6 runs on another machine than the server, or on cores the server doesn't use (`taskset`).

### Client addresses

Core's per-address limits allow ten sign-ins a minute from one address. A load test sends from many addresses instead of turning the limits off:

1. Set `RouterConfig.TrustedProxies` to the load generator's network.
2. Have each virtual user send an `X-Forwarded-For` address of its own, from a pool.

The limiter then counts as it does in production. A scenario that measures something else may turn a rule off with `RateLimitConfig.Disabled`, and says so in its script.

### Scenarios

| # | Scenario | Traffic | What to watch |
| --- | --- | --- | --- |
| 1 | Session checks | A protected route at a rising rate | The rate at which p99 latency passes its limit. PostgreSQL connections in use. CPU per replica. Cache hits and misses in `behemoth.session.validated`. |
| 2 | Sign-in burst | 10, then 50, 100, 200, and 500 sign-ins at once | Peak RSS, out-of-memory kills, 5xx answers, and p99. It shows the bound of `crypto.PasswordLimit` at work: peak RSS stays flat as the burst grows, and what does not fit in `MaxWait` gets `503` with `server_busy`. |
| 3 | Mixed workload | 1 sign-up for 10 sign-ins and 1,000 protected requests, for 30 minutes | p50, p95, and p99 per route. Error rate. Database calls per second. |
| 4 | One hot counter | Many requests on one rate-limit key, with the database counter and with Redis | Rate-limit operations per check in `behemoth.store.duration`, where each lost race adds a read and a write. Checks that end in `error` in `behemoth.ratelimit.checks`, which fail open by default. |
| 5 | Several replicas | Scenario 3 on two replicas, with sign-outs and revocations during the run | Counts shared across replicas. A revoked session refused on every replica from the next request on, with Redis as the session cache ([ADR-0011](../decisions/0011-ordering-session-cache-writes-by-row-version.md)). |
| 6 | Soak | Scenario 3 at half its sustainable rate, for 8 hours, with `Store.Purge` running | Heap after each collection, goroutines, open connections, the mail queue, table sizes, and how long a purge takes (L54). |
| 7 | Slow database | Scenario 3, with 50 ms added to every PostgreSQL round trip by a proxy such as Toxiproxy | Pool exhaustion, queueing, and timeouts. How the latency of one call adds up in a flow. |

### Observing

A load test reads these sources:

- Behemoth's metrics, through the OpenTelemetry adapter: `behemoth.http.requests` and `behemoth.http.duration` per route and status, `behemoth.store.duration` per operation and table, `behemoth.session.validated` with the cache result, and `behemoth.ratelimit.checks` per rule and result.
- Traces: the `behemoth.request` span of a slow request, with the store's spans below it.
- The Go runtime: CPU and heap profiles from pprof during the run, mutex and block profiles (the server sets `runtime.SetMutexProfileFraction` and `runtime.SetBlockProfileRate`), and the heap and collector metrics.
- PostgreSQL: `pg_stat_statements` for the statements and their time, and `pg_stat_activity` for the connections.
- The container runtime: the server's peak memory and any out-of-memory kill.

### Recording results

A run records its environment next to its numbers: the commit, the Go version, the machine and its power state, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, the pool settings, the database and Redis versions, the number of replicas, and the k6 script. A result without its environment can't be compared with another. Baselines go in a dated section of this document until there are enough of them to need a file of their own.

## Size and dependency checks

### Size budget

The three programs of [Binary size and dependencies](#binary-size-and-dependencies) move to `tests/perf/size/base`, `min`, and `all`. A CI step builds each with `-trimpath -ldflags="-s -w"`. It compares how much `min` and `all` add over `base` with the baseline in `tests/perf/size/baseline.txt`. Growth of more than 5% fails the step. A pull request that accepts the growth updates the baseline. On failure, the step prints the symbol size of each package (`go tool nm -size -sort size`), so that the growth has a name.

The step measures the difference over `base`, not the absolute size, because a new Go release changes the size of every program.

### Dependency allowlist

A test next to `layout_test.go`, in the root module, parses the imports of every file of the root module except the tests, as `TestPackageLayout` does. It fails on a third-party import that is not in this list:

- `github.com/golang-jwt/jwt/v5`
- `github.com/google/uuid`
- `golang.org/x/crypto/argon2`
- `golang.org/x/crypto/hkdf`

A new dependency then comes with a change to the list, which a reviewer sees. The adapters' modules have dependencies of their own and are not checked.

### Rules that keep the size down

Follow these rules in the root module:

- Put a dependency that only an adapter needs in that adapter's module.
- Keep each plugin a package of its own, so that an application that doesn't import it doesn't link it.
- Do no work at `init`, and keep no large package-level values. Both are linked whether or not the application uses the package.
- Don't call `reflect.Value.Method` or `MethodByName` with a computed name, and don't use `text/template` or `html/template`. The linker then keeps every exported method of every type it reaches.

### What an application can do

The API docs can give applications this advice, in [step 7](#implementation-steps):

- Build with `-trimpath -ldflags="-s -w"`, which removes about 30% of the program.
- Size the database pool for the queries that run at once: `SetMaxIdleConns` at least as high as the expected concurrency.
- Plan memory for the password flows: `PasswordLimit.MaxConcurrent`, times `Params.Memory`, times about two at the default `GOGC`. In a container without a CPU limit, set `MaxConcurrent` (M32).
- Profile-guided optimization (a `default.pgo` in the main package) is the application's choice. It gains little here: Argon2id is assembly on amd64, and the rest is database calls.

## CI

| When | What | Effect |
| --- | --- | --- |
| Every pull request | The layer 2 budget tests on SQLite, in the "other" group. The dependency allowlist, in the root module's tests. The size budget, in a new step. | Fails the run. |
| Every pull request that changes Go code | The layer 1 benchmarks and the layer 2 benchmarks on SQLite, for the base and the head in the same job, with `-count 10`. The `benchstat` table goes in the job summary. | Report only. |
| Every night, and on demand | Layer 2 on every database and with Redis, and the timing parity checks, compared with the previous night. | Report only. |
| Before a release, and on demand | Load scenarios 1 to 3 on a dedicated machine. Scenarios 4 to 7 before a release that changes their area. | A maintainer compares them with the last baseline. |

The base and the head run in the same job because each job can get another runner. Load tests don't run on GitHub's shared runners: their CPU and their neighbors change from run to run, and the load generator would share the machine with the server.

## Decisions

### Gate on counts, report timings

The same laptop timed a session check at 19 µs one day and 33 µs the next. A gate on timings either fails at random, or has limits so wide that it catches nothing. Database calls, allocations, binary size, and imports are the same on every run, so they can fail CI. Timings are compared with `benchstat` and read by a maintainer.

### Count database calls at the `Database` interface

The store's instrumentation already reports each call, so the budget needs no new hook, and the count is the same on every adapter. It misses the statements that an adapter runs inside one call. Counting statements would need each driver's own hooks, which is not worth it yet.

### Flow benchmarks live in the `tests` module

The `tests` module has the containers, the schemas, and `testutils`, and its tests already run in CI. The root module keeps its test dependencies to testify.

### A dedicated load target, not `examples/init`

`examples/init` shows how an application uses Behemoth, with Gin, demo pages, and settings fixed for the demo. A load target needs its settings from the environment, and pprof, which a demo should not carry.

### Measure size through programs

Go doesn't build a binary of a library. The size an application pays is the difference between a program with Behemoth and the same program without it, built the same way. The difference also leaves out what a Go release changes in every program.

### Many client addresses through the trusted-proxy header

A load test with the limits turned off measures a deployment nobody runs, and leaves the counters out of the load. Sending many addresses through `X-Forwarded-For` keeps the counters in.

### Open-model load

A closed-model generator waits for each response before it sends the next request. When the server slows down, the load drops with it, and the measured latency stays low. This is known as coordinated omission. k6's arrival-rate executors keep sending at the set rate.

## Alternatives considered

- **A dashboard of benchmark results over time,** such as github-action-benchmark. Deferred: a `benchstat` table per pull request covers the first need, and a chart of timings from shared runners would show their noise.
- **An in-memory `behemoth.Database` for allocation budgets without a driver.** Deferred, see [Open questions](#open-questions).
- **wrk or hey for load tests.** Rejected: both are closed-model, so their latency numbers are low when the server slows down. vegeta is open-model and fits a test of one route. k6 is chosen because a scenario chains requests, such as a sign-in and then requests with its token.
- **Load tests on GitHub's shared runners.** Rejected, see [CI](#ci).

## Open questions

- **The headroom of allocation budgets on driver paths.** 15% is a guess, and a driver update could move the count by more.
- **An in-memory `Database` for budgets.** It would give exact allocation budgets for whole flows, without the driver. It would also be a second implementation of the `Database` contract to keep right.
- **The machine for load tests.** It has to be the same machine each time, with fixed power settings: a self-hosted runner, or a cloud machine of a fixed type. Who owns it is open.
- **The pass criteria of the load scenarios.** They are set after the first baseline. For example, the rate at which p99 passes its limit must not drop by more than 10%.
- **Why session check throughput on SQLite falls past 8 goroutines.** The profile shows cgo calls and runtime spinning, not Behemoth. The flow benchmark on PostgreSQL shows whether Behemoth scales past that point.
- **The default Argon2id parameters.** `DefaultParams` is the second option RFC 9106 recommends: 64 MiB, 3 iterations, and 4 lanes. OWASP's minimum is lower (19 MiB, 2 iterations, 1 lane), which fits more than three times the hashes in flight in the same memory, at a cost in resistance to guessing. Whether to change the default, or to document the trade-off, is still open. `PasswordLimit` bounds the memory, and the params decide how many hashes fit in it.

## Implementation steps

The work is done in this order. Each step ends with its tests and its docs, and leaves the code working.

| Step | Content | Status |
| --- | --- | --- |
| 1 | Layer 1 for `crypto`: the two key manager benchmarks rewritten (L55, which removes its entry), and the Argon2id, secret hasher, JWT provider, `VerifyJWT`, encryptor, and signer benchmarks, serial and parallel. `BenchmarkBuildSQLiteWhereClause` moved to `storage/adapters`, with `?` placeholders. | Not started |
| 2 | `tests/perf`: the boot helper with a sized pool, and `testutils.MigrateApp` taking a `testing.TB`. The benchmarks of the session check, sign-in, and sign-up. The database call budgets of those flows. | Not started |
| 3 | The size and dependency checks: `tests/perf/size`, its baseline, the CI step, and the allowlist test. | Not started |
| 4 | Layer 1 for `httpapi`, the request pipeline, `dispatch`, `ratelimit`, `storage/adapters`, `models`, and `auth`. The allocation budgets. | Not started |
| 5 | Layer 2 for the other flows, the database matrix and Redis, and the timing parity checks. The CI jobs for pull requests and the nightly run. | Not started |
| 6 | The load target in `tests/perf/server`, its Compose file, the k6 scripts of scenarios 1 to 3, and the first baseline. | Not started |
| 7 | Scenarios 4 to 7. In the API docs: the pool size and the memory of password flows. | Not started |

Steps 1 to 3 need no new infrastructure, and they bring the checks that can fail CI. Steps 4 and 5 widen the coverage. Steps 6 and 7 need a machine for load tests.

## Related entries

These entries in `docs/ongoing.md` affect the plan:

- **M32, "The default password hash bound ignores a container's memory".** Scenario 2 run in a container without a CPU limit shows it. Scenario 2 is also the regression test of the bound that closed M28.
- **L55, "The key manager benchmarks time its construction".** Step 1 resolves it and removes the entry.
- **L56, "Key lookups contend on the key manager's read lock".** The parallel key manager benchmark measures it. Today it is under 1% of a session check.
- **L23, "`UpdateAge` has no default, and a large value writes on every request".** The session check benchmark with `UpdateAge` set measures the write.
- **L27, "After-commit callbacks run synchronously".** The sign-up benchmark with a slow `data.user.created` handler measures the delay.
- **L29, "The logger cannot say whether a level is enabled".** An allocation budget with a logger on the SQL adapter shows its cost.
- **L54, "A purge deletes all of its rows in one statement".** The soak test measures a purge on tables that have grown.
- **M1, "A link request for a known email takes longer than one for an unknown email".** The timing parity checks cover the flows it lists.
- **M12, "A data hook that reads before the write can fail with "database is locked" on SQLite".** Parallel benchmarks of writing flows on SQLite open the database with `_txlock=immediate`.
- **M13, "The store does not retry on a deadlock error".** Load tests on MySQL and SQL Server may hit it.
