# Command line

`behemoth generate` compares the tables your application declares with your database, and writes the migration that makes the database match.

```bash
behemoth generate            # show the next migration
behemoth generate -confirm   # write it to your migrations folder
```

`behemoth migrate` applies it, for applications that let Behemoth apply their migrations.

When a difference has more than one answer, such as a column that becomes shorter, `generate` asks you in your terminal, or takes the answer from `-answer`. See [Questions](#questions).

The command reads your plugins, your schema, and your migration settings from your own code. You don't describe them a second time in a config file.

**Experimental:** the command line is a first version, with a ready-made setup for PostgreSQL and MySQL. See [Limits](#limits) for what it does not cover.

## Setup

The command needs two things from your application: what it declares, and how to reach its database. You give it both as functions.

### 1. Put your setup in a package

Create a file named `behemoth.go` in a package other than `main`, with these two functions:

```go
// internal/auth/behemoth.go
package auth

import (
	"context"

	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/engine"
)

// Prepare returns what the application declares. It opens no connection.
func Prepare() (*engine.PreparedApp, error) {
	plugins := []core.Plugin{emailpassword.New(emailpassword.Options{})}
	return engine.Prepare(plugins, engine.PrepareConfig{
		Migration: migration.MigrationConfig{FolderPath: "migrations"},
		Schema:    declareSchema, // your own tables, if you have any
	})
}

// MigrationBackend opens the database the command compares with.
func MigrationBackend(ctx context.Context, app *engine.PreparedApp) (migration.Backend, error) {
	db, err := OpenDB(ctx) // your own function: sql.Open and a ping
	if err != nil {
		return migration.Backend{}, err
	}
	return postgres.MigrationBackend(db, app.Resolver), nil
}
```

On MySQL, return `mysql.MigrationBackend(db, app.Resolver)` instead, from `github.com/go-behemoth/behemoth/storage/adapters/mysql`. For another database, see [Other databases](#other-databases).

Your server calls the same `Prepare` and passes its result to `Boot`:

```go
app, err := auth.Prepare()
// ...
db := postgres.NewPostgresAdapter(sqlDB, app.Resolver)
ac, err := engine.Boot(ctx, app, db, engine.BootConfig{ /* ... */ })
```

Because both start from one function, the tables a migration creates are the tables your server reads. `MigrationBackend` is the only code you write for the command alone.

Keep `Prepare` free of connections and secrets. The command calls it before it opens the database, and a migration needs none of `Boot`'s settings (crypto, sessions, and mail).

### 2. Install the launcher

```bash
go install github.com/go-behemoth/behemoth/cmd/behemoth@latest
```

### 3. Run it where your application runs

```bash
cd my-app
behemoth generate
```

The command resolves a relative `FolderPath` such as `"migrations"` against the directory you run it in, as your own binary does. If you run it from another directory, it starts a second migrations folder there.

## `generate`

```
$ behemoth generate
Migration 0001 ready (not yet written). Re-run with --confirm to write it to migrations.

0001, 30 operation(s):
  create table accounts
  add index idx_accounts_user_id on accounts
  add index uq_accounts_provider_account on accounts
  ...
  create table users
  add foreign key fk_accounts_user on accounts
  add foreign key fk_notes_user on notes
  add foreign key fk_reserved_values_user on reserved_values
  add foreign key fk_sessions_user on sessions
```

Without `-confirm`, the command writes nothing except the draft file described below. With `-confirm`, it writes two more files to your migrations folder:

| File | Holds |
| --- | --- |
| `0001_create_accounts_and_8_more.json` | The migration as Behemoth records it. Keep it in the folder, because the next migration's number comes from it. |
| `0001_create_accounts_and_8_more.sql` | The same migration as a script for your database. Apply this one with your own tool, such as `psql`. |

The name after the number says what the migration does. Behemoth derives it from the schema operations:

| Migration | Name |
| --- | --- |
| One operation | `0002_add_column_users_plan` |
| Several operations on one table | `0003_alter_notes` |
| New tables only | `0001_create_users_sessions_tokens`, or `0001_create_accounts_and_8_more` beyond three |
| Changes across tables | `0004_update_notes_users` |

Only the number identifies a migration. You can't choose the name.

`generate` never changes your database.

After you apply a script, run `behemoth generate` again. When the database matches your declarations, it answers:

```
No schema changes detected.
```

### Questions

Some differences have more than one answer. If a column becomes shorter in your declaration, Behemoth can alter it, which may reject existing rows, or leave it as it is. `generate` asks you:

```
$ behemoth generate
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

Type the number of an answer, or press Enter for the default. Each answer is saved to `draft.json` in your migrations folder as soon as you give it, so the answers you gave are kept if you stop the command with Ctrl+C. A question that has an answer isn't asked again.

The questions go to stderr and the rest of the output to stdout, so they still show when you redirect the output to a file.

#### Without a terminal

The command asks only when its input is a terminal. In CI, in a script, or in a container started without a TTY, a run with a question that has no answer stops with exit code 1 and lists the question:

```
behemoth generate: 1 question has no answer:

  alter_column_users_plan
  users: Column "plan"'s definition narrows (may reject or truncate existing data): length 32 -> 16.
    "chosen": 0  Apply alter
    "chosen": 1  Leave as-is (default)

Answer in a terminal, with -answer, such as -answer 'alter_column_users_plan=Leave as-is', or by setting "chosen" in migrations/draft.json (and deleting "stale" where it is set). Then run the command again.
```

`-no-input` makes the command behave this way in a terminal too, for a script that you run from one:

```bash
behemoth generate -confirm -no-input
```

#### Answer on the command line

`-answer` answers a question without a terminal, in a script or in CI. Give the question's id and the label of one of its options, and repeat the flag for each question:

```bash
behemoth generate -confirm -answer 'alter_column_users_plan=Apply alter'
```

The answer works like one given at the prompt:

- The label is compared without regard to case.
- The answer is saved to the draft file, so a later run reads it there. It replaces an answer the file held.
- In a terminal, the command asks only the questions that the flags leave open.

The command refuses an answer for a question the run doesn't raise, or with a label that isn't one of the question's options, and exits with code 1:

```
behemoth generate: the answer "Alter" for alter_column_users_plan is not one of its options: "Apply alter", "Leave as-is"
```

A value without `=`, or an id given twice, is a usage error, with exit code 2.

#### The draft file

The draft file holds each question, its options, and its answer. `chosen` is the position of the answer in `options`, counted from 0. It is `null` until the question is answered. `default` is the answer Behemoth suggests, the one Enter picks:

```json
{
  "entries": [
    {
      "id": "alter_column_users_plan",
      "table": "users",
      "description": "Column \"plan\"'s definition narrows (may reject or truncate existing data): length 32 -> 16.",
      "options": ["Apply alter", "Leave as-is"],
      "default": 1,
      "chosen": null
    }
  ]
}
```

To answer without a terminal, set `chosen` and run the command again.

If the options of an answered question change, for example because a new candidate for a rename appears, the entry gets `"stale": true` and a note. In a terminal, the question is asked again. In the file, check `chosen` against the new options, then delete `stale`.

Commit the file with your migrations. A "Leave as-is" answer stays in it, and a run without a terminal, such as one in CI, reads it instead of stopping on the question again.

#### An answer is used once

An answer that changes your database, such as "Apply alter", is used once. When `generate -confirm` writes the migration, it removes that answer from the file and says so:

```
Migration 0002 written to migrations/0002_alter_column_users_plan.json, with its script at migrations/0002_alter_column_users_plan.sql. Hand off the script to your own migration tool. The 1 answer(s) it used were removed from the draft, so a later change to the same column is asked again.
```

A later change to the same column is a new question. A "Leave as-is" answer stays in the file, so a column you chose to leave as it is isn't asked about again.

## `migrate`

`migrate` is for an application whose `MigrationConfig.Path` is `migration.PathManaged`. There Behemoth applies the migrations itself and keeps a record of them in two tables of its own.

```bash
behemoth generate -confirm   # write the next migration, for you to review
behemoth migrate             # show the migration that is waiting
behemoth migrate -confirm    # apply it
```

`generate` works as described above, with one difference: while a written migration is unapplied, it generates no further one and tells you to run `migrate` first.

The `.sql` file is there for you to read. `migrate` applies the migration from the `.json` file, so editing the script changes nothing. If you run the script yourself, Behemoth's record does not include it, and the next `migrate` tries to apply the migration again.

`migrate` never writes a migration. It applies the one that is waiting, and answers `Nothing to apply` when there is none.

On SQLite, MySQL, and SQL Server, some statements depend on the database as it is when they run. If the database changed since you generated the migration, `migrate` applies nothing, writes the script again, and asks you to review it and run `migrate` once more. On PostgreSQL, the script depends on the migration alone, so this does not happen.

If your database already holds some of the declared tables when you start, the first migration `generate` writes is a baseline, `0000_baseline.json`. It describes those tables as they are. `migrate -confirm` records it without changing the database. If the database changes between `generate` and `migrate`, `migrate` refuses the baseline and asks you to generate and review it again.

On the default path, `migrate` fails with an error that names the path. There you apply the files with your own tool.

## How the launcher finds your code

The launcher looks for a file named `behemoth.go` in the current directory and the directories below it. The package that file is in must export `Prepare` and `MigrationBackend`.

It does not look in directories whose name starts with `.` or `_`, in `testdata` or `vendor`, or in a directory that is another Go module.

If your setup is somewhere else, or the search finds more than one, name the directory:

```bash
behemoth -app ./internal/auth generate
BEHEMOTH_APP=./internal/auth behemoth generate
```

`-app` goes before the command.

The launcher then builds a small program inside your module and runs it:

```go
func main() { cli.Main(auth.Prepare, auth.MigrationBackend) }
```

Three things follow from that:

- **It needs the Go toolchain.** `go` must be on your `PATH`, as it is where you develop.
- **Your module decides the version.** The commands that run are those of the Behemoth version in your `go.mod`. Updating the launcher does not change them.
- **Nothing is written into your source tree.** The launcher builds the program in a temporary directory, and the program can still import a package under `internal/`.

## Without the launcher

Write the program yourself, once:

```go
// cmd/behemoth/main.go
package main

import (
	"github.com/go-behemoth/behemoth/cli"

	"example.com/my-app/internal/auth"
)

func main() { cli.Main(auth.Prepare, auth.MigrationBackend) }
```

```bash
go run ./cmd/behemoth generate
```

Use this where there is no Go toolchain, for example when you build the binary into your image, and in a module that vendors its dependencies. The launcher's program is not part of your code, so `go mod vendor` leaves out the `cli` package and the launcher's build fails.

To offer the commands from your application's own binary, call `cli.Run` with the arguments and use its result as the exit code:

```go
if len(os.Args) > 1 && os.Args[1] == "behemoth" {
	os.Exit(cli.Run(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr, auth.Prepare, auth.MigrationBackend))
}
```

## Exit codes

The commands exit with one of these codes:

| Code | Meaning |
| --- | --- |
| 0 | The command ran. It found changes or it found none. |
| 1 | It failed: `Prepare` returned an error, the database could not be reached, a question has no answer and nobody could be asked, an `-answer` was refused, or the migration could not be built. |
| 2 | Unknown command, unknown flag, a malformed `-answer`, or an argument it did not expect. |

The launcher returns the command's own exit code. It returns 1 itself when it cannot find or build your setup.

## Other databases

`postgres.MigrationBackend` and `mysql.MigrationBackend` are ready-made. For SQLite or SQL Server, fill `migration.Backend` yourself from the module's driver. `generate` needs `Introspector`, and `Renderer` for the script. `migrate` also needs `Driver` and `DB`: the module's driver and its adapter:

```go
func MigrationBackend(ctx context.Context, app *engine.PreparedApp) (migration.Backend, error) {
	db, err := OpenDB(ctx)
	if err != nil {
		return migration.Backend{}, err
	}
	driver := sqlserver.NewSQLServerDriver(db, app.Resolver)
	return migration.Backend{Introspector: driver, Renderer: driver, Close: db.Close}, nil
}
```

With GORM or Bun, take the `*sql.DB` your ORM holds and build the backend of the database underneath.

### Apply a MySQL script

A MySQL script holds several statements. The `mysql` client runs them all in one call:

```bash
mysql --host=127.0.0.1 --user=root --password behemoth < migrations/0001_create_accounts_and_8_more.sql
```

From Go, `github.com/go-sql-driver/mysql` runs a script in one `Exec` only when the DSN has `multiStatements=true`. That setting lets one call run several statements, so leave it off the connection your application's queries use, and set it only on the one that applies migrations.

MongoDB has no migrations. See [MongoDB](./mongodb.md).

## Without the command line

`migration.Generate` and `migration.Migrate` are what the commands call. Use them from your own code, for example in a test that fails when a migration is missing:

```go
res, err := migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: false})
// res.Status is migration.StatusNoChanges when the database matches
```

## Limits

The command line has these limits:

- **No vendored modules.** The launcher can't build in a vendored module. Use the hand-written program.
- **Moved files.** Migration numbers come from the files in your migrations folder. If you move a generated `.json` out of it, the next migration gets its number again.
- **Answers while a migration waits.** On the managed path, while a written migration waits for `migrate`, `generate` plans nothing, so it reads no `-answer`. Run `migrate -confirm` first.

The [examples/cli](../../examples/cli) directory holds a complete application set up this way.
