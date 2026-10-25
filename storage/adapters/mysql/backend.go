package mysql

import (
	"database/sql"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/migration"
)

// MigrationBackend returns the migration backend of db for migration.Generate, migration.Migrate and the
// cli package: one MySQLDriver as introspector, renderer and schema driver,
// and a MySQLAdapter over the same connection, through which the managed path
// reads its ledger.
//
// Pass the resolver Prepare built (PreparedApp.Resolver), the one the
// application's own adapter gets, so that migrations and queries agree on
// physical table and column names.
//
// The rendered script is a series of statements, each ended by a semicolon.
// A tool that runs it in one call needs multiple statements enabled on its
// connection: the mysql client has them, and github.com/go-sql-driver/mysql
// needs multiStatements=true in its DSN.
//
// The backend's Close closes db. Don't call it when db is shared with code
// that outlives the migration run.
func MigrationBackend(db *sql.DB, resolver behemoth.SchemaResolver) migration.Backend {
	driver := NewMySQLDriver(db, resolver)
	return migration.Backend{
		Introspector: driver,
		Renderer:     driver,
		Driver:       driver,
		DB:           NewMySQLAdapter(db, resolver),
		Close:        db.Close,
	}
}
