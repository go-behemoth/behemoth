
# Schema migration
This section explains what behemoth does about schema definition migration. Any information on how to migrate existing data to behemoths structure should be put in the *Data Migration* topic.

## Two scenarios, based on scope
When it comes to schema migration, there are two clear cut scenarios. One with execution and the other without. 

Why based on scope and not on greenfield versus brownfield: both scenarios can start from an empty database or from existing data, and both deal with **initial integration** rather than the **lifetime flow** of the component. Greenfield and brownfield are therefore not a good basis for the architecture.

## Path I: diff, plan, generate, apply

When behemoth is responsible for generation and execution of the schema migrations. Even though this might be rarely the case, sometimes users might have a backend service that handles only authentication logic and user management. It's better to have the full capability instead of forcing users to introduce separate tools (like goose, golang-migrate)  

## Path II: diff, plan, generate

For common cases and larger services, users use dedicated tools that manage and apply migrations. In this case it's best to stop at generation and handing the rest to the tool. Since multiple modules editing and executing migration data is a bad design that leads to inconsistency and multiple failure points. 
Behemoth might provide tool-specific generation (for goose or golang-migrate for for example,) but as a separate packaging that's optionally installed instead of crammed in the core package.


## Inputs, packages, and driver capabilities

### Where things live

| Package | Holds |
| --- | --- |
| `schema` | The declaration vocabulary shared across packages: `schema.Table`, `Column`, `Index`, `ForeignKey`, `ColumnOverride`, the `ColumnType` and `FK*` action constants, the contribution types, and `schema.Registry` (`DefaultRegistry`, `NewRegistry`). `[Convention, important]` It must never import `core`: it sits below it so `PluginInitContext` can carry a registry without an import cycle. |
| `migration` | Everything about migrating: operations, `Migration`, `MigrationConfig`, `CustomMigration`, introspection/diff, planning, resolution, generation, the runner, `BuildSchemaResolverTable`, the driver capability interfaces below, and `Backend` with `Generate` and `Migrate`, which bundle those capabilities and pick the entry point by path (see *Entry points*). |
| `storage/adapters/postgres`, `storage/adapters/sqlite`, `storage/adapters/mysql`, `storage/adapters/sqlserver` | One module per database, each holding the `behemoth.Database` adapter **and** the migration driver, built with the same `SchemaResolver` so application queries and migrations agree on physical names. SQLite is its own module so only applications using it link `go-sqlite3` (and therefore cgo). The Postgres module also has `MigrationBackend`, which returns both as one `migration.Backend`. |
| `cli`, `cmd/behemoth` | The command line: the commands as a library an application links, and the launcher that builds that link for it. Described in [`cli.md`](cli.md). |

### Declaration

Tables are declared once, during `Prepare` (`engine`), into a single registry. Each declarer receives a scoped registry that stamps its own name as `Owner` (a declarer can never misattribute a table) and refuses `Freeze`:

1. **core**: `CoreDeclareSchema(ic)`, owner `"core"`.
2. **each plugin**, in dependency order: inside its existing `Declare(ic)`, through `ic.Schemas`, owner = plugin name. There is no separate schema interface: tables are declared next to hooks, tokens, and rate limits.
3. **the application**: `PrepareConfig.Schema`, owner `"app"`, last, so it can also extend plugin tables.

`Prepare` then freezes the registry and derives the `SchemaResolver` from it (`BuildSchemaResolverTable`). `[Convention, important]` The resolver is therefore complete **before** any adapter or migration driver exists; the application builds both with `PreparedApp.Resolver`. Migration tooling calls `Prepare` only: it never needs `Boot` or a running application.

Every migration entry point (`Generate`, `Migrate`, and `RunGenerateCLI`, `RunManagedGenerate`, `RunManagedApply` below them) takes the declaration as one value, `migration.Declared{Schemas, Custom}`, which `PreparedApp.Declared()` builds (see *Custom Migrations*). Introspection and baselining use only `Declared.Schemas`.

### Entry points

```go
func Generate(ctx context.Context, cfg MigrationConfig, declared Declared, backend Backend, opts RunOptions) (*RunResult, error)
func Migrate(ctx context.Context, cfg MigrationConfig, declared Declared, backend Backend, opts RunOptions) (*RunResult, error)
```

These two are the entry points for an application or the command line. They follow what a developer does: `Generate` writes the next migration and never changes the database, and `Migrate` applies what `Generate` wrote.

| | `PathGenerateOnly` | `PathManaged` |
| --- | --- | --- |
| `Generate` | `RunGenerateCLI`: compares with the live database | `RunManagedGenerate`: compares with the snapshot |
| `Migrate` | configuration error: the application applies the files with its own tool | `RunManagedApply` |
| Needs from `Backend` | `Introspector`; `Renderer` for a script | `Introspector`, `Driver` and `DB`; `Renderer` likewise |

`opts.Confirm` means the same on every cell: without it the call reports what it would do, with it `Generate` writes the file and `Migrate` applies it.

`Backend` is one database's side of a run in one value. A dialect module returns it from one constructor (`postgres.MigrationBackend(db, resolver)`), so the caller no longer passes the same driver three times and builds the runner itself. `core` still imports no database: the fields are its own interfaces.

Both build what the paths share and the caller used to wire by hand: the `FilePresenter` on `DraftPath(cfg)` (`draft.json` inside `FolderPath`), the `DefaultMigrationGenerator` and, for `PathManaged`, the `MigrationRunner`. One presenter serves both the baseline and the generation after it, as `MigrationDeps` requires.

`[Convention]` Both return a configuration error, before they create the folder or read the database, when `Declared.Schemas` or `Backend.Introspector` is nil, when `PathManaged` lacks `Driver` or `DB`, or when the path is unknown.

`[Convention]` `RunGenerateCLI`, `RunManagedGenerate`, and `RunManagedApply` stay exported for a caller that brings its own presenter, generator, or runner. Each refuses another path's configuration (`checkPath`). Before that check, `RunGenerateCLI` with a managed configuration dereferenced the nil `Runner`.

### Generating and applying are separate entry points on the managed path

**Context:** the managed path used to have one entry point, `RunMigration`, driven by one `confirm` flag. A run wrote the next migration, and the next confirmed run applied it. What a call did depended on the state it found, so the same command with the same flag sometimes wrote a file and sometimes changed the schema.

**Options considered:**

- *Keep one entry point and one command.* Nothing to change. A developer cannot ask for one of the two things, and a command named after either half misdescribes the other.
- *Two entry points.* `generate` means the same on both paths, and `migrate` can refuse an application on `PathGenerateOnly` outright.

**Decision:** two. `RunManagedGenerate` writes and never touches the database. `RunManagedApply` applies or records and never writes a file. The rule that a migration is reviewed before it is applied is now structural: no single call does both. `Generate` also previews on the managed path now, as on the other: before, a managed run wrote its file without being asked to confirm.

**Revisit if:** a one-shot mode for CI is wanted. It would be a third call that composes the two.

### Driver capabilities

| Interface | Required | Purpose |
| --- | --- | --- |
| `SchemaDriver` | for Path I | `ApplyMigration`, `RecordBaseline`, `AtomicityLevel`. |
| `SchemaIntrospector` | yes | `TableExists`, `Introspect`: reverse-maps a live table into the canonical model. |
| `MigrationRenderer` | optional | Renders a migration as a script (`.sql`) written next to its `.json`. It must be rendered against the state the migration will be applied to. |
| `ColumnNormalizer` | optional; required in practice for any driver whose DDL loses information | Describes how the driver's DDL changes a declared column, so an unchanged column compares equal. See *Column Comparison*. |

Optional capabilities are discovered by type assertion. The Postgres, SQLite, MySQL, and SQL Server drivers implement all four, and each declares that at compile time.

`[Implementation Detail]` Rendering: Postgres builds every statement with pure SQL builders, so the script is by construction exactly what `ApplyMigration` executes. SQLite's table rebuilds read the live schema, so its renderer executes `Up` in a transaction that is always rolled back and records every statement. A baseline renders the existing definitions of the tables it records, since its `Up` is never executed. MySQL sits between the two: `ApplyMigration` and `RenderMigration` share one planner whose builders are pure except for an index on a `TEXT`/`BLOB` column, where the column's type is read from the live table unless the migration itself defines the column (see *MySQL Driver*). SQL Server renders the way SQLite does, by applying `Up` in a transaction that is rolled back, because most of its operations read the catalog (see *SQL Server Driver*).


## Introspection and diff stage: common to both scenarios

In this stage, the mapped schema registry is compared against the live database. This stage produces a structured description of where the live database's shape disagrees with the declared `schema.Registry` (`Declared.Schemas`).

Roughly:
Introspect -> Diff -> Report Ambiguities 

### Pre-step
Before any comparison happens, every declared table/column is resolved to its `PhysicalName`. All comparison below happens in **physical (DB-side) identifier space**. 
`[Convention]` The Introspection Report itself must translate physical identifiers back to canonical `Name` before being shown to a human.

### Note on extra entities (columns, indexes, and foreign keys)
Extra entities are handled as follows:

- `[Convention]` Behemoth will not and should not manage any extra column, index, or foreign key unless the migration management is set to `PathManaged`(Path I).
- Extra objects found live are not tracked unless the `trackExtraColumns` flag is true. The flag is set to true for `PathManaged` and the objects are subject to drop operations.
#### Branch: table level

A table is handled as follows:

- **Declared table missing live** -> fresh table; queued as a create candidate.
- **Declared table found live** -> proceed to Column/Index/Foreign-Key branches below.
- **Non-declared extra table found live** → ignored entirely. 
	- `[Convention]` Behemoth only ever manages what is declared; an extra table is invisible to this pipeline by design.
