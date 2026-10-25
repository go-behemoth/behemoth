package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/cli"
	"github.com/go-behemoth/behemoth/core"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/plugins/emailpassword"
	"github.com/go-behemoth/behemoth/schema"
	"github.com/go-behemoth/behemoth/storage/adapters/mysql"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backendDialect is what the backend tests need to know about one database:
// its MigrationBackend, how its script quotes a name, and how an
// application's own tool runs a script.
type backendDialect struct {
	newBackend func(db *sql.DB, resolver behemoth.SchemaResolver) migration.Backend
	quote      func(name string) string
	apply      func(ctx context.Context, db *sql.DB, script string) error
}

var postgresDialect = backendDialect{
	newBackend: postgres.MigrationBackend,
	quote:      func(name string) string { return `"` + name + `"` },
	apply: func(ctx context.Context, db *sql.DB, script string) error {
		_, err := db.ExecContext(ctx, script) // lib/pq runs several statements in one call
		return err
	},
}

var mysqlDialect = backendDialect{
	newBackend: mysql.MigrationBackend,
	quote:      func(name string) string { return "`" + name + "`" },
	apply:      execStatements,
}

// execStatements runs a MySQL script one statement at a time, as the mysql
// client does. The test's connection has no multiStatements, which one call
// for the whole script would need. The scripts here have no semicolon at the
// end of a line inside a statement.
func execStatements(ctx context.Context, db *sql.DB, script string) error {
	var body strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, "--") {
			body.WriteString(line + "\n")
		}
	}
	for _, stmt := range strings.Split(body.String(), ";\n") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return nil
}

// ownBackend is d's backend for app, with Close left unset: db belongs to
// the test.
func ownBackend(d backendDialect, db *sql.DB, resolver behemoth.SchemaResolver) migration.Backend {
	backend := d.newBackend(db, resolver)
	backend.Close = nil
	return backend
}

// scriptOf returns the script generate wrote next to m.
func scriptOf(t *testing.T, cfg migration.MigrationConfig, m *migration.Migration) string {
	t.Helper()
	require.NotNil(t, m)
	b, err := os.ReadFile(filepath.Join(cfg.FolderPath, m.ID+"_"+m.Name+".sql"))
	require.NoError(t, err, "the backend's renderer writes the script")
	return string(b)
}

func operations(m *migration.Migration) string {
	if m == nil {
		return "no migration"
	}
	ids := make([]string, len(m.Up))
	for i, op := range m.Up {
		ids[i] = op.ID
	}
	return strings.Join(ids, ", ")
}

// migrationBackend drives a dialect's MigrationBackend the way the behemoth
// command line does, through migration.Generate, and then checks the two
// halves of it that only the managed path uses.
//
// The table has a physical name of its own and a renamed column. Rendering,
// introspection and the ledger all go through the resolver the backend was
// given, so a backend that dropped it would fail here.
func migrationBackend(t *testing.T, db *sql.DB, d backendDialect) {
	ctx := context.Background()

	members := schema.Table{
		Name: "members", PhysicalName: "app_members",
		Columns: []schema.Column{
			{Name: "id", Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: "email", PhysicalName: "email_address", Type: schema.ColTypeString, Length: 255},
		},
	}
	registry := schema.NewRegistry()
	require.NoError(t, registry.Declare(tableModel{name: members.Name}, members))
	require.NoError(t, registry.Freeze())

	cfg := migration.NewMigrationConfig(migration.MigrationConfig{FolderPath: filepath.Join(t.TempDir(), "migrations")})
	resolver := migration.NewSchemaResolver()
	resolver.Freeze(migration.BuildSchemaResolverTable(registry, cfg))

	backend := ownBackend(d, db, resolver)
	declared := migration.Declared{Schemas: registry}

	// Without Confirm the migration is reported and nothing is written.
	res, err := migration.Generate(ctx, cfg, declared, backend, migration.RunOptions{})
	require.NoError(t, err)
	require.Equal(t, migration.StatusAwaitingConfirmation, res.Status)
	require.NoFileExists(t, filepath.Join(cfg.FolderPath, "0001_create_members.json"))

	res, err = migration.Generate(ctx, cfg, declared, backend, migration.RunOptions{Confirm: true})
	require.NoError(t, err)
	require.Equal(t, migration.StatusGenerated, res.Status)
	require.FileExists(t, filepath.Join(cfg.FolderPath, "0001_create_members.json"))

	script := scriptOf(t, cfg, res.Migration)
	assert.Contains(t, script, d.quote("app_members"))
	assert.Contains(t, script, d.quote("email_address"))

	// The application's own tool applies the script.
	require.NoError(t, d.apply(ctx, db, script))

	// The introspector reads the physical names back as the declared ones,
	// so the database now matches.
	res, err = migration.Generate(ctx, cfg, declared, backend, migration.RunOptions{Confirm: true})
	require.NoError(t, err)
	assert.Equal(t, migration.StatusNoChanges, res.Status, res.Message)

	// The managed path's halves: what Driver records, DB reads back.
	runner := migration.NewMigrationRunner(backend.DB, backend.Driver, cfg, nil)
	next := migration.Migration{ID: "0002", Up: []migration.SchemaOperation{
		addColumnOp(members.Name, schema.Column{Name: "nickname", Type: schema.ColTypeString, Length: 64, Nullable: true}),
	}}
	pending, err := runner.Pending(ctx, []migration.Migration{next})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, runner.Apply(ctx, pending))

	pending, err = runner.Pending(ctx, []migration.Migration{next})
	require.NoError(t, err)
	assert.Empty(t, pending, "the adapter reads the ledger the driver wrote")
	snapshot, err := runner.LoadSnapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, next.ID, snapshot.Version)
}

