package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/storage/adapters/postgres"
)

// migrate needs the full declared schema but must not boot the
// application: Prepare alone is enough, and it already carries the resolver
// the migration driver needs.
func migrate(ctx context.Context, confirm bool) error {
	all := plugins().all
	app, err := engine.Prepare(all, prepareConfig())
	if err != nil {
		return err
	}

	sqlDB, err := openDB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	driver := postgres.NewPostgreSQLDriver(sqlDB, app.Resolver)
	res, err := migration.RunGenerateCLI(ctx, app.Migration, app.Declared(), migration.GenerateDeps{
		Introspector: driver,
		Presenter:    migration.NewFilePresenter(filepath.Join(app.Migration.FolderPath, "draft.json")),
		Generator:    &migration.DefaultMigrationGenerator{},
		Renderer:     driver,
	}, confirm)
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	return nil
}
