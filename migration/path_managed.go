package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
	"github.com/go-behemoth/behemoth/telemetry"
)

type RunState string

const (
	StateFirstRunEmpty                   RunState = "first_run_empty"
	StateFirstRunAwaitingBaselineConfirm RunState = "first_run_awaiting_confirm"
	StateOngoing                         RunState = "ongoing"
)

type RunStatus string

const (
	StatusApplied              RunStatus = "applied"
	StatusAwaitingConfirmation RunStatus = "awaiting_confirmation"
	StatusNoChanges            RunStatus = "no_changes"
)

type RunResult struct {
	Status    RunStatus
	Migration *Migration
	Message   string // CLI-ready human text; not meant to be parsed
}

// The managed path (Path I) has two entry points, one per thing a developer
// asks for. RunManagedGenerate writes the next migration file and never
// changes the database. RunManagedApply applies the file that is waiting and
// never writes one. A new migration is therefore always reviewed between the
// two calls.
//
// Both start from DetermineRunState, so they agree on where a database
// stands: untouched, waiting for its baseline to be recorded, or ongoing.
// Both serve PathManaged alone and return a configuration error for any
// other path, before they create the folder or read the database. Generate
// and Migrate build deps from a Backend and call them.

// RunManagedGenerate produces the next migration of the managed path: the
// baseline of a database that already holds declared tables, or else the
// difference between the declaration and the snapshot. Without confirmWrite
// it reports that migration. With it, it writes the .json and its script to
// FolderPath for review (writeForReview).
//
// It generates nothing while a migration on disk is unapplied: the snapshot
// the next one is compared with would be out of date.
func RunManagedGenerate(
	ctx context.Context,
	cfg MigrationConfig,
	declared Declared,
	deps MigrationDeps,
	confirmWrite bool,
) (*RunResult, error) {
	const op = "Migration.RunManagedGenerate"
	if err := checkPath(op, cfg, PathManaged); err != nil {
		return nil, err
	}
	if err := EnsureMigrationFolder(cfg); err != nil {
		return nil, err
	}
	state, disk, err := DetermineRunState(ctx, cfg, deps)
	if err != nil {
		return nil, err // both unhandled edge cases surface here, as a clear abort
	}

	switch state {
	case StateFirstRunEmpty:
		candidates, _, err := PartitionForBaseline(ctx, declared.Schemas, deps.Introspector)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return generateNext(ctx, cfg, declared, deps, confirmWrite)
		}
		baseline, err := buildBaselineMigrationOnly(ctx, candidates, deps)
		if err != nil {
			return nil, err
		}
		if !confirmWrite {
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: baseline,
				Message: fmt.Sprintf("Some declared tables already exist, so the first migration is a baseline that records them (not yet written). Re-run with --confirm to write it to %s.", cfg.FolderPath)}, nil
		}
		written, err := writeForReview(ctx, cfg, deps, *baseline)
		if err != nil {
			return nil, err
		}
		return &RunResult{Status: StatusGenerated, Migration: baseline,
			Message: fmt.Sprintf("Baseline migration written to %s. Review it, then run migrate to record it.", written)}, nil

	case StateFirstRunAwaitingBaselineConfirm:
		existing := disk[0]
		regenerated, err := regenerateBaseline(ctx, declared, deps, existing)
		if err != nil {
			return nil, err
		}
		if migrationsEqual(existing, *regenerated) {
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: &existing,
				Message: fmt.Sprintf("Baseline migration %s is written and waiting. Run migrate to record it.", existing.ID)}, nil
		}
		// Live schema drifted since the baseline was written: a stale
		// review is never recorded. It is written again, and reviewed again.
		if !confirmWrite {
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: regenerated,
				Message: fmt.Sprintf("Live schema changed since baseline %s was written. Re-run with --confirm to write it again.", existing.ID)}, nil
		}
		written, err := writeForReview(ctx, cfg, deps, *regenerated)
		if err != nil {
			return nil, err
		}
		return &RunResult{Status: StatusGenerated, Migration: regenerated,
			Message: fmt.Sprintf("Live schema changed since the baseline was written. It has been written again to %s. Review it, then run migrate to record it.", written)}, nil

	case StateOngoing:
		// Reusing Runner.Pending (dependency-ordered, ledger-diffed) rather than
		// a naive "compare snapshot.Version to the last file" check — more
		// robust if history is ever non-linear, and avoids duplicating logic
		// Runner already owns correctly.
		pending, err := deps.Runner.Pending(ctx, disk)
		if err != nil {
			return nil, err
		}
		if len(pending) > 0 {
			next := pending[0]
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: &next,
				Message: fmt.Sprintf("Migration %s is written and not applied yet. Run migrate before generating the next one.", next.ID)}, nil
		}
		return generateNext(ctx, cfg, declared, deps, confirmWrite)

	default:
		return nil, errs.NewInternalError(op, fmt.Errorf("unhandled run state %q", state))
	}
}

