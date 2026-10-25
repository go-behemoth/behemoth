package migration

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/telemetry"
)

// Backend is one database's side of a migration run: everything the pipeline
// needs from the database, in one value. A dialect module builds it from a
// connection (postgres.MigrationBackend) and Generate and Migrate take it
// from there, so an
// application does not assemble GenerateDeps and MigrationDeps by hand.
//
// The two paths need different parts of it:
//
//	PathGenerateOnly  Introspector
//	PathManaged       Introspector, Driver and DB
//
// This package imports no database: the fields are its own interfaces, and
// each dialect module fills them.
type Backend struct {
	// Introspector reads the live schema. Required on both paths.
	Introspector SchemaIntrospector

	// Renderer writes a migration as a script next to its .json file.
	// Optional: nil writes the .json alone.
	Renderer MigrationRenderer

	// Driver applies migrations and records them. PathManaged only.
	Driver SchemaDriver

	// DB reads back the ledger and the snapshot Driver writes. PathManaged
	// only. It has to be built with the same SchemaResolver as Driver, or
	// the two disagree on the names of those tables.
	DB behemoth.Database

	// Close releases the connection the backend was built on. Optional.
	// Generate and Migrate do not call it: it is for whoever asked for the backend, such as the
	// cli package, to call when its command ends.
	Close func() error
}

// RunOptions are the choices of one call to Generate or Migrate.
type RunOptions struct {
	// Confirm is the developer's go-ahead for the step the call exists for.
	// For Generate that is writing the migration to FolderPath, and for
	// Migrate it is applying one. Without Confirm the call reports what it
	// would do.
	Confirm bool

	// Telemetry is optional. nil = no-op. PathManaged uses it for the audit
	// event of an applied migration and for warnings about a script that
	// could not be written.
	Telemetry *telemetry.Telemetry

	// Presenter shows the questions planning raises and collects their
	// answers. nil keeps them in the draft file at DraftPath, where a run
	// with an open question stops. A PromptPresenter over that file asks
	// them in a terminal. Only Generate raises questions.
	Presenter ResolutionPresenter
}

// draftFileName is the resolution draft's name inside FolderPath.
// ReadDiskMigrations and LatestMigrationID skip it, since it does not start
// with a migration number.
const draftFileName = "draft.json"

// DraftPath is where Generate keeps the resolution draft: the file a developer
// edits to answer the questions planning raised (see FilePresenter).
func DraftPath(cfg MigrationConfig) string {
	return filepath.Join(cfg.FolderPath, draftFileName)
}

// Generate produces the next migration for declared and never changes the
// database. It is the entry point for an application or a CLI on either
// path: it checks that backend has what the path needs, and builds the rest
// itself. That is the draft file (DraftPath), the default generator and, on
// PathManaged, the runner.
//
//	PathGenerateOnly  RunGenerateCLI: compares with the live database
//	PathManaged       RunManagedGenerate: compares with the snapshot
//
// Without opts.Confirm it reports the migration. With it, it writes the
// migration to cfg.FolderPath.
//
// Zero fields of cfg take the defaults of NewMigrationConfig. A
// PreparedApp's Migration already has them.
func Generate(ctx context.Context, cfg MigrationConfig, declared Declared, backend Backend, opts RunOptions) (*RunResult, error) {
	const op = "Migration.Generate"
	cfg = NewMigrationConfig(cfg)
	generate, err := generateDeps(op, cfg, declared, backend, opts.Presenter)
	if err != nil {
		return nil, err
	}

	switch cfg.Path {
	case PathGenerateOnly:
		return RunGenerateCLI(ctx, cfg, declared, generate, opts.Confirm)
	case PathManaged:
		deps, err := managedDeps(op, cfg, backend, generate, opts)
		if err != nil {
			return nil, err
		}
		return RunManagedGenerate(ctx, cfg, declared, deps, opts.Confirm)
	default:
		return nil, errs.NewConfigurationError(op, fmt.Sprintf("unknown MigrationPath %q", cfg.Path), nil)
	}
}

// Migrate applies the migration that Generate wrote and that is not applied
// yet (RunManagedApply). Without opts.Confirm it reports which one that is.
//
// It serves PathManaged alone. On PathGenerateOnly the application applies
// the files with its own tool, and Migrate returns a configuration error
// without reading the database.
func Migrate(ctx context.Context, cfg MigrationConfig, declared Declared, backend Backend, opts RunOptions) (*RunResult, error) {
	const op = "Migration.Migrate"
	cfg = NewMigrationConfig(cfg)
	if err := checkPath(op, cfg, PathManaged); err != nil {
		return nil, err
	}
	generate, err := generateDeps(op, cfg, declared, backend, opts.Presenter)
	if err != nil {
		return nil, err
	}
	deps, err := managedDeps(op, cfg, backend, generate, opts)
	if err != nil {
		return nil, err
	}
	return RunManagedApply(ctx, cfg, declared, deps, opts.Confirm)
}

// generateDeps checks what both paths need and builds the dependencies they
// share. A nil presenter is the FilePresenter at DraftPath.
func generateDeps(op string, cfg MigrationConfig, declared Declared, backend Backend, presenter ResolutionPresenter) (GenerateDeps, error) {
	if declared.Schemas == nil {
		return GenerateDeps{}, errs.NewConfigurationError(op, "Declared.Schemas is nil: pass PreparedApp.Declared()", nil)
	}
	if backend.Introspector == nil {
		return GenerateDeps{}, errs.NewConfigurationError(op, "Backend.Introspector is required", nil)
	}
	if presenter == nil {
		presenter = NewFilePresenter(DraftPath(cfg))
	}
	return GenerateDeps{
		Introspector: backend.Introspector,
		Presenter:    presenter,
		Generator:    &DefaultMigrationGenerator{},
		Renderer:     backend.Renderer,
	}, nil
}

// managedDeps adds what PathManaged needs to generate: the runner, built on
// the backend's driver and adapter. One presenter serves the baseline and
// the generation after it.
func managedDeps(op string, cfg MigrationConfig, backend Backend, generate GenerateDeps, opts RunOptions) (MigrationDeps, error) {
	if backend.Driver == nil || backend.DB == nil {
		return MigrationDeps{}, errs.NewConfigurationError(op,
			fmt.Sprintf("the %s path keeps a ledger, so it needs Backend.Driver and Backend.DB", PathManaged), nil)
	}
	runner := NewMigrationRunner(backend.DB, backend.Driver, cfg, opts.Telemetry)
	generate.Runner = runner
	return MigrationDeps{
		Introspector: backend.Introspector,
		Runner:       runner,
		GenerateDeps: generate,
		Presenter:    generate.Presenter,
		Telemetry:    opts.Telemetry,
	}, nil
}

// checkPath refuses an entry point called with another path's configuration.
// Each entry point reads only its own path's dependencies, so without the
// check a mismatch ends in a nil dereference, or in a migration applied by
// an application that asked for files only.
func checkPath(op string, cfg MigrationConfig, want MigrationPath) error {
	if cfg.Path == want {
		return nil
	}
	return errs.NewConfigurationError(op,
		fmt.Sprintf("MigrationConfig.Path is %q and this entry point serves %q: use Generate or Migrate, which pick the entry point by path", cfg.Path, want), nil)
}