// notesTable is an application table next to core's: it references users,
// has an index, and a title of the given length, which a later version
// narrows. extra are columns a later version adds.
func notesTable(titleLength int, extra ...schema.Column) schema.Table {
	notes := schema.Table{
		Name: "notes",
		Columns: []schema.Column{
			{Name: "id", Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
			{Name: "user_id", Type: schema.ColTypeString, Length: 36},
			{Name: "title", Type: schema.ColTypeString, Length: titleLength},
			{Name: "body", Type: schema.ColTypeText},
			{Name: "created_at", Type: schema.ColTypeTimestamp},
		},
		Indexes: []schema.Index{{Name: "idx_notes_user_id", Columns: []string{"user_id"}}},
		ForeignKeys: []schema.ForeignKey{{
			Name: "fk_notes_user", Columns: []string{"user_id"},
			RefTable: "users", RefColumns: []string{"id"}, OnDelete: schema.FKCascade,
		}},
	}
	notes.Columns = append(notes.Columns, extra...)
	return notes
}

var pinned = schema.Column{Name: "pinned", Type: schema.ColTypeBoolean, Default: false}

// prepareNotesApp prepares an application with the email/password plugin,
// so every core table is declared, and notes.
func prepareNotesApp(t *testing.T, dir string, path migration.MigrationPath, notes schema.Table) *engine.PreparedApp {
	t.Helper()
	app, err := engine.Prepare([]core.Plugin{emailpassword.New(emailpassword.Options{})}, engine.PrepareConfig{
		Migration: migration.MigrationConfig{FolderPath: dir, Path: path},
		Schema:    func(reg schema.Registry) error { return reg.Declare(tableModel{name: notes.Name}, notes) },
	})
	require.NoError(t, err)
	return app
}

// requireLiveMatches compares app's declarations with the live database, as
// the generate-only path does, and fails unless they match: the introspector
// reads back what the driver created.
func requireLiveMatches(t *testing.T, ctx context.Context, app *engine.PreparedApp, backend migration.Backend) {
	t.Helper()
	cfg := app.Migration
	cfg.Path = migration.PathGenerateOnly
	cfg.FolderPath = t.TempDir()
	res, err := migration.Generate(ctx, cfg, app.Declared(), backend, migration.RunOptions{})
	require.NoError(t, err)
	require.Equal(t, migration.StatusNoChanges, res.Status, "%s\noperations: %s", res.Message, operations(res.Migration))
}

// requireColumnLength reads table.column's length back from the database.
func requireColumnLength(t *testing.T, ctx context.Context, backend migration.Backend, table, column string, want int) {
	t.Helper()
	live, err := backend.Introspector.Introspect(ctx, table)
	require.NoError(t, err)
	for _, col := range live.Schema.Columns {
		if col.Name == column {
			assert.Equal(t, want, col.Length, "%s.%s", table, column)
			return
		}
	}
	t.Errorf("%s has no column %s", table, column)
}

// applyAlter answers the question narrowing notes.title raises, before the
// run, as -answer does.
func applyAlter(app *engine.PreparedApp) *migration.FilePresenter {
	file := migration.NewFilePresenter(migration.DraftPath(app.Migration))
	file.Given = map[string]string{"alter_column_notes_title": "Apply alter"}
	return file
}

// migrationPipeline runs the pipeline the command line drives over every
// table core declares, and one of the application's own: on the generate-only
// path, on the managed path, from a baseline, and through the command line
// itself. On each path a later version narrows a column, which raises a
// question, and adds one. After each migration, the declarations are compared
// with the live database, which checks that the introspector reads back
// what the driver created.
func migrationPipeline(t *testing.T, db *sql.DB, d backendDialect, dropAll func(context.Context) error) {
	ctx := context.Background()

	t.Run("GenerateOnly", func(t *testing.T) {
		require.NoError(t, dropAll(ctx))
		dir := filepath.Join(t.TempDir(), "migrations")
		app := prepareNotesApp(t, dir, migration.PathGenerateOnly, notesTable(64))
		backend := ownBackend(d, db, app.Resolver)

		res, err := migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
		require.NoError(t, err)
		require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
		require.NoError(t, d.apply(ctx, db, scriptOf(t, app.Migration, res.Migration)))
		requireLiveMatches(t, ctx, app, backend)

		// A later version narrows title and adds pinned. The run stops on
		// the question, and an answer given before the run settles it.
		app = prepareNotesApp(t, dir, migration.PathGenerateOnly, notesTable(32, pinned))
		backend = ownBackend(d, db, app.Resolver)
		_, err = migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
		require.True(t, errs.IsCode(err, errs.CodeMigrationUnresolvedIssues), "%v", err)
		res, err = migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true, Presenter: applyAlter(app)})
		require.NoError(t, err)
		require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
		require.NoError(t, d.apply(ctx, db, scriptOf(t, app.Migration, res.Migration)))
		requireLiveMatches(t, ctx, app, backend)
		requireColumnLength(t, ctx, backend, "notes", "title", 32)
	})

	t.Run("Managed", func(t *testing.T) {
		require.NoError(t, dropAll(ctx))
		dir := filepath.Join(t.TempDir(), "migrations")
		app := prepareNotesApp(t, dir, migration.PathManaged, notesTable(64))
		backend := ownBackend(d, db, app.Resolver)
		generate := func(opts migration.RunOptions) *migration.RunResult {
			t.Helper()
			res, err := migration.Generate(ctx, app.Migration, app.Declared(), backend, opts)
			require.NoError(t, err)
			return res
		}
		migrate := func() *migration.RunResult {
			t.Helper()
			res, err := migration.Migrate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
			require.NoError(t, err)
			return res
		}

		res := generate(migration.RunOptions{Confirm: true})
		require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
		res = migrate()
		require.Equal(t, migration.StatusApplied, res.Status, res.Message)
		res = generate(migration.RunOptions{})
		require.Equal(t, migration.StatusNoChanges, res.Status, res.Message)
		requireLiveMatches(t, ctx, app, backend)

		app = prepareNotesApp(t, dir, migration.PathManaged, notesTable(32, pinned))
		backend = ownBackend(d, db, app.Resolver)
		_, err := migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
		require.True(t, errs.IsCode(err, errs.CodeMigrationUnresolvedIssues), "%v", err)
		res = generate(migration.RunOptions{Confirm: true, Presenter: applyAlter(app)})
		require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
		require.Equal(t, "0002", res.Migration.ID)
		res = migrate()
		require.Equal(t, migration.StatusApplied, res.Status, res.Message)
		res = generate(migration.RunOptions{})
		require.Equal(t, migration.StatusNoChanges, res.Status, res.Message)
		requireLiveMatches(t, ctx, app, backend)
		requireColumnLength(t, ctx, backend, "notes", "title", 32)
	})

	t.Run("Baseline", func(t *testing.T) {
		require.NoError(t, dropAll(ctx))
		// The tables exist already: the application applied them with its
		// own tool before it moved to the managed path.
		own := prepareNotesApp(t, filepath.Join(t.TempDir(), "own"), migration.PathGenerateOnly, notesTable(64))
		res, err := migration.Generate(ctx, own.Migration, own.Declared(), ownBackend(d, db, own.Resolver), migration.RunOptions{Confirm: true})
		require.NoError(t, err)
		require.NoError(t, d.apply(ctx, db, scriptOf(t, own.Migration, res.Migration)))

		app := prepareNotesApp(t, filepath.Join(t.TempDir(), "migrations"), migration.PathManaged, notesTable(64))
		backend := ownBackend(d, db, app.Resolver)
		res, err = migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
		require.NoError(t, err)
		require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
		require.True(t, res.Migration.IsBaseline, "the first migration of a database that holds the tables is a baseline")

		// migrate generates the baseline again and records it only if it
		// still matches: introspection has to read the same tables twice.
		res, err = migration.Migrate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{Confirm: true})
		require.NoError(t, err)
		require.Equal(t, migration.StatusApplied, res.Status, res.Message)
		res, err = migration.Generate(ctx, app.Migration, app.Declared(), backend, migration.RunOptions{})
		require.NoError(t, err)
		require.Equal(t, migration.StatusNoChanges, res.Status, "%s\noperations: %s", res.Message, operations(res.Migration))
	})

	t.Run("CommandLine", func(t *testing.T) {
		require.NoError(t, dropAll(ctx))
		dir := filepath.Join(t.TempDir(), "migrations")
		var app *engine.PreparedApp
		prepare := func() (*engine.PreparedApp, error) {
			app = prepareNotesApp(t, dir, migration.PathGenerateOnly, notesTable(64))
			return app, nil
		}
		backendFunc := func(_ context.Context, app *engine.PreparedApp) (migration.Backend, error) {
			return ownBackend(d, db, app.Resolver), nil
		}
		run := func(args ...string) (int, string, string) {
			var stdout, stderr bytes.Buffer
			code := cli.Run(ctx, args, nil, &stdout, &stderr, prepare, backendFunc)
			return code, stdout.String(), stderr.String()
		}

		code, stdout, stderr := run("generate", "-confirm")
		require.Equal(t, cli.ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
		require.Contains(t, stdout, "create table users")
		scripts, err := filepath.Glob(filepath.Join(dir, "0001_*.sql"))
		require.NoError(t, err)
		require.Len(t, scripts, 1)
		script, err := os.ReadFile(scripts[0])
		require.NoError(t, err)
		require.NoError(t, d.apply(ctx, db, string(script)))

		code, stdout, stderr = run("generate")
		require.Equal(t, cli.ExitOK, code, "stdout: %s\nstderr: %s", stdout, stderr)
		assert.Contains(t, stdout, "No schema changes detected.")
	})
}

