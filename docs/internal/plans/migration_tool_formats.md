# Script formats for golang-migrate and goose

This document plans how `behemoth generate`, on the generate-only path, writes each migration in the file layout of golang-migrate or goose, so that an application that applies its migrations with one of those tools takes Behemoth's files as they are. It records what the two tools expect, what changes in Behemoth, the order of the work, and the question left open.

**Status:** planned, not started, as of 2026-10-25.

The documents it builds on are:

- [`../migrations/migration_design_decision.md`](../migrations/migration_design_decision.md): `PathGenerateOnly`, on which Behemoth writes the migration and never changes the database, and the application applies it with its own tool.
- [`../migrations/cli.md`](../migrations/cli.md): the command line that writes the files.
- [`migration_questions.md`](migration_questions.md): the questions and the draft file, which this plan leaves as they are.

## The problem

On the generate-only path, `generate -confirm` writes two files per migration:

- `0002_alter_column_users_plan.json`, Behemoth's record, which the next migration's number comes from
- `0002_alter_column_users_plan.sql`, the up direction as a plain script, with a comment header

An application that applies its migrations with golang-migrate or goose has to rename that script into its tool's layout, and write the down direction by hand. Both tools are common in Go applications, and the statements Behemoth renders are already what they would run. What differs is the layout of the files around the statements.

## What the tools expect

golang-migrate's behavior was checked in its v4.20.1 source, and goose's in the source of its main branch, on 2026-10-25.

| | golang-migrate | goose |
| --- | --- | --- |
| Files per migration | `{version}_{title}.up.sql` and `{version}_{title}.down.sql` | one `{version}_{name}.sql` |
| Files it reads | names matching `^([0-9]+)_(.*)\.(down\|up)\.(.*)$`; it skips the others | `*.sql`, and Go migrations registered in code |
| Version | the number before `_` | the number before `_`, greater than zero |
| Down direction | the `.down.sql` file | the section after `-- +goose Down` |
| Annotations | none | `-- +goose Up` first, then optionally `Down`, `StatementBegin`, `StatementEnd`, `NO TRANSACTION` and `ENVSUB` |
| Running a file | one call with the whole file | split at semicolons at line ends, in one transaction by default |
| A direction with nothing to run | a missing `.down.sql` becomes an empty migration: the version moves without running anything (`newMigration`) | a section without statements runs as `EMPTY`, and the version is still recorded or removed |
| Applied versions | the `schema_migrations` table, with a `dirty` flag after a failure | the `goose_db_version` table |

Running a file differs by database:

| | golang-migrate | goose |
| --- | --- | --- |
| PostgreSQL | one call, which PostgreSQL runs as one implicit transaction; `x-multi-statement` splits it instead | one transaction |
| MySQL | one call, which needs several statements per call: the driver turns `multiStatements` on when it opens the database from a URL, and a connection given to `WithInstance` or `WithConnection` must have it. DDL commits as it runs, so a failure midway leaves earlier statements applied and the version dirty | one transaction, which DDL commits as it runs, so a failure midway leaves earlier statements applied |
| SQLite | one call | one transaction |
| SQL Server | one call, so the whole file is one batch. To test: a column added and then indexed in the same file, which T-SQL may refuse to compile in one batch | one transaction, each statement run on its own |

## What stays the same

- The planner, the questions, the draft file, and the answers a written migration uses up.
- The comparison with the live database. It reads only the declared tables, so the tools' `schema_migrations` and `goose_db_version` are never looked at.
- Custom migrations. They are schema operations, so they render like the generated ones.
- The managed path, which keeps the plain script. See *Generate-only only*.
- Behemoth's `.json` files and `draft.json`, which both tools skip: golang-migrate because the names don't match its pattern, goose because they aren't `.sql`.

## The design

### Statements, then files

`MigrationRenderer.RenderMigration` returns a whole file, a comment header and the statements, with one extension. Every driver already builds the list of statements before it joins them: MySQL's `plan` steps, SQL Server's `run.stmts`, SQLite's dry run.

A driver that can give its statements implements an optional interface next to `MigrationRenderer`, sketched:

