package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
	"github.com/go-behemoth/behemoth/utils"
)

type DefaultMigrationGenerator struct{}

func (*DefaultMigrationGenerator) Generate(resolvedPlan *ResolvedOperationSet, previousMigrationID string) (*Migration, error) {
	if len(resolvedPlan.Operations) == 0 && len(resolvedPlan.Custom) == 0 {
		return nil, errs.NewMigrationError("MigrationGenerator.Generate", errs.CodeMigrationNothingToGenerate,
			fmt.Errorf("resolved operation set is empty"))
	}

	// Defensive check for possible circular dependency between foreign key and referenced table.
	if err := checkNoInlineForeignKeys(resolvedPlan); err != nil {
		return nil, err
	}
	if err := checkNoInlineIndexes(resolvedPlan); err != nil {
		return nil, err
	}
	if err := checkCustomCollisions(resolvedPlan); err != nil {
		return nil, err
	}

	graphMap, nodeByID, err := buildDependencyGraph(resolvedPlan)
	if err != nil {
		return nil, err
	}

	alphabetical := func(a, b string) bool { return a < b } // deterministic default
	sortedIDs, cyclePath, ok := utils.KahnSort(graphMap, alphabetical)
	if !ok {
		return nil, errs.NewConfigurationError("MigrationGenerator.Generate",
			fmt.Sprintf("circular schema operation dependency: %s", strings.Join(cyclePath, " -> ")), nil)
	}

	var up []SchemaOperation
	for _, id := range sortedIDs {
		node := nodeByID[id]
		if node.kind == internalOp {
			up = append(up, *node.operation)
		} else {
			up = append(up, node.custom.Up...)
		}
	}

	down, _ := computeDown(sortedIDs, nodeByID) // nil Down is valid (irreversible migration)

	id := nextMigrationID(previousMigrationID)
	return &Migration{
		ID:        id,
		Name:      migrationName(resolvedPlan, id),
		Up:        up,
		Down:      down,
		DependsOn: dependsOnList(previousMigrationID),
		CreatedAt: time.Now(),
		Custom:    customNames(resolvedPlan),
	}, nil

}

const (
	internalOp = "operation"
	customOp   = "custom"
)

type planNode struct {
	id        string
	kind      string // "operation" | "custom"
	operation *SchemaOperation
	custom    *CustomMigration
}

