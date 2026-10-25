package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/go-behemoth/behemoth/errs"
)

type draftEntry struct {
	ID          string   `json:"id"`
	Table       string   `json:"table"`
	Description string   `json:"description"`
	Options     []string `json:"options"`         // labels only — the actual Operations stay in-process, never serialized to the draft, per the "developer picks an option, never hand-authors an operation" boundary
	Chosen      *int     `json:"chosen"`          // null = pending
	Stale       bool     `json:"stale,omitempty"` // set when a recorded decision's options have since changed; the decision is not applied until the developer removes it — see Identity Stability
	StaleNote   string   `json:"stale_note,omitempty"`
}

type draftFile struct {
	Entries []draftEntry `json:"entries"`
}

type FilePresenter struct {
	path string
}

func NewFilePresenter(path string) *FilePresenter { return &FilePresenter{path: path} }

func (p *FilePresenter) Present(ctx context.Context, issues []PlanIssue, priorDecisions map[string]int) error {
	existing, _ := p.readDraft() // missing file = first run, treated as empty prior state, not an error
	existingByID := map[string]draftEntry{}
	for _, e := range existing.Entries {
		existingByID[e.ID] = e
	}

	var out draftFile
	seen := map[string]bool{}
	for _, issue := range issues {
		if seen[issue.ID] {
			continue
		}

		seen[issue.ID] = true
		var labels []string
		for _, o := range issue.Options {
			labels = append(labels, o.Label)
		}

		if prior, ok := existingByID[issue.ID]; ok {
			// Identity Stability: prior decision found for this stable ID.
			if prior.Chosen != nil {
				entry := draftEntry{
					ID:          issue.ID,
					Table:       issue.Table,
					Description: issue.Description,
					Options:     labels,
					Chosen:      prior.Chosen,
				}
				// Sticky: once flagged, an entry stays stale until the
				// developer deletes the flag. Comparing only against the
				// options written on the previous run would clear it on the
				// next one, applying the decision without a review.
				if prior.Stale || IsStale(prior.Options, labels) {
					entry.Stale, entry.StaleNote = true, staleNote()
				}
				out.Entries = append(out.Entries, entry)
				continue
			}
		}

		// New issue, or previously pending, with Default pre-selected as a hint.
		def := issue.Default
		out.Entries = append(out.Entries,
			draftEntry{
				ID:          issue.ID,
				Table:       issue.Table,
				Description: issue.Description,
				Options:     labels,
				Chosen:      &def,
			})
	}

	// Entries whose ID no longer appears in `issues` are dropped
	_ = seen

	return p.writeDraft(out)
}

func staleNote() string {
	return `the options changed since this decision was recorded, so "chosen" may now point at a different option. ` +
		`Review it against the current options, then delete "stale" to confirm — until then this issue stays unresolved.`
}

// IsStale reports whether an issue's options changed since a decision was
// recorded against them (old: the labels in the draft, new: the labels the
// planner produces now).
//
// The comparison is positional, not just set membership: a decision is stored
// as Chosen, an index into the options, so the same labels in a new order
// would make that index select a different option — e.g. a confirmed rename
// silently becoming a destructive drop+add. Equal length and equal entries at
// every position is the only shape under which the recorded index still means
// what the developer chose.
func IsStale(old, new []string) bool {
	return !slices.Equal(old, new)
}

func (p *FilePresenter) Collect(ctx context.Context) (map[string]int, error) {
	draft, err := p.readDraft()
	if err != nil {
		return nil, errs.NewMigrationError("FilePresenter.Collect", errs.CodeMigrationDraftReadFailed, err)
	}
	decisions := map[string]int{}
	for _, e := range draft.Entries {
		if e.Chosen == nil {
			continue // still pending: not an error here; ResolveIssues (below) is what enforces fail-closed
		}
		if e.Stale {
			// Recorded against options that have since changed: the index may
			// now select a different option. Pending until reviewed.
			continue
		}
		if *e.Chosen < 0 || *e.Chosen >= len(e.Options) {
			// Manifesto's Input Validation branch: malformed entry treated
			// as still-pending.
			continue
		}
		decisions[e.ID] = *e.Chosen
	}
	return decisions, nil
}

