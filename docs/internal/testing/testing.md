# Tests, CI, and coverage

This document explains where Behemoth's tests are, what CI runs them against, how coverage is measured across the modules and checked against a target per package, and the tests that inject failures into every flow.

The code lives in:

- `.github/workflows/go.yml`: the CI workflow
- `scripts/coverage.sh`: runs the tests of a module with coverage, and merges and checks the profiles
- `scripts/coverage_targets.txt`: the coverage target of each published package
- `.github/codecov.yml`: what Codecov does with the uploaded profile
- `tests/testutils/test_setup.go`: the database containers and their images
- `tests/plugins/faults_test.go`, `tests/plugins/plugin_contract_test.go`: the failure injection tests

---

## Where the tests are

Each module's own tests cover what needs no service: the root module's packages, and the parts of each adapter that run without a database. `tests/` is a module of its own, never published, and holds the tests that need the rest of Behemoth around the code under test:

| Package | What it tests | What it needs |
|---|---|---|
| `tests/storage`, `tests/storage/migrations` | each storage adapter and migration driver against its database | Docker: PostgreSQL, MySQL, SQL Server, MongoDB, and Redis containers |
| `tests/store` | the store, and the session and token managers | Docker for the store contract on every database; SQLite for the rest |
| `tests/plugins` | the plugins, booted by `engine.Boot` over SQLite | nothing |
| `tests/models`, `tests/telemetry/otel` | the user model, the OpenTelemetry adapter | nothing |

The container suites start the oldest release of each database that Behemoth supports ([`docs/api/supported-databases.md`](../../api/supported-databases.md)). A `BEHEMOTH_TEST_*_IMAGE` variable names another image.

---

## What CI runs

`go.yml` builds, vets, and tests every module found by its `go.mod`, so a module added later is tested without a change to the workflow:

| Job | Runs | On |
|---|---|---|
| `test-modules` | every module except `tests/` | the two supported Go releases |
| `test-integration` | `tests/`, in three groups: `store`, `storage`, and every other package | two pairings of releases, below |
| `coverage` | merges the profiles and checks the targets | after both |
| `test-suite` | succeeds when every job above did | the check a branch ruleset requires |

### Two pairings of releases

`tests/` runs twice. `oldest` pairs the oldest supported Go release with the oldest supported release of each database, the images `tests/testutils` starts by default. `newest` pairs the newest Go release with the newest release of each database, which the matrix names in `BEHEMOTH_TEST_*_IMAGE`: `postgres:18`, `mysql:9`, `mcr.microsoft.com/mssql/server:2025-latest`, `mongo:8`, and `redis:8-alpine`.

Each supported release runs once, in two runs where four would test each Go release with each database release. The Go release and the database release change different code, the Go side and the server side of a connection, so a failure that only one combination shows is unlikely. If one turns up, add the combination to the matrix.

A tag such as `postgres:18` follows the patch releases of its major release. Move it when the database has a new major release, and the minimum when the maker ends support for the oldest one. Update `supported-databases.md` in the same change.

The integration job runs on `ubuntu-24.04` rather than `ubuntu-latest`: MongoDB 8 refuses to start on Linux 6.19 and later ([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)), and `ubuntu-latest` moves to a newer kernel with the next Ubuntu release. MongoDB 8 and SQL Server 2025 don't start on the development machine either, so CI is the only place they run (`docs/ongoing.md`, M31).

---

## Coverage

### How it is measured

Most of the root module is tested from `tests/`, and the storage adapters are tested there too. Coverage per module would leave that out: `store` is at 2% from its own module's tests and over 90% with `tests/`. So every test run measures the coverage of every published package that its test binaries link, with `-coverpkg=github.com/go-behemoth/behemoth/...`. The pattern matches the packages of every module in the run's build list, so the tests of `tests/` count for the root module and the adapters, and an adapter's tests count for the root module.

`scripts/coverage.sh report` merges the profiles. A block of statements counts as covered when a test in any profile ran it. The packages of `tests/` and `examples/` are left out. The report gives the coverage of each module and each package, and the merged profile is what `go tool cover` and Codecov read.

Choices:

- **Text profiles, not binary coverage data.** `go test -coverprofile` lists a package without test files with its statements at zero. With `-test.gocoverdir`, a package that no test binary links is missing from the data, and the report would not show that it has no tests.
- **`atomic` mode,** the one `-race` needs, so the same profiles work when CI runs with the race detector. Profiles of different modes can't be merged.
- **One Go release.** The blocks of a package can differ between Go releases, and the merge refuses a block whose statement count differs between profiles. CI takes the profiles of the runs on the newest release.
- **Every test runs through `coverage.sh test`,** in CI too, so a profile from CI and one from a local run measure the same thing.

### Targets

`scripts/coverage_targets.txt` gives each published package the share of its statements that the tests must run. Packages that decide who is signed in or hold keys have 90%: `crypto`, `keysource`, `auth`, `httpapi`, `identity`, `outbound`, the plugins, and the router adapters. Every other package has 80%. These follow the awesome-go requirement for a listing, 80% for a package in general, with the higher bar for the security-critical ones.

The check fails when a package is below its target, when a package has no target, and when a target names a package without statements. A new package therefore gets its target in the change that adds it, like a new module gets its line in `scripts/release.sh`. A target is not lowered to make a change pass: the change adds the tests.

`storage` has 0%: nothing imports it, and it is to be deleted (`docs/ongoing.md`).

### Where the report is

- The `coverage` job's summary has the tables of modules and packages.
- The job's `coverage` artifact has the merged profile and the annotated source view from `go tool cover -html`.
- Codecov keeps the profile of each push to `dev` and `main`, behind the badge in the README. It only shows coverage: `.github/codecov.yml` turns off its comments and status checks, and the upload proves its origin with an OIDC token, so no upload token is stored. A failed upload doesn't fail the run.

Locally, `scripts/coverage.sh` runs every module's tests and prints the report, and `--html FILE` writes the annotated source. It needs Docker for `tests/`, and takes about five minutes.

---

## Failure injection

Two test files in `tests/plugins` check every flow of every plugin under failures, systematically rather than one branch at a time.

`faults_test.go` boots one application with every plugin that has flows of its own, over a `faultyDB`, a `Database` whose n-th call fails. For each flow, it runs the flow with the first call failing, then the second, and so on, until the flow makes fewer calls than the one set to fail. It checks two things:

- **A database failure is the server's.** The flow either carries on, for a write it can do without, or returns an error that `DefaultErrorMapper` answers with 5xx. It never reports the failure as the caller's mistake, such as an invalid link or a wrong code: that would count against the user's attempts and hide the outage.
- **A failing hook handler passes through.** A handler on every hook point fails at the n-th run in the same way, and the flow returns the handler's error unchanged, or carries on past an after point. A sign-in that a handler refuses must not read as a wrong password.

`plugin_contract_test.go` checks the plugins outside a flow: each passes on a failed declaration or handler registration instead of starting with part of its declarations; a flow called before `Boot` returns a configuration error; and no malformed body makes a route answer 5xx.

To cover a new flow, add it to `faultFlows`: a function that prepares the application for a new user, and returns the flow. The SQLite database of these tests does not sync to disk, which keeps the thousands of transactions they run to a second or two.

---

## Deferred

- **The storage suites as an importable package.** `Deferred:` until the extension guide for storage adapters, which defines the contract the suites test. `tests/storage/database_suite.go` and `kv_storage_suite.go` would move to a package such as `storage/storagetest`, so that a third-party adapter runs them in its own tests.
- **The race detector.** CI does not run with `-race` yet. `GOFLAGS=-race` passes it to `coverage.sh test`.
