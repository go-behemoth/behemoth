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