// Consume implements [ResolutionConsumer]: it removes the entries of ids from
// the draft file and keeps every other entry. A missing file has nothing to
// remove.
func (p *FilePresenter) Consume(ctx context.Context, ids []string) error {
	draft, err := p.readDraft()
	if err != nil {
		return err
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	var kept []draftEntry // nil when nothing is left, as Present writes an empty draft
	for _, e := range draft.Entries {
		if !drop[e.ID] {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(draft.Entries) {
		return nil
	}
	draft.Entries = kept
	return p.writeDraft(draft)
}

// consumeAnswers removes from the draft the answers that the written
// migration m used (ids, from ResolvedOperationSet.Consumed), and returns the
// sentence the run's message ends with, or "" when nothing was removed.
//
// A draft belongs to one generation session (Draft lifecycle in the design
// document). An issue's ID names only the artifact it is about, such as
// alter_column_users_plan, so an answer left in the draft would be reused,
// without a question, for the next change to that column. "Leave as-is"
// answers are not in ids and stay: they produce no operation, and are how a
// developer says not to ask again.
//
// It runs after the migration's files are written. A failure leaves them in
// place and says which entries to remove by hand.
func consumeAnswers(ctx context.Context, presenter ResolutionPresenter, ids []string, m Migration) (string, error) {
	consumer, ok := presenter.(ResolutionConsumer)
	if !ok || len(ids) == 0 {
		return "", nil
	}
	if err := consumer.Consume(ctx, ids); err != nil {
		return "", errs.NewMigrationError("Migration.ConsumeAnswers", errs.CodeMigrationDraftWriteFailed,
			fmt.Errorf("migration %s was written, but the answers it used could not be removed from the draft. Remove %s from the draft by hand, or a later change to the same column reuses the answer: %w",
				m.ID, strings.Join(ids, ", "), err))
	}
	return fmt.Sprintf(" The %d answer(s) it used were removed from the draft, so a later change to the same column is asked again.", len(ids)), nil
}

func (p *FilePresenter) readDraft() (draftFile, error) {
	b, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return draftFile{}, nil
	}
	if err != nil {
		return draftFile{}, err
	}
	var df draftFile
	if err := json.Unmarshal(b, &df); err != nil {
		return draftFile{}, err
	}
	return df, nil
}

func (p *FilePresenter) writeDraft(df draftFile) error {
	b, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, b, 0644)
}

// [Deferred] Format is plain JSON for a working v1 — no comments support,
// which is a real ergonomic gap for a file a human is meant to hand-edit
// (a developer can't leave a note next to a decision). Intent: migrate to
// a YAML-with-comments format, keeping draftEntry's shape unchanged, once
// the format choice is finalized — this only touches readDraft/writeDraft.

// ResolveIssues is the Resolution stage's orchestrator: Present, then
// (for an interactive CLI) pause for edits, then Collect + gate.
func ResolveIssues(
	ctx context.Context,
	plan *MigrationPlan,
	issues []PlanIssue,
	presenter ResolutionPresenter,
	interactive bool,
) (*ResolvedOperationSet, error) {
	prior, _ := presenter.Collect(ctx) // best-effort prior state for merge inside Present
	if err := presenter.Present(ctx, issues, prior); err != nil {
		return nil, err
	}

	if interactive {
		// [Implementation Detail] actual pause-for-editor-then-continue loop
		// (e.g. shelling to $EDITOR, or an explicit re-run) lives in the CLI
		// layer, not here — ResolveIssues assumes Collect below reflects a
		// completed edit pass.
	}

	decisions, err := presenter.Collect(ctx)
	if err != nil {
		return nil, err
	}

	resolved := &ResolvedOperationSet{Operations: opsFromPlanned(plan.Operations), Custom: plan.Custom}
	var unresolved []string
	for _, issue := range issues {
		idx, ok := decisions[issue.ID]
		if !ok {
			unresolved = append(unresolved, issue.ID)
			continue
		}
		ops := issue.Options[idx].Operations
		resolved.Operations = append(resolved.Operations, ops...)
		if len(ops) > 0 && !slices.Contains(resolved.Consumed, issue.ID) {
			resolved.Consumed = append(resolved.Consumed, issue.ID)
		}
	}
	if len(unresolved) > 0 {
		// Fail-closed — identical in both interactive and non-interactive
		// contexts at THIS point; an interactive CLI is expected to have
		// already looped edit-then-retry before ever calling this in a way
		// that reaches here with unresolved entries still present.
		return nil, errs.NewMigrationError("Resolution.ResolveIssues", errs.CodeMigrationUnresolvedIssues,
			fmt.Errorf("unresolved: %s", strings.Join(unresolved, ", ")))
	}
	return resolved, nil
}

func opsFromPlanned(pos []PlannedOperation) []SchemaOperation {
	ops := make([]SchemaOperation, len(pos))
	for i, po := range pos {
		ops[i] = po.Operation
	}
	return ops
}