// mysqlScriptWentStale: the MySQL driver reads the live schema to render an
// index on a TEXT column, which needs a prefix length. When the column
// changes between generate and migrate, the operations render as other
// statements, so migrate writes the script again and applies nothing until
// it has been reviewed, then applies it on the next run.
func mysqlScriptWentStale(t *testing.T, db *sql.DB, dropAll func(context.Context) error) {
	ctx := context.Background()
	require.NoError(t, dropAll(ctx))

	declare := func(indexes ...schema.Index) migration.Declared {
		registry := schema.NewRegistry()
		require.NoError(t, registry.Declare(tableModel{name: "posts"}, schema.Table{
			Name: "posts",
			Columns: []schema.Column{
				{Name: "id", Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
				{Name: "body", Type: schema.ColTypeText},
			},
			Indexes: indexes,
		}))
		require.NoError(t, registry.Freeze())
		return migration.Declared{Schemas: registry}
	}
	cfg := migration.NewMigrationConfig(migration.MigrationConfig{FolderPath: filepath.Join(t.TempDir(), "migrations"), Path: migration.PathManaged})
	declared := declare()
	resolver := migration.NewSchemaResolver()
	resolver.Freeze(migration.BuildSchemaResolverTable(declared.Schemas, cfg))
	backend := ownBackend(mysqlDialect, db, resolver)

	run := func(f func(context.Context, migration.MigrationConfig, migration.Declared, migration.Backend, migration.RunOptions) (*migration.RunResult, error)) *migration.RunResult {
		t.Helper()
		res, err := f(ctx, cfg, declared, backend, migration.RunOptions{Confirm: true})
		require.NoError(t, err)
		return res
	}
	run(migration.Generate)
	require.Equal(t, migration.StatusApplied, run(migration.Migrate).Status)

	declared = declare(schema.Index{Name: "idx_posts_body", Columns: []string{"body"}})
	res := run(migration.Generate)
	require.Equal(t, migration.StatusGenerated, res.Status, res.Message)
	next := res.Migration
	require.Contains(t, scriptOf(t, cfg, next), "(`body`(255))", "an index on a TEXT column covers a prefix")

	// Out of band, before migrate: the column is no longer TEXT.
	_, err := db.ExecContext(ctx, "ALTER TABLE `posts` MODIFY `body` VARCHAR(100) NOT NULL")
	require.NoError(t, err)

	res = run(migration.Migrate)
	require.Equal(t, migration.StatusAwaitingConfirmation, res.Status, res.Message)
	assert.Contains(t, res.Message, "has been written again and nothing was applied")
	assert.Contains(t, scriptOf(t, cfg, next), "(`body`)", "the script now holds the statement that will run")

	res = run(migration.Migrate)
	require.Equal(t, migration.StatusApplied, res.Status, res.Message)
	live, err := backend.Introspector.Introspect(ctx, "posts")
	require.NoError(t, err)
	var names []string
	for _, idx := range live.Schema.Indexes {
		names = append(names, idx.Name)
	}
	assert.Contains(t, names, "idx_posts_body")
}