```go
// StatementRenderer gives the statements of a migration, for a script
// format to lay out.
type StatementRenderer interface {
	// UpStatements returns the statements that applying m's Up executes,
	// against the database as it is now.
	UpStatements(ctx context.Context, m Migration) ([]string, error)

	// DownStatements returns the statements that undo m: m.Down, rendered
	// against the database as it is after m's Up. reversible is false when
	// m has no Down.
	DownStatements(ctx context.Context, m Migration) (stmts []string, reversible bool, err error)
}
```

The plain script stays what `RenderMigration` writes today.

### The down direction

`Migration.Down` is computed when the migration is generated (`computeDown`), and is all-or-nothing: it is empty when any operation can't be inverted, such as a custom migration without a down part. It is in the `.json` today and is never rendered.

It is rendered against the state after the up direction, which is where it runs. The drivers differ in what they read while rendering:

- **PostgreSQL** reads nothing: its statements depend on the migration alone, so the down direction renders directly.
- **SQL Server and SQLite** already render by running the statements in a transaction they roll back. They run the up direction, render the down direction in the same transaction, and roll back.
- **MySQL** commits DDL, so it can't do the same. It reads the live schema only for the prefix of an index on a `TEXT` or `BLOB` column. The planner already knows the columns a migration creates (`keyPrefixes.known`); before it renders the down direction, it also takes the types the up direction's alters set.

The down direction restores the schema, not the data: a dropped column comes back empty. The header comment of the file says so.

**An irreversible migration fails on purpose.** When `Migration.Down` is empty, the down direction is a statement that names a table that doesn't exist, `SELECT 1 FROM irreversible_migration_0002`, which fails on every database with the migration's ID in the error. An empty down direction would let either tool record a revert that changed nothing, as the table above shows.

### Script formats

A format turns a migration's ID, name, creation time, and up and down statements into files:

| Format | Files |
| --- | --- |
| plain (the default) | `<ID>_<name>.sql`: today's script, unchanged |
| golang-migrate | `<ID>_<name>.up.sql` and `<ID>_<name>.down.sql`, each with the comment header |
| goose | `<ID>_<name>.sql`: the comment header, `-- +goose Up` and its statements, `-- +goose Down` and its statements |

For goose, a statement that spans lines is wrapped in `-- +goose StatementBegin` and `StatementEnd`, since goose splits at semicolons at line ends. A string default that holds a newline does span lines, because MySQL's `quoteLiteral` keeps newlines. `NO TRANSACTION` is not needed: the PostgreSQL driver emits nothing that can't run in a transaction, such as `CREATE INDEX CONCURRENTLY`, and MySQL commits DDL whether or not it runs in one.

The formats are text layout in `migration`, with the standard library only. Nothing imports the tools.

### Writing the files

`renderedDDL` holds one body and one extension, and `migrationDDLPath` builds one path. They take the format's list of files instead. The order of a confirmed run stays: the `.json`, then the format's files, then the answers the migration used are removed from the draft.

In the goose format, the plain `.sql` is not written: goose reads every `.sql` in the folder, and would stop on a file that does not start with `-- +goose Up`.

### Configuration

`MigrationConfig.ScriptFormat`: `""` (plain), `"golang-migrate"`, or `"goose"`. A string, so a configuration file can carry it (`../guidelines/service_ready_components.md`). An unknown value is a configuration error from `Prepare`.

A format other than plain needs a renderer that implements `StatementRenderer`. On a database whose driver does not yet, `generate` refuses with a configuration error that names the driver. Each step below adds a database.

### Messages

The confirmed run says which tool applies the files: "Apply it with golang-migrate: `migrate -path migrations -database URL up`", or the goose command, in place of "Hand off the script to your own migration tool".

## The open question: numbering

Behemoth's IDs are four digits, and the next one comes from Behemoth's own `.json` files (`^(\d{4})_.*\.json$`). Both tools read `0001` as version 1, so a project that starts with Behemoth's files needs nothing.

A project that already has migrations of its tool does. Their versions are often timestamps, `20240115093000`, and a new `0001` sorts below them:

- golang-migrate moves forward from the current version, and never applies a lower one.
- goose refuses an unapplied version below the current one, unless it runs with `-allow-missing`.

