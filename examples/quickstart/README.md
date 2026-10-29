# Quick start example

This example is the smallest Behemoth application: email and password sign-up and sign-in on PostgreSQL and `net/http`. It is the code of the [quick start in the main README](../../README.md#quick-start), and a test keeps the two the same.

| File | Holds |
| --- | --- |
| `main.go` | The server: `Prepare`, `Boot`, and Behemoth's routes on a `net/http` mux. |
| `auth/behemoth.go` | `Prepare`, `OpenDB`, and `MigrationBackend`, which the server and the `behemoth` command line share. |

## Prerequisites

- Go 1.26 or later.
- Docker, for PostgreSQL 15 or later. You can use your own PostgreSQL instead.
- `openssl`, to create the secret.

## Steps

1. Start PostgreSQL, and set the two variables the server reads:

   ```bash
   docker run -d --name behemoth-db -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=behemoth -p 5432:5432 postgres:17
   export DATABASE_URL='postgres://postgres:postgres@localhost:5432/behemoth?sslmode=disable'
   export BEHEMOTH_SECRET=$(openssl rand -hex 32)
   ```

2. Install the `behemoth` command line from the repository root, then create the tables from this directory:

   ```bash
   go install ./cmd/behemoth
   cd examples/quickstart
   behemoth generate -confirm
   behemoth migrate -confirm
   ```

3. Run the server:

   ```bash
   go run .
   ```

4. In another terminal, sign up and sign in:

   ```bash
   curl -X POST localhost:8080/api/auth/sign-up/email \
     -d '{"email":"ada@example.com","password":"correct horse battery"}'
   curl -i -X POST localhost:8080/api/auth/sign-in/email \
     -d '{"email":"ada@example.com","password":"correct horse battery"}'
   ```

5. Stop PostgreSQL: `docker rm -f behemoth-db`.

## Expected result

- `behemoth generate -confirm` answers `Migration 0001 written to migrations/...` and writes the migration to `./migrations`. `behemoth migrate -confirm` answers `Migration 0001 applied.`
- The sign-up answers `201 Created` with the new user in the body.
- The sign-in answers `200 OK`, with the user in the body and the session token in the `Set-Auth-Token` header.

See the [command line](../../docs/api/cli.md) page for the commands, and [Sessions](../../docs/api/sessions.md) for how the token travels and how to protect your own routes.
