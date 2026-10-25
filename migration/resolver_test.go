package migration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
)

func TestIsStale(t *testing.T) {
	for name, tc := range map[string]struct {
		old, new []string
		stale    bool
	}{
		"identical":         {[]string{"Rename", "Drop and add", "Leave"}, []string{"Rename", "Drop and add", "Leave"}, false},
		"both empty":        {nil, []string{}, false},
		"option added":      {[]string{"Rename", "Leave"}, []string{"Rename", "Drop and add", "Leave"}, true},
		"option removed":    {[]string{"Rename", "Drop and add", "Leave"}, []string{"Rename", "Leave"}, true},
		"label changed":     {[]string{"Rename a -> b", "Leave"}, []string{"Rename a -> c", "Leave"}, true},
		"reordered":         {[]string{"Rename", "Drop and add"}, []string{"Drop and add", "Rename"}, true},
		"duplicate differs": {[]string{"A", "A", "B"}, []string{"A", "B", "B"}, true},
	} {
		if got := IsStale(tc.old, tc.new); got != tc.stale {
			t.Errorf("%s: IsStale(%q, %q) = %v, want %v", name, tc.old, tc.new, got, tc.stale)
		}
	}
}

// TestFilePresenterMarksStaleDecisions: a recorded decision survives a re-run
// untouched while its options are unchanged, and is flagged — with a note —
// as soon as they aren't.
func TestFilePresenterMarksStaleDecisions(t *testing.T) {
	ctx := context.Background()
	issue := func(labels ...string) PlanIssue {
		var opts []ResolutionOption
		for _, l := range labels {
			opts = append(opts, ResolutionOption{Label: l})
		}
		return PlanIssue{ID: "rename_column_users_mail_to_email", Table: "users", Options: opts, Default: 1}
	}

	for name, tc := range map[string]struct {
		next  PlanIssue
		stale bool
	}{
		"unchanged":     {issue("Rename", "Drop and add", "Leave"), false},
		"reordered":     {issue("Drop and add", "Rename", "Leave"), true},
		"option added":  {issue("Rename", "Drop and add", "Split", "Leave"), true},
		"label changed": {issue("Rename mail -> email_address", "Drop and add", "Leave"), true},
	} {
		t.Run(name, func(t *testing.T) {
			p := NewFilePresenter(filepath.Join(t.TempDir(), "draft.json"))
			if err := p.Present(ctx, []PlanIssue{issue("Rename", "Drop and add", "Leave")}, nil); err != nil {
				t.Fatal(err)
			}

			// The developer picks "Rename" (index 0) instead of the default.
			draft, err := p.readDraft()
			if err != nil {
				t.Fatal(err)
			}
			zero := 0
			draft.Entries[0].Chosen = &zero
			if err := p.writeDraft(draft); err != nil {
				t.Fatal(err)
			}

			if err := p.Present(ctx, []PlanIssue{tc.next}, nil); err != nil {
				t.Fatal(err)
			}
			draft, err = p.readDraft()
			if err != nil {
				t.Fatal(err)
			}
			entry := draft.Entries[0]
			if entry.Stale != tc.stale {
				t.Errorf("Stale = %v, want %v", entry.Stale, tc.stale)
			}
			if tc.stale != (entry.StaleNote != "") {
				t.Errorf("StaleNote = %q, want it set only when stale", entry.StaleNote)
			}
			if entry.Chosen == nil || *entry.Chosen != 0 {
				t.Errorf("the recorded decision must be kept for review, got %v", entry.Chosen)
			}

			decisions, err := p.Collect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, collected := decisions[tc.next.ID]; collected == tc.stale {
				t.Errorf("Collect returned the decision: %v, want %v", collected, !tc.stale)
			}
		})
	}
}

// TestStaleDecisionRequiresReview walks a decision through ResolveIssues: it
// applies while its options are unchanged, blocks generation once they change
// — on every run, not just the first — and applies again only after the
// developer re-checks the choice and deletes the stale flag.
func TestStaleDecisionRequiresReview(t *testing.T) {
	ctx := context.Background()
	const id = "rename_column_users_mail_to_email"
	rename := ResolutionOption{Label: "Rename", Operations: []SchemaOperation{{ID: id, Kind: OpRenameColumn, Table: "users", ColumnName: "mail", NewColumnName: "email"}}}
	dropAdd := ResolutionOption{Label: "Drop and add", Operations: []SchemaOperation{
		{ID: id + "_drop", Kind: OpDropColumn, Table: "users", ColumnName: "mail"},
		{ID: id + "_add", Kind: OpAddColumn, Table: "users", Column: &schema.Column{Name: "email"}},
	}}
	leave := ResolutionOption{Label: "Leave as-is"}
	issueWith := func(opts ...ResolutionOption) []PlanIssue {
		return []PlanIssue{{ID: id, Table: "users", Options: opts, Default: len(opts) - 1}}
	}

	p := NewFilePresenter(filepath.Join(t.TempDir(), "draft.json"))
	resolve := func(issues []PlanIssue) (*ResolvedOperationSet, error) {
		return ResolveIssues(ctx, &MigrationPlan{}, issues, p, false)
	}
	edit := func(fn func(e *draftEntry)) {
		t.Helper()
		draft, err := p.readDraft()
		if err != nil {
			t.Fatal(err)
		}
		fn(&draft.Entries[0])
		if err := p.writeDraft(draft); err != nil {
			t.Fatal(err)
		}
	}
	appliedKinds := func(r *ResolvedOperationSet) []OperationKind {
		var kinds []OperationKind
		for _, op := range r.Operations {
			kinds = append(kinds, op.Kind)
		}
		return kinds
	}

	// The first run writes the question, open, and stops on it. The
	// developer then chooses "Rename".
	if _, err := resolve(issueWith(rename, dropAdd, leave)); !errs.IsCode(err, errs.CodeMigrationUnresolvedIssues) {
		t.Fatalf("first run: want unresolved_issues, got %v", err)
	}
	edit(func(e *draftEntry) { i := 0; e.Chosen = &i })

	resolved, err := resolve(issueWith(rename, dropAdd, leave))
	if err != nil {
		t.Fatal(err)
	}
	if got := appliedKinds(resolved); len(got) != 1 || got[0] != OpRenameColumn {
		t.Fatalf("unchanged options: applied %v, want the chosen rename", got)
	}

	// The options are reordered: index 0 would now mean "Drop and add".
	reordered := issueWith(dropAdd, rename, leave)
	for run := 1; run <= 2; run++ {
		_, err := resolve(reordered)
		if !errs.IsCode(err, errs.CodeMigrationUnresolvedIssues) {
			t.Fatalf("run %d after the options changed: want unresolved_issues, got %v", run, err)
		}
	}

	// Review: re-point the choice at "Rename" in the new order and confirm.
	edit(func(e *draftEntry) {
		if !e.Stale || e.StaleNote == "" {
			t.Errorf("entry should still be flagged for review: %+v", e)
		}
		i := 1
		e.Chosen, e.Stale, e.StaleNote = &i, false, ""
	})
	resolved, err = resolve(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if got := appliedKinds(resolved); len(got) != 1 || got[0] != OpRenameColumn {
		t.Errorf("after review: applied %v, want the rename the developer confirmed", got)
	}
}
