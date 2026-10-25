package migration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
)

// question is an issue whose first option alters a column of users and
// whose last is "Leave as-is", the default.
func question(id string, labels ...string) PlanIssue {
	issue := PlanIssue{ID: id, Table: "users", Description: "Column " + id + " differs.", Default: len(labels) - 1}
	for i, label := range labels {
		opt := ResolutionOption{Label: label}
		if i == 0 {
			opt.Operations = []SchemaOperation{{ID: id, Kind: OpAlterColumn, Table: "users", Column: &schema.Column{Name: id}}}
		}
		issue.Options = append(issue.Options, opt)
	}
	return issue
}

func draftQuestions(t *testing.T, p *FilePresenter) map[string]Question {
	t.Helper()
	questions, err := p.Questions()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Question{}
	for _, q := range questions {
		out[q.ID] = q
	}
	return out
}

// A new question is written without an answer, with the planner's
// suggestion as its default. A run with nobody to ask stops on it, instead
// of taking the default without anyone deciding.
func TestANewQuestionStaysOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), draftFileName)
	p := NewFilePresenter(path)

	_, err := ResolveIssues(ctx, &MigrationPlan{}, []PlanIssue{question("email", "Apply alter", "Leave as-is")}, p, false)
	if !errs.IsCode(err, errs.CodeMigrationUnresolvedIssues) || !strings.Contains(err.Error(), "email") {
		t.Fatalf("err = %v, want %s naming the question", err, errs.CodeMigrationUnresolvedIssues)
	}
	q := draftQuestions(t, p)["email"]
	if q.Chosen != nil || q.Default != 1 || !q.Open() {
		t.Errorf("question = %+v, want it open with default 1", q)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"default": 1`, `"chosen": null`, `"description": "Column email differs."`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the draft lacks %s:\n%s", want, b)
		}
	}
}

func TestQuestionOpen(t *testing.T) {
	index := func(i int) *int { return &i }
	for name, tc := range map[string]struct {
		q    Question
		open bool
	}{
		"no answer":    {Question{Options: []string{"a", "b"}}, true},
		"answered":     {Question{Options: []string{"a", "b"}, Chosen: index(1)}, false},
		"stale":        {Question{Options: []string{"a", "b"}, Chosen: index(0), Stale: true}, true},
		"out of range": {Question{Options: []string{"a", "b"}, Chosen: index(2)}, true},
		"negative":     {Question{Options: []string{"a", "b"}, Chosen: index(-1)}, true},
	} {
		if got := tc.q.Open(); got != tc.open {
			t.Errorf("%s: Open() = %v, want %v", name, got, tc.open)
		}
	}
}

// The prompt asks each open question, takes a number or Enter for the
// default, and saves the answers to the draft, which ResolveIssues then
// reads like any other.
func TestPromptAsksAndSavesTheAnswers(t *testing.T) {
	ctx := context.Background()
	file := NewFilePresenter(filepath.Join(t.TempDir(), draftFileName))
	var out strings.Builder
	prompt := NewPromptPresenter(file, strings.NewReader("1\n\n"), &out)
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Leave as-is")}

	resolved, err := ResolveIssues(ctx, &MigrationPlan{}, issues, prompt, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].ID != "email" {
		t.Errorf("operations = %+v, want the alter of email alone", resolved.Operations)
	}
	got := draftQuestions(t, file)
	if c := got["email"].Chosen; c == nil || *c != 0 {
		t.Errorf("email: chosen = %v, want 0 (Apply alter)", c)
	}
	if c := got["name"].Chosen; c == nil || *c != 1 {
		t.Errorf("name: chosen = %v, want 1, the default", c)
	}
	for _, want := range []string{
		"2 questions need an answer. Each answer is saved to " + file.path,
		"Question 1 of 2 (email)",
		"users: Column email differs.",
		"  1) Apply alter\n  2) Leave as-is (default)\n",
		"Answer [2]: ",
		"Question 2 of 2 (name)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// An answered question is not asked again. A stale one is, with why, and
// its new answer clears the flag.
func TestPromptAsksOnlyOpenQuestions(t *testing.T) {
	ctx := context.Background()
	file := NewFilePresenter(filepath.Join(t.TempDir(), draftFileName))
	if err := file.Present(ctx, []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Leave as-is")}, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(file.path)
	choose(t, dir, "email", 1)
	choose(t, dir, "name", 0)

	// name's options change, so its answer goes stale.
	var out strings.Builder
	prompt := NewPromptPresenter(file, strings.NewReader("2\n"), &out)
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Drop and add", "Leave as-is")}
	if err := prompt.Present(ctx, issues, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "Question 1 of 1 (name)") || strings.Contains(out.String(), "(email)") {
		t.Errorf("want name asked alone:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "It was answered before, but its options have changed since.") {
		t.Errorf("the stale question does not say why it is asked:\n%s", out.String())
	}
	got := draftQuestions(t, file)
	if q := got["name"]; q.Stale || q.Chosen == nil || *q.Chosen != 1 {
		t.Errorf("name = %+v, want answer 1 and no stale flag", q)
	}
	if q := got["email"]; q.Chosen == nil || *q.Chosen != 1 {
		t.Errorf("email = %+v, want its answer kept", q)
	}
}

// An answer that names no option is asked again. When the input ends, the
// answers given are saved, the questions left stay open, and the run stops
// on them.
func TestPromptRetriesAndStopsAtTheEndOfInput(t *testing.T) {
	ctx := context.Background()
	file := NewFilePresenter(filepath.Join(t.TempDir(), draftFileName))
	var out strings.Builder
	prompt := NewPromptPresenter(file, strings.NewReader("x\n9\n1\n"), &out)
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Leave as-is")}

	_, err := ResolveIssues(ctx, &MigrationPlan{}, issues, prompt, true)
	if !errs.IsCode(err, errs.CodeMigrationUnresolvedIssues) || !strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "email") {
		t.Fatalf("err = %v, want the run stopped on name alone", err)
	}
	if n := strings.Count(out.String(), "Answer with a number from 1 to 2."); n != 2 {
		t.Errorf("asked again %d time(s), want 2:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "No more input. The questions left stay open in "+file.path) {
		t.Errorf("output does not say the input ended:\n%s", out.String())
	}
	got := draftQuestions(t, file)
	if c := got["email"].Chosen; c == nil || *c != 0 {
		t.Errorf("email: chosen = %v, want the answer given saved", c)
	}
	if !got["name"].Open() {
		t.Errorf("name = %+v, want it open", got["name"])
	}
}

// A prompt that waits for an answer gives up when its context ends, as on
// an interrupt, and the answers given before it stay in the draft.
func TestPromptGivesUpWhenTheContextEnds(t *testing.T) {
	file := NewFilePresenter(filepath.Join(t.TempDir(), draftFileName))
	in, answers := io.Pipe()
	defer answers.Close() // ends the read the prompt left waiting
	prompt := NewPromptPresenter(file, in, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	issues := []PlanIssue{question("email", "Apply alter", "Leave as-is"), question("name", "Apply alter", "Leave as-is")}

	done := make(chan error, 1)
	go func() { done <- prompt.Present(ctx, issues, nil) }()
	if _, err := io.WriteString(answers, "1\n"); err != nil {
		t.Fatal(err)
	}
	// The write returns once the first answer is read; the prompt then
	// saves it and waits for the second.
	deadline := time.Now().Add(5 * time.Second)
	for draftQuestions(t, file)["email"].Open() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt kept waiting after its context ended")
	}
	got := draftQuestions(t, file)
	if c := got["email"].Chosen; c == nil || *c != 0 {
		t.Errorf("email: chosen = %v, want the answer given before the interrupt", c)
	}
	if !got["name"].Open() {
		t.Errorf("name = %+v, want it open", got["name"])
	}
}

func TestLineReader(t *testing.T) {
	lr := newLineReader(strings.NewReader("one\r\ntwo\nlast"))
	for _, want := range []string{"one", "two", "last"} {
		got, err := lr.readLine(context.Background())
		if err != nil || got != want {
			t.Fatalf("readLine = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := lr.readLine(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("after the last line: err = %v, want io.EOF", err)
	}
}

// Through Generate: RunOptions.Presenter puts the prompt in front of the
// draft, and the answer it reads decides the migration.
func TestGenerateWithAPrompt(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "migrations")
	cfg := MigrationConfig{FolderPath: dir}
	declared := Declared{Schemas: declaredRegistry(t, usersWithEmail(100))}
	backend := Backend{Introspector: newMemoryDatabase(usersWithEmail(255)), Renderer: scriptRenderer{}}
	var out strings.Builder
	prompt := NewPromptPresenter(NewFilePresenter(DraftPath(NewMigrationConfig(cfg))), strings.NewReader("1\n"), &out)

	res, err := Generate(ctx, cfg, declared, backend, RunOptions{Presenter: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAwaitingConfirmation || len(res.Migration.Up) != 1 || res.Migration.Up[0].Kind != OpAlterColumn {
		t.Fatalf("status = %s, migration = %+v; want the alter", res.Status, res.Migration)
	}
	if want := `users: Column "email"'s definition narrows (may reject or truncate existing data): length 255 -> 100.`; !strings.Contains(out.String(), want) {
		t.Errorf("the prompt lacks %q:\n%s", want, out.String())
	}
}