Options:

- **Continue after the highest version in the folder,** the `.json` files and the tool's files alike. A four-digit ID can't follow a timestamp, so the ID pattern widens, and `LatestMigrationID` compares IDs as numbers, not as strings.
- **Timestamp IDs,** as an option of `MigrationConfig`. The same widening, and no reading of the tool's files.
- **Document it:** in such a project, the application moves its tool's history aside, or starts Behemoth after it, by hand.

To decide when a project with an existing history adopts a format. Until then the docs state the limit.

## Implementation steps

| Step | Content | Status |
| --- | --- | --- |
| 1 | `StatementRenderer` for PostgreSQL, both directions. The formats, the writing of several files, `MigrationConfig.ScriptFormat` with its checks, the refusal on the managed path and on drivers without statements, the messages. Tests that apply the files with golang-migrate and goose, used as libraries in the `tests` module, against PostgreSQL: up, `generate` finds no changes, down, up again, and the irreversible down failing. `docs/api/cli.md` and the internal docs. | Not started |
| 2 | `StatementRenderer` for SQL Server, SQLite and MySQL, with the down direction as described. The same tests against each, including the SQL Server batch question. The per-database notes in `docs/api/cli.md`. | Not started |
| 3 | Numbering in a project with an existing history. | To decide |

## Design decisions

### Text layouts in core, the tools in the tests

**Context:** a format has to produce files a tool accepts.

**Options considered:**

- *A module per tool that imports it, and checks or writes the files through its own code.* The tool's parser would confirm each file. Each format would carry the tool's dependencies, and its release would follow the tool's.
- *The layouts as text in `migration`, and the tools only in the tests.* No dependency for applications. The tests confirm the files are accepted, so a tool that changes its rules fails a test, not an application.

**Decision:** the text layouts, with the tools in the `tests` module, which has its own `go.mod`.

### Down fails on purpose when a migration is irreversible

**Context:** `Migration.Down` is empty when a migration can't be undone. Both tools treat an empty or missing down direction as a success that only moves the version.

**Options considered:**

- *No down file, or an empty section.* The tool records a revert that changed nothing, and the next up skips the migration, which is still in the schema.
- *A comment only.* The same, with a note nobody reads while the command succeeds.
- *A statement that fails, naming the migration.* The revert stops at that version, with an error that says which migration it was.

**Decision:** the failing statement.

**Cost:** golang-migrate marks the version dirty after the failure, and the developer clears it with `migrate force` once they have undone the migration by hand. goose leaves the version applied.

### Generate-only only

**Context:** on the managed path, `migrate` applies the migration from its `.json` and first compares the plain script with a fresh rendering (`scriptWentStale`).

**Decision:** the managed path keeps the plain script, and a format is a configuration error there. Files of a tool in the same folder would invite applying a migration twice, once by `migrate` and once by the tool.

**Revisit if:** an application wants Behemoth to apply its migrations and keep a tool's files for another environment. Writing the tool's files to another folder would be the start.

### A format is available where its statements are

**Context:** the down direction needs work per database, and that work comes in steps.

**Options considered:**

- *Every database at once.* Nothing ships until the hardest case, MySQL's down direction, is done.
- *The up direction only where the down direction isn't built.* A down file that fails as irreversible on a migration that can be undone is a wrong statement about it.
- *The format refused on a database whose driver can't give both directions yet.* Each step adds databases, and none is half supported.

**Decision:** refused until the driver implements `StatementRenderer`.

## Where the code lives

Planned:

| Piece | Code |
| --- | --- |
| `StatementRenderer` | `migration/driver.go`, next to `MigrationRenderer` |
| the formats | `migration/script_format.go` |
| several files per migration | `migration/generator.go` (`renderedDDL`, `migrationDDLPath`) |
| `MigrationConfig.ScriptFormat` and its checks | `migration/types.go` (`MigrationConfig`), `migration/migration.go` (`NewMigrationConfig`), `engine/init.go` (`Prepare`) |
| the statements of each database | `storage/adapters/postgres`, `sqlserver`, `sqlite`, `mysql` |
| the tests with the tools | `tests/storage/migrations/` |
