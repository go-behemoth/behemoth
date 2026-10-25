package migration

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
)

type MigrationPlan struct {
	Operations []PlannedOperation // not yet topologically ordered here; sorted in MigrationGenerator
	Custom     []CustomMigration
}

type ResolutionOption struct {
	Label      string
	Operations []SchemaOperation
}

type PlanIssue struct {
	ID          string
	Table       string
	Description string
	Options     []ResolutionOption
	Default     int
}

func BuildPlan(report *IntrospectionReport, current schema.Registry) (*MigrationPlan, []PlanIssue, error) {
	plan := &MigrationPlan{}
	var issues []PlanIssue

	for table, ti := range report.Tables {
		if !ti.ExistsLive {
			declared, _ := current.Lookup(table)

			// The table is created from its columns alone. Its indexes and
			// foreign keys are operations of their own, and the table in the
			// create operation carries neither: no driver creates them in
			// its CREATE TABLE, and on the managed path the snapshot is
			// built from these operations, so an index left on the table
			// would be recorded without ever being created.
			bare := declared
			bare.Indexes = nil
			bare.ForeignKeys = nil
			plan.Operations = append(plan.Operations, PlannedOperation{
				Operation: SchemaOperation{
					ID:       createTableID(table),
					Kind:     OpCreateTable,
					Table:    table,
					NewTable: &bare,
				},
				Source: "generated",
			})

			// On a table that does not exist every declared index is missing,
			// so they are planned as a missing index of an existing table is.
			plan.Operations = append(plan.Operations, planIndexes(table, diffIndexes(declared.Indexes, nil, false))...)

			// A foreign key apart from its table also lets two new tables
			// reference each other.
			for _, fk := range declared.ForeignKeys {
				plan.Operations = append(plan.Operations, PlannedOperation{
					Operation: SchemaOperation{
						ID:         addForeignKeyID(table, fk.Name),
						Kind:       OpAddForeignKey,
						Table:      table,
						ForeignKey: &fk,
					},
					Source: "generated",
				})
			}
			continue
		}

		if ti.IncompatibleObject {
			// This is a blocking issue and no candidate option is generated
			// Must be resolved manually.
			return nil, nil, errs.NewMigrationError("Planning.BuildPlan", errs.CodeMigrationIncompatibleObject,
				fmt.Errorf("table %q exists live as a non-table object; resolve manually before continuing", table))
		}

		colOps, colIssues := planColumns(table, ti.Columns)
		renameIssues := planRenames(table, ti.Renames)
		plan.Operations = append(plan.Operations, colOps...)
		issues = append(issues, colIssues...)
		issues = append(issues, renameIssues...)
		plan.Operations = append(plan.Operations, planIndexes(table, ti.Indexes)...)
		plan.Operations = append(plan.Operations, planForeignKeys(table, ti.ForeignKeys)...)
	}

	return plan, issues, nil
}