// Rule 1 (STRUCTURAL): any op targeting table T must follow T's own CreateTable, if T is created in this plan.
// Rule 2 (STRUCTURAL): AddForeignKey depends on its RefTable's CreateTable, if RefTable is created in this plan.
// Rule 3 (STRUCTURAL): DropForeignKey must run before its RefTable's DropTable, if both are in this plan
// Rule 4 (EXPLICIT): SchemaOperation.DependsOn for operations with no structural signal.
// Rule 5 (EXPLICIT): CustomMigration.DependsOn.
func buildDependencyGraph(plan *ResolvedOperationSet) (map[string][]string, map[string]planNode, error) {
	nodeByID := map[string]planNode{}
	createTableID := map[string]string{} // table -> its CreateTable op ID, if created in this plan
	dropTableID := map[string]string{}   // table -> its DropTable op ID, if dropped in this plan

	for i := range plan.Operations {
		op := &plan.Operations[i]
		if op.ID == "" {
			return nil, nil, errs.NewConfigurationError("MigrationGenerator.buildGraph", "operation missing ID", nil)
		}
		if _, dup := nodeByID[op.ID]; dup {
			return nil, nil, errs.NewConfigurationError("MigrationGenerator.buildGraph", fmt.Sprintf("duplicate operation ID %q", op.ID), nil)
		}
		nodeByID[op.ID] = planNode{id: op.ID, kind: internalOp, operation: op}
		switch op.Kind {
		case OpCreateTable:
			createTableID[op.Table] = op.ID
		case OpDropTable:
			dropTableID[op.Table] = op.ID
		}
	}

	for i := range plan.Custom {
		c := &plan.Custom[i]
		if c.Name == "" {
			return nil, nil, errs.NewConfigurationError("MigrationGenerator.buildGraph", "custom migration missing Name", nil)
		}
		if _, dup := nodeByID[c.Name]; dup {
			return nil, nil, errs.NewConfigurationError("MigrationGenerator.buildGraph", fmt.Sprintf("duplicate node name %q", c.Name), nil)
		}
		nodeByID[c.Name] = planNode{id: c.Name, kind: customOp, custom: c}
	}

	edges := map[[2]string]bool{} // (from,to) a pair added twice by different rules must not inflate in-degree
	addEdge := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if _, exists := nodeByID[from]; !exists {
			return // referenced node isn't part of this plan: e.g. the table already existed before this migration
		}
		edges[[2]string{from, to}] = true
	}

	// Rule 1 (STRUCTURAL): any op targeting table T must follow T's own CreateTable, if T is created in this plan.
	for id, node := range nodeByID {
		if node.kind == internalOp {
			if node.operation.Kind == OpCreateTable {
				continue
			}
			addEdge(createTableID[node.operation.Table], id)
		} else {
			for _, op := range node.custom.Up {
				addEdge(createTableID[op.Table], id)
			}
		}
	}

	// Rule 2 (STRUCTURAL): AddForeignKey depends on its RefTable's CreateTable, if RefTable is created in this plan.
	for id, node := range nodeByID {
		if node.kind != internalOp || node.operation.Kind != OpAddForeignKey || node.operation.ForeignKey == nil {
			continue
		}
		addEdge(createTableID[node.operation.ForeignKey.RefTable], id)
	}

	// Rule 3 (STRUCTURAL): DropForeignKey must run before its RefTable's DropTable, if both are in this plan
	for id, node := range nodeByID {
		if node.kind != internalOp || node.operation.Kind != OpDropForeignKey || node.operation.ForeignKey == nil {
			continue
		}
		addEdge(id, dropTableID[node.operation.ForeignKey.RefTable])
	}

	// Rule 4 (EXPLICIT): SchemaOperation.DependsOn for operations with no structural signal.
	for id, node := range nodeByID {
		if node.kind != internalOp {
			continue
		}
		for _, dep := range node.operation.DependsOn {
			addEdge(dep, id)
		}
	}

	// Rule 5 (EXPLICIT): CustomMigration.DependsOn.
	for id, node := range nodeByID {
		if node.kind != customOp {
			continue
		}
		for _, dep := range node.custom.DependsOn {
			addEdge(dep, id)
		}
	}

	graphMap := map[string][]string{}
	for id := range nodeByID {
		graphMap[id] = nil // ensure isolated nodes still appear
	}

	for pair := range edges {
		graphMap[pair[0]] = append(graphMap[pair[0]], pair[1])
	}

	return graphMap, nodeByID, nil
}

// computeDown walks sortedIDs in reverse and inverts each step.
// The operation is ALL-OR-NOTHING: if any single step can't be inverted (e.g. a custom
// migration with no authored Down), the entire migration's Down is nil
// a partial rollback with incomplete information is dangerous in an undocumented intermediate state.
func computeDown(sortedIDs []string, nodeByID map[string]planNode) ([]SchemaOperation, bool) {
	var down []SchemaOperation
	for i := len(sortedIDs) - 1; i >= 0; i-- {
		node := nodeByID[sortedIDs[i]]
		if node.kind == customOp {
			if len(node.custom.Up) > 0 && len(node.custom.Down) == 0 {
				return nil, false
			}
			down = append(down, node.custom.Down...) // custom migration down ops
		} else {
			inv, ok := invertOperation(*node.operation)
			if !ok {
				return nil, false
			}
			down = append(down, *inv)
		}
	}
	return down, true
}

