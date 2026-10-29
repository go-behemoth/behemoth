# Behemoth

[![CI](https://github.com/go-behemoth/behemoth/actions/workflows/go.yml/badge.svg?branch=dev)](https://github.com/go-behemoth/behemoth/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-behemoth/behemoth.svg)](https://pkg.go.dev/github.com/go-behemoth/behemoth)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-behemoth/behemoth)](https://goreportcard.com/report/github.com/go-behemoth/behemoth)
[![Coverage](https://codecov.io/gh/go-behemoth/behemoth/branch/dev/graph/badge.svg)](https://app.codecov.io/gh/go-behemoth/behemoth/tree/dev)

Behemoth is a plugin-based authentication library for Go. You choose the plugins and the database, and Behemoth sets up the tables, routes, sessions, hooks, and telemetry around them.

Behemoth is at `v0.x`, and the API can change between minor versions until `v1.0.0`. The [changelog](./CHANGELOG.md) lists every change. Pin the version in your `go.mod` and read the changelog before you upgrade.

[Website](https://go-behemoth.dev) · [Guides](https://go-behemoth.dev/docs) · [Reference](https://go-behemoth.dev/reference) · [Go package documentation](https://pkg.go.dev/github.com/go-behemoth/behemoth)

## What you get

| Plugin | What it does |
| --- | --- |
| [`emailpassword`](./docs/api/emailpassword.md) | Sign-up, sign-in, sign-out, and password reset |
| [`username`](./docs/api/username.md) | Unique usernames and sign-in by username, on top of `emailpassword` |
| [`phonenumber`](./docs/api/phonenumber.md) | A phone number confirmed by one-time code, sign-in by code or password, and password reset by code |
| [`magiclink`](./docs/api/magiclink.md) | Sign-in by a link sent to the user's email |
| [`emailverification`](./docs/api/emailverification.md) | Confirms a user's email address by link |
| [`oauth`](./docs/api/oauth.md) | Sign-in through OAuth 2.0 or OpenID Connect, such as Google or GitHub, linking provider accounts to a user, and stored provider tokens |
| [`twofactor`](./docs/api/twofactor.md) | A second factor at sign-in: an authenticator app, a code by email or text message, and recovery codes. Experimental. |

Around the plugins:

- **Migrations.** Behemoth builds a migration from the tables your plugins and your own schema declare, and writes it to a folder for you to review. The `behemoth` command line does it from your application's directory.
- **Hooks.** Run your code before or after a user is written, or around a whole flow such as sign-up. A handler can change data, stop an operation, or react afterwards.
- **Sessions and tokens.** Sessions live in your database or a key-value store, and travel as a cookie or a header.
- **Router adapters.** Mount the routes on `net/http`, Gin, Echo, chi, or Fiber.
- **Telemetry.** Logs, audit events, metrics, and traces, with an OpenTelemetry adapter.
- **Rate limiting** and **Argon2id password hashing**, on by default.

## Installation

Behemoth needs Go 1.26 or later.

```bash
go get github.com/go-behemoth/behemoth
```

Database and router adapters are separate modules, so you download only what you use:

```bash
go get github.com/go-behemoth/behemoth/storage/adapters/postgres
go get github.com/go-behemoth/behemoth/plugins/adapters/gin
```

The adapter for the standard library's `http.ServeMux` needs no dependency of its own, so it comes with the core module: import `github.com/go-behemoth/behemoth/plugins/adapters/nethttp`.

Use one version for every Behemoth module in your `go.mod`.

## Quick start

This quick start builds a server with email and password sign-in on PostgreSQL and `net/http`. You need Go 1.26 or later, Docker, and `openssl`.

Setup has two steps. `Prepare` collects the declarations of your plugins and schema, with no database. `Boot` connects them to a database and a router.

1. Start PostgreSQL, and set the two variables the application reads:

   ```bash
   docker run -d --name behemoth-db -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=behemoth -p 5432:5432 postgres:17
   export DATABASE_URL='postgres://postgres:postgres@localhost:5432/behemoth?sslmode=disable'
   export BEHEMOTH_SECRET=$(openssl rand -hex 32)
   ```

   `BEHEMOTH_SECRET` is the master secret that every key is derived from. Keep the same value between runs, or sessions stop working. If the server stops with `need >= 32`, the variable is not set in that terminal.

2. Create a module:

   ```bash
   mkdir myapp && cd myapp
   go mod init example.com/myapp
   ```

3. Put the setup in `auth/behemoth.go`. The server and the `behemoth` command line both start from it, so the tables a migration creates are the tables the server reads:

   ```go
   // Package auth holds the quickstart's Behemoth setup. The server and the
   // behemoth command line both start from Prepare, so the tables a migration
   // creates are the tables the server reads.
   package auth

   import (
   	"context"
   	"database/sql"
   	"os"

   	"github.com/go-behemoth/behemoth/core"
   	"github.com/go-behemoth/behemoth/engine"
   	"github.com/go-behemoth/behemoth/migration"
   	"github.com/go-behemoth/behemoth/plugins/emailpassword"
   	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
   	_ "github.com/lib/pq"
   )

   // Prepare returns what the application declares. It opens no connection.
   func Prepare() (*engine.PreparedApp, error) {
   	plugins := []core.Plugin{emailpassword.New(emailpassword.Options{})}
   	return engine.Prepare(plugins, engine.PrepareConfig{
   		Migration: migration.MigrationConfig{
   			FolderPath: "migrations",
   			Path:       migration.PathManaged, // Behemoth applies the migrations itself
   		},
   	})
   }

   // MigrationBackend opens the database the command line compares with.
   func MigrationBackend(ctx context.Context, app *engine.PreparedApp) (migration.Backend, error) {
   	db, err := OpenDB(ctx)
   	if err != nil {
   		return migration.Backend{}, err
   	}
   	return postgres.MigrationBackend(db, app.Resolver), nil
   }

   // OpenDB opens the PostgreSQL database that DATABASE_URL names.
   func OpenDB(ctx context.Context) (*sql.DB, error) {
   	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
   	if err != nil {
   		return nil, err
   	}
   	if err := db.PingContext(ctx); err != nil {
   		db.Close()
   		return nil, err
   	}
   	return db, nil
   }
   ```

4. Write the server in `main.go`:

   ```go
   // Command quickstart is the smallest Behemoth application: email and
   // password sign-up and sign-in on PostgreSQL and net/http.
   package main

   import (
   	"context"
   	"log"
   	"net/http"
   	"os"
   	"time"

   	"github.com/go-behemoth/behemoth/core"
   	"github.com/go-behemoth/behemoth/crypto"
   	"github.com/go-behemoth/behemoth/engine"
   	"github.com/go-behemoth/behemoth/httpapi"
   	"github.com/go-behemoth/behemoth/plugins/adapters/nethttp"
   	"github.com/go-behemoth/behemoth/storage/adapters/postgres"

   	"example.com/myapp/auth"
   )

   func main() {
   	ctx := context.Background()

   	// 1. Declarations: plugins and schema. No database yet.
   	app, err := auth.Prepare()
   	if err != nil {
   		log.Fatal(err)
   	}

   	// 2. Storage, bound to the resolver that Prepare derived.
   	sqlDB, err := auth.OpenDB(ctx)
   	if err != nil {
   		log.Fatal(err)
   	}
   	defer sqlDB.Close()
   	db := postgres.NewPostgresAdapter(sqlDB, app.Resolver)

   	// 3. Runtime: sessions and the routes, on your own mux.
   	mux := http.NewServeMux()
   	_, err = engine.Boot(ctx, app, db, engine.BootConfig{
   		Crypto: crypto.Config{
   			// The master secret: 64 hex characters, from `openssl rand -hex 32`.
   			Secrets: crypto.StaticSecretSource{
   				Secrets: map[int]string{1: os.Getenv("BEHEMOTH_SECRET")},
   				Current: 1,
   			},
   		},
   		HTTP:    nethttp.New(mux),
   		Session: core.SessionConfig{ExpiresIn: 24 * time.Hour},
   		// The session token travels in a header, which curl shows.
   		Router: httpapi.RouterConfig{
   			Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader},
   		},
   	})
   	if err != nil {
   		log.Fatal(err)
   	}

   	log.Fatal(http.ListenAndServe(":8080", mux))
   }
   ```

5. Download the modules the code imports, install the command line, and create the tables. `generate` writes the first migration to `./migrations`, and `migrate` applies it:

   ```bash
   go mod tidy
   go install github.com/go-behemoth/behemoth/cmd/behemoth@latest
   behemoth generate -confirm
   behemoth migrate -confirm
   ```

6. Run the server, and in another terminal sign up and sign in:

   ```bash
   go run .
   ```

   ```bash
   curl -X POST localhost:8080/api/auth/sign-up/email \
     -d '{"email":"ada@example.com","password":"correct horse battery"}'
   curl -i -X POST localhost:8080/api/auth/sign-in/email \
     -d '{"email":"ada@example.com","password":"correct horse battery"}'
   ```

   The sign-up answers `201 Created` with the new user. The sign-in answers `200 OK` and carries the session token in the `Set-Auth-Token` header. A client sends it back as `Authorization: Bearer TOKEN`. Without the `Router` setting in `main.go`, the token travels in a cookie.

7. Stop PostgreSQL when you are done: `docker rm -f behemoth-db`.

To protect your own routes and read who is signed in, see [Sessions](./docs/api/sessions.md). To call the same flows from code, without a router, see [Email and password](./docs/api/emailpassword.md#call-the-flows-from-code).

## Examples

The [`examples`](./examples) directory holds applications you can run:

- [`examples/quickstart`](./examples/quickstart) is the quick start above.
- [`examples/cli`](./examples/cli) is a small application on PostgreSQL and `net/http` whose migrations the `behemoth` command line writes. Its [README](./examples/cli/README.md) covers Docker, schema changes, and answering a question. It runs with `docker compose up --build`.
- [`examples/init`](./examples/init) is a complete application on PostgreSQL and Gin. It shows migrations, a custom plugin, hooks, telemetry with OpenTelemetry, and sign-in with GitHub, Google, or another OpenID Connect provider.

## Supported databases

| Kind | Options |
| --- | --- |
| `database/sql` | PostgreSQL, MySQL, SQLite, SQL Server |
| Document | MongoDB |
| ORM | Bun, GORM |
| Key-value (sessions, rate limits) | Redis |

[Supported databases](./docs/api/supported-databases.md) lists the releases of each database that Behemoth supports and tests on.

Migrations can be generated for PostgreSQL, MySQL, SQLite, and SQL Server. MongoDB has no migrations: the adapter creates the declared indexes (`EnsureIndexes`), and `Boot` checks them.

## Documentation

The [guides](https://go-behemoth.dev/docs) and the [reference](https://go-behemoth.dev/reference) are on the website. The reference pages are built from the pages below, which also read well on GitHub.

| Page | Covers |
| --- | --- |
| [Email and password](./docs/api/emailpassword.md) | The plugin's options, routes, and hook points |
| [Username](./docs/api/username.md) | Usernames on top of email and password: the rules, sign-up, sign-in by username, and rate limits |
| [Phone number](./docs/api/phonenumber.md) | Confirming a number by one-time code, sign-in by code or password, password reset by code, and rate limits |
| [SMS and one-time codes](./docs/api/sms.md) | The SMS sender plugins use, and the code manager |
| [Magic link](./docs/api/magiclink.md) | Sign-in by emailed link: setup, sending the link, routes, redirects, and rate limits |
| [Two-factor authentication](./docs/api/twofactor.md) | A second factor at sign-in: TOTP, codes by email or SMS, recovery codes, the lock, routes, and rate limits |
| [OAuth](./docs/api/oauth.md) | Sign-in through an OAuth 2.0 or OpenID Connect provider: providers, the callback page, linking provider accounts, the provider's tokens, routes, and rate limits |
| [Email verification](./docs/api/emailverification.md) | Confirming a user's address: when a link is sent, routes, requiring a verified email, and changing an address safely |
| [Mail](./docs/api/mail.md) | The mail sender plugins use, and waiting or background sends |
| [Router adapters](./docs/api/routers.md) | Mounting the routes on `net/http`, Gin, Echo, chi, or Fiber, and what is specific to each |
| [Callers and permissions](./docs/api/callers.md) | Who a call runs for: reading the caller, naming your own code, authenticating another transport, and the permissions flows require |
| [Sessions](./docs/api/sessions.md) | Session settings, how the token travels (cookie, header, body), the sign-in response, and protecting your own routes |
| [Session tokens](./docs/api/session-tokens.md) | Short-lived JWTs that tell another service who a request is from, and the published keys it checks them with |
| [JWT verification](./docs/api/jwt-verification.md) | Checking a JWT signed with an asymmetric key, such as a session token in another Go service: the checks, the key set cache, and the errors |
| [Outbound calls](./docs/api/outbound.md) | The client Behemoth calls other services with: its settings, the rules every call follows, and using it in your code |
| [Hooks](./docs/api/hooks.md) | Hook points, handlers, ordering, and failure behavior |
| [Telemetry](./docs/api/telemetry.md) | Loggers, audit, metrics, tracing, and the OpenTelemetry adapter |
| [Command line](./docs/api/cli.md) | `behemoth generate` and `behemoth migrate`: setup, the launcher, its questions, and the draft file |
| [Core tables](./docs/api/core-tables.md) | Writing Behemoth's tables from a plugin |
| [Supported databases](./docs/api/supported-databases.md) | The releases of PostgreSQL, MySQL, SQL Server, MongoDB, Redis, and SQLite that Behemoth supports, and running the tests against another release |
| [MongoDB](./docs/api/mongodb.md) | What the server needs, and creating the indexes a schema declares |

## Contributing

Contributions are welcome. Open an issue to discuss a change before you send a large pull request.

1. Fork the repository, and create a branch from `dev`.
2. Format the code with `gofmt`, and follow the Go Code Review Comments.
3. Add tests for your change, and run `go test ./...`. The `tests` module and each adapter module have their own `go.mod`, so run their tests from their directories. CI runs every module, and the container suites in `tests` need Docker.
4. Update the docs in `docs/` together with the code.
5. Open a pull request against `dev`.

[Internal docs](./docs/internal) explain how the system works.

## License

MIT. See [LICENSE.txt](./LICENSE.txt).
