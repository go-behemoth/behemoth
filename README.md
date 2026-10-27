# Behemoth: Authentication Library for Go ( <img src="./docs/repairing-tools-svgrepo-com.svg" alt="Behemoth is currently under maintenance" width="20"> )

Behemoth adds authentication to a Go application through plugins. You pick the plugins and the database, and Behemoth sets up the tables, the routes, sessions, hooks and telemetry around them.

The project is under active development, and the API may still change.

## Features

- **Plugins.** Each plugin declares its routes, tables, hooks and rate limits. The first one, `emailpassword`, handles sign-up, sign-in, sign-out and password reset. `magiclink` signs a user in with a link sent by email, `emailverification` confirms a user's address, `username` adds sign-in by username to `emailpassword`, and `phonenumber` adds a confirmed phone number, sign-in by one-time code or password, and password reset by code. `twofactor` asks for a second factor at every sign-in of a user who turns one on: a code from an authenticator app, or a code sent by email or text message, with recovery codes. `oauth` signs a user in through an OAuth 2.0 or OpenID Connect provider, such as Google or GitHub, links provider accounts to an existing user, and can keep the provider's tokens for calls to its API.
- **Migrations.** Behemoth builds a migration from the tables your plugins and your own schema declare, and writes it to a folder for you to review. `behemoth generate` does it from your application's directory.
- **Hooks.** Run your code before or after a user is written, or around a whole flow such as sign-up. A handler can change data, stop an operation or react afterwards.
- **Sessions and tokens.** Sessions are stored in your database or a key-value store, and delivered as a cookie or a header.
- **Router adapters.** Mount the routes on `net/http`, Gin, Echo, chi or Fiber.
- **Telemetry.** Logs, audit events, metrics and traces, with an OpenTelemetry adapter.
- **Rate limiting** and **Argon2id password hashing** out of the box.

## Installation

```bash
go get github.com/go-behemoth/behemoth
```

Database and router adapters are separate modules, so you only download what you use:

```bash
go get github.com/go-behemoth/behemoth/storage/adapters/postgres
go get github.com/go-behemoth/behemoth/plugins/adapters/gin
```

The adapter for the standard library's `http.ServeMux` has no dependency of its own, so it comes with Behemoth: import `github.com/go-behemoth/behemoth/plugins/adapters/nethttp`.

## Quick start

Setup has two steps. `Prepare` collects the declarations of your plugins and schema. `Boot` connects them to a database and a router.

```go
import (
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/httpapi"
	ginadapter "github.com/go-behemoth/behemoth/plugins/adapters/gin"
)

plugin := emailpassword.New(emailpassword.Options{})

// 1. Declarations: plugins, hooks, schema.
app, err := engine.Prepare([]core.Plugin{plugin}, engine.PrepareConfig{})
if err != nil {
	log.Fatal(err)
}

// 2. Runtime: storage, routes, sessions.
db := postgres.NewPostgresAdapter(sqlDB, app.Resolver)
engine := gin.New()

ac, err := engine.Boot(ctx, app, db, engine.BootConfig{
	Crypto: cryptoCfg, // the master secret(s) every key is derived from
	HTTP:   ginadapter.New(engine),
	Session: core.SessionConfig{ExpiresIn: 24 * time.Hour},
	// The session token travels in a header and comes back as a bearer token.
	Router: httpapi.RouterConfig{
		Carrier: httpapi.TokenCarrierConfig{Delivery: httpapi.DeliveryHeader},
	},
})
if err != nil {
	log.Fatal(err)
}

engine.Run(":8080")
```

The routes are now live under `/api/auth`:

```bash
curl -X POST localhost:8080/api/auth/sign-up/email \
  -d '{"email":"ada@example.com","password":"correct horse battery"}'
curl -X POST localhost:8080/api/auth/sign-in/email \
  -d '{"email":"ada@example.com","password":"correct horse battery"}'
```

The sign-in response carries the session token where `Delivery` says: here in the `Set-Auth-Token` header, which the client sends back as `Authorization: Bearer <token>`. The default is a cookie.

Without a router, call the same flows from code with `plugin.SignUp` and `plugin.SignIn`.

## Examples

[examples/init](./examples/init) is a complete application on PostgreSQL and Gin. It shows migrations, a custom plugin, hooks, telemetry with OpenTelemetry, and sign-in with GitHub, Google, or another OpenID Connect provider when one is configured in its environment.

```bash
cd examples/init
go run . migrate -confirm   # write the first migration to ./migrations
go run . serve              # Prepare + Boot, then serve HTTP
go run . signup -email ada@example.com -password 'correct horse'
```

`DATABASE_URL` defaults to a local PostgreSQL at `postgres://postgres:postgres@localhost:5432/behemoth`.

[examples/cli](./examples/cli) is a smaller application on PostgreSQL and `net/http` whose migrations are generated with the `behemoth` command line. Its setup lives in one package that the server and the command line share.