func planColumns(table string, findings []ColumnFinding) ([]PlannedOperation, []PlanIssue) {
	var ops []PlannedOperation
	var issues []PlanIssue

	for _, f := range findings {
		switch f.Kind {

		case ColMatch:
		// no action

		case ColMissingLive:
			ops = append(ops, PlannedOperation{
				Operation: SchemaOperation{
					ID:     addColumnID(table, f.Name),
					Kind:   OpAddColumn,
					Table:  table,
					Column: f.Declared,
				}, Source: "generated",
			}) // automatic addition

		case ColExtraLive:
			// extra live column that reached here (i.e.
			// trackExtraColumns was true for this source) is a real drop
			// candidate. But requires manual confirmation.
			opID := dropColumnID(table, f.Name)
			issues = append(issues, PlanIssue{
				ID: opID, Table: table,
				Description: fmt.Sprintf("Column %q exists in the database but is no longer declared.", f.Name),
				Options: []ResolutionOption{
					{
						Label: "Drop column " + f.Name,
						Operations: []SchemaOperation{
							{
								ID:         opID,
								Kind:       OpDropColumn,
								Table:      table,
								ColumnName: f.Name,
								Column:     f.Live,
								Confirmed:  true,
							},
						},
					},
					{
						Label:      "Leave as-is",
						Operations: nil,
					},
				},
				Default: 1, // "Leave as-is" is the default selection
			})

		case ColDiffers:
			declared := f.Normalized
			if declared == nil {
				declared = f.Declared
			}
			isNarrowing := isNarrowingChange(*f.Live, *declared)
			opID := alterColumnID(table, f.Name)
			if !isNarrowing {
				ops = append(ops, PlannedOperation{
					Operation: SchemaOperation{
						ID:         opID,
						Kind:       OpAlterColumn,
						Table:      table,
						Column:     f.Declared,
						PrevColumn: f.Live,
					}, Source: "generated"}) // automatic additive (widening operation)
			} else {
				issues = append(issues, PlanIssue{
					ID: opID, Table: table,
					Description: fmt.Sprintf("Column %q's definition narrows (may reject or truncate existing data): %s.", f.Name, columnChange(*f.Live, *declared)),
					Options: []ResolutionOption{
						{
							Label: "Apply alter",
							Operations: []SchemaOperation{
								{
									ID:         opID,
									Kind:       OpAlterColumn,
									Table:      table,
									Column:     f.Declared,
									PrevColumn: f.Live,
									Confirmed:  true,
								},
							},
						},
						{
							Label:      "Leave as-is",
							Operations: nil,
						},
					},
					Default: 1,
				})
			}
		}

		// if f.TypeAmbiguity != nil {
		// 	// type reverse-mapping ambiguityPlanning refuses
		// 	// to finalize tiering until resolved via its own PlanIssue,
		// 	// independent of whatever Kind-based issue was already raised.
		// 	issues = append(issues, PlanIssue{
		// 		ID: "type_ambiguity_" + table + "_" + f.Name, Table: table,
		// 		Description: fmt.Sprintf("Column %q: %s", f.Name, f.TypeAmbiguity.Reason),
		// 		Options: []ResolutionOption{
		// 			{
		// 				Label:      "Accept guessed type",
		// 				Operations: nil,
		// 			}, // no-op: the guess is what Declared/Live already reflect
		// 			{
		// 				Label:      "Leave as-is",
		// 				Operations: nil,
		// 			},
		// 			// [Deferred] "Override with a specified type" requires a
		// 			// free-form type input from the developer, which the
		// 			// simple option-index selection model can't
		// 			// carry. It needs a richer draft-entry format than v1
		// 			// supports.
		// 			// Intent: add a third option here once the
		// 			// FilePresenter's draft format supports a free-text
		// 			// field per issue, not just an option index.
		// 		},
		// 		Default: 0,
		// 	})
		// }
	}

	return ops, issues
}

func planRenames(table string, renames []RenameCandidate) []PlanIssue {
	var issues []PlanIssue
	byGroup := map[string][]RenameCandidate{}

	for _, r := range renames {
		if r.Ambiguous {
			byGroup[r.GroupID] = append(byGroup[r.GroupID], r)
			continue
		}
		opID := renameColumnID(table, r.From.Name, r.To.Name)
		issues = append(issues, PlanIssue{
			ID:          opID,
			Table:       table,
			Description: fmt.Sprintf("Column %q appears renamed to %q.", r.From.Name, r.To.Name),
			Options: []ResolutionOption{
				{
					Label: fmt.Sprintf("Rename %s -> %s", r.From.Name, r.To.Name),
					Operations: []SchemaOperation{
						{
							ID:            opID,
							Kind:          OpRenameColumn,
							Table:         table,
							ColumnName:    r.From.Name,
							NewColumnName: r.To.Name,
							Confirmed:     true},
					},
				},
				{
					Label: "Drop old, add new independently",
					Operations: []SchemaOperation{
						{
							ID:         renameFallbackDropID(opID),
							Kind:       OpDropColumn,
							Table:      table,
							ColumnName: r.From.Name,
							Column:     &r.From,
							Confirmed:  true,
						},
						{
							ID:        renameFallbackAddID(opID),
							Kind:      OpAddColumn,
							Table:     table,
							Column:    &r.To,
							Confirmed: true,
						},
					}},
				{
					Label:      "Leave as-is",
					Operations: nil,
				},
			},
			Default: 2,
		})
	}

	for groupID, members := range byGroup {
		var names []string
		for _, m := range members {
			if m.To.Name != "" {
				names = append(names, m.To.Name)
			}
		}
		issues = append(issues, PlanIssue{
			ID: renameGroupIssueID(table, groupID), Table: table,
			Description: fmt.Sprintf("Multiple equally-plausible rename candidates found among columns: %s.", strings.Join(names, ", ")),
			Options: []ResolutionOption{
				{
					Label:      "Fall back to independent add/drop for all involved",
					Operations: buildFallbackOps(table, members),
				},
				{
					Label:      "Leave as-is",
					Operations: nil,
				},
			},
			Default: 1,
		})
	}

	return issues
}