// generateNext plans the migration that follows the snapshot. Resolution's
// own review (renames, narrowing alters) runs inside it, through the draft
// file.
func generateNext(ctx context.Context, cfg MigrationConfig, declared Declared, deps MigrationDeps, confirmWrite bool) (*RunResult, error) {
	m, consumed, err := buildMigrationPlan(ctx, cfg, declared, deps.GenerateDeps, true)
	if err != nil {
		if errs.IsCode(err, errs.CodeMigrationNothingToGenerate) {
			return &RunResult{Status: StatusNoChanges, Message: "No schema changes detected."}, nil
		}
		return nil, err
	}
	if !confirmWrite {
		return &RunResult{Status: StatusAwaitingConfirmation, Migration: m,
			Message: fmt.Sprintf("Migration %s ready (not yet written). Re-run with --confirm to write it to %s.", m.ID, cfg.FolderPath)}, nil
	}
	written, err := writeForReview(ctx, cfg, deps, *m)
	if err != nil {
		return nil, err
	}
	used, err := consumeAnswers(ctx, deps.GenerateDeps.Presenter, consumed, *m)
	if err != nil {
		return nil, err
	}
	return &RunResult{Status: StatusGenerated, Migration: m,
		Message: fmt.Sprintf("Migration %s written to %s. Review it, then run migrate to apply it.", m.ID, written) + used}, nil
}

// writeForReview writes m and, when the driver renders, its script next to
// it, and returns what it wrote as text for a message.
//
// The script is what a developer reads to review the migration. It is not
// what gets applied: RunManagedApply runs m's operations, and checks first
// that they still render to this script.
//
// It is rendered before anything is written, so a failure leaves no .json
// without its script.
func writeForReview(ctx context.Context, cfg MigrationConfig, deps MigrationDeps, m Migration) (string, error) {
	ddl, err := renderMigrationDDL(ctx, m, deps.GenerateDeps.Renderer)
	if err != nil {
		return "", err
	}
	if err := writeMigrationFile(cfg, m); err != nil {
		return "", err
	}
	if err := ddl.write(cfg, m); err != nil {
		return "", err
	}
	if ddl == nil {
		return migrationFilePath(cfg, m), nil
	}
	return fmt.Sprintf("%s, with its script at %s", migrationFilePath(cfg, m), migrationDDLPath(cfg, m, ddl.ext)), nil
}

