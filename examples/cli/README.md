# Command line example

This example is a small application on PostgreSQL and `net/http` whose migrations are written by the `behemoth` command line. Use it to see `behemoth generate` and `behemoth migrate` work against a real database.

The server and the command line start from the same function, `auth.Prepare`, so a migration creates the tables the server reads. The example applies its migrations with its own tool, `psql`: `behemoth generate` writes each migration as a `.sql` script, and Behemoth never changes the database itself. This is the default migration path, `migration.PathGenerateOnly`. [Try `migrate` on the managed path](#try-migrate-on-the-managed-path) shows the other one.

## What is in the example

| File | Holds |
| --- | --- |
| `main.go` | The server: `Prepare`, `Boot`, and Behemoth's routes on a `net/http` mux. |
| `auth/behemoth.go` | `Prepare`, `OpenDB`, and `MigrationBackend`, which the server and the command line share. |
| `auth/schema.go` | The application's own schema: a `plan` column on `users`, and a `notes` table. |
| `cmd/behemoth/main.go` | The command line, written by hand, for where there is no Go toolchain. |
| `Dockerfile`, `compose.yaml`, `migrate.sh` | The Docker setup. |

## Run it with Docker

You need Docker with Compose. You don't need Go or a local PostgreSQL.

```bash
cd examples/cli
docker compose up --build -d
```

Compose starts three services:

1. `db`: PostgreSQL 18.
2. `migrate`: a job that runs `behemoth generate -confirm`, applies the script it wrote with `psql`, and exits.
3. `server`: the application on port 8080. It starts once the job has succeeded.

Read the job's output, then sign up and sign in:

```bash
docker compose logs migrate

curl -X POST localhost:8080/api/auth/sign-up/email -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct horse battery"}'

curl -i -X POST localhost:8080/api/auth/sign-in/email -H 'Content-Type: application/json' \
  -d '{"email":"ada@example.com","password":"correct horse battery"}'
```

The sign-in response carries the session token in its `Set-Auth-Token` header.

Stop the application:

```bash
docker compose down      # keeps the database and the migrations
docker compose down -v   # also deletes them
```

Two settings are read from your environment, or from a `.env` file next to `compose.yaml`:

- `PORT` is the port on your machine. The default is `8080`.
- `BEHEMOTH_SECRET` keeps sessions valid across restarts. Without it, the server makes a random secret each time it starts. Create one with `echo "BEHEMOTH_SECRET=$(openssl rand -hex 32)" > .env`.

The migrations are in a Docker volume, not in the `migrations` folder on your machine. To read them:

```bash
docker compose run --rm --entrypoint ls migrate migrations
docker compose run --rm --entrypoint cat migrate migrations/0001_create_accounts_and_8_more.sql
```

## Run it without Docker

You need Go 1.26, and PostgreSQL 15 or later with a database named `behemoth`. `DATABASE_URL` names the database. It defaults to `postgres://postgres:postgres@localhost:5432/behemoth?sslmode=disable`.

```bash
go install ./cmd/behemoth     # the launcher, once, from the repository root
cd examples/cli
behemoth generate             # show the first migration
behemoth generate -confirm    # write it to ./migrations
psql "$DATABASE_URL" -f migrations/0001_create_accounts_and_8_more.sql
go run .                      # the server, on :8080
```

Without the launcher, run the command that `cmd/behemoth` holds: `go run ./cmd/behemoth generate`.

## Change the schema

`auth/schema.go` declares the application's tables. Add a column to `notes`:

```go
{Name: "body", Type: schema.ColTypeText},
{Name: "title", Type: schema.ColTypeString, Length: 200, Nullable: true},
```

Make a new column nullable, or give it a `Default`. A `NOT NULL` column without a default can't be added to a table that has rows.

The schema is compiled into the images, so build them again:

```bash
docker compose run --rm --build --entrypoint behemoth migrate generate   # preview; writes nothing
docker compose up --build -d                                             # write the migration, apply it, restart the server
docker compose logs migrate
docker compose exec db psql -U postgres -d behemoth -c '\d notes'
```

The job logs `Migration 0002 written to migrations/0002_add_column_notes_title.json`, then `Applying migrations/0002_add_column_notes_title.sql`. Run `docker compose up -d` again without a change, and it logs `No schema changes detected.`

Without Docker:

```bash
behemoth generate -confirm
psql "$DATABASE_URL" -f migrations/0002_add_column_notes_title.sql
```

## Answer a question

Some changes need your decision. A narrower column can reject or cut existing values, so Behemoth doesn't alter it unless you choose to. Narrow `plan` from 32 to 16 characters in `auth/schema.go`:

```go
Type: schema.ColTypeString, Length: 16, Default: "free",
```

The command asks you when it runs in a terminal, and `docker compose run` gives it one:

```bash
docker compose run --rm --build --entrypoint behemoth migrate generate
```

```
1 question needs an answer. Each answer is saved to migrations/draft.json as you give it.

Question 1 of 1 (alter_column_users_plan)
users: Column "plan"'s definition narrows (may reject or truncate existing data): length 32 -> 16.

  1) Apply alter
  2) Leave as-is (default)
Answer [2]: 1

Migration 0002 ready (not yet written). Re-run with --confirm to write it to migrations.

0002, 1 operation(s):
  alter column users.plan

Answers read from migrations/draft.json:
  alter_column_users_plan: Apply alter
To change an answer, edit the file and run the command again.
```

Type the number of an answer, or press Enter for the default. The answer is saved in `migrations/draft.json`, in the volume. Run the job, and it writes and applies the migration with that answer:

```bash
docker compose up --build -d
docker compose logs migrate
docker compose exec db psql -U postgres -d behemoth -c '\d users'
```

The job writes and applies `0002_alter_column_users_plan`, and `plan` becomes `character varying(16)`. Its output ends with `The 1 answer(s) it used were removed from the draft`. Narrow `plan` again, to 8 for example, and you are asked again. Press Enter for "Leave as-is", and the job reports `No schema changes detected.` and names the answer it read. A "Leave as-is" answer stays in the draft, so a column you chose to leave as it is isn't asked about again.

Without Docker, run `behemoth generate` in your terminal to answer, then `behemoth generate -confirm`, and apply the script with `psql`.

### Without a terminal

The job that `docker compose up` runs has no terminal. If you run `up` before you answer, the job stops and lists the question, with the `"chosen"` value of each option:

```
behemoth generate: 1 question has no answer:

  alter_column_users_plan
  users: Column "plan"'s definition narrows (may reject or truncate existing data): length 32 -> 16.
    "chosen": 0  Apply alter
    "chosen": 1  Leave as-is (default)

Answer in a terminal, with -answer, such as -answer 'alter_column_users_plan=Leave as-is', or by setting "chosen" in migrations/draft.json (and deleting "stale" where it is set). Then run the command again.
```

The server stays stopped until the job succeeds. Answer the question, then run `docker compose up -d` again.

To answer on the command line, give `-answer` the question's id and the label of one of its options. The job passes its arguments to `generate`, so one run of it answers, writes the migration, and applies it:

```bash
docker compose run --rm --build migrate -answer 'alter_column_users_plan=Apply alter'
docker compose up --build -d
```

The second command starts the server. Its job finds no change left to make.

You can also answer in the draft file. `chosen` is the position of the answer in `options`, counted from 0, and is `null` until the question is answered. `default` is the answer Enter would pick:

```json
{
  "entries": [
    {
      "id": "alter_column_users_plan",
      "table": "users",
      "description": "Column \"plan\"'s definition narrows (may reject or truncate existing data): length 32 -> 16.",
      "options": [
        "Apply alter",
        "Leave as-is"
      ],
      "default": 1,
      "chosen": null
    }
  ]
}
```

The file is in the volume. Open it in `vi`:

```bash
docker compose run --rm --entrypoint vi migrate migrations/draft.json
```

Or answer with one command. This answers every open question in the file with its first option, so use it only while the file holds one question:

```bash
docker compose run --rm --entrypoint sed migrate -i 's/"chosen": null/"chosen": 0/' migrations/draft.json
```

## The commands

| Command | What it does |
| --- | --- |
| `behemoth generate` | Compares your declarations with the database and shows the next migration. It writes only the draft file. |
| `behemoth generate -confirm` | Writes the migration to `migrations`: a `.json` file that Behemoth reads, and a `.sql` script. |
| `behemoth migrate` | Shows the migration that is waiting to be applied. Managed path only. |
| `behemoth migrate -confirm` | Applies that migration and records it. Managed path only. |

`generate` never changes the database. In a terminal, it asks the questions a change raises, and `-no-input` turns that off. `-answer ID=LABEL` answers a question on the command line. In Docker, run a command with `docker compose run --rm --entrypoint behemoth migrate COMMAND`, where `COMMAND` is `generate`, `migrate`, and their flags.

This example is on the generate-only path, so `migrate` refuses:

```
behemoth migrate: MigrationConfig.Path is "generate_only": this application applies its migrations with its own tool, from the files "behemoth generate" writes. Set the path to "managed" to have behemoth apply them
```

### Try `migrate` on the managed path

On the managed path, Behemoth applies the migrations itself and records them in two tables of its own, `behemoth_auth_schema` and `behemoth_auth_schema_snapshot`.

1. Set the path in `auth/behemoth.go`:

   ```go
   Migration: migration.MigrationConfig{
   	FolderPath: "migrations",
   	Path:       migration.PathManaged,
   },
   ```

2. Start from an empty database, and run the commands yourself:

   ```bash
   docker compose down -v
   docker compose build
   docker compose up -d db
   docker compose run --rm --entrypoint behemoth migrate generate -confirm   # writes 0001
   docker compose run --rm --entrypoint behemoth migrate migrate             # shows 0001
   docker compose run --rm --entrypoint behemoth migrate migrate -confirm    # applies 0001
   docker compose up -d
   ```

   The commands answer `Migration 0001 written to ... Review it, then run migrate to apply it.`, `Migration 0001 is ready. Re-run with --confirm to apply it.`, and `Migration 0001 applied.` The job then finds no change, and the server starts.

After each later schema change, run `generate -confirm` and `migrate -confirm` before `docker compose up`. The job's `psql` step is for the generate-only path: if it finds a change, it applies the script without Behemoth's record, and the next `migrate` tries to apply the migration again.

Without Docker, the commands are `behemoth generate -confirm`, `behemoth migrate`, and `behemoth migrate -confirm`. On a database that already holds the tables, the first migration is a baseline that records them; see [Command line](../../docs/api/cli.md#migrate).

## Learn more

- [Command line](../../docs/api/cli.md): every command, its questions and the draft file, and the launcher.
- [Email and password](../../docs/api/emailpassword.md): the routes the server mounts.