- **Declared table exists live as an incompatible object** (for example it's a VIEW, not a TABLE) → 
	- `[Advanced]` treated as a hard ambiguity, surfaced as a blocking error rather than silently attempting to replace it.
	- `[Deferred]` No automatic handling in this phase. A manual intervention is required.

#### Branch: column level (within a table found live)

A column is handled as follows:

- **Declared column found live, definition matches** (every field `columnsEqual` compares, after the driver normalizes the declaration; see *Column Comparison*) → no action.
- **Declared column found live, definition differs** → divergence recorded (candidate alter). `[Implementation Detail]` Whether this is auto-safe or requires confirmation is decided by the Plan stage.
- **Declared column missing live** → candidate add, _unless_ it pairs with an extra live column below (see Rename Detection).
- **Non-declared extra column found live** → no action. 
	- `[Convention]` The developer's own custom columns are left untouched, provided the owning `Model` implements `Serializable`. Behemoth will persist what it's given without requiring exclusive ownership of every column.
	
- **Rename Detection** (a "declared missing" + "extra live" pair, matched by structural signature that is, type/length/nullable/primary-key/unique equivalence, using the **normalized** declaration) → always surfaced as an **unresolved ambiguity requiring explicit confirmation**. `[Convention, important]` An extra live column whose type couldn't be mapped never takes part in rename matching.

	- **Multiple equally-plausible candidates for one column** → no pairing is guessed; every involved column falls back to independent add/drop candidates, each still individually flagged for review. `[Implementation Detail]`

#### Branch: type reverse-mapping ambiguity (column level)

When the live database column type have no direct mapping to behemoth's list of supported column types, a type mapping ambiguity arises. This is out of scope for behemoth to handle, since it's not a comprehensive migration engine that's able to map every existing database types. The introspector's type support can grow over time as the need and relevance dictate, but that doesn't guarantee a complete coverage for all databases.

For a column of such a type, see *Custom User Models → Scenario 2*: leave it undeclared if behemoth doesn't need it, or declare it with a type the driver can read and write and give its `schema.Field` a codec. (Custom models, previously advised here, are removed.)

- `[Convention, important]` The introspector reports such a column as `Text` with a `ColumnAmbiguity`, and `RejectAmbiguousTypes` **stops generation** (both paths): there is no option to offer for a type nobody can determine. Each driver maps the type names it renders plus common SQL names (SQLite: `INT`, `CHAR(n)`, `DOUBLE`, `DECIMAL(p,s)`, `BOOL`, ...) and flags everything else (Postgres: enums, `POINT`, ...; SQLite: `DATE`, a column with no declared type). SQLite deliberately does **not** fall back on its type-affinity rules: an affinity says how values are stored, not what the column means (`DATE` gets NUMERIC affinity).
- `[Convention]` A **lossy but determinable** mapping is not an ambiguity. SQLite's `BLOB` (uuid, blob, or bytes) and `TEXT` (text or json) read back as `blob` and `text`; the column is fully usable as the type reported, and `ColumnNormalizer` makes the declaration compare equal (see *Column Comparison*).

#### Branch: index level (within a table found live)

Mirrors Column Level exactly: 
**Declared Index found live** → no action; 
**Declared Index found live, definition differs** (columns or uniqueness disagree) → divergence; 
**Declared Index missing live** → add candidate (additive, safe); 
**Non-declared extra Index found live** → no action, 
`[Convention]` The same rule for extra columns/tables apply here.

#### Branch: foreign key level (within a table found live)

Same four sub-cases as Index Level: 
**Declared Foreign Key found live** → no action; 
**Declared Foreign Key found live, definition differs** (RefTable/RefColumns/OnDelete disagree ) → divergence; 
**Declared Foreign Key missing live** → add candidate (additive, safe); 
**Non-declared extra Foreign Key found live** → no action, 

### Output of this stage

One Introspection Report: per table, per column/index/FK  matches, divergences, and ambiguities, all keyed by canonical name. This report is an input to the Plan stage.

Notes
- `[Convention, important]` Since introspection is now required for _both_ paths, `SchemaIntrospector` is a **required** driver capability for migration support.
- `[Convention]` Findings come out in declaration order (then live order for extras), so the report (and every baseline built from it) is identical across runs.
- `[Implementation Detail]` SQLite specifics: structure comes from `PRAGMA table_info`, `index_list`/`index_info` and `foreign_key_list`; what the pragmas don't report (foreign-key constraint names and `AUTOINCREMENT`) is read from the table's stored `CREATE TABLE` text with the same parser table rebuilds use. Partial and expression indexes are skipped (the canonical `Index` can't express them); a composite `UNIQUE` constraint keeps its constraint name; `REFERENCES parent` without columns resolves to the parent's primary key; an unnamed foreign key keeps an empty name. Views and indexes sharing the table's name are reported as incompatible objects.


### Introspection frequency by path

Each path introspects at a different rate:

- **Path I** → Introspection required **once**, at onboarding (Baseline/Adopt) only. After baseline, behemoth's own ledger + snapshot are authoritative for "previous," and standing `generate` calls diff the canonical-snapshot-vs-canonical-current without a live DB introspection. `[Convention]`
- **Path II** → Introspection required **on every `generate` call**, since behemoth never applies anything and therefore never owns a ledger telling it what's actually live. Live reality is the only trustworthy "previous." `[Convention, important]`

#### `[Advanced]` avoiding introspection on every call, for Path II

**Proposed mechanism: trusted, fingerprint-verified snapshot cache**, not a way to skip verification entirely:

- A one-time explicit command (for example `behemoth schema snapshot`) performs a real introspection and writes the result to a file the developer commits to their repository (`.behemoth/schema-cache.json`). The file serves as a **cache**, and behemoth does not write to it automatically.
- On a subsequent `generate` call, behemoth runs one **cheap fingerprint check** against the live database first: table names, column counts, a lightweight checksum. If the fingerprint matches the cached file's recorded fingerprint, the cached snapshot is trusted as "previous" and full introspection is skipped. If it doesn't match, behemoth **automatically falls back to full introspection** and a warning is issued stating that the cache is stale.
- `[Convention, important]` The cache never silently allows a drift. A mismatch always forces the safe path. A developer can force full introspection regardless of a matching fingerprint via an explicit flag, but there is no flag to force _trusting_ a cache despite a fingerprint mismatch. That direction of override is not offered, not to risk a data loss.
- `[Implementation Detail]` Fingerprint composition (which fast facts are cheap enough to check on every call versus worth caching) is left open for the implementation pass.

**Driver without introspection capability** → `[Deferred]` fall back to the previously-designed pure canonical-snapshot diffing for that driver only, as a reduced-capability mode, rather than refusing migration support outright. Not designed in this pass.

## Column comparison: normalization, equality, and narrowing

This section defines when a declared column and a live (or previously recorded) column are **the same**, and, when they are not, whether altering one into the other is **narrowing**. Both answers drive Planning: a match produces nothing, a widening difference an automatic `OpAlterColumn`, a narrowing one a confirmation issue.

### The problem normalization solves

A driver's DDL cannot always preserve every distinction a declaration makes. The introspector can only report what the database stored, so a column read back after the driver created it may legitimately differ from its declaration:

| Declared | Created as | Read back as |
| --- | --- | --- |
| `uuid`, `bytes` (SQLite) | `BLOB` | `blob` |
| `json` (SQLite) | `TEXT` | `text` |
| `blob` (Postgres) | `BYTEA` | `bytes` |
| `bytes` (MySQL) | `LONGBLOB` | `blob` |
| `uuid` (MySQL, SQL Server) | `CHAR(36)` / `NCHAR(36)` | `string(36)` |
| `json` (SQL Server) | `NVARCHAR(MAX)` | `text` |
| `bytes` (SQL Server) | `VARBINARY(MAX)` | `blob` |
| `string` longer than 4000 (SQL Server) | `NVARCHAR(MAX)` | `text` |
| `timestamp` (MySQL) | `DATETIME(6)` | `datetime` |
| `string`, no length (all) | `VARCHAR(255)` | `string(255)` |
| `text` with a `Length` (all) | `TEXT` (`LONGTEXT` on MySQL) | `text`, no length |
| `bigint` + `AutoInc` (SQLite) | `INTEGER PRIMARY KEY AUTOINCREMENT` | `integer` |
| primary key declared nullable / unique (all) | `NOT NULL`, no separate `UNIQUE` | not nullable, not unique |
| default `-1` on an integer (Postgres) | `'-1'::integer` | literal `-1` only if parsed with the column type in mind |

Compared naively, every such column differs on **every** run, and the planner proposes changing it into what it already is (on SQLite, a table rebuild).

### `ColumnNormalizer`

```go
type ColumnNormalizer interface {
	// NormalizeColumn returns col as Introspect would report it after the
	// driver created it in table (canonical name).
	NormalizeColumn(table string, col schema.Column) schema.Column
}
```

It is an optional capability of the `SchemaIntrospector`, discovered by type assertion. The question it answers is *"if I create this declared column and read it back, what do I get?"*: the same lossy mapping the DDL performs, applied in advance.

#### Where core applies it

Core applies normalization in these places:

- **Equality in `RunIntrospection`** (Path II, and Path I's baseline): `diffColumns` compares `normalize(declared)` against the live column.
- **Rename matching** (`matchRenameCandidates`): structural signatures compare the normalized declaration, so a renamed blob column still pairs with its live bytes counterpart.
- **Narrowing:** every finding with both sides carries `ColumnFinding.Normalized`, and the planner judges narrowing on it (see below).
- **Not** in snapshot diffs (`RunIntrospectionFromSnapshotDiff`, Path I after baseline): both sides there are declarations, so there is nothing to normalize; `nil` is passed.

`[Convention, important]` Normalization only decides **whether** something changed. Findings always carry the original declaration in `ColumnFinding.Declared`, and every operation (`OpAddColumn`, `OpAlterColumn`, rename) is built from it, so the renderer receives exactly what was declared.

#### Baseline recording

A baseline (Path I, brownfield) records tables into the snapshot that every later snapshot diff compares declarations against. `[Convention, important]` For a column that **matched**, the baseline records the **declaration**, not the live column: the two only matched up to normalization, and recording the live shape (for example `bytes` for a declared `blob`) would make the column differ on every run after the baseline. Columns that differ, and undeclared live columns, are recorded as found live.

#### Contract for implementers

An implementation follows these rules:

- **Replay the renderer, nothing else.** Every rule must correspond to something the driver's own DDL does. A rule that "helps a comparison pass" without the DDL doing it hides real changes.
- **Share code with the introspector wherever both sides go through the same transformation.** Defaults are the strongest example: the normalizer runs the declared default through the driver's renderer and then through the **same parser** the introspector uses (`pgDefaultFromStored`, `sqliteDefaultFromStored`), so the two cannot drift apart.
- **Apply the driver's own override first** (`Overrides[driver].Type`, `.AutoInc`, `.Default`), exactly as the renderer does, and **drop other drivers' overrides**: they never reach this database.
- **Leave names alone** (`Name`, `PhysicalName`); name mapping is the resolver's job.
- **Change it together with the renderer.** `NormalizeColumn` lives in the introspector file next to the reverse type mapping, and its comment lists the renderer functions it mirrors.
- **Tests every driver must have:**
  1. unit tests for each rule, including other drivers' overrides being ignored and an already-normal column passing through unchanged;
  2. an integration test that creates one column per rule through the driver and expects every column to match after `RunIntrospection`;
  3. the same test with the normalizer hidden (`struct{ migration.SchemaIntrospector }{driver}`), expecting every column to **differ**: proof the normalizer is what makes them match;
  4. a "real changes still differ" test: a different length, type, or nullability is still reported.

#### Current rules

**Postgres** (`storage/adapters/postgres/introspector.go`):

| Rule | Because the DDL... |
| --- | --- |
| `Overrides["postgres"].Type` / `.AutoInc` replace the declared ones | renders the override |
| string without a length → length 255 | renders `VARCHAR(255)` |
| `blob` → `bytes` | renders both as `BYTEA` |
| every type except `string` → length 0 | only `VARCHAR` carries a length |
| primary key → not nullable, not unique | renders `NOT NULL`; the key implies uniqueness |
| default → `pgDefaultFromStored(renderDefaultExpr(col))`, none on an identity column | see *Defaults* |

**SQLite** (`storage/adapters/sqlite/introspector.go`):

| Rule | Because the DDL... |
| --- | --- |
| `Overrides["sqlite"].Type` / `.AutoInc` replace the declared ones | renders the override |
| `uuid`, `bytes` → `blob`; `json` → `text` | renders `BLOB` and `TEXT` |
| string without a length → length 255 | renders `VARCHAR(255)` |
| every type except `string` → length 0 | only `VARCHAR` carries a length |
| `AutoInc` → `integer` | `AUTOINCREMENT` is only valid on `INTEGER PRIMARY KEY` |
| primary key → not nullable, not unique | renders `NOT NULL`, or the column is a rowid alias (never NULL) |
| default → `sqliteDefaultFromStored(storedDefault(col))` | see *Defaults* |

**MySQL** (`storage/adapters/mysql/introspector.go`):

| Rule | Because the DDL... |
| --- | --- |
| `Overrides["mysql"].Type` / `.AutoInc` replace the declared ones | renders the override |
| `uuid` → `string`, length 36 | renders `CHAR(36)` |
| `timestamp` → `datetime` | renders `DATETIME(6)` for both |
| `bytes` → `blob` | renders `LONGBLOB` for both |
| string without a length → length 255 | renders `VARCHAR(255)` |
| every type except `string` → length 0 | only `CHAR`/`VARCHAR` carry a length |
| primary key → not nullable, not unique | renders `NOT NULL`; the key implies uniqueness |
| default → `mysqlDefaultFromStored(storedDefault(col))`, none on an `AUTO_INCREMENT` column | see *Defaults* |

`boolean` needs no rule: it is rendered as `TINYINT(1)`, and the introspector reads `tinyint(1)` back as `boolean` (any other `TINYINT` is an integer).

**SQL Server** (`storage/adapters/sqlserver/introspector.go`):

| Rule | Because the DDL... |
| --- | --- |
| `Overrides["sqlserver"].Type` / `.AutoInc` replace the declared ones | renders the override |
| `uuid` → `string`, length 36 | renders `NCHAR(36)` |
| `json` → `text` | renders `NVARCHAR(MAX)` |
| `bytes` → `blob` | renders `VARBINARY(MAX)` for both |
| string without a length → length 255; longer than 4000 → `text` | renders `NVARCHAR(255)`; `NVARCHAR(n)` stops at 4000, so `NVARCHAR(MAX)` |
| every type except `string` → length 0 | only character types carry a length |
| primary key → not nullable, not unique; `AutoInc` → not nullable | renders `NOT NULL`; an identity column can't be `NULL` |
| default → `sqlServerDefaultFromStored(renderDefaultExpr(col))`, none on an identity column | see *Defaults* |

`datetime` and `timestamp` need no rule here: they are `DATETIME2(6)` and `DATETIMEOFFSET(6)`, which read back as themselves.

### Equality: `columnsEqual`

| Field | Compared | Notes |
| --- | --- | --- |
| `Name` | yes | Live physical names are mapped back to canonical before comparing. |
| `Type`, `Length` | yes | After normalization. |
| `Nullable`, `PrimaryKey`, `Unique` | yes | After normalization. |
| `AutoInc` | yes | |
| `Default` (+ override `Default`s) | yes | `defaultsEqual`, see *Defaults*. |
| other `Overrides` fields | no | `Type`/`AutoInc` overrides are already folded in by normalization. |
| `Check` | None | Removed from the model; see *Constraints*. |

`[Convention]` Findings are produced in **declaration order** (then live order for extras), never map-iteration order, for columns, indexes, and foreign keys alike. Baselines are regenerated and compared against the confirmed file (`migrationsEqual`), so a random order would read as "the live schema changed".

### Narrowing: `isNarrowingChange(live, declared)`

`declared` is the normalized declaration (`ColumnFinding.Normalized`), so both sides speak the database's terms. A change is **narrowing**, raised as a `PlanIssue` (*Apply alter* · *Leave as-is*), when any of these holds; otherwise it is planned as an automatic `OpAlterColumn`:

| Change | Narrowing | Why |
| --- | --- | --- |
| any type change | yes | Whether existing values convert depends on the data, which planning never reads, including usually-safe changes (integer → bigint, string → text). A type the driver stores identically (uuid in SQLite's `BLOB`) is not a change, because the comparison is normalized. |
| nullable → `NOT NULL` | yes | Existing NULLs are rejected. |
| `NOT NULL` → nullable | no | |
| length shrinks | yes | Longer values are rejected or truncated. |
| length grows | no | |
| auto-increment added | yes | A new generator can hand out values that already exist (drivers start it past the existing maximum, but the change still alters how ids are produced). |
| auto-increment removed | yes | Every insert that leaves the column out breaks. |
| default added or changed | no | A default only applies to future inserts, never to existing rows. |
| default removed | yes | Inserts relying on it start failing (or storing NULL). |

`[Convention]` A new default on a column that also becomes `NOT NULL` is narrowing through the nullability rule, not the default rule: the existing rows are what's at risk, and the default never fills them.

#### Applying an auto-increment change

An `OpAlterColumn` that changes `AutoInc` must actually change it, otherwise the column differs again on the next run. Drivers act only when `PrevColumn` says `AutoInc` changed:

- **Postgres**: *adding*: `DROP DEFAULT` (an identity and a default can't coexist), `ADD GENERATED BY DEFAULT AS IDENTITY`, then `setval(pg_get_serial_sequence(table, column), COALESCE(MAX(column), 0) + 1, false)` so the sequence starts past existing values. *Removing*: `DROP IDENTITY IF EXISTS`, emitted **before** the default clause, since Postgres rejects `SET/DROP DEFAULT` on an identity column; a legacy `serial` column loses its `nextval()` default through the ordinary default clause. Adding to a non-integer column is rejected.
- **SQLite**: the alter is a table rebuild that re-renders the column, so `AUTOINCREMENT` follows the declaration; the rebuild preserves `sqlite_sequence`, so ids continue past the existing maximum. `AUTOINCREMENT` is only valid on a lone `INTEGER PRIMARY KEY`. Removing it from a rowid alias still lets SQLite assign rowids (possibly reusing them).
- **MySQL**: every alter is a `MODIFY COLUMN` that restates the whole column, so `AUTO_INCREMENT` follows the declaration whether or not `PrevColumn` is set. Added to a populated column, MySQL continues past the largest existing value on its own (observed: ids 3 and 7, next insert 8). The column must be a key, which MySQL reports itself; a non-integer column is rejected by the driver.
- **SQL Server**: `IDENTITY` can't be added to or removed from a column in place. The driver compares the live column with the declaration and, when `AutoInc` differs, rebuilds the table (`rebuildTable`, see *SQL Server Driver*). Rows are copied with `IDENTITY_INSERT ON`, which moves a new identity past the largest copied value (observed: ids 3 and 7, next insert 8).

### Defaults

#### Representation

A column's default has two forms, and exactly one applies:

- `Column.Default`: a **literal value** (`"active"`, `7`, `true`).
- `Overrides[driver].Default`: a **raw expression** in that driver's language (`now()`, `CURRENT_TIMESTAMP`). It wins over the literal.

Introspectors report a live default the same way: a literal they can parse becomes `Default`; anything else is kept verbatim under **their own** override. `NormalizeColumn` maps a declaration onto exactly that shape.

#### Comparison: `defaultsEqual`

A default is compared by kind:

- **Literals** compare by value; numbers numerically, whatever their Go type: a declaration holds `7` (`int`), an introspector reads `int64(7)`, and a migration file or Path I snapshot that went through JSON holds `float64(7)`.
- **Expressions** compare per driver with `sameExpression`: case-insensitive, ignoring whitespace and parentheses that enclose the whole expression, but **exact inside quoted strings and quoted identifiers** (`'A'` ≠ `'a'`, `'a b'` ≠ `'ab'`).
- `hasDefault` (used by the narrowing rule) is true for a non-nil literal or any non-empty override expression.

#### Postgres workflow

The Postgres workflow has these steps:

1. **Render** (`renderDefaultExpr`): the postgres override expression if set, otherwise the literal (`'it''s'`, `7`, `TRUE`). An identity column gets no default.
2. **Store**: Postgres rewrites what it stores. Observed with a probe against Postgres 15:

| Declared | Stored (`information_schema.columns.column_default`) |
| --- | --- |
| `-1` on `INTEGER`; `-5` on `BIGINT` | `'-1'::integer`; `'-5'::integer` |
| `7`, `1.5`, `1.50` | `7`, `1.5`, `1.50` |
| `TRUE` | `true` |
| `'x'` on `TEXT` / `VARCHAR` | `'x'::text` / `'x'::character varying` |
| `NOW()`, `now()` | `now()` |
| `current_timestamp` | `CURRENT_TIMESTAMP` |
| `(1+1)` | `(1 + 1)` |
| `'{}'` on `JSONB` | `'{}'::jsonb` |
| `NULL` | no default |
| `'2020-01-01'` on `TIMESTAMP` | `'2020-01-01 00:00:00'::timestamp without time zone` |

3. **Read back** (`pgDefaultFromStored` → `parsePgDefault(expr, columnType)`): a quoted literal with an optional cast is a literal, and on a numeric column it is parsed as a **number** (Postgres quotes negatives); bare numbers, `true`/`false` (any case) and `NULL` are literals; `nextval(...)` is handled before this as `AutoInc` (serial); everything else is an expression, kept verbatim under `Overrides["postgres"]`.
4. **Normalize** = step 3 applied to step 1's output.

#### SQLite workflow

The SQLite workflow has these steps:

1. **Render**: the sqlite override as `DEFAULT (expr)`, otherwise the literal (`DEFAULT 'x'`, `DEFAULT 1` for `true`).
2. **Store**: SQLite keeps the text as written, except that it **drops exactly one pair of parentheses** around an expression default. Observed with a probe:

| Written | Reported (`PRAGMA table_info` → `dflt_value`) |
| --- | --- |
| `DEFAULT ('{}')`, `DEFAULT '{}'` | `'{}'` |
| `DEFAULT (CURRENT_TIMESTAMP)` | `CURRENT_TIMESTAMP` |
| `DEFAULT (1+1)` | `1+1` |
| `DEFAULT (datetime('now'))` | `datetime('now')` |
| `DEFAULT (-1)`, `DEFAULT -1` | `-1` |
| `DEFAULT ((('x')))` | `(('x'))` |

3. **Read back** (`sqliteDefaultFromStored` → `parseSQLiteDefault(text, columnType)`): a single quoted string, a number, `NULL`, `TRUE`/`FALSE` are literals; `0`/`1` on a `BOOLEAN` column read back as booleans; everything else is an expression under `Overrides["sqlite"]` (one more enclosing pair stripped).
4. **Normalize** = step 3 applied to the text step 2 would report (`storedDefault`): the override expression itself, or the literal's text. Consequently an override that is only a quoted literal (`'{}'`) normalizes, and reads back, as the literal `{}`.

#### MySQL workflow

The MySQL workflow has these steps:

1. **Render** (`renderDefaultExpr`): the mysql override as `DEFAULT (expr)`. MySQL requires the parentheses for every expression except `CURRENT_TIMESTAMP`, and with them `now()` is accepted on a `DATETIME(6)` column whatever its precision. Otherwise the literal: `'it''s'` (backslashes doubled), `TRUE`/`FALSE`, `7`. On a `TEXT`, `BLOB`, or `JSON` column MySQL accepts no literal default, so the literal is rendered as a quoted string in parentheses: `('it''s')`, `('-1')`. An `AUTO_INCREMENT` column gets no default.
2. **Store**: observed with a probe against MySQL 8.0.36 (`information_schema.COLUMNS`):

| Written | `COLUMN_DEFAULT` | `EXTRA` |
| --- | --- | --- |
| `DEFAULT 'it''s'` | `it's` | |
| `DEFAULT ''` | empty string (not `NULL`) | |
| `DEFAULT 'NULL'` | `NULL` as a string; no default is SQL `NULL` | |
| `DEFAULT 7`, `-1`, `1.5`, `1.50` | `7`, `-1`, `1.5`, `1.5` | |
| `DEFAULT 1.5` on `DECIMAL(65,30)` | `1.500000000000000000000000000000` | |
| `DEFAULT TRUE` / `FALSE` | `1` / `0` | |
| `DEFAULT '2020-01-01'` on `DATETIME(6)` | `2020-01-01 00:00:00.000000` | |
| `DEFAULT CURRENT_TIMESTAMP(6)` | `CURRENT_TIMESTAMP(6)` | `DEFAULT_GENERATED` |
| `DEFAULT (CURRENT_TIMESTAMP(6))`, `(NOW(6))` | `now(6)` | `DEFAULT_GENERATED` |
| `DEFAULT (now())`, `(CURRENT_TIMESTAMP)` on `DATETIME(6)` | `now()` | `DEFAULT_GENERATED` |
| `DEFAULT (LOCALTIMESTAMP(3))` | `now(3)` | `DEFAULT_GENERATED` |
| `DEFAULT (UUID())` | `uuid()` | `DEFAULT_GENERATED` |
| `DEFAULT (1+1)` | `(1 + 1)` | `DEFAULT_GENERATED` |
| `DEFAULT (7)`, `(1.50)`, `(true)` | `7`, `1.50`, `true` | `DEFAULT_GENERATED` |
| `DEFAULT (-1)` | `-(1)` | `DEFAULT_GENERATED` |
| `DEFAULT ('it''s')` | `_utf8mb4\'it\\\'s\'` | `DEFAULT_GENERATED` |
| `DEFAULT ('a\\b')` (the value `a\b`) | `_utf8mb4\'a\\\\b\'` | `DEFAULT_GENERATED` |
| `DEFAULT (concat('a', 'b'))` | `concat(_utf8mb4\'a\',_utf8mb4\'b\')` | `DEFAULT_GENERATED` |

   A literal default is bare text. Every parenthesized default is flagged `DEFAULT_GENERATED`, and its text is the expression with each quote and backslash escaped once more. String literals in it get a charset introducer that names the charset of the connection that created the column (`_latin1` from a latin1 client).
3. **Read back** (`mysqlDefaultFromStored(text, generated, columnType)`):
   - not generated: the column type decides. `1`/`0` on a boolean column are booleans, a number on a numeric column is a number, anything else is a string.
   - generated: the text is unescaped (`unescapeStoredExpression`), charset introducers and enclosing parentheses are removed, and then a single quoted string, a number, `true`/`false` are literals; `NULL` is no default; `CURRENT_TIMESTAMP`, `LOCALTIME`, `LOCALTIMESTAMP`, and `NOW` are spelled `now(n)`; everything else is an expression under `Overrides["mysql"]`.
4. **Normalize** = step 3 applied to what step 2 would report (`storedDefault`): the override expression flagged generated, a literal on a `TEXT`/`BLOB`/`JSON` column as its quoted string flagged generated, any other literal as its bare text (`1`/`0` for a boolean).

`[Known limitation]` `-(1)` shows that MySQL rewrites expressions beyond case and spacing. The driver never writes a negative number in parentheses itself (a number on a `TEXT` column is quoted), but an override such as `(-1)` or one with a string literal containing a quote inside a larger expression reads back differently from its declaration and is altered on every run. Declare such defaults in their stored form.

#### SQL Server workflow

The SQL Server workflow has these steps:

1. **Render** (`renderDefaultExpr`): the sqlserver override expression as written, otherwise the literal: `N'it''s'`, `7`, and `1`/`0` for a boolean (T-SQL has no `TRUE`). A default is a constraint; it is written inline (`DEFAULT expr`) on `CREATE TABLE` and `ADD`, and as `ALTER TABLE ... ADD DEFAULT expr FOR column` on an alter. The constraint is left unnamed, so SQL Server names it (`DF__table__column__hash`), and the driver finds it in `sys.default_constraints` when it has to drop it. An identity column gets no default.
2. **Store**: observed with a probe against SQL Server 2019 (`sys.default_constraints.definition`):

| Written | Stored |
| --- | --- |
| `N'it''s'`, `'x'`, `N''` | `(N'it''s')`, `('x')`, `(N'')` |
| `7`, `-1`, `1.5`, `1.50` | `((7))`, `((-1))`, `((1.5))`, `((1.50))` |
| `1` / `0` on `BIT` | `((1))` / `((0))` |
| `'2020-01-01'` on `DATETIME2` | `('2020-01-01')` |
| `GETDATE()`, `CURRENT_TIMESTAMP` | `(getdate())` |
| `SYSDATETIMEOFFSET()`, `(sysutcdatetime())`, `NEWID()` | `(sysdatetimeoffset())`, `(sysutcdatetime())`, `(newid())` |
| `(1+1)` | `((1)+(1))` |
| `CONVERT(nvarchar(10), 5)` | `(CONVERT([nvarchar](10),(5)))` |
| `NULL` | `(NULL)` |

   Everything is wrapped in one pair of parentheses and each number in another. Function names are lower-cased, type names bracketed.
3. **Read back** (`sqlServerDefaultFromStored(expr, columnType)`): enclosing parentheses are removed; a single string literal (`'x'` or `N'x'`) or a number is a literal, and `1`/`0` on a `BIT` column are booleans; `NULL` is no default; `CURRENT_TIMESTAMP` is spelled `getdate()`; everything else is an expression under `Overrides["sqlserver"]`.
4. **Normalize** = step 3 applied to step 1's output.

`[Known limitation]` SQL Server rewrites expressions beyond case and spacing (`1+1` → `(1)+(1)`, `nvarchar` → `[nvarchar]`). An override it rewrites this way reads back differently from its declaration and is altered on every run. Declare such defaults in their stored form.

#### Known limitation

`[Known limitation]` A literal the database **reformats** on storage never compares equal. Postgres stores `'2020-01-01'` on a timestamp column as `'2020-01-01 00:00:00'::timestamp...`, MySQL as `2020-01-01 00:00:00.000000`. Such a column gets an automatic `SET DEFAULT` on every run: harmless, but noisy. Declare these defaults in their stored form, or as an override expression. A full fix would need each driver to reproduce the database's value formatting; deferred until it matters.

### Guidance for future drivers (MongoDB and others)

`[Convention, important]` **Probe before encoding.** Every storage rule above was observed against the real database (a throwaway container or in-memory instance, a probe table with one column per case, and the catalog query the introspector uses), not recalled. A new driver starts the same way and records its probe table in this document. The points below are what to probe for, not facts to rely on.

Checklist:

1. **Types:** list every canonical type the renderer collapses, and every type the database stores differently from how it was written; each becomes a normalization rule.
2. **Lengths:** which types carry one, what a length-less string becomes.
3. **Keys:** whether primary keys are implicitly `NOT NULL` / unique, and how the catalog reports it.
4. **Auto-increment:** how it is declared, how the catalog reports it, whether it can be added or removed in place, and how to start it past existing values.
5. **Defaults:** the catalog's stored form for literals (quoting, casts, negatives, booleans), keywords, functions, and parenthesized expressions; share one parser between `Introspect` and `NormalizeColumn`.
6. **Tests:** the four tests in *Contract for implementers*, plus a defaults round-trip test like `DefaultsMatch`.

Points to probe per database (expected, unverified):

- **MongoDB / other document stores**: no DDL-level defaults or auto-increment, and no enforced lengths. The driver's `NormalizeColumn` clears `Default`, `AutoInc`, and `Length` (whatever the database doesn't store), so those fields always compare equal; core needs no special case. Row consistency is the application's concern there (see *Constraints* for validation).
  MongoDB has no driver yet. Its indexes are created by the adapter's `EnsureIndexes` and checked at `Boot`. [`../database/adapters/mongo_indexes.md`](../database/adapters/mongo_indexes.md) records what was probed about indexes on MongoDB 6.0 and, under *The future migration driver*, how each operation maps, what the introspector can and can't report, and what to reuse from the adapter.


## MySQL driver

`storage/adapters/mysql` holds `MySQLDriver` next to `MySQLAdapter`. It targets MySQL 8.0.13 and later, the first version with expression defaults, and was probed against 8.0.36. MariaDB is not covered: its `JSON` is an alias of `LONGTEXT` and its catalog reports defaults differently.

Type mapping (`renderMySQLType`, reverse in `mysqlTypeMapping`):

| Canonical | Created as | Read back as |
| --- | --- | --- |
| `string` | `VARCHAR(n)`, 255 without a length | `string` (`CHAR` too) |
| `text` | `LONGTEXT` | `text` (all four `TEXT` sizes) |
| `int` | `INT` | `int` (`TINYINT`, `SMALLINT`, `MEDIUMINT` too) |
| `bigint` | `BIGINT` | `bigint` |
| `real` | `DOUBLE` | `real` (`FLOAT` too) |
| `numeric` | `DECIMAL(65,30)` | `numeric` |
| `bool` | `TINYINT(1)` | `bool` |
| `datetime`, `timestamp` | `DATETIME(6)` | `datetime` (`TIMESTAMP` too) |
| `uuid` | `CHAR(36)` | `string(36)` |
| `json` | `JSON` | `json` |
| `bytes`, `blob` | `LONGBLOB` | `blob` (`BINARY`/`VARBINARY` read back as `bytes`) |

`ENUM`, `SET`, `DATE`, `TIME`, `YEAR`, `BIT`, spatial types and generated columns are reported as text with an ambiguity.

How each operation is applied:

| Operation | Statement |
| --- | --- |
| create table | `CREATE TABLE` with the columns, `PRIMARY KEY (...)` and one `UNIQUE KEY` per unique column |
| add column | `ALTER TABLE ... ADD COLUMN ...`, plus `, ADD UNIQUE KEY ...` in the same statement for a unique column |
| alter column | `ALTER TABLE ... MODIFY COLUMN <full definition>`, plus `, ADD UNIQUE KEY ...` / `, DROP INDEX ...` when `PrevColumn` says uniqueness changed |
| rename column | `ALTER TABLE ... RENAME COLUMN ... TO ...` |
| drop column | `ALTER TABLE ... DROP COLUMN ...`; MySQL removes the column from its indexes |
| add / drop index | `CREATE [UNIQUE] INDEX ... ON ...` / `DROP INDEX ... ON ...` (index names are per table, so `OpDropIndex` needs its `Table`) |
| add / drop foreign key | `ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY ...` / `ALTER TABLE ... DROP FOREIGN KEY ...` |

Every operation is one statement, and MySQL 8 runs a single DDL statement atomically. A column and its unique key therefore succeed or fail together.

### Apply order under best-effort atomicity

**Context:** MySQL commits each DDL statement as it runs, so `ApplyMigration` cannot roll back a migration that fails midway (`AtomicityBestEffort`). The Postgres order (DDL, then the ledger insert, all in one transaction) would run the DDL of an already recorded migration before the duplicate ledger row stops it.

**Options considered:**

- *Postgres order.* Simple, but a reapplied migration executes its DDL and only then fails.
- *Ledger row first, DDL second.* Stops a reapply, but a migration that fails midway is recorded as applied.
- *Check first, DDL second, record last.* Plan every statement, create the bookkeeping tables, look the ID up in the ledger, run the DDL, then write the ledger row and the snapshot in one transaction.

**Decision:** the third. A malformed migration (unknown kind, missing payload, unrenderable type) and a recorded one both fail before the first statement. A failure during the DDL leaves the earlier operations applied and the migration unrecorded; the error lists the operations that stay applied. Everything runs on one `*sql.Conn` holding `GET_LOCK('behemoth.migration.<hash of the ledger table>', -1)`, MySQL's session-scoped counterpart of Postgres's advisory lock, so two migrators can't interleave. The server releases the lock if the session dies.

**Revisit if:** the runner gains a recovery path for partially applied migrations; it would need the ledger to record which operations ran.

### `DATETIME(6)` for both `datetime` and `timestamp`

**Context:** MySQL's `TIMESTAMP` ends in January 2038 and converts values through the session time zone. Expiry times of long-lived tokens can reach past 2038 today.

**Options considered:**

- *`TIMESTAMP(6)` for `timestamp`.* Keeps the two canonical types apart, with the 2038 limit and results that depend on each connection's `time_zone`.
- *`DATETIME(6)` for both.* No range limit, and a value round-trips through `go-sql-driver/mysql` as written (it formats `time.Time` in its `loc`, UTC by default). The two canonical types collapse.

**Decision:** `DATETIME(6)` for both, with a normalization rule (`timestamp` → `datetime`). The introspector maps a live `TIMESTAMP` column to `datetime` as well, so an existing table that uses `TIMESTAMP` matches either declaration instead of raising a type change on every run. Six fractional digits match Postgres's microseconds.

**Revisit if:** the canonical model gains a way to ask for a native type by name.

### Indexes on `TEXT` and `BLOB` columns

**Context:** MySQL can't index a `TEXT` or `BLOB` column whole; the key part needs a prefix length. The canonical `Index` has no such field, and core's own schema and the driver suite index `text` columns.

**Options considered:**

- *Fail.* Honest, but `text` columns could then never be indexed or unique on MySQL.
- *Always add a prefix.* Not valid on other column types.
- *Add a prefix where the column needs one.* The builder has to know the column's type, which an `OpAddIndex` doesn't carry.

**Decision:** the third, with a fixed prefix of 255 (`keyPrefixLength`; 1020 bytes in utf8mb4, so three such columns fit InnoDB's 3072-byte key). `keyPrefixes` learns column types from the migration's own operations (create table, add, alter, and rename column) and reads `information_schema.COLUMNS` for any other column. This is why `RenderMigration` should run against the database the migration will be applied to. The introspector ignores `SUB_PART`, so the index reads back as declared. `[Known limitation]` A unique key or unique index on such a column enforces uniqueness of the first 255 characters only. Changing an indexed column to `text` fails with MySQL's error 1170; drop the index first.

**Revisit if:** `schema.Index` gains per-column lengths.

### Unique columns are named unique keys

**Context:** MySQL has no unique constraint apart from a unique index, so `Column.Unique` and a declared `Index{Unique: true}` on one column are the same kind of object live. The introspector has to tell them apart, and `OpAlterColumn` has to drop the key of a column that stops being unique.

**Options considered:**

- *Inline `UNIQUE`.* MySQL names the key after the column, or `<column>_2` when that name is taken, so the name can't be predicted.
- *A named key, `<table>_<column>_key`.* The same convention as the Postgres driver.

**Decision:** the named key (`uniqueKeyName`, from physical names). A name over 64 characters, which MySQL rejects, is cut and ends in a hash of the full name. `classifyKeys` reads a single-column unique key as `Column.Unique` when it has that name or the column's own name (what an inline `UNIQUE` on an existing table produced); any other unique key is an `Index`. `MODIFY COLUMN` never mentions uniqueness, so an alter without `PrevColumn` leaves it alone, as on Postgres.

### Adding a `NOT NULL` column without a default

**Context:** Postgres and SQLite reject adding such a column to a table with rows, and the driver suite expects that. MySQL fills the rows with the type's implicit value (`''`, `0`) instead, even in strict mode (observed).

**Options considered:** let MySQL fill the rows; or check for rows first.

**Decision:** `ApplyMigration` runs `SELECT 1 FROM <table> LIMIT 1` right before the statement and fails when a row exists (`emptyTableGuard`). The check is not a statement, so the rendered script doesn't contain it: someone running the script by hand gets MySQL's behavior.

### The index MySQL creates for a foreign key

**Context:** InnoDB needs an index on a foreign key's columns. When none starts with them, `ADD CONSTRAINT` creates one named after the constraint (after the first column for an unnamed constraint), and `DROP FOREIGN KEY` leaves it behind.

**Decision:** the introspector leaves that index out (`classifyKeys`: non-unique, on exactly the key's columns, named after the key or its first column), so it never shows as an extra live index. `OpDropForeignKey` drops the constraint only: the operation can't tell an index MySQL created from one the application declared under the same name. Once the key is gone the index is reported like any other index. See `docs/ongoing.md`.


## SQL Server driver

`storage/adapters/sqlserver` holds `SQLServerDriver` next to `SQLServerAdapter`. It was probed against SQL Server 2019 and uses nothing newer than 2016 (filtered indexes, `THROW`, `DATETIMEOFFSET`). It works in the connection's default schema (`SCHEMA_NAME()`).

DDL is transactional, so the driver reports `AtomicityFull` and applies like Postgres: one transaction holding `sp_getapplock` (`@LockOwner = 'Transaction'`, keyed on the ledger table) around the operations, the ledger insert and the snapshot write.

Type mapping (`renderSQLServerType`, reverse in `mapSQLServerType`):

| Canonical | Created as | Read back as |
| --- | --- | --- |
| `string` | `NVARCHAR(n)`, 255 without a length, `MAX` above 4000 | `string` (`VARCHAR`, `CHAR`, `NCHAR` too) |
| `text` | `NVARCHAR(MAX)` | `text` (`VARCHAR(MAX)`, `TEXT`, `NTEXT` too) |
| `int` | `INT` | `int` (`SMALLINT`, `TINYINT` too) |
| `bigint` | `BIGINT` | `bigint` |
| `real` | `FLOAT` | `real` (`REAL` too) |
| `numeric` | `DECIMAL(38,10)` | `numeric` (`NUMERIC`, `MONEY`, `SMALLMONEY` too) |
| `bool` | `BIT` | `bool` |
| `datetime` | `DATETIME2(6)` | `datetime` (`DATETIME`, `SMALLDATETIME` too) |
| `timestamp` | `DATETIMEOFFSET(6)` | `timestamp` |
| `uuid` | `NCHAR(36)` | `string(36)`; a live `UNIQUEIDENTIFIER` reads back as `uuid` |
| `json` | `NVARCHAR(MAX)` | `text` |
| `bytes`, `blob` | `VARBINARY(MAX)` | `blob` (`BINARY(n)`/`VARBINARY(n)` read back as `bytes`) |

`sys.columns.max_length` counts bytes, so an `NVARCHAR`/`NCHAR` length is half of it. `DATE`, `TIME`, `XML`, `rowversion`, `sql_variant`, spatial types and computed columns are reported as text with an ambiguity.

How each operation is applied:

| Operation | Statements |
| --- | --- |
| create table | `CREATE TABLE` with the columns and `PRIMARY KEY (...)`, then one `CREATE UNIQUE INDEX` per unique column |
| add column | `ALTER TABLE ... ADD ...`, then the unique index for a unique column. SQL Server itself rejects a `NOT NULL` column without a default on a table with rows. |
| alter column | drop the default constraint; if type, length or nullability differ from the live column: drop the indexes on the column, `ALTER COLUMN`, create them again; add the default; add or drop the column's unique index when `PrevColumn` says uniqueness changed. A change of `AutoInc` rebuilds the table instead. |
| rename column | `EXEC sp_rename ..., 'COLUMN'`; filtered indexes on the column are dropped before and created after; the column's unique index is renamed with it |
| drop column | drop its default constraint and every index it is part of, then `DROP COLUMN` |
| add / drop index | `CREATE [UNIQUE] INDEX ... [WHERE ...]` / `DROP INDEX ... ON ...` |
| add / drop foreign key | `ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY ... ON DELETE ...` / `DROP CONSTRAINT`. `restrict` is written `NO ACTION`: T-SQL has no `RESTRICT`. |

Observed behavior behind that table (probe, SQL Server 2019): `ALTER COLUMN` accepts a widening or a nullability change on an indexed column with a default, and fails for a type change (`The index ... is dependent on column`); `DROP COLUMN` fails for a column with a default or an index; `sp_rename` fails for a column a filtered index names in its predicate; a unique index treats NULLs as equal; an index on `NVARCHAR(MAX)` is rejected (error 1919).

### Operations read the live schema

**Context:** on Postgres an operation maps to statements that depend on nothing but the operation. On SQL Server most don't: a default constraint has a generated name, the indexes to drop around `ALTER COLUMN` and `DROP COLUMN` are whatever exists, a unique index's filter depends on its columns' nullability, and an identity change recreates the whole table.

**Options considered:**

- *Pure builders with dynamic T-SQL* (`DECLARE @n sysname; SELECT @n = name FROM sys.default_constraints ...; EXEC(...)`). The script stays independent of the database, but it becomes procedural code nobody can read, and a table rebuild can't be written that way at all.
- *Read the catalog in Go while applying.* Each statement is plain DDL. The statements then depend on the database the migration runs against.

**Decision:** read the catalog (`readTable`, `liveIndexes`, `nullableColumns`, `dropDefault`), inside the migration's transaction so an operation sees what the operations before it did. `RenderMigration` follows SQLite's approach: it applies `Up` in a transaction that is always rolled back and prints the statements that ran (`run.exec` records every one). A baseline's tables already exist, so its `Up` is built without being executed (`run.dry`). Rendering therefore takes schema locks on the tables involved for its duration and has to run against the database the migration will be applied to.

**Revisit if:** rendering against a busy production database turns out to block for too long.

### Unique means a filtered unique index

**Context:** a SQL Server unique index or `UNIQUE` constraint treats NULLs as equal, so a nullable unique column could hold one NULL. Postgres, SQLite, and MySQL allow any number, and the driver suite adds a nullable unique column to a table with two rows.

**Options considered:** keep SQL Server's behavior and document it; or filter NULLs out of the index.

**Decision:** `Column.Unique` is a unique *index* named `<table>_<column>_key` (`uniqueKeyName`), not a constraint, and every unique index, the column's or a declared `Index{Unique: true}`, gets `WHERE c IS NOT NULL` for each nullable key column (`indexSQL`). A row with a NULL in the key is then never indexed and never conflicts, which is Postgres's behavior. Indexes on `NOT NULL` columns have no filter. When `ALTER COLUMN` changes nullability, the column's indexes are recreated so the filter follows. The introspector accepts exactly that filter (`isNullFilter`); an index with any other filter, or with included columns, can't be expressed and is left out. It reads a single-column unique index as `Column.Unique` when it has the driver's name or is a `UNIQUE` constraint. `[Known limitation]` SQL Server doesn't accept a filtered index as the target of a foreign key, so a *nullable* unique column can't be referenced by one.

### `uuid` is `NCHAR(36)`, not `UNIQUEIDENTIFIER`

**Context:** models hold ids as text. `database/sql` drivers return a `UNIQUEIDENTIFIER` as 16 bytes in SQL Server's mixed byte order, which `FromMap` would read as garbage.

**Options considered:** `UNIQUEIDENTIFIER` with a conversion in every adapter read; or a character column.

**Decision:** `NCHAR(36)`, the same choice as the MySQL driver. `N` rather than plain `CHAR` because `go-mssqldb` sends Go strings as `NVARCHAR`, and comparing them to a `CHAR` column converts the column and defeats its index. A live `UNIQUEIDENTIFIER` column still reads back as `uuid`, so against a declared `uuid` it shows as a type change the developer can leave as it is.

**Revisit if:** the adapters gain per-type value conversion.

### An auto-increment change rebuilds the table

**Context:** `IDENTITY` can't be added or removed with `ALTER COLUMN`, and the driver contract requires both, keeping existing values.

**Options considered:**

- *Swap the column* (add a new one, copy, drop the old, rename). Values can't be copied *into* an identity column with `UPDATE`, and the column is usually the primary key that other tables reference.
- *`ALTER TABLE ... SWITCH`* into a twin table. Metadata-only, but it needs identical indexes and constraints on both sides and fails for tables that foreign keys reference.
- *Rebuild:* create a new table, copy the rows, drop the old one, rename.

**Decision:** rebuild (`rebuildTable`), in the migration's transaction. The altered column is rendered from its declaration. Every other column is recreated from the catalog in native terms (`liveColumn.nativeDefinition`: type with length, precision, or scale, collation when it isn't the database's, identity seed and increment, nullability, default), so a `VARCHAR` or `MONEY` column stays what it was. The primary key, the indexes, the table's foreign keys and the foreign keys other tables hold on it are dropped and created again, with their `ON DELETE` and `ON UPDATE` actions. The rebuild refuses, changing nothing, when the table has something it can't carry over: a check constraint, a trigger, a computed or `rowversion` column, or an index the introspector can't express. Constraint names SQL Server generated (primary key, defaults) change.

**Revisit if:** check constraints come back into the canonical model.

`[Known limitation]` `text`, `json`, `bytes`, and `blob` columns are `MAX` types, which SQL Server can't index, make unique or use in a key at all; there is no prefix index as on MySQL. Give such a column the override `Overrides["sqlserver"].Type = string` (with a `Length`) when it has to be indexed. The driver suite does this for the text columns it indexes (`indexableText`).


## Planning stage

This stage converts the Introspection Report's divergences and ambiguities into a concrete, tiered list of **candidate `SchemaOperation`s**, deciding _what kind of change_ each divergence implies and _whether it's safe to auto-apply_. Plan's output is an unordered set of candidates plus their tier; Generate stage is responsible for turning that into an ordered, immutable artifact.

### Operation tiers
Each operation has one of these tiers:

1. Safe/Auto - Can be applied automatically
2. Data Dependent - Affects existing data. for example, add unique index. Resolved to either 'Safe' or 'Manual'
3. Requires Confirmation - Potential data loss. for example, drop table
4. Manual - Should be performed manually 

### Note on extra entities (columns, indexes, and foreign keys)
Extra entities are handled as follows:

- The introspection stage has already handled extra objects found live. If any extra entities are encountered in the planning stage, it means they were part of the old schema registry and are now subject to drop operation after user confirmation. 

#### Branch: table-level divergences

A table-level divergence is planned as follows:

- **Fresh table (declared, missing live)** → `OpCreateTable` candidate. `[Tier: Auto]`. The table in it carries columns only. Each declared index becomes its own `OpAddIndex` and each foreign key its own `OpAddForeignKey`, both `[Tier: Auto]` (see *Indexes of a new table are operations of their own* in the Generation stage). 
- **Table found live, matches** → no candidate generated.
- **Table found live, incompatible object** (Introspection's view-versus-table case) `[Tier: Manual]` → blocking; no candidate generated, Plan won't proceed for that table until resolved outside the tool. `[Convention]`

#### Branch: column-level divergences

A column-level divergence is planned as follows:

- **Declared column missing live, no rename pairing found** → `OpAddColumn` candidate. `[Tier: Auto]`
- **Extra live column, no rename pairing found** → `OpDropColumn` candidate. `[Tier: Requires Confirmation]`. Here the extra live columns are categorized to "drop column".
		~~**Distinguishing "ignore custom field" from "op: drop column"**~~
		~~An extra live column is only turned into an `OpDropColumn` candidate if it was _previously_ declared in behemoth's own last-known canonical state (Path I's snapshot) or was explicitly flagged by the developer as behemoth-managed (Path II has no snapshot, so this case is narrower there - see Path-specific note below). A column that has _never_ been part of any behemoth-authored declaration is never turned into a drop candidate automatically, regardless of Path: this is what actually implements the "developer's custom field" convention from your original draft, rather than leaving it as an unenforced assumption. `[Convention, important]`~~

	- **Path I** → "previously declared" is answered exactly by the snapshot.
	- **Path II** → no snapshot exists, and the introspector excludes all extra columns before passing the live state to planner. 

- **Rename-paired columns** (from Introspection's structural-signature matching) → `OpRenameColumn` candidate. `[Tier: Requires Confirmation]`
- **Definition divergence** (any field `columnsEqual` compares differs: type, length, nullability, primary key, uniqueness, auto-increment, default) → `OpAlterColumn` candidate, built from the declaration with `PrevColumn` set to the live column. Tier determined by direction (full rules and rationale in *Column Comparison → Narrowing*):

	- **Widening** (nullable→true, length increases, a default is added or changed) → `[Tier: Auto]`
	- **Narrowing** (any type change, nullable→false, length decreases, auto-increment added or removed, a default is removed) → `[Tier: Data Dependent]`, raised as a `PlanIssue`. Judged on the normalized declaration (`ColumnFinding.Normalized`), so a type the driver stores identically (uuid in SQLite's BLOB) is not a change, and a string declared without a length is 255, not 0.

- **Defaults**: compared in the shape introspectors report them, after normalization; adding or changing one is widening, removing one is narrowing. Details, per-driver behavior and the known limitation are in *Column Comparison → Defaults*.

- ~~**Type reverse-mapping ambiguity carried over from Introspection** → `[Convention]` Plan refuses to finalize tiering for that column until the ambiguity is resolved via the draft-review mechanism: an unresolved ambiguity is never silently defaulted to a tier.~~

#### Branch: index-level divergences

An index-level divergence is planned as follows:

- **Declared, missing live** → `OpAddIndex`. `[Tier: Auto]`
- **Extra live, unpaired** → `OpDropIndex`. `[Tier: Auto]`.
- **Definition differs** (columns or uniqueness disagree) → treated as drop-and-recreate (no `OpAlterIndex` kind exists) → both candidates generated together, `[Tier: Auto]` for the add half, `[Tier: Auto]` for the drop half, since dropping an index alone still can't lose row data.

#### Branch: foreign-key-level divergences

A foreign-key-level divergence is planned as follows:

- **Declared, missing live** → `OpAddForeignKey`. `[Tier: Auto]`
- **Extra live, unpaired** → `OpDropForeignKey`. `[Tier: Auto]`. It removes a constraint, not data.
- **Definition differs** (RefTable/RefColumns/OnDelete disagree) → drop-and-recreate, both halves `[Tier: Auto]`.

#### Branch: developer-authored custom operations

Custom operations follow these rules:

- A developer may inject a hand-written operation (typically a data-affecting step with no structural divergence behind it. for example, a backfill) directly into the candidate set at Plan time. 
- These are `CustomMigration`s: declared in code, attached to the plan as-is (never tiered, never raised as issues), and ordered by the Generation stage. See *Custom Migrations*.


#### Issue planner

For the divergences and ambiguities identified, the full set of candidate operations are presented for resolution. 
#### Types

```go
type ResolutionOption struct {
	Label      string            // human-readable, for example, "Rename email -> email_address"
	Operations []SchemaOperation // what accepting this option produces -> built by Planning, never by Resolution
}

type PlanIssue struct {
	ID          string // STABLE, content-derived; see Resolution's Identity Stability branch.
	Table       string
	Description string
	Options     []ResolutionOption
	Default     int // Planning's suggested option index. a hint for the presenter's default selection
}
```

`[Convention, important]` A `PlanIssue`'s `ID` is composed only from the **stable artifact the issue is about** (for example `table + ":dropped_column:" + columnName`). The computed pairing or option set is not included to allow separate runs to be able to propose a different set of options to a given issue even when the database state changes.

#### Handled issues and their options

| Issue                                                             | Options offered                                                                                   |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Rename candidate (single structural match)                        | Rename · Drop old + Add new independently · Leave as-is                                           |
| Ambiguous rename (multiple equally-plausible candidates)          | Manually pair specific columns · Fall back to independent add/drop for all involved · Leave as-is |
| Narrowing alter (type change, nullable→false, length shrinks, auto-increment added or removed, default removed) | Apply alter · Leave as-is (`[Deferred]` Apply alter with a specified default for existing rows) |
| ~~Type reverse-mapping ambiguity~~                                | ~~Accept Planning's guessed canonical type · Override with a specified type · Leave as-is~~       |
| Table found live as an incompatible object (for example a view)           | _(no automated option: blocking; message directs to manual resolution outside the tool)_         |
| Extra live column, never behemoth-declared                        | _(no options: not an issue since it's never surfaced)_                                           |

`[Convention]` "Leave as-is" is a first-class option everywhere it applies. It produces zero operations and is recorded as a real decision. It is the mechanism by which a developer who resolves something manually, directly against the database, tells the system not to ask again.

#### Output of this stage

A flat set of candidate operations, each tagged with a tier and, where relevant, a confirmation state (resolved / unresolved). 

`[Convention]` Plan doesn't produce operation ordering: two candidates with a real dependency (for example a column addition on a table also being created in this same run) are both present in the output, unordered. Sequencing them correctly is explicitly out of scope here and belongs to the Generation stage.

**Convention:** Migrations that drop, rename, or otherwise destroy data must not be applied without explicit human confirmation. This is an invariant of the migration engine, not a preference.


#### Branch: confirmation workflow (for every candidate that requires confirmation)

The workflow depends on the context:

- **Interactive context** (local developer machine) → each unresolved candidate is presented for explicit accept/reject; rejecting a proposed `OpRenameColumn` decomposes it back into independent `OpAddColumn` (auto) + `OpDropColumn` (now itself requiring its own separate confirmation).
- **Non-interactive context** (CI) → `[Convention, important]` Plan fails closed. Any unresolved "Requires Confirmation" candidate blocks the run entirely with an explicit listing: there is no default answer, ever, for a destructive or ambiguous candidate in an unattended context.
- **Multiple ambiguous rename candidates for one column** (Introspection's multi-candidate case) → surfaced as a named group in the same review mechanism; resolution requires the developer to manually pick a pairing or accept the fallback independent add/drop for all involved columns. `[Implementation Detail]`
- Output of a resolved review session is a human-editable **draft**: the same mechanism regardless of Path, and the same mechanism used later for Baseline's own review step. It is never an immediately-finalized artifact. `[Convention]`


## Resolution stage

This stage produces a resolved set of operations by taking the `PlanIssue` types has Planning stage generated and presenting them to the user. 

### Communication medium

Multiple candidate media exist; the design picks one default and keeps the others swappable behind a single interface.
```go
type ResolutionPresenter interface {
	// Present writes/updates whatever medium holds unresolved issues,
	// alongside decisions already made in a prior session, so a partial
	// resolution session is never silently discarded.
	Present(ctx context.Context, issues []PlanIssue, priorDecisions map[string]int) error
	// Collect reads back developer decisions, keyed by PlanIssue.ID.
	Collect(ctx context.Context) (decisions map[string]int, err error)
}
```

- **Editable draft file** _(default implementation)_ - stored on disk, git-diffable and survives process restarts. 
- **Live terminal prompt** - a thin front-end only. Every answer is written through to the same durable file immediately, never held only in process memory. 
	- `[Convention, important]` A prompt-only implementation with no backing file is explicitly disallowed, because an interrupted session must not lose already-made decisions - this is the exact failure mode observed in other tools' interactive-only flows, where an interrupted or non-interactive run has no persisted answer to fall back on.
- **Remote/shared registry** - appropriate for team-scale and multi-environment setups; not required infrastructure for a self-hosted default. `[Deferred]` A concrete implementation is not built in this phase, but the interface is shaped to accommodate one without redesign.
- **Programmatic resolver** - a developer-supplied callback resolving specific issue IDs in code, for CI automation of known/expected changes. `[Advanced]` Composes with the same interface rather than being a special case.

`[Convention, important]` Non-interactive context with any unresolved issue **fails closed**. The run is aborted with an explicit listing of every unresolved `PlanIssue.ID` and its available options. There is no default answer for a destructive or ambiguous issue in an unattended context.

### Execution flow

The `PlanIssue.ID` being **stable and content-derived**( a hash of `table, kind, involved column/index/FK names)`, can be used to track and identify tracks across multiple runs. The draft file, on disk, is a list of `{id, description, options, chosen: <index or null>}` entries. On every subsequent run:

1. Planning re-derives the current issue set fresh (Planning is a pure function and doesn't depend on any persisted state).
2. Resolution loads the _existing_ draft file, if one exists, and matches by `ID`. An issue whose `ID` already has a non-null `chosen` value in the file is treated as already-resolved and is never re-presented.
3. Only new issues (new IDs, that are not present in the old draft) get appended, pending, for the developer to address.
4. An issue whose `ID` no longer appears at all (because live state changed and the divergence is gone) is automatically dropped from the next draft.


### Identity stability (edge cases)

`[Convention, important]` The most important correctness property of this stage: a `PlanIssue`'s `ID` must be derived from the artifact it describes, not from Planning's currently-computed resolution options. Without this, the following scenarios fail:

#### A decision was not made, and the issue option set changed

A pending issue whose option set changes is handled as follows:

- **A pending issue's option set changes between runs** - Safe to discard the old issue and write the new one. This happens when the user manually fixes/updates the live state between runs. 
	*(for example a new column appears that's also a structurally plausible rename target, turning a single-candidate rename into an ambiguous one)*. 
	
  Because identity is anchored to the stable side of the divergence (the actual live-or-declared artifact), the issue's `ID` is unchanged. Resolution simply re-presents it with the newly expanded option set. No decision is lost and no issue is duplicated.

#### A decision was made, but the issue option set changed
A confirmed decision whose option set changes is handled as follows:

- **An already-confirmed issue's option set changes before the draft is consumed** (a new candidate appears after the developer already chose an option, but before `Generate` has run). 
	- `[Convention, important]` A confirmed decision is **sticky**: it is never silently reopened just because Planning surfaced a new candidate on a later run, since auto-reopening risks discarding a deliberate human decision. But it isn't silently ignored either. The draft instead carries a visible warning against that entry: _"resolved as X; a new candidate (Y) has since appeared; review recommended."_ The developer can explicitly reopen it; the system never does so on their behalf.
	
- **A pending issue's underlying divergence disappears entirely** (fixed manually outside the tool, or the model change was reverted). Its `ID` simply no longer appears in Planning's freshly-derived issue set on the next run and is dropped from the draft and nothing is left to reconcile.
- **ID collision across unrelated issues.** `[Convention]` Composition always includes table name and operation kind alongside the artifact name specifically to prevent two unrelated divergences in different tables from ever hashing to the same identity.

#### Input validation (edge case)

`[Convention, important]` A hand-edited draft may contain a malformed decision: an out-of-range option index, two options marked chosen, a decision recorded against an `ID` that no longer exists in the current issue set. `Collect` must treat any such entry as **invalid and still pending**, surfaced as a reviewable error, not silently defaulted to any option, including `Default`.

#### Draft lifecycle (edge case)

`[Convention, important]` A draft is scoped to a single generation session, not left to accumulate indefinitely. Once `Generate` successfully consumes a `ResolvedOperationSet` and freezes a `Migration`, the draft is consumed/archived, not left present for a future `generate` run to confusingly compare against, since the "previous" state has now moved forward and old resolved entries would no longer correspond to anything Planning could regenerate.

How it is built (2026-10-24):

- `ResolveIssues` lists in `ResolvedOperationSet.Consumed` the issues whose chosen option produced operations. After the migration's files are written, `consumeAnswers` passes them to the presenter's `Consume` (`ResolutionConsumer`, which `FilePresenter` implements), and the run's message says how many were removed. A preview removes nothing.
- "Leave as-is" answers stay. They produce no operation, and the convention above makes them the way to say "don't ask again". Every issue's default is "Leave as-is", so a kept answer, or a question that starts over, never changes the database.
- Why it matters: an issue's ID names only its artifact (`alter_column_users_plan`). Before this, an "Apply alter" answer stayed in the draft until a run in which the issue was absent, and a second narrowing of the same column, with no run in between, was altered without a question.
- `[Limit]` If a written migration is deleted before it is applied and generated again, its questions come back with their defaults and have to be answered again.
- `[Limit]` A kept "Leave as-is" answer also answers a later, different change to the same column. It changes nothing, but the developer is not told that the change differs. Storing a fingerprint of the divergence with each answer would flag it.
- The migration's files are written first. If `Consume` fails, the run returns `draft_write_failed` with the IDs to remove by hand, and the files stay.

#### Output

`ResolvedOperationSet`, as defined in the Generate-stage manifesto. Every operation present is either auto-tiered (never required a decision) or was explicitly resolved by a human through this stage. `[Convention]` `Generate`'s function signature accepts only this type, never `MigrationPlan` or raw `PlanIssue` data, making it a compile-time error (not merely a documented expectation) to wire the pipeline in a way that bypasses Resolution.

```go
type ResolvedOperationSet struct {
	Operations []SchemaOperation
	Custom     []CustomMigration
}
```


## Generation stage

This stage generates the migration object with ordered list of schema operations.

### A migration's name is derived from its operations

**Context:** a file is named `<ID>_<Name>`. The generator set the name to the ID, so every generated file was `0001_0001.json`, and a folder of them said nothing about what each migration does.

**Options considered:**

- *Drop the name when it equals the ID* (`0001.json`). Shortest, and no more telling.
- *Derive it from the operations.* Readable without any input, and the same for a preview and the confirmed run after it.
- *Ask the developer for it.* The best names, and one more thing to supply on every run.

**Decision:** derive it (`migrationName` in `naming.go`). One operation is named by its kind and target (`add_column_users_plan`), several on one table by the table (`alter_notes`), new tables by their names (`create_users_sessions`), a mix by the tables it touches (`update_notes_users`), and a migration of custom migrations only by the first of their names. Beyond three tables the name is the first and a count (`create_accounts_and_6_more`). Tables are sorted first, because the planner walks a map. The name is reduced to lower-case letters, digits, and underscores and cut to 60 characters. The baseline keeps its fixed name, `baseline`.
`[Convention, important]` The ID is the number alone and is the only identity: the ledger, the snapshot's version, `DependsOn`, `LatestMigrationID`, and the file pattern read nothing else. The name is read by nothing but the file name. Files generated before this change (`0001_0001.json`) carry their name inside and keep working.

**Revisit if:** developers want to name migrations themselves. A `-name` flag on `generate` would override the derived name.

### Overall steps
Generation runs these steps:

1. Build dependency-graph with structural and explicit edges and create topological ordering
2. Build reverse operations for each schema operation 
3. Compute Next migration ID and create the object

#### Branch: structural edge inference and dependency graph building

Structural edges follow these rules:

- **Rule 1:** Any operation targeting table T depends on T's own `OpCreateTable`, if T is created within this same `ResolvedOperationSet`.
- **Rule 2:** `OpAddForeignKey` depends on its `RefTable`'s `OpCreateTable`, if `RefTable` is created within this same set.
- **Rule 3:** `OpDropForeignKey` must precede its `RefTable`'s `OpDropTable`, if both exist within this same set.
- **Rule 4:** Explicitly declared dependencies in `SchemaOperation.DependsOn` are added.
- **Rule 5:** Explicitly declared dependencies in `CustomMigration.DependsOn` are added (see *Custom Migrations*).

##### Dependency ordering / deferred constraint creation problem

The dependency ordering logic should support logically valid cyclic dependencies, such as two tables with foreign keys referencing each other. Naively creating each table with its foreign key constraint can fail because neither table can be created before the other. Instead, the generator should create all cyclic tables without their foreign key constraints first, then add those constraints afterward using `ALTER TABLE`. This allows both tables to be created successfully while preserving the intended foreign key relationships.
	**The fix: declare foreign keys separately from `CreateTable`.** The planner emits the two operations separately. `OpCreateTable` does not carry foreign keys in its `NewTable.ForeignKeys`.

### Indexes of a new table are operations of their own

**Context:** no driver creates a table's indexes in its `CREATE TABLE`: each builder reads the columns and the primary key and nothing else. `BuildPlan` used to leave a new table's indexes on the table inside `OpCreateTable` and plan nothing for them. The first migration of a new database therefore created no declared index, unique ones included (`uq_tokens_kind_lookup_hash`, `uq_accounts_provider_account`). On `PathGenerateOnly` a second run found them missing and added them. On `PathManaged` the snapshot recorded the table with its indexes, so no later diff would have.

**Options considered:**

- *Have every driver create `t.Indexes` in its create-table builder.* Four implementations, each with its own rules for an index (MySQL's prefix lengths, SQL Server's NULL filter), next to the `OpAddIndex` builders that already have them.
- *Plan one `OpAddIndex` per index.* One change in `core`. It is what the drivers' comments, `BuildBaselineMigration`, and the hand-built schemas of the test suites already assumed.

**Decision:** the planner. For a new table `BuildPlan` clears `Indexes` and `ForeignKeys` on the table it puts in `OpCreateTable`, and plans the indexes with `planIndexes`, the function that plans a missing index of an existing table, so both cases produce the same operation and the same ID (`add_index_<table>_<index>`). Rule 1 orders each after its table's creation. `BuildBaselineMigration` clears `Indexes` too: it already emitted the separate operations, and the snapshot, which is built from the operations, listed each index twice. `checkNoInlineIndexes` makes the generator reject an `OpCreateTable` that still carries an index, as `checkNoInlineForeignKeys` does for foreign keys, so a hand-written custom migration fails instead of losing its indexes.
`[Convention]` A database whose tables were created by an earlier first migration needs no special step: the next `generate` finds the indexes missing and adds them.
`TestPlannedTablesGetTheirIndexes` in the driver suite runs the planner's own output through each driver and expects the indexes to exist and a second plan to be empty.

#### Branch: explicit edge declaration

`SchemaOperation.DependsOn` / `CustomMigration.DependsOn`: the escape hatch for anything with no structural signal (a data backfill ordered relative to an unrelated table).

#### Branch: mutual table reference

**Handled, in scope:**

- Two (or more) tables whose only interdependency is via foreign keys → resolved automatically per Rule 2, with zero developer intervention. Output ordering: `CreateTable(A)`, `CreateTable(B)` (relative order between these two is now a tie, broken by the existing alphabetical/registration-order tiebreak, then `AddForeignKey(A->B)`, `AddForeignKey(B->A)` in whichever order their own dependencies resolve.
- **N-table mutual reference cycles** (A->B->C->A) → handled by the identical mechanism, since Rule 2 makes _every_ FK edge point only at a `CreateTable`, never at another FK: a cycle among FK operations themselves cannot form regardless of how many tables are involved. `[Convention]`

**Explicitly out of scope, documented for future reference:**

- **`[Deferred]`** The generator doesn't handle a pair of mutually dependent NOT NULL foreign-key columns when neither side can initially contain a valid reference. Therefore, either a multi-step migration that temporarily allows NULLs, or database-specific support for deferred foreign-key constraints(`DEFERRABLE INITIALLY DEFERRED` in Postgres) is required.
	- A good example: Assuming there are two tables `users` and `profiles`, Every `users` row must point to an existing `profiles` row, **and** every `profiles` row must point to an existing `users` row.
	
- **`[Deferred]` Circular dependencies through custom/manual migrations**: for example, a hand-authored data migration that depends on a table which itself indirectly depends (via `DependsOn`) back on that same custom migration. Since `DependsOn` is explicit and developer-authored, a cycle here is a **developer authoring error**, not a structural inevitability like the FK case. Generate's job is only to **detect and report it clearly** (via the existing `KahnSort` cycle-path reporting), never to silently resolve it. `[Convention]`

- **`[Deferred]` Circular check constraints or triggers referencing each other across tables**: genuinely rare, dialect-heavy, and not addressed by this design at all in the current phase.

- **`[Deferred]` Self-referencing single-table foreign keys** (a table with a FK to itself, for example, an `employees.manager_id → employees.id` hierarchy). This is **not actually a cycle** in the graph sense (Rule 1 already handles it: the FK operation depends on the table's own `CreateTable`, which necessarily precedes it, no different-table dependency exists at all), but worth explicitly documenting as a _non-issue_ so it's never mistaken for one of the deferred cases above. `[Convention]`

`[Question, resolved]` "should Generate attempt to resolve every theoretically possible cyclic schema"? No. The single-rule fix (FKs are never inline) resolves the one realistic, common case (mutual/circular table references) completely and generically, without needing cycle-detection-and-repair logic in the graph algorithm itself. Anything beyond that (the four deferred cases above) is rare enough, and dialect-specific enough, that hand-authoring a `CustomMigration` with explicit `DependsOn` is the correct escape hatch, not a feature to generalize now.

### Branch: reverse operation construction (`Down`)

`Down` is built as follows:

- Down is computed by walking the finalized `Up` ordering **in reverse** and inverting each operation (`invertOperation`): `OpCreateTable - OpDropTable`, `OpAddColumn - OpDropColumn` (using the retained prior definition), `OpRenameColumn` swaps its two names, `OpAlterColumn` reverts to `PrevColumn`, `OpAddIndex/FK - OpDrop*`.

- **`[Convention, important]` All-or-nothing reversibility,:** if any single step in `Up` cannot be inverted (a `CustomMigration` with no authored `Down`, a retained-definition field unexpectedly missing), the **entire** migration's `Down` is `nil` to prevent a partial rollback stopping midway.

- **Mutual-FK case, specifically:** inverting the deferred-constraint sequence is naturally symmetric and requires no special handling: `Down` of `[CreateTable(A), CreateTable(B), AddFK(A->B), AddFK(B->A)]` is simply `[DropFK(B->A), DropFK(A->B), DropTable(B), DropTable(A)]`, which is itself correctly ordered by the same reversal, and satisfies Rule 3 (`DropForeignKey` before its `RefTable`'s `DropTable`) automatically. `[Convention]` No additional rule is needed for reversing the deferred-constraint pattern: straight reversal of a correctly-ordered forward sequence is always itself correctly ordered backward, given Rules 1–5 hold.


`[Convention]` `LatestMigrationID`'s limitation for `PathGenerateOnly`: if the developer has already moved a previously generated file out of `FolderPath` (per that Path's own contract: "user is responsible to put it in their desired location"), behemoth has no way to see it and will compute IDs as if that migration never existed, producing a duplicate ID on the next run. This is an accepted consequence of `PathGenerateOnly` owning no persistent record beyond the staging folder, with a possible workaround being an explicit `--previous <id>` CLI override for that mode.

## Runner stage: Path I (apply)

**Purpose:** the Path I entry points, covering both greenfield (no live tables yet) and brownfield (adopting tables that already exist live) cases. This is the stage where a schema change reaches the database by executing the DDL and write to the ledger/snapshot tables.`[Convention, important]`

### Entry point

```go
func RunManagedGenerate(ctx context.Context, cfg MigrationConfig, declared Declared, deps MigrationDeps, confirmWrite bool) (*RunResult, error)
func RunManagedApply(ctx context.Context, cfg MigrationConfig, declared Declared, deps MigrationDeps, confirmApply bool) (*RunResult, error)
```

The steps below are shared out between the two. `RunManagedGenerate` does the detection, the introspection and the writing of a baseline (Steps 1, 2 and the first half of 4) and the ordinary generation (Step 5). `RunManagedApply` does the detection again and the recording or applying. The table under *Execution Flow* lists what each does in each state.

### Step 0: precondition

`[Convention]` Only reachable when `MigrationConfig.Path == PathManaged`. Path II stops at the generation stage. Both entry points enforce this themselves: any other path is a configuration error, returned before the folder is created.

### Step 1: folder and candidate detection

Step 1 does the following:

- `EnsureMigrationFolder` creates `cfg.FolderPath` if absent.
- `PartitionForBaseline` checks every table in `declared.Schemas` against live existence via `SchemaIntrospector.TableExists`.
    - **No candidates found** → pure greenfield. Baseline phase is skipped entirely; control passes straight to Step 5.
    - **One or more candidates found** → brownfield adoption; Baseline Phase (Steps 2–4) runs before Step 5 is reached.

`[Convention]` Behemoth detects the current database live state automatically.
### Baseline phase (steps 2 to 4, brownfield only)

### Step 2: introspection

`RunIntrospection` runs against only the candidate tables (never the full declared set: the greenfield tables have nothing to introspect).

`[Convention]` `trackExtraColumns` is always passed as `true` in this call. This differs from Path II's standing introspection call, which passes `false`.( *see the Path I/II Divergence Rule below for why the same function takes different arguments in each context*.)

### Step 3: human review (deprecated)
**Obsolete:** this step no longer applies, because Behemoth takes the existing table as it is. Behemoth does no type guessing. Only the database driver resolves types. The user can edit the generated migration. 

This is a mandatory stage with no bypass, `[Convention, important]`, split into two calls mirroring the ordinary Resolution stage's shape:

- **`BuildBaselineIssues(report)`** - the Issue Organizer half. Walks the `IntrospectionReport` and produces `(provisional map[string]TableSchema, issues []BaselineIssue, error)`. Unlike the ordinary Planning stage, this only concerns itself with **type reverse-mapping ambiguity**: a baseline has no "missing declared column" or "extra column" divergence to resolve, because there is no prior canonical record yet to diverge from. Those divergences are deliberately deferred to Step 5's first ordinary `generate` call, run immediately after baseline completes.
- `ResolveBaselineIssues(ctx, provisional, issues, presenter, interactive)`: drives every `BaselineIssue` to a decided canonical `Column`, using the same `ResolutionPresenter` interface (and therefore the same `FilePresenter` draft file) as ordinary Resolution. Output is `*ResolvedBaseline`, a type that, like `ResolvedOperationSet`, cannot represent an unresolved decision.
- **Non-interactive/CI baselining is refused entirely.** `[Convention, important]` No default answer exists for a guessed type: a wrong baseline poisons every migration generated on top of it, so unlike Path II's ordinary Resolution (which merely fails closed on unresolved issues), baseline additionally refuses to even _attempt_ a non-interactive run when any ambiguity exists.

`[Limitation, important]` `ResolveBaselineIssues` currently reuses `ResolutionPresenter`/`FilePresenter` by wrapping each `BaselineIssue` into a `PlanIssue` shell purely to reuse the existing draft wire format (label list + chosen index). `PlanIssue.Options[].Operations` is left empty in this path and is never read: the presenter only ever serializes `Label` strings and reads back a selected index, so this works today, but it is a **borrowed shape, not a shared type**. If the draft file format is ever extended to carry richer per-option data (for example supporting the deferred "override with a specified type" option below), this adapter will need to be revisited: it currently has no way to carry a `BaselineFieldOption`'s eventual free-text override through the same channel.

`[Deferred]` "Override with a specified type" is documented as a desired option for a type-ambiguity `BaselineIssue` but is not implemented: the current option-index-only draft format has no field for free-text input. Only "Accept guessed type" is currently offered as a real choice.

### Step 4: recording

`BuildBaselineMigration(tables map[string]schema.Table) Migration` constructs one `Migration{ID: "0000", Name: "baseline", IsBaseline: true}` (the file is `0000_baseline.json`) whose `Up` comprehensively reflects the recorded state: one `OpCreateTable` per table, plus separate `OpAddIndex`/`OpAddForeignKey` operations for every index and foreign key on that table. The table in `OpCreateTable` carries neither.

`[Convention, important]` A migration's ID is its number alone, and the baseline's is `0000`. The runner stores the ID as the snapshot's version, and `checkNoUnappliedMigrations` compares that version with the number a file name starts with (`LatestMigrationID`). The baseline's ID used to be `0000_baseline`: the two never matched, so every run after a recorded baseline was refused as if a migration were unapplied.

`[Convention, important]` The recorded tables come from the introspection report (`introspectedShape`): a column that **matched** its declaration is recorded **as declared**, everything else as found live. See *Column Comparison → Baseline recording*. Tables are sorted by name and columns, indexes, and foreign keys keep declaration order, so rebuilding the baseline gives an identical migration (it is regenerated and compared with the confirmed file before being recorded).

`[Convention]` `OpCreateTable.NewTable.ForeignKeys` is always empty here to prevent table-foreign-key cycles. `NewTable.Indexes` is empty too, so the snapshot built from the operations lists each index once.

Before the migration is applied, the user can review and edit the file. Then `deps.Runner.Apply(ctx, []Migration{baselineMigration})` is called with this single migration. Because `IsBaseline` is `true`, the Runner's `applyOne` routes it to `driver.RecordBaseline` instead of `driver.ApplyMigration`. See Execution Semantics below for exactly what this means and does not mean.


### Step 5: ordinary flow takeover

`[Convention, important]` Once nothing is waiting to be applied, `RunManagedGenerate` plans the next migration from the snapshot (`generateNext`), whether or not a baseline was recorded before. This applies both to greenfield and brownfield cases. Any divergence between what baseline recorded and what `current` actually declares surfaces here, through the ordinary Planning/Resolution machinery, as the first real generated migration.

### Execution semantics: `ApplyMigration` versus `RecordBaseline`

`[Convention, important]` This is the mechanism that makes "replay produces accurate state" true without literally re-executing history:

- **`ApplyMigration`** (every ordinary migration): executes every operation in `Up` as real DDL against the live database, atomically with the ledger insert and snapshot upsert, per the driver's own `AtomicityLevel`.
- **`RecordBaseline`** (the baseline migration, `0000`, only): writes the ledger row and the snapshot upsert with the **exact same atomicity contract**, but executes **zero** schema-modifying statements. The tables already exist live; issuing `CREATE TABLE` against them would simply fail.
- **What "replay" actually means, precisely stated:** nothing re-runs the baseline's DDL. What is reproduced accurately is the **in-memory snapshot projection**: `applyOperationsToSnapshot` computes the resulting canonical state from a migration's `Up` operations identically whether that migration was really executed or only recorded. The distinction between "did DDL happen" and "is the snapshot accurate" is what allows baseline to seed history without rewriting it.

### Path I and II divergence rule: `trackExtraColumns`

`[Convention, important]` `RunIntrospection` is shared code between both Paths and both a standing (Path II, every `generate`) and one-time (Path I, baseline only) use, but its `trackExtraColumns` argument differs by caller:

- **Path I baseline** → `true`. Path I owns a durable canonical snapshot; a column that exists live but was never part of any behemoth declaration is unambiguous: it's real data the tool must account for, so it belongs in the baseline recording.
- **Path II standing diff** → `false`. Path II has no prior canonical record on a first run to distinguish "behemoth's own column that isn't declared anymore" from "the developer's own unrelated custom field", per the earlier documented `[Deferred]` gap, an unmatched extra live column is silently omitted rather than risk a wrong drop candidate.

### `MigrationDeps`: dependency shape

```go
type MigrationDeps struct {
	Introspector SchemaIntrospector
	Runner       MigrationRunner
	GenerateDeps GenerateDeps       // reused verbatim from RunGenerate; Presenter, Generator live here
	Presenter    ResolutionPresenter // same underlying instance as GenerateDeps.Presenter; one draft file per run, not two
}
```

`[Convention]` `Presenter` is duplicated as a field only to make the Baseline Phase's dependency explicit without reaching into `GenerateDeps`: it must always be the same concrete instance, and not a second, independently-configured presenter, or Steps 3 and 5 would write to two different draft files in one run.

`Generate` and `Migrate` fill this struct from a `Backend` and satisfy the rule above by construction (see *Entry points*).

### Helper: `snapshotAsRegistry`

`[Implementation Detail]` Adapts a persisted `SchemaSnapshot` into the read-only `schema.Registry` shape `FromSnapshotDiff` expects as "previous." `Declare`/`ExtendColumn`/`ExtendIndex` are unreachable on this adapter: it exists purely to carry values for a diff, never to accept new registrations, and returns an `Internal`-classified error if called, since that should never happen given how it's constructed.


## Execution flow

### Path I: managed

Both commands start by reading the run state (`DetermineRunState`): whether the ledger table exists, and which files are in the folder.

| State | `generate` (`RunManagedGenerate`) | `migrate` (`RunManagedApply`) |
| --- | --- | --- |
| No ledger, empty folder, no declared table exists | Plans the first migration from scratch. Writes it with `--confirm`. | Nothing to apply. |
| No ledger, empty folder, declared tables exist | Builds the baseline from the live tables (`PartitionForBaseline`, `buildBaselineMigrationOnly`). Writes it with `--confirm`. | Nothing to apply. |
| No ledger, one baseline file | Builds the baseline again and compares. Unchanged: reports that it is waiting. Changed: writes it again with `--confirm`, to be reviewed again. | Builds the baseline again and compares. Unchanged: records it with `--confirm` (`RecordBaseline`, no DDL). Changed: refuses, and points to `generate`. |
| No ledger, one file that is not a baseline | Reports that it is waiting to be applied. | Applies it with `--confirm`. The driver creates the ledger with it. |
| No ledger, several files | Error, `unapplied_migrations_no_ledger`. | The same error. |
| Ledger, no files | Error, `ledger_without_files`. | The same error. |
| Ledger and files, one unapplied | Reports that it is waiting. Generates nothing: the snapshot the next migration is compared with would be out of date. | Applies it with `--confirm`, after the script check below. |
| Ledger and files, all applied | Plans the next migration from the snapshot. Writes it with `--confirm`, or reports no changes. | Nothing to apply. |

`[Convention, important]` A stale baseline is never recorded. Both commands build the baseline again from the live database and compare it with the file on disk, because the file is a record of the database as it was reviewed. Only `generate` writes the new one.

#### The managed path writes the script at generate, and checks it at migrate

**Context:** the managed path used to render a migration's script just before applying it and write it just after, so that a script only existed for a migration that ran. With generating and applying apart, the review between them had the `.json` alone to go by.

**Options considered:**

- *Write it at generate only.* The script could then differ from what was executed: SQLite's table rebuilds, most SQL Server operations and MySQL's index on a text column derive statements from the live schema, which can change before `migrate` runs.
- *Write it at generate and overwrite it at migrate.* The file ends up as what ran, but what ran was never reviewed.
- *Write it at generate, and have migrate compare.* `migrate` renders again against the database as it is and applies only when the result equals the script on disk.

**Decision:** the third. `RunManagedGenerate` writes the `.json` and the script together (`writeForReview`), rendering first so that a failure writes neither, as `RunGenerateCLI` does. `RunManagedApply` renders again and compares byte for byte (`scriptWentStale`). The header carries the migration's `CreatedAt`, which is in the file, so an unchanged migration renders identically. On a difference it writes the new script, applies nothing and reports it; the next `migrate` finds them equal. This is the baseline's rule applied to scripts: what is applied is what was reviewed. On PostgreSQL the script is built from the operations alone and cannot differ.
`[Convention]` A migration without a script on disk is applied without the check and gets its script then: a driver that does not render, a file from before this change, or a script someone deleted. A render failure at `migrate` is a warning, as before.
`[Convention, important]` The script on this path is a rendering for review. `migrate` applies the operations in the `.json`. Running the script by hand leaves the ledger without the migration, and the next `migrate` applies it again.

`[Convention]` The fourth row is a new database after its first `generate`: no ledger table exists yet because drivers create it with the first migration they record. `DetermineRunState` returns the ongoing state for it. Several files without a ledger are still refused: the tool never leaves that state behind. `[Known limitation]` A single file that was applied by hand, before the application moved to this path, is taken for unapplied too. Applying it fails in the database because its tables exist, and on a driver with full atomicity nothing is recorded.

### Path II: generate-only

Path II runs these steps:

1. Run command: the application calls `Prepare` (not `Boot`), builds the migration backend with `PreparedApp.Resolver`, and calls `Generate(ctx, app.Migration, app.Declared(), backend, RunOptions{Confirm: confirmWrite})`, which calls `RunGenerateCLI`. `behemoth generate` is this step as a command (see [`cli.md`](cli.md)).
2. Introspect the live database against `Declared.Schemas` (`trackExtraColumns = false`), normalizing declarations through the driver's `ColumnNormalizer`; reject unmappable types.
3. Plan → attach pending custom migrations → Resolve (draft file) → Generate.
4. Without `confirmWrite`: report the migration that would be written. With it: render the script first (a render failure writes nothing), then write the `.json` and the script to `FolderPath`. Applying it is the developer's own tool's job.
			

## Custom migrations

A `CustomMigration` is a hand-authored, named group of operations that rides along with the generated ones: it is ordered among them in the dependency graph, frozen into the same `Migration`, and applied (Path I) or handed off (Path II) exactly like generated operations. It exists for steps the differ cannot derive from comparing declared and live shapes.

```go
type CustomMigration struct {
	Name      string            // permanent identity, see Emitted Once below
	Up        []SchemaOperation
	Down      []SchemaOperation // optional; nil makes the whole migration irreversible
	DependsOn []string          // generated operation IDs, or other custom migrations by Name
}
```

`[Convention, important]` **Emitted exactly once.** A custom migration is frozen into one generated `Migration` and is history from then on. The `Name` is the only thing that tracks this, so names are permanent.

### Stage 0: declaration

Declaration works as follows:

- The application declares custom migrations in `PrepareConfig.Migrations`. `Prepare` carries them on `PreparedApp.Custom`, and `PreparedApp.Declared()` pairs them with the frozen schema registry into `migration.Declared{Schemas, Custom}`, the single input every migration entry point takes (see *Entry points*).
- `Prepare` validates them up front, before plugin ordering, via `ValidateCustomMigrations`. Each rule fails `Prepare` with a configuration error:
	- every custom migration has a non-empty `Name`, and names are unique
	- every operation, in `Up` and `Down`, has a non-empty `ID`
	- no operation ID is used by two different custom migrations
	- no operation ID equals a custom migration `Name` (both are node identities in the Generation stage graph)
- These are the checks that need no plan. Collisions with *generated* operation IDs depend on the plan and are checked at Generation.
- `[Deferred]` **Plugin-declared custom migrations.** `PluginInitContext` lives in `core`, which cannot reference `migration` types without an import cycle. Supporting plugins means moving `SchemaOperation` and `CustomMigration` into `schema`, which is deferred until a plugin needs it.

### Stage 1: introspection and diff

Introspection and diff treat custom migrations as follows:

- Custom migrations take no part. Introspection compares `Declared.Schemas` against the live database (Path II) or the snapshot (Path I) only.
- **Baseline phase (Path I, brownfield):** the baseline records live state only and never includes custom migrations. Pending custom migrations go into the first ordinary migration generated after the baseline is recorded.

### Stage 2: planning

Planning treats custom migrations as follows:

- `buildMigrationPlan` runs `BuildPlan` on the schemas as usual, then sets `MigrationPlan.Custom` to the *pending* custom migrations (`pendingCustomMigrations`):
	1. Read every migration file in `FolderPath` (`ReadDiskMigrations`).
	2. Collect the names listed in each file's `Migration.Custom`.
	3. Keep only the declared custom migrations whose `Name` is not among them, in declaration order.
- Custom migrations never produce `PlanIssue`s and are never tiered. The planner did not derive them from a divergence, so there is nothing for it to propose options for.
- `[Convention]` A plan with no generated operations but at least one pending custom migration is still a migration. Generation proceeds and does not report "nothing to generate".

### Stage 3: resolution

Resolution treats custom migrations as follows:

- Pass-through. `ResolveIssues` copies `MigrationPlan.Custom` into `ResolvedOperationSet.Custom` untouched.
- Custom migrations never appear in the draft file and need no developer decision: declaring one is the decision. The fail-closed gate on unresolved issues applies to generated operations only.

### Stage 4: generation

Checks, before the graph is built (both abort generation):

- **Inline foreign keys:** `checkNoInlineForeignKeys` covers each custom migration's `Up`. An `OpCreateTable` carrying `ForeignKeys` is rejected, and the foreign key must be authored as its own `OpAddForeignKey`, same as the planner's output.
- **Inline indexes:** `checkNoInlineIndexes` does the same for `Indexes`. No driver creates them from the table, so each must be authored as its own `OpAddIndex`.
- **Collisions with generated operations:** `checkCustomCollisions` rejects a custom migration whose `Name`, or any `Up`/`Down` operation ID, equals a generated operation ID in this plan. Once frozen, `Up` and `Down` are flat lists, and `Down` construction, rendering, and the runner all key on operation IDs, so two operations with one ID cannot coexist in a migration.

Dependency graph:

- Each custom migration is **one node**, keyed by its `Name`, not one node per operation. Its operations are never interleaved with anything else.
- **Rule 1 applies:** the node depends on `OpCreateTable` of every table its `Up` operations target, when that table is created in the same plan.
- **Rules 2 and 3 do not apply.** They are evaluated for generated foreign-key operations only. A custom migration that needs a referenced table created first must say so through `DependsOn`.
- **Rule 5 - explicit edges:** each `DependsOn` entry becomes an edge. An entry may name a generated operation ID or another custom migration's `Name`.
- `[Convention]` A `DependsOn` entry naming something not in this plan is dropped silently, on the same assumption as every other edge: the target was applied in an earlier migration. The consequence is that a typo, or a reference to an operation *inside* another custom migration (those IDs are not graph nodes), is also dropped silently rather than reported.
- Cycles through `DependsOn` are authoring errors, reported with `KahnSort`'s cycle path (see Mutual Table Reference above).

Freezing:

- **`Up`:** walking the sorted nodes, a custom node contributes its `Up` operations as one contiguous block, in authored order.
- **`Down`:** walking the sorted nodes in reverse, a custom node contributes its authored `Down` as written. It is **not** inverted or reversed, so it must already be in execution order. A custom migration with a non-empty `Up` and no `Down` makes the **entire** migration's `Down` nil, per the all-or-nothing rule in Reverse Operation Construction.
- **`Migration.Custom`:** the names of every custom migration frozen into this migration, sorted so that a regenerated migration compares equal to the one on disk. The field is omitted from the JSON when empty, so migrations without custom migrations keep their existing file shape.

### Stage 5: write and render

Writing and rendering treat custom migrations as follows:

- The migration file carries `"Custom": [...]`. This field is the only durable record that a custom migration was emitted, and the next Planning stage reads it back.
- Custom operations are `SchemaOperation`s, so the driver's `MigrationRenderer` renders them into the `.sql` script with no special handling.

### Stage 6: apply

Applying treats custom migrations as follows:

- **Path I:** the runner applies the migration's `Up` as a unit through `ApplyMigration`. Custom operations run in the same transaction as the generated ones, where the driver's atomicity level allows. `applyOperationsToSnapshot` folds every `Up` operation, custom ones included, into the next `SchemaSnapshot`, so the snapshot reflects custom changes for the next diff.
- **Path II:** behemoth stops at the written files, and the developer's migration tool runs the script.

### After emission: identity rules

After a custom migration is emitted, these rules apply:

- Editing an emitted custom migration in code has no effect. The frozen file is what applies.
- Deleting an emitted custom migration from code is harmless. It is already history.
- `[Convention, important]` **Renaming** an emitted custom migration makes it a new one, and it **is emitted again** under the new name. **Reusing** an emitted name for a different migration means the new one is **never emitted**. Neither case is detectable, because the name is the identity.
- `[Convention]` **Path II limitation**, the same one as `LatestMigrationID`: when the developer moves a generated file out of `FolderPath`, its `Migration.Custom` record goes with it, and every custom migration it contained is emitted again on the next run. As with migration IDs, behemoth owns no record beyond the staging folder.

### Known limitations

These limitations apply:

- `[Important]` **No safe schema-only use yet.** The differ converges on the declared schema regardless of custom migrations, so with only schema operations available:
	- a custom operation that **overlaps** the declared schema (for example adds a column the registry also declares) is generated by the differ as well, because the diff runs before the custom migration is applied, so the change is applied twice. Both paths. If the custom operation happens to reuse the generated operation's ID, the collision check rejects the migration instead;
	- a custom operation that goes **beyond** the declared schema (for example an index the registry doesn't declare) is planned for removal on the next run **in Path I**: the snapshot diff tracks extras, so an extra index or foreign key is dropped and an extra column is raised as a drop issue. **Path II** ignores live extras (`trackExtraColumns = false`), so there it survives, but only by that rule, not by design.

	`[Convention]` A custom migration must end at the declared schema, not elsewhere. Its legitimate uses are steps the differ cannot infer, ordered relative to generated operations, and those are mostly data steps.
- `[Deferred]` **Data operations.** `SchemaOperation` has only structural kinds, so the backfill use case named above is not expressible yet. The intended shape is an `OpExec` kind carrying per-driver SQL and an optional `Down`. Drivers execute and render it like any other operation, and it leaves the snapshot untouched.
- `[Deferred]` **Plugin-declared custom migrations**, see Stage 0.

## Constraints (`Check`): removed, with a deferred design

### Why `Check` was removed

`Column.Check` and `ColumnOverride.Check` held a raw SQL expression rendered as a `CHECK` constraint. It was removed because it was half a feature:

- **Never read back:** neither introspector reported it, and `columnsEqual` never compared it, so a changed check was never migrated and nothing said so.
- **Not database-neutral:** an untyped SQL string in a model meant to describe any database, with no meaning for document stores.

`[Convention]` What remains is deliberate: SQLite's table rebuild still parses and **preserves** `CHECK` constraints that already exist in a live table (`itemCheck` in `rebuild.go`). That is about not destroying the database's existing constraints, not about declaring new ones.

### Deferred: a future design for constraints

The design would have these parts:

1. **Typed, declarative constraints instead of SQL text**: for example, `Enum []any`, `Min`/`Max`, `MinLength`/`MaxLength`, `Pattern`. Structured values can be compared and diffed; strings of SQL can't.
2. **Table-level and named, like indexes**: `schema.Table.Constraints []Constraint{Name, Columns, ...}`. A name is what makes diffing and dropping possible (Postgres needs one to drop a constraint), and multi-column rules (`starts_at < ends_at`) need table level anyway. Contributions from other plugins would work like `ExtendIndex`.
3. **Rendered natively per driver**: SQL drivers as named `CHECK` constraints; MongoDB as a `$jsonSchema` collection validator (which supports `enum`, `minimum`/`maximum`, `minLength`/`maxLength`, `pattern`).
4. **Introspected and normalized like everything else**: each driver reads its constraints back into the same structure, and `NormalizeColumn`'s counterpart for constraints absorbs whatever the database rewrites (Postgres re-formats `CHECK` expressions; a probe decides how).
5. **Narrowing**: adding or tightening a constraint is narrowing (existing rows may violate it); removing or loosening one is widening.
6. **Also enforced in the application**, so databases that enforce nothing still get the guarantee.
7. **No raw escape hatch in core.** Database-specific SQL belongs in a custom migration, once data operations (`OpExec`) exist.

## Model extensions: contributed columns at runtime

A table can gain columns from declarers other than its owner: a plugin's `ExtendColumn` (`two_factor_enabled` on `users`), or the application's (`plan` on `users`). Migrations always created those columns: `schema.Registry` merges contributions into the table, but at runtime they were invisible: every adapter derived its column list from the Go model's own `ToMap`, so a contributed column was never inserted, never selected, and the store rejected updates to it as unknown. A contributed `NOT NULL` column without a default made **every** insert into the table fail.

This section describes how contributed columns now travel between the database and the models.

### 1. The schema decides which columns exist

The schema works as follows:

- `behemoth.SchemaResolver` has `Columns(canonicalTable) []string`: the table's declared columns, canonical names, in declaration order, contributions included (base columns first, as the registry merges them). `migration.DefaultSchemaResolver` serves it from `SchemaResolverTable.ColumnOrder`, built by `BuildSchemaResolverTable` from the frozen registry. `IdentityResolver` returns `nil`.
- Adapters read through `adapters.ReadColumns(resolver, model, selected)`: the resolver's columns when it knows the table, otherwise the model's own `ToMap` keys (sorted, so query text is stable), which is the fallback for tables nobody declared, such as the migration ledger. `QueryOptions.Select` narrows that list.
- `[Convention]` The column list for **reads** comes from the schema; the column list for **writes** comes from the model's row (`ToMap`, extras included). A contributed column a model doesn't carry a value for is simply not written, and the database applies its default.

### 2. Models carry contributed values

Models carry values as follows:

- `behemoth.Extensible` is a model with `Extras() M` and `SetExtra(column, value)`. `models.Extension` (embeddable) implements it; `User`, `Session`, and `Token` embed it.
- `ToMap` merges the extras into the row; the model's own columns win (the registry already rejects a contribution that reuses a base column's name, so they never legitimately collide).
- `FromMap` keeps every column that isn't one of the model's own as an extra, and **replaces** the extras each time. A model re-read never keeps stale values.
- Each model's own columns are listed once (`userColumns`, ...) from its column constants, the same constants its `ToMap`/`FromMap` and its table declaration use.

### 3. Typed access: `schema.Field[T]`

```go
var TwoFactorEnabled = schema.Field[bool]{Table: "users", Name: "two_factor_enabled"}

ic.Schemas.ExtendColumn(TwoFactorEnabled.Contribution(schema.Column{Type: schema.ColTypeBoolean, Default: false}))
on, ok, err := TwoFactorEnabled.Get(user)     // ok=false: no value (absent or NULL)
_ = TwoFactorEnabled.Set(newUser, true)       // before CreateUser
changes, err := TwoFactorEnabled.Update(true) // for Store.UpdateUser
```

- A field declares its type once; plugins never handle raw column-name strings or untyped values.
- `Get` converts what drivers actually return: integers and `0/1` into `bool`, `[]byte` into strings and numbers, `int64`/`float64` across numeric kinds (losslessly), text into `time.Time`, named types by their kind (`type Plan string`). Anything else is an error naming the field and suggesting a codec.
- `Decode`/`Encode` replace the built-in conversions, the per-column codec for a type no driver maps (an enum, a database-specific type).
- `Get`/`Set` on a model of another table is an error.

### 4. The store

The store handles contributed columns as follows:

- `store.WithSchema(resolver)` tells the store each table's columns; `Boot` passes `PreparedApp.Resolver`. The store's column check (create rewrites, updates, update-hook rewrites) accepts a model's own columns **and** the declared ones, so contributed columns can be written; a typo is still a validation error naming the column. Without the option, only a model's own columns are accepted.
- Data hooks see contributed columns in the row like any other column, and may set them: this is how a contributing plugin fills its own column on create.

### 5. Core declares its tables

`models.UserTableSchema()`, `SessionTableSchema()`, and `TokenTableSchema()` describe `users`, `sessions` (foreign key to `users`, cascade; index on `user_id`) and `tokens` (unique on `kind, lookup_hash`; index on `kind, subject`), built from the models' column constants; `CoreDeclareSchema` declares them. Without a declaration the resolver wouldn't know the table and contributions would fall back to invisibility. A test checks each declared table lists exactly its model's own columns. Ids are bounded strings (`VARCHAR(36)`), not a database UUID type, so every driver stores and returns the string the models hold.

### 6. Adapter support

| Adapter | Contributed columns |
| --- | --- |
| SQLite, Postgres, MySQL, SQL Server | Read through `ReadColumns`; physical names mapped through the resolver. |
| MongoDB | Documents carry every field; `CanonicalFields` maps contributed physical names back using the declared columns. |
| GORM, bun | Map-based I/O through the application's `*gorm.DB` / `bun.IDB` (below); same behavior as the SQL adapters. |

**ORM adapters: map-based I/O.** `gorm.NewGormAdapter(db, resolver)` and `bun.NewBunAdapter(db, resolver)` run every operation through the application's `*gorm.DB` / `bun.IDB` (its pool, dialect, logger, hooks, or plugins, and transactions), but rows never pass through the ORM's struct mapping:

- **Writes**: GORM's `Table(physical).Create(row)` / `Updates(row)`, bun's `NewInsert().Model(&row).TableExpr(physical)` / `NewUpdate().Set(...)`, where `row` is the model's `ToMap` keyed by physical column (`PhysicalDocument`). Extras are in `ToMap`, so contributed columns are written.
- **Reads**: a select of the physical `ReadColumns` with the physical condition, executed with `Rows()` (both ORMs have it), scanned into `ScanTargets` and handed to `FromMap` under canonical names, the SQL adapters' path, so drivers' value shapes are the same.
- **Conditions**: `PhysicalExpression`, rendered with `?` placeholders; GORM rewrites them to the dialect's bind variables, bun formats the arguments into the query itself.
- **Single-row operations**: `UpdateOne` / `DeleteOne` pick the row as `pk IN (SELECT pk FROM (SELECT pk ... WHERE expr LIMIT 1) AS _sub)` and repeat `expr` on the row (the guard). The derived table is MySQL's requirement (no subquery on the table being updated) and is valid everywhere, so there's no per-dialect branch. Zero matches are `NotFound` via `ExpectOneRow`, with the count fallback for dialects reporting changed rows.
- **Errors**: unique and foreign-key violations map to `DuplicateKey` / `ForeignKeyViolation` whatever driver the application uses. A duplicate key names the columns of the key it broke ([`../database/adapters/unique_keys.md`](../database/adapters/unique_keys.md)). GORM: the dialect's `gorm.ErrorTranslator`, applied whether or not the application set `Config.TranslateError`. bun has no translator: `adapters.ConstraintKind` reads the SQLSTATE where the driver exposes one (`SQLState()`: lib/pq, pgx; `Field('C')`: bun's pgdriver) and otherwise the engine's code in the message (SQLite, MySQL 1062 / 1452, SQL Server).
- **Transactions**: bun's `RunInTx` is on `bun.IDB`, so an adapter holding a `bun.Tx` nests as a savepoint instead of refusing.
- `[Decision]` **ORM model hooks don't run** on behemoth's models (GORM's `BeforeCreate`, bun's `BeforeAppendModel`, ...): the ORM never sees a struct. Behemoth's data hooks (`data.user.beforeCreate`, ...) are the extension point for these writes. Behemoth's models need no gorm or bun tags.

`tests/store/contract_test.go` runs the store, session manager and token manager (including concurrent single-use consumption) over raw SQLite / Postgres as control, and GORM and bun over SQLite / Postgres, with behemoth's own models and a contributed column under another physical name.

### 7. Conventions

Follow these conventions:

- `[Convention, important]` **Never build a model from a request body** (`FromMap(payload)`). With extensions, every column a model knows (contributed ones included) would become client-writable (mass assignment: `"role": "admin"`, a verified flag). Copy an explicit list of fields; other plugins add their columns through data hooks. (`emailpassword`'s sign-up did this, and was fixed with the change; its test sends `"email_verified": true` and `"role": "admin"` and checks neither is stored.)
- `[Convention]` A contributed column that is `NOT NULL` needs a `Default`, or the contributing plugin must set it on every create (a `data.<table>.beforeCreate` hook); otherwise inserts into the table fail.
- `[Convention]` A contribution is owned by its declarer: other plugins read it through the declarer's exported `schema.Field`, never by name.

### Lifecycle of a contributed column

A contributed column goes through these steps:

1. **Declare**: the two-factor plugin calls `ic.Schemas.ExtendColumn(TwoFactorEnabled.Contribution(...))` in `Declare`.
2. **Prepare**: the registry merges it into `users`; the resolver lists it in `Columns("users")`.
3. **Migrate**: the planner sees a declared column missing live and adds it (`OpAddColumn`, automatic).
4. **Create**: `CreateUser` writes the model's row; the column is set if the model (or a before-create hook) set it, otherwise the database default applies.
5. **Read**: adapters select every declared column; `FromMap` puts `two_factor_enabled` into the user's extras; `TwoFactorEnabled.Get(user)` returns `true`/`false`, converted from whatever the driver returned.
6. **Update**: `ac.Store.UpdateUser(ctx, id, TwoFactorEnabled.Update(true))` passes the store's column check because the schema lists the column.

## Custom user models: replaced by canonical models and extensions

### Why custom models existed

Two reasons: an application with an existing `User` struct (and table) could integrate without switching to `models.User`, and a user table with a column whose type the database driver can't map (see *Branch - Type Reverse-Mapping Ambiguity*) could be handled by a model with its own `ToMap`/`FromMap`.

### Why they are removed

Custom models were removed for these reasons:

- **They don't solve contributions.** A custom model knows its own fields, not the columns other plugins contribute (`two_factor_enabled`), so it would need the extension mechanism anyway.
- **They cost an interface.** Core and plugins need to read and write user fields (email, verified flag, name, ...): with arbitrary models that means a `UserModel` interface with an accessor per field, a prototype (`AuthContext.User`), and type assertions on every store call. Plugins never get a concrete type.
- **The unsupported-type case is per column, not per model.** Reimplementing a whole model (`New`, `ToMap`, `FromMap`, every core field) to handle one column is the wrong granularity; a per-column codec covers it.

**Decision:** one canonical model per core table, plus extensions (`models.Extension`), typed field keys (`schema.Field[T]`) and per-column codecs. `behemoth.User`, `AuthContext.User`, `models.UserFactory`, the user-model fields of the legacy `behemoth.Config`/`DatabaseConfig`, and the custom-user examples are removed. The OAuth profile a provider returns (`behemoth.UserInfo`, previously `models.UserInfo` posing as a model to satisfy `behemoth.User`) is plain data in package `behemoth`; `Provider.FetchUserInfo` returns `*UserInfo`. `UserInfo`, `Provider`, `User.FromUserInfo`, and the `providers` package were removed later, ahead of the OAuth redesign. Core code that needs "the id of whatever model this is" (audit subjects) reads `Model.PrimaryKeyField()`.

### Scenarios

**1. The application has its own `User` struct with extra fields** (`plan`, `company`).
- Each extra field becomes a contribution the application declares in `PrepareConfig.Schema` (`reg.ExtendColumn(Plan.Contribution(...))`), with one `schema.Field` per column.
- At runtime the values live in the user's extras; the application reads them through its fields. To keep its own type it can wrap the model:
  ```go
  type AppUser struct{ *models.User }
  func (u AppUser) Plan() string { p, _, _ := Plan.Get(u.User); return p }
  ```
  or convert at its own boundary.
- **Existing table, Path II:** the contributions match the live columns; the diff is clean if the declared types match (otherwise ordinary alter/narrowing rules apply).
- **Existing table, Path I:** the baseline records the declared shape for matching columns (see *Baseline recording*).

**2. An existing users table has a column the driver can't map** (a Postgres enum `mood`).
- **Not needed by behemoth:** leave it undeclared. It's not in `Columns("users")`, so it is never selected and never written; Path II ignores undeclared live columns. **Path I's baseline still stops** on it: it records every live column, and an unmappable one is rejected (`RejectAmbiguousTypes`), unchanged from before.
- **Needed at runtime:** declare it with a canonical type the driver can read and write as (a string, for an enum) and give its `schema.Field` a `Decode`/`Encode` for the Go type. Reads and writes then work.
- **Migrations with such a column declared:** the introspector still can't map the live type, so generation stops for it in both paths. `[Deferred]` A per-driver *native type override* ("this column is `mood` in Postgres") would let the declaration describe it exactly; until then such a column must be managed outside behemoth's migrations. Custom models never solved this part either: they only covered runtime serialization.

**3. A plugin needs a column on users** (two-factor). Covered by *Model Extensions*: `schema.Field` + `ExtendColumn` in `Declare`, a before-create hook if the column has no default, `Get`/`Update` at runtime.

**4. An existing users table with a different primary-key type** (integer, auto-increment). `[Known limitation]` `models.User.ID` is a string and core tables reference users by `VARCHAR(36)`; such a table can't be adopted as `users` directly. Options (deferred until needed): a string-typed id column alongside, or making the id type configurable for core tables.

**5. An existing users table with extra columns behemoth shouldn't touch.** Undeclared columns are never read or written, and Path II never plans them. In Path I the baseline records them (it tracks extras), so every later run raises each as a drop issue needing confirmation (default: leave as-is). Declare them as contributions to stop the question.
