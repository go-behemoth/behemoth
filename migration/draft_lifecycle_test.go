package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/schema"
)

// usersWithEmail is the users table with an email column of length n.
func usersWithEmail(n int) schema.Table {
	return schema.Table{Name: "users", Columns: []schema.Column{
		{Name: "id", Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
		{Name: "email", Type: schema.ColTypeString, Length: n},
	}}
}

const narrowEmail = "alter_column_users_email"

// answers returns the draft's chosen answers by issue ID.
func answers(t *testing.T, dir string) map[string]int {
	t.Helper()
	draft, err := NewFilePresenter(filepath.Join(dir, draftFileName)).readDraft()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range draft.Entries {
		if e.Chosen != nil {
			out[e.ID] = *e.Chosen
		}
	}
	return out
}

// choose answers the draft's question id with option index, as a developer
// does by editing the file.
func choose(t *testing.T, dir, id string, index int) {
	t.Helper()
	p := NewFilePresenter(filepath.Join(dir, draftFileName))
	draft, err := p.readDraft()
	if err != nil {
		t.Fatal(err)
	}
	for i := range draft.Entries {
		if draft.Entries[i].ID == id {
			draft.Entries[i].Chosen = &index
			if err := p.writeDraft(draft); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("the draft has no question %q: %+v", id, draft.Entries)
}

// An answer that produced operations is used up by the migration it was
// written into. A later change to the same column, here a second narrowing,
// is asked again and starts from "Leave as-is", instead of reusing "Apply
// alter". On both paths.
func TestAnAnswerIsUsedUpByTheMigrationItWrites(t *testing.T) {
	ctx := context.Background()

	t.Run("generate only", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "migrations")
		cfg := MigrationConfig{FolderPath: dir}
		db := newMemoryDatabase(usersWithEmail(255))
		backend := Backend{Introspector: db, Renderer: scriptRenderer{}}
		generate := func(length int, confirm bool) *RunResult {
			t.Helper()
			declared := Declared{Schemas: declaredRegistry(t, usersWithEmail(length))}
			res, err := Generate(ctx, cfg, declared, backend, RunOptions{Confirm: confirm})
			if err != nil {
				t.Fatal(err)
			}
			return res
		}

		// The first narrowing is asked, and answered with its default.
		if res := generate(100, false); res.Status != StatusNoChanges {
			t.Fatalf("first narrowing: status = %s, want %s", res.Status, StatusNoChanges)
		}
		choose(t, dir, narrowEmail, 0) // Apply alter

		res := generate(100, true)
		if res.Status != StatusGenerated || len(res.Migration.Up) != 1 || res.Migration.Up[0].Kind != OpAlterColumn {
			t.Fatalf("with Apply alter: status = %s, migration = %+v; want the alter written", res.Status, res.Migration)
		}
		if !strings.Contains(res.Message, "1 answer(s) it used were removed from the draft") {
			t.Errorf("message does not say the answer was used up: %q", res.Message)
		}
		if _, ok := answers(t, dir)[narrowEmail]; ok {
			t.Errorf("the answer is still in the draft after its migration was written: %v", answers(t, dir))
		}

		// The application applies the script, and narrows the column again.
		db.live["users"] = usersWithEmail(100)
		if res := generate(50, false); res.Status != StatusNoChanges {
			t.Fatalf("second narrowing: status = %s, migration = %+v; want it asked again, not altered", res.Status, res.Migration)
		}
		if got, ok := answers(t, dir)[narrowEmail]; !ok || got != 1 {
			t.Errorf("second narrowing: answer = %d (present %v), want the default, Leave as-is (1)", got, ok)
		}
	})

	t.Run("managed", func(t *testing.T) {
		r := newManagedRun(t, newMemoryDatabase(), usersWithEmail(255))
		r.generate(true)
		r.migrate(true)

		r.declared = Declared{Schemas: declaredRegistry(t, usersWithEmail(100))}
		if res := r.generate(false); res.Status != StatusNoChanges {
			t.Fatalf("first narrowing: status = %s, want %s", res.Status, StatusNoChanges)
		}
		choose(t, r.cfg.FolderPath, narrowEmail, 0)
		if res := r.generate(true); res.Status != StatusGenerated || !strings.Contains(res.Message, "removed from the draft") {
			t.Fatalf("with Apply alter: status = %s, message = %q", res.Status, res.Message)
		}
		r.migrate(true)

		r.declared = Declared{Schemas: declaredRegistry(t, usersWithEmail(50))}
		if res := r.generate(false); res.Status != StatusNoChanges {
			t.Fatalf("second narrowing: status = %s, migration = %+v; want it asked again, not altered", res.Status, res.Migration)
		}
		if got := answers(t, r.cfg.FolderPath)[narrowEmail]; got != 1 {
			t.Errorf("second narrowing: answer = %d, want the default, Leave as-is (1)", got)
		}
	})
}

// A "Leave as-is" answer produces no operation, so a migration written in
// the same run does not use it up: it keeps telling planning not to ask
// again.
func TestLeaveAsIsSurvivesAWrittenMigration(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "migrations")
	cfg := MigrationConfig{FolderPath: dir}
	db := newMemoryDatabase(usersWithEmail(255))

	declaredUsers := usersWithEmail(100) // narrowed, and left as it is
	declaredUsers.Columns = append(declaredUsers.Columns, schema.Column{Name: "name", Type: schema.ColTypeString, Length: 64, Nullable: true})
	declared := Declared{Schemas: declaredRegistry(t, declaredUsers)}

	res, err := Generate(ctx, cfg, declared, Backend{Introspector: db, Renderer: scriptRenderer{}}, RunOptions{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusGenerated || len(res.Migration.Up) != 1 || res.Migration.Up[0].Kind != OpAddColumn {
		t.Fatalf("status = %s, migration = %+v; want the added column alone", res.Status, res.Migration)
	}
	if strings.Contains(res.Message, "removed from the draft") {
		t.Errorf("message says an answer was used up: %q", res.Message)
	}
	if got, ok := answers(t, dir)[narrowEmail]; !ok || got != 1 {
		t.Errorf("Leave as-is: answer = %d (present %v), want it kept", got, ok)
	}
}

// Consume removes the listed entries and keeps the others. A missing draft
// has nothing to remove and is not created.
func TestFilePresenterConsume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), draftFileName)
	p := NewFilePresenter(path)

	if err := p.Consume(ctx, []string{"a"}); err != nil {
		t.Fatalf("missing draft: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Consume created the draft: %v", err)
	}

	var issues []PlanIssue
	for _, id := range []string{"a", "b", "c"} {
		issues = append(issues, PlanIssue{ID: id, Options: []ResolutionOption{{Label: "Do it"}, {Label: "Leave as-is"}}, Default: 1})
	}
	if err := p.Present(ctx, issues, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Consume(ctx, []string{"b", "unknown"}); err != nil {
		t.Fatal(err)
	}
	left, err := p.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := left["b"]; ok || len(left) != 2 {
		t.Errorf("after consuming b: %v, want a and c", left)
	}

	if err := p.Consume(ctx, []string{"a", "c"}); err != nil {
		t.Fatal(err)
	}
	if left, _ := p.Collect(ctx); len(left) != 0 {
		t.Errorf("after consuming all: %v, want none", left)
	}
}

