package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/go-behemoth/behemoth/errs"
)

type draftEntry struct {
	ID          string   `json:"id"`
	Table       string   `json:"table"`
	Description string   `json:"description"`
	Options     []string `json:"options"`         // labels only — the actual Operations stay in-process, never serialized to the draft, per the "developer picks an option, never hand-authors an operation" boundary
	Default     int      `json:"default"`         // the planner's suggestion, an index into Options; a hint for whoever answers, never read as an answer
	Chosen      *int     `json:"chosen"`          // null = open: no answer yet
	Stale       bool     `json:"stale,omitempty"` // set when a recorded decision's options have since changed; the decision is not applied until the developer removes it — see Identity Stability
	StaleNote   string   `json:"stale_note,omitempty"`
}

type draftFile struct {
	Entries []draftEntry `json:"entries"`
}

// FilePresenter keeps the questions of a run and their answers in the draft
// file (DraftPath): JSON that survives between runs and can be reviewed and
// committed. A developer answers by setting an entry's "chosen", or through
// a front-end that writes to the same file, such as PromptPresenter.
type FilePresenter struct {
	path string

	// Given holds answers decided before the run, such as the command
	// line's -answer flags: option labels, by question ID. Present writes
	// each one to the draft as it writes the question. A given answer
	// replaces an answer the draft held and clears a stale flag. Labels are
	// compared without regard to case.
	//
	// An answer for a question the run does not raise, or with a label that
	// is not one of its question's options, makes Present return an error
	// of code errs.CodeMigrationInvalidAnswer, after it has written the
	// draft. A mistyped answer is refused rather than dropped.
	Given map[string]string
}

// NewFilePresenter returns a FilePresenter for the draft file at path.
func NewFilePresenter(path string) *FilePresenter { return &FilePresenter{path: path} }

// Present implements [ResolutionPresenter]: it writes the run's questions to
// the draft. An answered question keeps its answer, flagged as stale when its
// options have changed since. A new question is written open, with the
// planner's suggestion as "default". A question the run no longer raises is
// dropped.
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
					Default:     issue.Default,
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

		// New, or still open: written without an answer. The default is a
		// hint for whoever answers, not an answer: a run with nobody to ask
		// stops on the question (Collect skips it, ResolveIssues fails
		// closed), as the design document asks of an unattended run.
		out.Entries = append(out.Entries,
			draftEntry{
				ID:          issue.ID,
				Table:       issue.Table,
				Description: issue.Description,
				Options:     labels,
				Default:     issue.Default,
			})
	}

	// Entries whose ID no longer appears in `issues` are dropped
	_ = seen

	refused := p.applyGiven(out.Entries)
	if err := p.writeDraft(out); err != nil {
		return err
	}
	if len(refused) > 0 {
		return errs.NewMigrationError("FilePresenter.Present", errs.CodeMigrationInvalidAnswer, errors.New(strings.Join(refused, "; ")))
	}
	return nil
}

