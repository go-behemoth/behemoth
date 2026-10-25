package migration

import (
	"context"
	"fmt"

	"github.com/go-behemoth/behemoth/errs"
)

const StatusGenerated RunStatus = "generated" // Path II's terminal state - file written

// RunGenerateCLI is Path II's CLI-facing entry point.
// NOTE, unlike Path I's baseline branch: no regenerate-and-compare
// staleness check is needed here. Path I's baseline needed one because
// RecordBaseline permanently freezes introspected reality into history — a
// stale review would poison that history. Path II's ordinary generate has
// no such permanence: re-running it always reflects current live/canonical
// truth fresh, so a drift between the preview call and the confirm call
// self-corrects automatically rather than needing to be detected.
//
// It serves PathGenerateOnly alone and returns a configuration error for any
// other path. Generate picks between this and RunManagedGenerate by cfg.Path
// and builds deps from a Backend.
func RunGenerateCLI(ctx context.Context, cfg MigrationConfig, declared Declared, deps GenerateDeps, confirmWrite bool) (*RunResult, error) {
	if err := checkPath("Migration.RunGenerateCLI", cfg, PathGenerateOnly); err != nil {
		return nil, err
	}
	m, consumed, err := buildMigrationPlan(ctx, cfg, declared, deps, true)
	if err != nil {
		if errs.IsCode(err, errs.CodeMigrationNothingToGenerate) {
			return &RunResult{Status: StatusNoChanges, Message: "No schema changes detected."}, nil
		}
		return nil, err
	}

	if !confirmWrite {
		return &RunResult{
			Status:    StatusAwaitingConfirmation,
			Migration: m,
			Message:   fmt.Sprintf("Migration %s ready (not yet written). Re-run with --confirm to write it to %s.", m.ID, cfg.FolderPath),
		}, nil
	}
	// Rendered BEFORE anything is written: here a render failure CAN be fatal
	// (nothing external depends on these files yet, unlike Path I where Apply
	// already committed), and it must not leave a .json without its script.
	ddl, err := renderMigrationDDL(ctx, *m, deps.Renderer)
	if err != nil {
		return nil, err
	}
	if err := writeMigrationFile(cfg, *m); err != nil {
		return nil, err
	}
	if err := ddl.write(cfg, *m); err != nil {
		return nil, err
	}
	used, err := consumeAnswers(ctx, deps.Presenter, consumed, *m)
	if err != nil {
		return nil, err
	}

	msg := fmt.Sprintf("Migration %s written to %s. Hand off to your own migration tool for application.", m.ID, migrationFilePath(cfg, *m))
	if ddl != nil {
		script := migrationDDLPath(cfg, *m, ddl.ext)
		msg = fmt.Sprintf("Migration %s written to %s, with its script at %s. Hand off the script to your own migration tool.", m.ID, migrationFilePath(cfg, *m), script)
	}
	return &RunResult{
		Status:    StatusGenerated,
		Migration: m,
		Message:   msg + used,
	}, nil

}