// planIndexes converts IndexFinding entries into operations.
// every index-level outcome is Automatic since ropping
// an index removes a constraint/performance structure and not row data, so
// none of these ever become a PlanIssue. Definition mismatches are handled
// as drop-then-recreate.
func planIndexes(table string, findings []IndexFinding) []PlannedOperation {
	var ops []PlannedOperation

	for _, f := range findings {
		switch f.Kind {
		case IdxMatch:
			// no action

		case IdxMissing:
			idx := *f.Declared
			ops = append(ops, PlannedOperation{
				Operation: SchemaOperation{
					ID:        addIndexID(table, f.Name),
					Kind:      OpAddIndex,
					Table:     table,
					Index:     &idx,
					Confirmed: true,
				},
				Source: "generated",
			})

		case IdxExtra:
			idx := *f.Live
			ops = append(ops, PlannedOperation{
				Operation: SchemaOperation{
					ID:        dropIndexID(table, f.Name),
					Kind:      OpDropIndex,
					Table:     table,
					IndexName: f.Name,
					Index:     &idx,
					Confirmed: true,
				},
				Source: "generated",
			})

		case IdxDiffers:
			// Drop-then-recreate: emit both halves. Generate's dependency
			// graph will naturally order the drop before the add for the
			// SAME index name via ordinary same-table/registration-order
			// resolution. no special edge is needed here since both target
			// the same table.
			liveIdx, declaredIdx := *f.Live, *f.Declared
			ops = append(ops,
				PlannedOperation{Operation: SchemaOperation{
					ID:        dropIndexID(table, f.Name),
					Kind:      OpDropIndex,
					Table:     table,
					IndexName: f.Name,
					Index:     &liveIdx,
					Confirmed: true,
				}, Source: "generated"},
				PlannedOperation{Operation: SchemaOperation{
					ID:        addIndexID(table, f.Name),
					Kind:      OpAddIndex,
					Table:     table,
					Index:     &declaredIdx,
					Confirmed: true,
				}, Source: "generated"},
			)
		}
	}
	return ops
}

// planForeignKeys
func planForeignKeys(table string, findings []ForeignKeyFinding) []PlannedOperation {
	var ops []PlannedOperation

	for _, f := range findings {
		switch f.Kind {
		case FKMatch:
			// no action

		case FKMissing:
			fk := *f.Declared
			ops = append(ops, PlannedOperation{
				Operation: SchemaOperation{
					ID:         addForeignKeyID(table, f.Name),
					Kind:       OpAddForeignKey,
					Table:      table,
					ForeignKey: &fk,
					Confirmed:  true,
				},
				Source: "generated",
			})

		case FKExtra:
			fk := *f.Live
			ops = append(ops, PlannedOperation{
				Operation: SchemaOperation{
					ID:             dropForeignKeyID(table, f.Name),
					Kind:           OpDropForeignKey,
					Table:          table,
					ForeignKeyName: f.Name,
					ForeignKey:     &fk,
					Confirmed:      true,
				},
				Source: "generated",
			})

		case FKDiffers:
			liveFK, declaredFK := *f.Live, *f.Declared
			ops = append(ops,
				PlannedOperation{Operation: SchemaOperation{
					ID:             dropForeignKeyID(table, f.Name),
					Kind:           OpDropForeignKey,
					Table:          table,
					ForeignKeyName: f.Name,
					ForeignKey:     &liveFK,
					Confirmed:      true,
				}, Source: "generated"},
				PlannedOperation{Operation: SchemaOperation{
					ID:         addForeignKeyID(table, f.Name),
					Kind:       OpAddForeignKey,
					Table:      table,
					ForeignKey: &declaredFK,
					Confirmed:  true,
				}, Source: "generated"},
			)
		}
	}

	return ops
}

// buildFallbackOps is the operation-construction half of the "ambiguous
// rename group" PlanIssue's "Fall back to independent add/drop for all
// involved" option — called from planRenames.
//
// members is one ambiguousGroup's flattened RenameCandidate list: some
// entries carry only `To` (declared-only columns, i.e. add candidates),
// others carry only `From` (live-only columns, i.e. drop candidates)
// Every member in a group is, part of the same group precisely because no
// single confident pairing existed, so this deliberately makes no pairing
// decision; it just emits one operation per involved column.