func invertOperation(op SchemaOperation) (*SchemaOperation, bool) {
	base := func(kind OperationKind) SchemaOperation {
		return SchemaOperation{ID: downID(op.ID), Kind: kind, Table: op.Table}
	}

	switch op.Kind {
	case OpCreateTable:
		d := base(OpDropTable)
		return &d, true

	case OpDropTable:
		if op.NewTable == nil {
			return nil, false
		}
		d := base(OpCreateTable)
		d.NewTable = op.NewTable
		return &d, true

	case OpAddColumn:
		d := base(OpDropColumn)
		d.ColumnName = op.Column.Name
		return &d, true

	case OpDropColumn:
		if op.Column == nil {
			return nil, false
		}
		d := base(OpAddColumn)
		d.Column = op.Column
		return &d, true

	case OpRenameColumn:
		d := base(OpRenameColumn)
		d.ColumnName, d.NewColumnName = op.NewColumnName, op.ColumnName
		return &d, true

	case OpAlterColumn:
		if op.PrevColumn == nil {
			return nil, false
		}
		d := base(OpAlterColumn)
		d.Column = op.PrevColumn
		return &d, true

	case OpAddIndex:
		d := base(OpDropIndex)
		d.IndexName = op.Index.Name
		return &d, true

	case OpDropIndex:
		if op.Index == nil {
			return nil, false
		}
		d := base(OpAddIndex)
		d.Index = op.Index
		return &d, true

	case OpAddForeignKey:
		d := base(OpDropForeignKey)
		d.ForeignKeyName = op.ForeignKey.Name
		return &d, true

	case OpDropForeignKey:
		if op.ForeignKey == nil {
			return nil, false
		}
		d := base(OpAddForeignKey)
		d.ForeignKey = op.ForeignKey
		return &d, true

	default:
		return nil, false
	}

}

// checkNoInlineForeignKeys enforces the foreign key dependency
// FKs must always arrive as their own
// separate OpAddForeignKey operations to prevent circular table references
func checkNoInlineForeignKeys(resolved *ResolvedOperationSet) error {
	check := func(ops []SchemaOperation, source string) error {
		for _, op := range ops {

			// A new table with foreign-key declaration
			if op.Kind == OpCreateTable && op.NewTable != nil && len(op.NewTable.ForeignKeys) > 0 {
				return errs.NewInternalError("MigrationGenerator.checkNoInlineForeignKeys",
					fmt.Errorf("%s: operation %q (CreateTable %q) carries inline ForeignKeys. Planning must emit these as separate AddForeignKey operations",
						source, op.ID, op.Table))
			}
		}
		return nil
	}

	if err := check(resolved.Operations, "resolved.Operations"); err != nil {
		return err
	}

	for _, c := range resolved.Custom {
		if err := check(c.Up, "custom migration "+c.Name); err != nil {
			return err
		}
	}
	return nil
}

// checkNoInlineIndexes is checkNoInlineForeignKeys for indexes. No driver
// creates a table's indexes in its CREATE TABLE, so an index left on the
// table of an OpCreateTable would be missing from the database without an
// error, and on the managed path the snapshot would list it all the same.
// Each index has to arrive as its own OpAddIndex.
func checkNoInlineIndexes(resolved *ResolvedOperationSet) error {
	check := func(ops []SchemaOperation, source string) error {
		for _, op := range ops {
			if op.Kind == OpCreateTable && op.NewTable != nil && len(op.NewTable.Indexes) > 0 {
				return errs.NewInternalError("MigrationGenerator.checkNoInlineIndexes",
					fmt.Errorf("%s: operation %q (CreateTable %q) carries inline Indexes. Planning must emit these as separate AddIndex operations",
						source, op.ID, op.Table))
			}
		}
		return nil
	}

	if err := check(resolved.Operations, "resolved.Operations"); err != nil {
		return err
	}

	for _, c := range resolved.Custom {
		if err := check(c.Up, "custom migration "+c.Name); err != nil {
			return err
		}
	}
	return nil
}

func nextMigrationID(previous string) string {
	n := 1
	if previous != "" {
		var prevN int
		fmt.Sscanf(previous, "%04d", &prevN)
		n = prevN + 1
	}
	return fmt.Sprintf("%04d", n)
}

func dependsOnList(previous string) []string {
	if previous == "" {
		return nil
	}
	return []string{previous}
}

// EnsureMigrationFolder creates cfg.FolderPath if it doesn't exist yet —
// the check-and-create step requested up front, run once at the start of
// every generate invocation, identically for both Paths.
func EnsureMigrationFolder(cfg MigrationConfig) error {
	if err := os.MkdirAll(cfg.FolderPath, 0755); err != nil {
		return errs.NewMigrationError("Migration.EnsureFolder", errs.CodeMigrationMkdirFailed, err)
	}
	return nil
}

var migrationFilePattern = regexp.MustCompile(`^(\d{4})_.*\.json$`)

