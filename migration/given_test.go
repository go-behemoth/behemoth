package migration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-behemoth/behemoth/errs"
)

// A given answer names its option by label, without regard to case or
// surrounding spaces. It is written to the draft, so a later run without it
// reads it there.
func TestGivenAnswersAreWrittenToTheDraft(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), draftFileName)
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is")}

	file := NewFilePresenter(path)
	file.Given = map[string]string{"email": "  apply ALTER "}
	resolved, err := ResolveIssues(ctx, &MigrationPlan{}, issues, file, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].ID != "email" {
		t.Fatalf("operations = %+v, want the alter of email", resolved.Operations)
	}

	resolved, err = ResolveIssues(ctx, &MigrationPlan{}, issues, NewFilePresenter(path), false)
	if err != nil || len(resolved.Operations) != 1 {
		t.Errorf("a later run without the answer: operations = %+v, err = %v; want the saved answer read", resolved, err)
	}
}

// A given answer is an explicit decision against the current options: it
// replaces the draft's answer, and clears a stale flag.
func TestAGivenAnswerReplacesTheDraftsAnswer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, draftFileName)
	if err := NewFilePresenter(path).Present(ctx, []PlanIssue{question("email", "Apply alter", "Leave as-is")}, nil); err != nil {
		t.Fatal(err)
	}
	choose(t, dir, "email", 1)
	changed := []PlanIssue{question("email", "Apply alter", "Apply alter in two steps", "Leave as-is")}
	if err := NewFilePresenter(path).Present(ctx, changed, nil); err != nil {
		t.Fatal(err)
	}
	if q := draftQuestions(t, NewFilePresenter(path))["email"]; !q.Stale {
		t.Fatalf("question = %+v, want its answer stale after the options changed", q)
	}

	file := NewFilePresenter(path)
	file.Given = map[string]string{"email": "Apply alter in two steps"}
	if err := file.Present(ctx, changed, nil); err != nil {
		t.Fatal(err)
	}
	q := draftQuestions(t, file)["email"]
	if q.Stale || q.Chosen == nil || *q.Chosen != 1 {
		t.Errorf("question = %+v, want answer 1 and no stale flag", q)
	}
}

// An answer for a question the run does not raise, or with a label that is
// not one of the options, is refused, and every refused answer is named.
// The draft is still written with the run's questions.
func TestAGivenAnswerThatAnswersNothingIsRefused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), draftFileName)
	file := NewFilePresenter(path)
	file.Given = map[string]string{"emial": "Apply alter", "email": "Apply altr"}

	err := file.Present(ctx, []PlanIssue{question("email", "Apply alter", "Leave as-is")}, nil)
	if !errs.IsCode(err, errs.CodeMigrationInvalidAnswer) {
		t.Fatalf("err = %v, want %s", err, errs.CodeMigrationInvalidAnswer)
	}
	for _, want := range []string{
		`the answer "Apply altr" for email is not one of its options: "Apply alter", "Leave as-is"`,
		`the answer for "emial" names a question this run does not raise: it raises email`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if q, ok := draftQuestions(t, NewFilePresenter(path))["email"]; !ok || !q.Open() {
		t.Errorf("draft question = %+v (present %v), want it written, open", q, ok)
	}

	file.Given = map[string]string{"email": "Apply alter"}
	if err := file.Present(ctx, nil, nil); !errs.IsCode(err, errs.CodeMigrationInvalidAnswer) || !strings.Contains(err.Error(), "it raises none") {
		t.Errorf("a run without questions: err = %v, want the answer refused", err)
	}
}

// Given answers are applied before the prompt, which asks only the
// questions still open.
func TestGivenAnswersComeBeforeThePrompt(t *testing.T) {
	ctx := context.Background()
	file := NewFilePresenter(filepath.Join(t.TempDir(), draftFileName))
	file.Given = map[string]string{"email": "Apply alter"}
	var out strings.Builder
	prompt := NewPromptPresenter(file, strings.NewReader("\n"), &out)
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Leave as-is")}

	if _, err := ResolveIssues(ctx, &MigrationPlan{}, issues, prompt, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Question 1 of 1 (name)") || strings.Contains(out.String(), "(email)") {
		t.Errorf("want name asked alone:\n%s", out.String())
	}
	got := draftQuestions(t, file)
	if c := got["email"].Chosen; c == nil || *c != 0 {
		t.Errorf("email: chosen = %v, want 0, the given answer", c)
	}
	if c := got["name"].Chosen; c == nil || *c != 1 {
		t.Errorf("name: chosen = %v, want 1, the default taken at the prompt", c)
	}
}