// One thing worth flagging rather than silently absorbing: buildFallbackOps's resulting OpDropColumn operations are marked Confirmed: true immediately,
// with no further per-column confirmation — this is a deliberate reading of the manifesto's ambiguous-rename row
//
//	("Fall back to independent add/drop for all involved" is itself the option the developer explicitly chose),
//
// not an oversight of the general "drops require confirmation" rule.
// The confirmation already happened at the group level when this option was selected;
// re-prompting per-column afterward would contradict the developer's own explicit choice rather than protect against an unreviewed one.
func buildFallbackOps(table string, members []RenameCandidate) []SchemaOperation {
	var ops []SchemaOperation
	seen := map[string]bool{} // guards against double-emission if a column somehow appears twice within the same group's flattened list

	for _, m := range members {
		switch {
		case m.To.Name != "" && !seen["add:"+m.To.Name]:
			seen["add:"+m.To.Name] = true
			col := m.To
			ops = append(ops, SchemaOperation{
				ID:        addColumnID(table, col.Name),
				Kind:      OpAddColumn,
				Table:     table,
				Column:    &col,
				Confirmed: true,
			})

		case m.From.Name != "" && !seen["drop:"+m.From.Name]:
			seen["drop:"+m.From.Name] = true
			col := m.From
			ops = append(ops, SchemaOperation{
				ID:         dropColumnID(table, col.Name),
				Kind:       OpDropColumn,
				Table:      table,
				ColumnName: col.Name,
				Column:     &col,
				Confirmed:  true,
			})
		}
	}
	return ops
}

// func indexByName(tables []TableSchema) map[string]TableSchema {
// 	tableMap := make(map[string]TableSchema)
// 	for _, table := range tables {
// 		tableMap[table.Name] = table
// 	}
// 	return tableMap
// }

func indexColumnsByName(columns []schema.Column) map[string]schema.Column {
	columnMap := make(map[string]schema.Column)
	for _, col := range columns {
		columnMap[col.Name] = col
	}

	return columnMap
}

func indexIndexesByName(indexes []schema.Index) map[string]schema.Index {
	indMap := make(map[string]schema.Index)
	for _, ind := range indexes {
		indMap[ind.Name] = ind
	}

	return indMap
}

func indexFKsByName(indexes []schema.ForeignKey) map[string]schema.ForeignKey {
	fkMap := make(map[string]schema.ForeignKey)
	for _, fk := range indexes {
		fkMap[fk.Name] = fk
	}

	return fkMap
}

type renamePair struct{ From, To schema.Column }
type ambiguousGroup struct {
	ID       string
	Declared []schema.Column
	Other    []schema.Column
}

// matchRenameCandidates proposes rename pairs by structural signature
// (Type, Length, Nullable, PrimaryKey, Unique all equal
// For columns more than 2, with possible rename match, the function groups them into
// ambiguousGroup
// matchRenameCandidates pairs declared-only with other-only columns by
// structural signature, comparing normalize(declared) — see diffColumns.
func matchRenameCandidates(declared, other []schema.Column, normalize func(schema.Column) schema.Column) (pairs []renamePair, groups []ambiguousGroup, unmatchedDeclared, unmatchedOther []schema.Column) {
	usedOther := make(map[string]bool)
	groupSeq := 0

	for _, d := range declared {
		var candidates []schema.Column
		for _, a := range other {
			if usedOther[a.Name] {
				continue
			}
			if sameStructuralSignature(normalize(d), a) {
				candidates = append(candidates, a)
			}
		}
		switch len(candidates) {
		case 0:
			unmatchedDeclared = append(unmatchedDeclared, d)
		case 1:
			pairs = append(pairs, renamePair{From: d, To: candidates[0]})
			usedOther[candidates[0].Name] = true
		default:
			// AMBIGUOUS: e.g. two dropped columns and two added columns all
			// share the same type/nullable signature.
			groupSeq++
			groups = append(groups, ambiguousGroup{ID: fmt.Sprintf("group_%d", groupSeq), Declared: []schema.Column{d}, Other: candidates})
			for _, c := range candidates {
				usedOther[c.Name] = true
			}
		}
	}
	for _, o := range other {
		if !usedOther[o.Name] {
			unmatchedOther = append(unmatchedOther, o)
		}
	}
	return pairs, groups, unmatchedDeclared, unmatchedOther
}