// RunManagedApply applies the migration that is written and unapplied, or
// records a baseline. Without confirmApply it reports which migration that
// is. It never writes a migration file: one that does not exist yet, or a
// baseline that no longer matches the database, is RunManagedGenerate's to
// write.
//
// Before it applies, it renders the migration again and compares the result
// with the script that was reviewed (scriptWentStale). Some drivers derive
// statements from the live schema, so the same operations can become other
// statements once the database has changed. A script that differs is written
// again and nothing is applied until it has been reviewed.
func RunManagedApply(
	ctx context.Context,
	cfg MigrationConfig,
	declared Declared,
	deps MigrationDeps,
	confirmApply bool,
) (*RunResult, error) {
	const op = "Migration.RunManagedApply"
	if err := checkPath(op, cfg, PathManaged); err != nil {
		return nil, err
	}
	state, disk, err := DetermineRunState(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}

	var next Migration
	switch state {
	case StateFirstRunEmpty:
		return &RunResult{Status: StatusNoChanges, Message: "Nothing to apply: no migration has been written yet. Run generate first."}, nil

	case StateFirstRunAwaitingBaselineConfirm:
		next = disk[0]
		regenerated, err := regenerateBaseline(ctx, declared, deps, next)
		if err != nil {
			return nil, err
		}
		if !migrationsEqual(next, *regenerated) {
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: &next,
				Message: fmt.Sprintf("Live schema changed since baseline %s was written, so it was not recorded. Run generate with --confirm to write it again, and review it.", next.ID)}, nil
		}

	case StateOngoing:
		pending, err := deps.Runner.Pending(ctx, disk)
		if err != nil {
			return nil, err
		}
		if len(pending) == 0 {
			return &RunResult{Status: StatusNoChanges, Message: "Nothing to apply: every migration on disk is applied."}, nil
		}
		if len(pending) > 1 {
			// Should be structurally impossible through these entry points:
			// RunManagedGenerate refuses to produce a new file while one is
			// pending. Surfaced rather than silently assumed away.
			return nil, errs.NewMigrationError(op, errs.CodeMigrationMultipleUnapplied,
				fmt.Errorf("%d unapplied migrations found — expected at most one; resolve manually", len(pending)))
		}
		next = pending[0]

	default:
		return nil, errs.NewInternalError(op, fmt.Errorf("unhandled run state %q", state))
	}

	// A baseline is recorded, not executed: its tables already exist.
	what, apply, applied := "Migration", "apply", "applied"
	if next.IsBaseline {
		what, apply, applied = "Baseline migration", "record", "recorded. The database was not changed"
	}
	ddl, renderErr := renderMigrationDDL(ctx, next, deps.GenerateDeps.Renderer)
	if renderErr == nil {
		stale, err := scriptWentStale(cfg, next, ddl)
		if err != nil {
			return nil, err
		}
		if stale {
			return &RunResult{Status: StatusAwaitingConfirmation, Migration: &next,
				Message: fmt.Sprintf("The database changed since %s was generated, and its operations now run as different statements. The script at %s has been written again and nothing was applied. Review it, then run migrate again.",
					next.ID, migrationDDLPath(cfg, next, ddl.ext))}, nil
		}
	}
	if !confirmApply {
		return &RunResult{Status: StatusAwaitingConfirmation, Migration: &next,
			Message: fmt.Sprintf("%s %s is ready. Re-run with --confirm to %s it.", what, next.ID, apply)}, nil
	}
	if err := deps.Runner.Apply(ctx, []Migration{next}); err != nil {
		return nil, err
	}
	// The script is there already when generate wrote it. A migration file
	// from before generate did, or one whose script was deleted, gets it
	// now. Neither failure is fatal: Apply has committed, and a missing
	// script must not be reported as if the migration had failed.
	if renderErr != nil {
		warn(ctx, deps.Telemetry, "failed to render migration script", telemetry.ErrorFields(renderErr, behemoth.M{"id": next.ID}))
	} else if err := ddl.write(cfg, next); err != nil {
		warn(ctx, deps.Telemetry, "failed to write migration script", telemetry.ErrorFields(err, behemoth.M{"id": next.ID}))
	}
	return &RunResult{Status: StatusApplied, Migration: &next, Message: fmt.Sprintf("%s %s %s.", what, next.ID, applied)}, nil
}

func buildBaselineMigrationOnly(ctx context.Context, candidates []BaselineCandidate, deps MigrationDeps) (*Migration, error) {
	report, err := RunIntrospection(ctx, registryFromCandidates(candidates), deps.Introspector, true)
	if err != nil {
		return nil, err
	}
	if err := RejectAmbiguousTypes(report); err != nil {
		return nil, err
	}
	tables := map[string]schema.Table{}
	for table, ti := range report.Tables {
		if ti.IncompatibleObject {
			return nil, errs.NewMigrationError("Baseline.Build", errs.CodeMigrationIncompatibleObject,
				fmt.Errorf("table %q exists live as a non-table object; resolve manually before baselining", table))
		}
		tables[table] = introspectedShape(ti)
	}
	m := BuildBaselineMigration(tables)
	return &m, nil
}