func TestColumnChange(t *testing.T) {
	base := schema.Column{Name: "plan", Type: schema.ColTypeString, Length: 32, Nullable: true, Default: "free"}
	for want, declared := range map[string]schema.Column{
		"length 32 -> 16":                       {Name: "plan", Type: schema.ColTypeString, Length: 16, Nullable: true, Default: "free"},
		"null allowed -> not null":              {Name: "plan", Type: schema.ColTypeString, Length: 32, Default: "free"},
		`default "free" -> none`:                {Name: "plan", Type: schema.ColTypeString, Length: 32, Nullable: true},
		"type string -> int, length 32 -> none": {Name: "plan", Type: schema.ColTypeInteger, Nullable: true, Default: "free"},
		"auto-increment added":                  {Name: "plan", Type: schema.ColTypeString, Length: 32, Nullable: true, Default: "free", AutoInc: true},
	} {
		if got := columnChange(base, declared); got != want {
			t.Errorf("columnChange = %q, want %q", got, want)
		}
	}
}

// The draft is read by people, so the text in it stays as written: an arrow
// is not escaped the way encoding/json escapes it for HTML.
func TestTheDraftKeepsTextAsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), draftFileName)
	issue := question("plan", "Apply alter", "Leave as-is")
	issue.Description = "length 32 -> 16 & <more>"
	if err := NewFilePresenter(path).Present(context.Background(), []PlanIssue{issue}, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "length 32 -> 16 & <more>") {
		t.Errorf("the description was escaped:\n%s", b)
	}
}