func sameStructuralSignature(a, b schema.Column) bool {
	return a.Type == b.Type &&
		a.Length == b.Length &&
		a.Nullable == b.Nullable &&
		a.PrimaryKey == b.PrimaryKey &&
		a.Unique == b.Unique
}

func columnsEqual(a, b schema.Column) bool {
	return a.Name == b.Name &&
		a.Type == b.Type &&
		a.Length == b.Length &&
		a.Nullable == b.Nullable &&
		a.PrimaryKey == b.PrimaryKey &&
		a.Unique == b.Unique &&
		a.AutoInc == b.AutoInc &&
		defaultsEqual(a, b)
}

func indexesEqual(a, b schema.Index) bool {
	return a.Name == b.Name && a.Unique == b.Unique && slices.Equal(a.Columns, b.Columns)

}

func fkEqual(a, b schema.ForeignKey) bool {
	return a.Name == b.Name &&
		a.RefTable == b.RefTable &&
		slices.Equal(a.RefColumns, b.RefColumns) &&
		a.OnDelete == b.OnDelete &&
		slices.Equal(a.Columns, b.Columns)
}

// isNarrowingChange reports whether altering live into declared may reject
// or truncate existing data, or break existing writers: changing the type,
// adding NOT NULL, changing auto-increment, shrinking the length, or
// removing the default.
//
// Auto-increment counts in both directions. Removing it breaks every insert
// that leaves the column out; adding it to a populated column hands out
// values that may already exist, unless the driver starts it past them.
//
// Removing a default narrows too: inserts that relied on it start failing (or
// storing NULL). Adding or changing one doesn't — a default only applies to
// future inserts, never to existing rows.
//
// Every type change counts, including ones that are usually safe (integer to
// bigint, string to text): whether existing values convert depends on the
// data, which planning never reads, so the developer confirms it.
//
// declared should be the normalized declaration (ColumnFinding.Normalized),
// so both sides speak the database's terms — a string declared without a
// length is VARCHAR(255), not 0, and a type the driver stores the same way as
// the live one (a uuid in SQLite's BLOB) is not a type change.
func isNarrowingChange(live, declared schema.Column) bool {
	return live.Type != declared.Type ||
		(live.Nullable && !declared.Nullable) ||
		live.AutoInc != declared.AutoInc ||
		(hasDefault(live) && !hasDefault(declared)) ||
		declared.Length < live.Length
}

// columnChange says what differs between a column's live and declared
// definitions, in the terms isNarrowingChange decides on, for the question
// a narrowing raises: "length 32 -> 16, null allowed -> not null".
func columnChange(live, declared schema.Column) string {
	var parts []string
	if live.Type != declared.Type {
		parts = append(parts, fmt.Sprintf("type %s -> %s", live.Type, declared.Type))
	}
	if live.Length != declared.Length {
		parts = append(parts, fmt.Sprintf("length %s -> %s", lengthText(live.Length), lengthText(declared.Length)))
	}
	if live.Nullable != declared.Nullable {
		parts = append(parts, nullText(live.Nullable)+" -> "+nullText(declared.Nullable))
	}
	if from, to := defaultText(live), defaultText(declared); from != to {
		parts = append(parts, fmt.Sprintf("default %s -> %s", from, to))
	}
	if live.AutoInc != declared.AutoInc {
		if declared.AutoInc {
			parts = append(parts, "auto-increment added")
		} else {
			parts = append(parts, "auto-increment removed")
		}
	}
	return strings.Join(parts, ", ")
}

func lengthText(n int) string {
	if n == 0 {
		return "none"
	}
	return strconv.Itoa(n)
}

func nullText(nullable bool) string {
	if nullable {
		return "null allowed"
	}
	return "not null"
}

// defaultText is a column's default as the question shows it. A default set
// only in a database override has no single value to show.
func defaultText(col schema.Column) string {
	switch {
	case col.Default != nil:
		if s, ok := col.Default.(string); ok {
			return strconv.Quote(s)
		}
		return fmt.Sprint(col.Default)
	case hasDefault(col):
		return "set per database"
	default:
		return "none"
	}
}