// regenerateBaseline builds the baseline again from the live database, to
// be compared with the one on disk. A baseline that was reviewed against a
// schema that has since changed must not be recorded.
func regenerateBaseline(ctx context.Context, declared Declared, deps MigrationDeps, existing Migration) (*Migration, error) {
	candidates, _, err := PartitionForBaseline(ctx, declared.Schemas, deps.Introspector)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, errs.NewMigrationError("Migration.regenerateBaseline", errs.CodeMigrationBaselineCandidatesVanished,
			fmt.Errorf("no baseline candidate tables found live, but %s is on disk awaiting confirmation — resolve manually", existing.ID))
	}
	return buildBaselineMigrationOnly(ctx, candidates, deps)
}

// scriptWentStale compares ddl, m rendered against the database as it is
// now, with the script on disk, and writes ddl in its place when they
// differ. A migration without a script on disk, or a driver that does not
// render, has nothing to go stale.
func scriptWentStale(cfg MigrationConfig, m Migration, ddl *renderedDDL) (bool, error) {
	if ddl == nil {
		return false, nil
	}
	reviewed, err := os.ReadFile(migrationDDLPath(cfg, m, ddl.ext))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, errs.NewMigrationError("Migration.scriptWentStale", errs.CodeMigrationReadFileFailed, err)
	}
	if string(reviewed) == ddl.body {
		return false, nil
	}
	return true, ddl.write(cfg, m)
}

func warn(ctx context.Context, tel *telemetry.Telemetry, msg string, fields behemoth.M) {
	if tel != nil && tel.Logger != nil {
		telemetry.Named(tel.Logger, "migration").Warn(ctx, msg, fields)
	}
}

// DetermineRunState
func DetermineRunState(ctx context.Context, cfg MigrationConfig, deps MigrationDeps) (RunState, []Migration, error) {
	tableExists, err := deps.Introspector.TableExists(ctx, LedgerCanonicalName)
	if err != nil {
		return "", nil, err
	}

	disk, err := ReadDiskMigrations(cfg)
	if err != nil {
		return "", nil, err
	}

	switch {
	case !tableExists && len(disk) == 0:
		return StateFirstRunEmpty, disk, nil
	case !tableExists && len(disk) == 1 && disk[0].IsBaseline:
		return StateFirstRunAwaitingBaselineConfirm, disk, nil
	case !tableExists && len(disk) == 1:
		// A new database after its first run: that run wrote this migration
		// for review, and the ledger table does not exist yet because
		// drivers create it with the first migration they record. The
		// ordinary flow finds the file pending and applies it once the run
		// is confirmed.
		//
		// A file that was applied by hand before the application moved to
		// this path lands here too. Applying it again fails in the database
		// (the tables exist) and nothing is recorded.
		return StateOngoing, disk, nil
	case !tableExists:
		// Edge case: "folder has multiple migration files but no table"
		// NOT handled. There is no safe way to guess whether these files
		// were partially applied by hand, generated by a different process,
		// or simply never run.
		return "", disk, errs.NewMigrationError("Migration.DetermineRunState", errs.CodeMigrationUnappliedNoLedger,
			fmt.Errorf("%d migration file(s) found in %q but no migrations table exists — cannot determine a safe starting point; resolve manually", len(disk), cfg.FolderPath))
	case len(disk) == 0:
		// Edge case: "table exists (with snapshot) but no folder"
		// NOT handled. The ledger/snapshot has real history the folder can't
		// reconstruct; regenerating from here risks silently redoing work.
		return "", disk, errs.NewMigrationError("Migration.DetermineRunState", errs.CodeMigrationLedgerWithoutFiles,
			fmt.Errorf("a migrations table exists but no migration files were found in %q — cannot reconstruct history; resolve manually", cfg.FolderPath))

	default:
		return StateOngoing, disk, nil
	}

}