// applyGiven writes the given answers into entries, and returns one line per
// answer it refused, in the order of their question IDs.
func (p *FilePresenter) applyGiven(entries []draftEntry) []string {
	ids := make([]string, 0, len(p.Given))
	for id := range p.Given {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	var refused []string
	for _, id := range ids {
		label := strings.TrimSpace(p.Given[id])
		i := slices.IndexFunc(entries, func(e draftEntry) bool { return e.ID == id })
		if i < 0 {
			raised := make([]string, len(entries))
			for j, e := range entries {
				raised[j] = e.ID
			}
			if len(raised) == 0 {
				refused = append(refused, fmt.Sprintf("the answer for %q names a question this run does not raise: it raises none", id))
			} else {
				refused = append(refused, fmt.Sprintf("the answer for %q names a question this run does not raise: it raises %s", id, strings.Join(raised, ", ")))
			}
			continue
		}
		e := &entries[i]
		option := slices.IndexFunc(e.Options, func(o string) bool { return strings.EqualFold(o, label) })
		if option < 0 {
			refused = append(refused, fmt.Sprintf("the answer %q for %s is not one of its options: %s", label, id, quoteAll(e.Options)))
			continue
		}
		e.Chosen = &option
		e.Stale, e.StaleNote = false, ""
	}
	return refused
}

func quoteAll(labels []string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = strconv.Quote(l)
	}
	return strings.Join(quoted, ", ")
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

// Collect implements [ResolutionPresenter]: it returns the answers in the
// draft, by question ID. An open question (see [Question.Open]) has none; it
// is not an error here, ResolveIssues is what fails closed on it.
func (p *FilePresenter) Collect(ctx context.Context) (map[string]int, error) {
	draft, err := p.readDraft()
	if err != nil {
		return nil, errs.NewMigrationError("FilePresenter.Collect", errs.CodeMigrationDraftReadFailed, err)
	}
	decisions := map[string]int{}
	for _, e := range draft.Entries {
		if q := e.question(); !q.Open() {
			decisions[q.ID] = *q.Chosen
		}
	}
	return decisions, nil
}

// Question is a draft entry as a command shows it: a difference planning
// can't settle alone, its options, and its answer.
type Question struct {
	ID          string
	Table       string
	Description string
	Options     []string // the labels, in the order Default and Chosen count
	Default     int      // the planner's suggestion, an index into Options
	Chosen      *int     // the answer, an index into Options; nil while there is none
	Stale       bool     // the answer was given for options that have changed since
}

// Open reports whether q waits for an answer: it has none, the one it has
// was given for options that have changed since (it is stale, and the index
// may now name another option), or it names no option (a hand edit out of
// range, which the design document's input validation treats as no answer).
func (q Question) Open() bool {
	return q.Chosen == nil || q.Stale || *q.Chosen < 0 || *q.Chosen >= len(q.Options)
}

// Questions returns the questions in the draft, in its order. A missing
// draft has none.
func (p *FilePresenter) Questions() ([]Question, error) {
	draft, err := p.readDraft()
	if err != nil {
		return nil, errs.NewMigrationError("FilePresenter.Questions", errs.CodeMigrationDraftReadFailed, err)
	}
	questions := make([]Question, len(draft.Entries))
	for i, e := range draft.Entries {
		questions[i] = e.question()
	}
	return questions, nil
}

func (e draftEntry) question() Question {
	return Question{ID: e.ID, Table: e.Table, Description: e.Description, Options: e.Options, Default: e.Default, Chosen: e.Chosen, Stale: e.Stale}
}

// answer records index as the answer to the question id, clears a stale
// flag, and writes the draft at once. A front-end saves each answer this way
// as it is given, so an interrupted session loses none.
func (p *FilePresenter) answer(id string, index int) error {
	draft, err := p.readDraft()
	if err != nil {
		return err
	}
	for i := range draft.Entries {
		if e := &draft.Entries[i]; e.ID == id {
			e.Chosen = &index
			e.Stale, e.StaleNote = false, ""
			return p.writeDraft(draft)
		}
	}
	return fmt.Errorf("the draft has no question %q", id)
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
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // people read the file: "length 32 -> 16" stays as written, not "-\u003e"
	enc.SetIndent("", "  ")
	if err := enc.Encode(df); err != nil {
		return err
	}
	return os.WriteFile(p.path, buf.Bytes(), 0644)
}

// [Deferred] Format is plain JSON for a working v1 — no comments support,
// which is a real ergonomic gap for a file a human is meant to hand-edit
// (a developer can't leave a note next to a decision). Intent: migrate to
// a YAML-with-comments format, keeping draftEntry's shape unchanged, once
// the format choice is finalized — this only touches readDraft/writeDraft.

// ResolveIssues is the Resolution stage's orchestrator: Present, then
// Collect + gate. A presenter that asks the questions, such as
// PromptPresenter, does so in Present; interactive is not read.
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
		// Fail-closed: an open question has no default answer. A presenter
		// that could ask has asked in Present; what is still open stops the
		// run, which is the design document's rule for an unattended one.
		return nil, errs.NewMigrationError("Resolution.ResolveIssues", errs.CodeMigrationUnresolvedIssues,
			fmt.Errorf("%d question(s) have no answer: %s", len(unresolved), strings.Join(unresolved, ", ")))
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