// LatestMigrationID scans FolderPath for existing generated migration
// files and returns the highest ID found, or "" if the folder is empty (the greenfield case),
// handled identically to nextMigrationID's existing "" -> "0001" behavior.
// Same scan works for both Paths, since both write their generated file to this folder.
// PathGenerateOnly's contract is only that the user can do whatever they want with the file after Behemoth wrote to it.
func LatestMigrationID(cfg MigrationConfig) (string, error) {
	entries, err := os.ReadDir(cfg.FolderPath)
	if err != nil {
		return "", errs.NewMigrationError("Migration.LatestID", errs.CodeMigrationReadDirFailed, err)
	}

	latest := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue // not one of ours — ignored, same "extra/unrelated file" posture as everything else in this pillar
		}
		if latest == "" || m[1] > latest { // zero-padded fixed-width IDs compare correctly as strings
			latest = m[1]
		}
	}
	return latest, nil
}

// buildReport branches by Path exactly per the earlier frequency
// correction: PathManaged diffs its own canonical snapshot (no live DB
// call, standing behavior after onboarding); PathGenerateOnly always
// introspects live, since it owns no snapshot of its own to trust.
func buildReport(ctx context.Context, cfg MigrationConfig, current schema.Registry, deps GenerateDeps) (*IntrospectionReport, error) {
	switch cfg.Path {
	case PathManaged:
		snapshot, err := deps.Runner.LoadSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		if err := checkNoUnappliedMigrations(cfg, snapshot); err != nil {
			return nil, err
		}

		return RunIntrospectionFromSnapshotDiff(snapshotAsRegistry(snapshot), current), nil

	case PathGenerateOnly:
		report, err := RunIntrospection(ctx, current, deps.Introspector, false) // trackExtraColumns=false
		if err != nil {
			return nil, err
		}
		if err := RejectAmbiguousTypes(report); err != nil {
			return nil, err
		}

		return report, nil

	default:
		return nil, errs.NewConfigurationError("Migration.buildReport", fmt.Sprintf("unknown MigrationPath %q", cfg.Path), nil)
	}
}

func writeMigrationFile(cfg MigrationConfig, m Migration) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return errs.NewMigrationError("Migration.Write", errs.CodeMigrationMarshalFailed, err)
	}
	path := filepath.Join(cfg.FolderPath, m.ID+"_"+m.Name+".json")
	if err := os.WriteFile(path, b, 0644); err != nil {
		return errs.NewMigrationError("Migration.Write", errs.CodeMigrationWriteFailed, err)
	}
	return nil
}

// renderedDDL is a migration's script, rendered but not yet written.
type renderedDDL struct {
	body string
	ext  string
}