func BuildBaselineMigration(tables map[string]schema.Table) Migration {
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names) // Required for Migration equality check performed later. A mere map-iteration order is non-deterministic

	var up []SchemaOperation

	for _, table := range names {
		ts := tables[table]
		// Neither is inline, as in a planned migration (checkNoInlineIndexes,
		// checkNoInlineForeignKeys). The operations below carry them, and
		// the snapshot is built from the operations: an index left on the
		// table would be recorded twice.
		bare := ts
		bare.Indexes = nil
		bare.ForeignKeys = nil
		up = append(up, SchemaOperation{
			ID:        baselineTableID(table),
			Kind:      OpCreateTable,
			Table:     table,
			NewTable:  &bare,
			Confirmed: true,
		})

		for _, idx := range ts.Indexes {
			up = append(up, SchemaOperation{
				ID:        baselineIndexID(table, idx.Name),
				Kind:      OpAddIndex,
				Table:     table,
				Index:     &idx,
				Confirmed: true,
			})
		}

		for _, fk := range ts.ForeignKeys {
			up = append(up, SchemaOperation{
				ID:         baselineForeignKeyID(table, fk.Name),
				Kind:       OpAddForeignKey,
				Table:      table,
				ForeignKey: &fk,
				Confirmed:  true,
			})
		}
	}

	return Migration{
		// The ID is the number alone, as for every migration: the snapshot's
		// version is this ID, and checkNoUnappliedMigrations compares it
		// with the number a file name starts with (LatestMigrationID). The
		// file is 0000_baseline.json.
		ID:         "0000",
		Name:       "baseline",
		Up:         up,
		Down:       nil,
		IsBaseline: true,
		CreatedAt:  time.Now(),
	}
}

func ReadDiskMigrations(cfg MigrationConfig) ([]Migration, error) {
	entries, err := os.ReadDir(cfg.FolderPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errs.NewMigrationError("Migration.ReadDiskMigrations", errs.CodeMigrationReadDirFailed, err)
	}

	var migrations []Migration
	for _, e := range entries {
		if e.IsDir() || !migrationFilePattern.MatchString(e.Name()) {
			continue // non-matching files ignored, same "unrelated file" rule used everywhere else in this pillar
		}
		b, err := os.ReadFile(filepath.Join(cfg.FolderPath, e.Name()))
		if err != nil {
			return nil, errs.NewMigrationError("Migration.ReadDiskMigrations", errs.CodeMigrationReadFileFailed, err)
		}
		var m Migration
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, errs.NewMigrationError("Migration.ReadDiskMigrations", errs.CodeMigrationUnmarshalFailed, fmt.Errorf("%s: %w", e.Name(), err))
		}
		migrations = append(migrations, m)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].ID < migrations[j].ID })
	return migrations, nil
}

func migrationFilePath(cfg MigrationConfig, m Migration) string {
	return filepath.Join(cfg.FolderPath, m.ID+"_"+m.Name+".json")
}

func registryFromCandidates(candidates []BaselineCandidate) schema.Registry {
	r := &snapshotRegistry{tables: map[string]schema.Table{}}
	for _, c := range candidates {
		r.tables[c.Table] = c.Current
	}
	return r
}

func introspectedShape(ti TableIntrospection) schema.Table {
	var cols []schema.Column
	var indexes []schema.Index
	var fks []schema.ForeignKey

	// A matching column records its declaration, not the live column: the
	// two only match up to driver normalization (ColumnNormalizer), and every
	// later snapshot diff compares declarations with what is recorded here.
	// Recording the live shape would make a normalized column (e.g. a blob
	// read back as bytes) differ on every run after the baseline.
	for _, f := range ti.Columns {
		switch {
		case f.Kind == ColMatch && f.Declared != nil:
			cols = append(cols, *f.Declared)
		case f.Live != nil:
			cols = append(cols, *f.Live)
		}
	}

	for _, f := range ti.Indexes {
		if f.Live != nil {
			indexes = append(indexes, *f.Live)
		}
	}

	for _, f := range ti.ForeignKeys {
		if f.Live != nil {
			fks = append(fks, *f.Live)
		}
	}

	return schema.Table{
		Name:         ti.Table,
		PhysicalName: ti.Table,
		Columns:      cols,
		Indexes:      indexes,
		ForeignKeys:  fks,
	}
}

func migrationsEqual(a, b Migration) bool {
	ab, errA := json.Marshal(a.Up)
	bb, errB := json.Marshal(b.Up)
	return errA == nil && errB == nil && string(ab) == string(bb) // migration is built deterministically
}