```bash
go install ./cmd/behemoth     # the launcher, once, from the repository root
cd examples/cli
behemoth generate -confirm    # write the next migration to ./migrations
psql "$DATABASE_URL" -f migrations/0001_create_accounts_and_8_more.sql
go run .                      # Prepare + Boot, then serve HTTP
```

[Its README](./examples/cli/README.md) has the full steps: Docker, a schema change, answering a question, and `migrate`. To run it with Docker, without Go or a local database:

```bash
cd examples/cli
docker compose up --build     # PostgreSQL, the migration, then the server on :8080
docker compose down -v        # stop, and drop the database
```

## Supported databases

| Kind | Options |
| --- | --- |
| `database/sql` | PostgreSQL, MySQL, SQLite, SQL Server |
| Document | MongoDB |
| ORM | Bun, GORM |
| Key-value (sessions, rate limits) | Redis |

[Supported databases](./docs/api/supported-databases.md) lists the releases of each database that Behemoth supports and tests on.

Migrations can be generated for PostgreSQL, MySQL, SQLite and SQL Server. MongoDB has no migrations: the adapter creates the declared indexes (`EnsureIndexes`), and `Boot` checks them.

## Documentation

| Page | Covers |
| --- | --- |
| [Email and password](./docs/api/emailpassword.md) | The plugin's options, routes and hook points |
| [Username](./docs/api/username.md) | Usernames on top of email and password: the rules, sign-up, sign-in by username and rate limits |
| [Phone number](./docs/api/phonenumber.md) | Confirming a number by one-time code, sign-in by code or password, password reset by code, and rate limits |
| [SMS and one-time codes](./docs/api/sms.md) | The SMS sender plugins use, and the code manager |
| [Magic link](./docs/api/magiclink.md) | Sign-in by emailed link: setup, sending the link, routes, redirects and rate limits |
| [Two-factor authentication](./docs/api/twofactor.md) | A second factor at sign-in: TOTP, codes by email or SMS, recovery codes, the lock, routes and rate limits |
| [OAuth](./docs/api/oauth.md) | Sign-in through an OAuth 2.0 or OpenID Connect provider: providers, the callback page, linking provider accounts, the provider's tokens, routes and rate limits |
| [Email verification](./docs/api/emailverification.md) | Confirming a user's address: when a link is sent, routes, requiring a verified email, and changing an address safely |
| [Mail](./docs/api/mail.md) | The mail sender plugins use, and waiting or background sends |
| [Router adapters](./docs/api/routers.md) | Mounting the routes on `net/http`, Gin, Echo, chi or Fiber, and what is specific to each |
| [Callers and permissions](./docs/api/callers.md) | Who a call runs for: reading the caller, naming your own code, authenticating another transport, and the permissions flows require |
| [Sessions](./docs/api/sessions.md) | Session settings, how the token travels (cookie, header, body), the sign-in response, protecting your own routes |
| [Session tokens](./docs/api/session-tokens.md) | Short-lived JWTs that tell another service who a request is from, and the published keys it checks them with |
| [JWT verification](./docs/api/jwt-verification.md) | Checking a JWT signed with an asymmetric key, such as a session token in another Go service: the checks, the key set cache, and the errors |
| [Outbound calls](./docs/api/outbound.md) | The client Behemoth calls other services with: its settings, the rules every call follows, and using it in your code |
| [Hooks](./docs/api/hooks.md) | Hook points, handlers, ordering and failure behavior |
| [Telemetry](./docs/api/telemetry.md) | Loggers, audit, metrics, tracing and the OpenTelemetry adapter |
| [Command line](./docs/api/cli.md) | `behemoth generate`: setup, the launcher, its questions and the draft file, running it without the launcher |
| [Core tables](./docs/api/core-tables.md) | Writing Behemoth's tables from a plugin |
| [Supported databases](./docs/api/supported-databases.md) | The releases of PostgreSQL, MySQL, SQL Server, MongoDB, Redis and SQLite Behemoth supports, and running the tests against another release |
| [MongoDB](./docs/api/mongodb.md) | What the server needs, and creating the indexes a schema declares |
| [Internal docs](./docs/internal) | How the system works, for contributors |

## Roadmap

Not built yet:

- Password change for a signed-in user

## Benchmarks

Not available yet.

## Contributing

Contributions are welcome. Open an issue to discuss a change before sending a large pull request.

1. Fork the repository and create a branch.
2. Format the code with `gofmt` and follow the Go Code Review Comments.
3. Add tests for your change and run `go test ./...`. The `tests` module and each adapter module have their own `go.mod`, so run their tests from their directories. CI runs every module, and the `tests` module's container suites need Docker.
4. Update the docs in `docs/` together with the code.

## License

MIT. See [LICENSE.txt](./LICENSE.txt).