// failingConsumer is a FilePresenter whose Consume fails, as a draft that
// can no longer be written does.
type failingConsumer struct{ *FilePresenter }

func (failingConsumer) Consume(context.Context, []string) error { return errors.New("disk full") }

// When the used answers can't be removed, the run fails with a code of its
// own and names the entries. The migration's files stay: they were written
// first.
func TestAFailedConsumeNamesTheAnswers(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "migrations")
	cfg := NewMigrationConfig(MigrationConfig{FolderPath: dir})
	declared := Declared{Schemas: declaredRegistry(t, usersWithEmail(100))}
	deps := GenerateDeps{
		Introspector: newMemoryDatabase(usersWithEmail(255)),
		Presenter:    failingConsumer{NewFilePresenter(DraftPath(cfg))},
		Generator:    &DefaultMigrationGenerator{},
		Renderer:     scriptRenderer{},
	}

	if _, err := RunGenerateCLI(ctx, cfg, declared, deps, false); err != nil {
		t.Fatal(err)
	}
	choose(t, dir, narrowEmail, 0)
	_, err := RunGenerateCLI(ctx, cfg, declared, deps, true)
	if !errs.IsCode(err, errs.CodeMigrationDraftWriteFailed) || !strings.Contains(err.Error(), narrowEmail) {
		t.Fatalf("err = %v, want %s naming %s", err, errs.CodeMigrationDraftWriteFailed, narrowEmail)
	}
	if files := migrationFiles(t, dir); len(files) != 2 {
		t.Errorf("files = %v, want the migration and its script", files)
	}
}