// renderMigrationDDL renders m through the driver's native script language.
// A nil renderer (the driver can't express migrations as a script) yields a
// nil result and no error. Must run before m is applied — see MigrationRenderer.
func renderMigrationDDL(ctx context.Context, m Migration, renderer MigrationRenderer) (*renderedDDL, error) {
	if renderer == nil {
		return nil, nil
	}
	body, err := renderer.RenderMigration(ctx, m)
	if err != nil {
		return nil, errs.NewMigrationError("Migration.RenderDDL", errs.CodeMigrationRenderFailed, err)
	}
	ext := renderer.FileExtension()
	if ext == "" {
		return nil, errs.NewMigrationError("Migration.RenderDDL", errs.CodeMigrationMissingFileExtension, fmt.Errorf("renderer %T returned an empty file extension", renderer))
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return &renderedDDL{body: body, ext: ext}, nil
}

// write puts the script next to the migration's .json file. A nil receiver
// (no renderer) is a no-op.
func (r *renderedDDL) write(cfg MigrationConfig, m Migration) error {
	if r == nil {
		return nil
	}
	if err := os.WriteFile(migrationDDLPath(cfg, m, r.ext), []byte(r.body), 0644); err != nil {
		return errs.NewMigrationError("Migration.WriteDDL", errs.CodeMigrationWriteFailed, err)
	}
	return nil
}

// writeMigrationDDL renders and writes the script sibling of a migration's
// .json file. Never called instead of writeMigrationFile — always alongside it.
func writeMigrationDDL(ctx context.Context, cfg MigrationConfig, m Migration, renderer MigrationRenderer) error {
	r, err := renderMigrationDDL(ctx, m, renderer)
	if err != nil {
		return err
	}
	return r.write(cfg, m)
}

func migrationDDLPath(cfg MigrationConfig, m Migration, ext string) string {
	return filepath.Join(cfg.FolderPath, m.ID+"_"+m.Name+ext)
}

// checkNoUnappliedMigrations compares the snapshot's recorded Version
// against the latest migration file on disk. If they differ, some
// on-disk migration exists that the snapshot doesn't yet reflect — either
// unapplied, or applied against a database this snapshot didn't come from.
// Generating against a stale snapshot in this state would silently ignore
// that migration's effects.
func checkNoUnappliedMigrations(cfg MigrationConfig, snapshot SchemaSnapshot) error {
	latestOnDisk, err := LatestMigrationID(cfg)
	if err != nil {
		return err
	}
	if latestOnDisk == "" {
		return nil // no files at disk
	}
	if snapshot.Version != latestOnDisk {
		return errs.NewMigrationError("Migration.checkNoUnappliedMigrations", errs.CodeMigrationUnappliedPending,
			fmt.Errorf("migration %q exists on disk but the snapshot is at %q — run Apply before generating a new migration", latestOnDisk, snapshot.Version))
	}
	return nil
}

// RejectAmbiguousTypes is the shared gate both paths call before proceeding with an IntrospectionReport.
// There is no "option" to offer for a type ambiguity.
// The introspector could not determine the real canonical type, so any operation built from its guess cannot be relied upon.
// This is a hard failure that should be resolved manually by the user.
// In this case, the user can:
//  1. use a custom model with their own Serialization + Model method implementation
//  2. update the database type to one that's supported by Behemoth
func RejectAmbiguousTypes(report *IntrospectionReport) error {
	var problems []string
	for table, ti := range report.Tables {
		for _, f := range ti.Columns {
			if f.TypeAmbiguity != nil {
				problems = append(problems, fmt.Sprintf("%s.%s: %s", table, f.Name, f.TypeAmbiguity.Reason))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems) // report.Tables is a map; keep the message stable
		return errs.NewMigrationError("Migration.RejectAmbiguousTypes", errs.CodeMigrationUnmappableColumnType,
			fmt.Errorf(`cannot generate a migration - unmappable column type(s) found:\n%s\n\nThe core model cannot represent these columns. 
			Supply a custom Model implementation for the affected table, or extend the driver's type mapping if this should be a recognized type.`, strings.Join(problems, "\n")))
	}
	return nil
}

// buildMigrationPlan plans and resolves the next migration. It also returns
// the IDs of the answers the migration used (ResolvedOperationSet.Consumed),
// for the caller to pass to consumeAnswers once it has written the files.
func buildMigrationPlan(
	ctx context.Context,
	cfg MigrationConfig,
	declared Declared,
	deps GenerateDeps,
	interactive bool,
) (*Migration, []string, error) {
	if err := EnsureMigrationFolder(cfg); err != nil {
		return nil, nil, err
	}
	previousID, err := LatestMigrationID(cfg)
	if err != nil {
		return nil, nil, err
	}
	report, err := buildReport(ctx, cfg, declared.Schemas, deps)
	if err != nil {
		return nil, nil, err
	}
	plan, issues, err := BuildPlan(report, declared.Schemas)
	if err != nil {
		return nil, nil, err
	}
	if plan.Custom, err = pendingCustomMigrations(cfg, declared.Custom); err != nil {
		return nil, nil, err
	}
	resolved, err := ResolveIssues(ctx, plan, issues, deps.Presenter, interactive)
	if err != nil {
		return nil, nil, err
	}
	m, err := deps.Generator.Generate(resolved, previousID)
	if err != nil {
		return nil, nil, err
	}
	return m, resolved.Consumed, nil
}

// RunGenerate is the full generate-time call chain:
// ensure folder -> resolve -> previous ID -> introspect -> plan -> resolve issues -> generate -> write.
// Application (PathManaged only) is a distinct, step and RunGenerate itself never applies anything.
func RunGenerate(
	ctx context.Context,
	cfg MigrationConfig,
	declared Declared,
	deps GenerateDeps,
	interactive bool,
) (*Migration, error) {

	m, consumed, err := buildMigrationPlan(ctx, cfg, declared, deps, interactive)
	if err != nil {
		return nil, err
	}

	if err := writeMigrationFile(cfg, *m); err != nil {
		return nil, err
	}
	if _, err := consumeAnswers(ctx, deps.Presenter, consumed, *m); err != nil {
		return nil, err
	}
	return m, nil
}
