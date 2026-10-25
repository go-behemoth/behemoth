package migrations

import (
	"context"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/storage/adapters/mysql"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
	"github.com/go-behemoth/behemoth/storage/adapters/sqlite"
	"github.com/go-behemoth/behemoth/storage/adapters/sqlserver"
	"github.com/go-behemoth/behemoth/tests/testutils"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

// TestSQLiteDriver runs the database-agnostic suite against SQLite.
func TestSQLiteDriver(t *testing.T) {
	db := openSQLite(t)
	RunDriverTests(t,
		func(r migration.SchemaResolver) migration.SchemaDriver { return sqlite.NewSQLiteDriver(db, r) },
		NewSQLiteTestManager(db),
	)
}

// TestPostgreSQLDriver runs the database-agnostic suite, then the
// Postgres-specific introspector tests, against one Postgres container.
func TestPostgreSQLDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Postgres container")
	}
	ctx := context.Background()
	db, cleanup := testutils.SetupPostgresTestDB(t, ctx)
	t.Cleanup(cleanup)

	// The container's port opens before Postgres finishes its init restart.
	require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 30*time.Second, 200*time.Millisecond)

	tm := NewPostgresTestManager(db, nil) // the container is torn down by t.Cleanup, after every subtest
	RunDriverTests(t,
		func(r migration.SchemaResolver) migration.SchemaDriver { return postgres.NewPostgreSQLDriver(db, r) },
		tm,
	)
	runPostgresIntrospectorTests(t, db, tm)

	t.Run("RunnerUsesConfiguredTables", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		runnerUsesConfiguredTables(t,
			func(r behemoth.SchemaResolver) migration.SchemaDriver { return postgres.NewPostgreSQLDriver(db, r) },
			func(r behemoth.SchemaResolver) behemoth.Database { return postgres.NewPostgresAdapter(db, r) },
			tm,
		)
	})

	t.Run("LedgerReadsBackThroughAdapter", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		readBackThroughAdapter(t, postgres.NewPostgreSQLDriver(db, nil), testutils.SetupPostgresAdapter(db))
	})

	t.Run("MigrationBackend", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		migrationBackend(t, db, postgresDialect)
	})

	t.Run("Pipeline", func(t *testing.T) {
		migrationPipeline(t, db, postgresDialect, tm.DropAllTables)
	})
}

// TestMySQLDriver runs the database-agnostic suite, then the MySQL-specific
// tests, against one MySQL container.
func TestMySQLDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a MySQL container")
	}
	ctx := context.Background()
	db, cleanup := testutils.SetupMySQLTestDB(t, ctx)
	t.Cleanup(cleanup)

	tm := NewMySQLTestManager(db) // the container is torn down by t.Cleanup, after every subtest
	RunDriverTests(t,
		func(r migration.SchemaResolver) migration.SchemaDriver { return mysql.NewMySQLDriver(db, r) },
		tm,
	)
	runMySQLDriverTests(t, db, tm)

	t.Run("RunnerUsesConfiguredTables", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		runnerUsesConfiguredTables(t,
			func(r behemoth.SchemaResolver) migration.SchemaDriver { return mysql.NewMySQLDriver(db, r) },
			func(r behemoth.SchemaResolver) behemoth.Database { return mysql.NewMySQLAdapter(db, r) },
			tm,
		)
	})

	t.Run("LedgerReadsBackThroughAdapter", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		readBackThroughAdapter(t, mysql.NewMySQLDriver(db, nil), testutils.SetupMySQLAdapter(db))
	})

	t.Run("MigrationBackend", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		migrationBackend(t, db, mysqlDialect)
	})

	t.Run("Pipeline", func(t *testing.T) {
		migrationPipeline(t, db, mysqlDialect, tm.DropAllTables)
	})

	t.Run("ScriptWentStale", func(t *testing.T) {
		mysqlScriptWentStale(t, db, tm.DropAllTables)
	})
}

// TestSQLServerDriver runs the database-agnostic suite, then the SQL
// Server-specific tests, against one SQL Server container.
func TestSQLServerDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a SQL Server container")
	}
	ctx := context.Background()
	db, cleanup := testutils.SetupMSSQLTestDB(t)
	t.Cleanup(cleanup)

	// The container's port opens before SQL Server accepts logins.
	require.Eventually(t, func() bool { return db.PingContext(ctx) == nil }, 60*time.Second, 500*time.Millisecond)

	tm := NewSQLServerTestManager(db) // the container is torn down by t.Cleanup, after every subtest
	RunDriverTests(t,
		func(r migration.SchemaResolver) migration.SchemaDriver { return sqlserver.NewSQLServerDriver(db, r) },
		tm,
	)
	runSQLServerDriverTests(t, db, tm)

	t.Run("RunnerUsesConfiguredTables", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		runnerUsesConfiguredTables(t,
			func(r behemoth.SchemaResolver) migration.SchemaDriver { return sqlserver.NewSQLServerDriver(db, r) },
			func(r behemoth.SchemaResolver) behemoth.Database { return sqlserver.NewSQLServerAdapter(db, r) },
			tm,
		)
	})

	t.Run("LedgerReadsBackThroughAdapter", func(t *testing.T) {
		require.NoError(t, tm.DropAllTables(ctx))
		readBackThroughAdapter(t, sqlserver.NewSQLServerDriver(db, nil), testutils.SetupMSSQLAdapter(db))
	})
}
