package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-behemoth/behemoth"
	"github.com/go-behemoth/behemoth/clause"
	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/schema"
)

// note is an application table, declared the way an application does it:
// in a function only its own build contains.
type note struct{}

func (note) SchemaName() string     { return "notes" }
func (note) PrimaryKeyName() string { return "id" }
func (note) PrimaryKeyField() any   { return nil }
func (note) New() behemoth.Model    { return note{} }

func declareNotes(reg schema.Registry) error {
	return reg.Declare(note{}, schema.Table{Name: "notes", Columns: []schema.Column{
		{Name: "id", Type: schema.ColTypeString, Length: 36, PrimaryKey: true},
		{Name: "body", Type: schema.ColTypeText},
	}})
}

// liveTables is the database: the tables that exist, by canonical name.
type liveTables map[string]schema.Table

func (l liveTables) TableExists(_ context.Context, name string) (bool, error) {
	_, ok := l[name]
	return ok, nil
}

func (l liveTables) Introspect(_ context.Context, name string) (migration.IntrospectedTable, error) {
	t, ok := l[name]
	return migration.IntrospectedTable{Schema: t, Kind: migration.ObjectTable, Exists: ok}, nil
}

type scriptRenderer struct{}

func (scriptRenderer) RenderMigration(context.Context, migration.Migration) (string, error) {
	return "-- script\n", nil
}
func (scriptRenderer) FileExtension() string { return ".sql" }

// fixture is an application and its database, as the two functions the
// command line is given.
type fixture struct {
	dir     string // the migrations folder
	path    migration.MigrationPath
	live    liveTables
	ledger  []migration.MigrationLedgerEntry
	version string // the snapshot's, set by the last applied migration
	tables  map[string]schema.Table
	opened  int // calls to the BackendFunc
	closed  int // calls to the backend's Close
	prepErr error
	openErr error
}

func (f *fixture) prepare() (*engine.PreparedApp, error) {
	if f.prepErr != nil {
		return nil, f.prepErr
	}
	return engine.Prepare(nil, engine.PrepareConfig{
		Migration: migration.MigrationConfig{FolderPath: f.dir, Path: f.path},
		Schema:    declareNotes,
	})
}

func (f *fixture) backend(context.Context, *engine.PreparedApp) (migration.Backend, error) {
	f.opened++
	if f.openErr != nil {
		return migration.Backend{}, f.openErr
	}
	return migration.Backend{
		Introspector: fixtureIntrospector{f},
		Renderer:     scriptRenderer{},
		Driver:       fixtureDriver{f},
		DB:           fixtureLedger{f: f},
		Close:        func() error { f.closed++; return nil },
	}, nil
}

// fixtureIntrospector adds the ledger table to the live ones: like a real
// driver's, it exists once a migration has been recorded.
type fixtureIntrospector struct{ f *fixture }

func (i fixtureIntrospector) TableExists(ctx context.Context, name string) (bool, error) {
	if name == migration.LedgerCanonicalName {
		return len(i.f.ledger) > 0, nil
	}
	return i.f.live.TableExists(ctx, name)
}

func (i fixtureIntrospector) Introspect(ctx context.Context, name string) (migration.IntrospectedTable, error) {
	return i.f.live.Introspect(ctx, name)
}

// fixtureDriver applies a migration by taking the tables the runner
// projected for the snapshot as the live ones.
type fixtureDriver struct{ f *fixture }

func (d fixtureDriver) ApplyMigration(_ context.Context, req migration.MigrationRequest) error {
	for name, table := range req.SnapshotUpdate.Tables {
		d.f.live[name] = table
	}
	return d.RecordBaseline(context.Background(), req)
}

func (d fixtureDriver) RecordBaseline(_ context.Context, req migration.MigrationRequest) error {
	d.f.ledger = append(d.f.ledger, req.LedgerEntry)
	d.f.version, d.f.tables = req.SnapshotUpdate.Version, req.SnapshotUpdate.Tables
	return nil
}

func (fixtureDriver) AtomicityLevel() migration.AtomicityLevel { return migration.AtomicityFull }

// fixtureLedger is what the runner reads: the ledger and the snapshot. It
// never writes through the database, so the rest is left to the nil
// interface.
type fixtureLedger struct {
	behemoth.Database
	f *fixture
}

func (l fixtureLedger) FindMany(context.Context, behemoth.Model, clause.Expression, *behemoth.QueryOptions) ([]behemoth.Model, error) {
	out := make([]behemoth.Model, len(l.f.ledger))
	for i := range l.f.ledger {
		entry := l.f.ledger[i]
		out[i] = &entry
	}
	return out, nil
}

func (l fixtureLedger) FindOne(context.Context, behemoth.Model, clause.Expression) (behemoth.Model, error) {
	if l.f.version == "" {
		return nil, errs.NewNotFound("fixtureLedger.FindOne", "schema_snapshot", nil)
	}
	return &migration.SchemaSnapshot{Version: l.f.version, Tables: l.f.tables}, nil
}

// run runs the command line against f, with nobody to answer its
// questions, and returns its exit code and output.
func (f *fixture) run(args ...string) (code int, stdout, stderr string) {
	return f.runWith(nil, args...)
}

// runWith runs the command line against f, reading answers from stdin.
func (f *fixture) runWith(stdin io.Reader, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, stdin, &out, &errOut, f.prepare, f.backend)
	return code, out.String(), errOut.String()
}

// narrowNotes makes the database's notes.id wider than the declared one,
// so generate raises a question about narrowing it.
func (f *fixture) narrowNotes(t *testing.T) {
	t.Helper()
	app, err := f.prepare()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range app.Schemas.All() {
		if table.Name == "notes" {
			cols := append([]schema.Column(nil), table.Columns...)
			cols[0].Length += 10
			table.Columns = cols
		}
		f.live[table.Name] = table
	}
}

func newFixture(t *testing.T) *fixture {
	return &fixture{dir: filepath.Join(t.TempDir(), "migrations"), live: liveTables{}}
}

func TestUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		"no command":      {nil, ExitUsage, "", "Usage: behemoth <command>"},
		"help":            {[]string{"help"}, ExitOK, "Usage: behemoth <command>", ""},
		"-h":              {[]string{"-h"}, ExitOK, "generate", ""},
		"unknown command": {[]string{"deploy"}, ExitUsage, "", `unknown command "deploy"`},
		"unknown flag":    {[]string{"generate", "-force"}, ExitUsage, "", "flag provided but not defined"},
		"stray argument":  {[]string{"generate", "now"}, ExitUsage, "", `unexpected argument "now"`},
		"command help":    {[]string{"generate", "-h"}, ExitOK, "", "Usage: behemoth generate [-confirm]"},
		"migrate help":    {[]string{"migrate", "-h"}, ExitOK, "", "Usage: behemoth migrate [-confirm]"},
	} {
		f := newFixture(t)
		code, stdout, stderr := f.run(tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit code = %d, want %d", name, code, tc.code)
		}
		if !strings.Contains(stdout, tc.inStdout) || !strings.Contains(stderr, tc.inStderr) {
			t.Errorf("%s: stdout = %q, stderr = %q; want %q and %q in them", name, stdout, stderr, tc.inStdout, tc.inStderr)
		}
		if f.opened != 0 {
			t.Errorf("%s: the database was opened", name)
		}
	}
}

func TestGenerateShowsThenWrites(t *testing.T) {
	f := newFixture(t)

	// Without -confirm: the migration is shown and nothing is written. It
	// holds the application's own table, which only its build declares.
	code, stdout, stderr := f.run("generate")
	if code != ExitOK {
		t.Fatalf("preview: exit code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"create table notes", "create table users", "0001"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("preview: stdout lacks %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "0001_create_accounts_and_8_more.json")); !os.IsNotExist(err) {
		t.Fatalf("preview wrote the migration (stat error: %v)", err)
	}

	// With -confirm: the migration and its script are written.
	code, stdout, stderr = f.run("generate", "-confirm")
	if code != ExitOK {
		t.Fatalf("confirm: exit code = %d, stderr = %q", code, stderr)
	}
	for _, file := range []string{"0001_create_accounts_and_8_more.json", "0001_create_accounts_and_8_more.sql"} {
		if _, err := os.Stat(filepath.Join(f.dir, file)); err != nil {
			t.Errorf("confirm: %s was not written: %v\nstdout: %s", file, err, stdout)
		}
	}

	if f.opened != 2 || f.closed != 2 {
		t.Errorf("opened = %d, closed = %d; want the backend closed after each of the two runs", f.opened, f.closed)
	}
}

func TestGenerateWithNothingToDo(t *testing.T) {
	f := newFixture(t)
	app, err := f.prepare()
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range app.Schemas.All() {
		f.live[table.Name] = table // the database already matches the declaration
	}

	code, stdout, stderr := f.run("generate", "-confirm")
	if code != ExitOK || !strings.Contains(stdout, "No schema changes") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q; want no changes", code, stdout, stderr)
	}
}

// A narrowing change raises a question. With nobody to answer it, the run
// stops and lists the question, with the "chosen" value of each option and
// the draft to set it in, instead of leaving the change out unasked.
func TestGenerateStopsOnAnOpenQuestion(t *testing.T) {
	f := newFixture(t)
	f.narrowNotes(t)

	code, stdout, stderr := f.run("generate", "-confirm")
	if code != ExitFailure {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, ExitFailure, stdout, stderr)
	}
	draft := filepath.Join(f.dir, "draft.json")
	for _, want := range []string{
		"behemoth generate: 1 question has no answer:\n",
		"  alter_column_notes_id\n",
		`notes: Column "id"'s definition narrows (may reject or truncate existing data): length 46 -> 36.`,
		`    "chosen": 0  Apply alter`,
		`    "chosen": 1  Leave as-is (default)`,
		"Answer in a terminal, with -answer, such as -answer 'alter_column_notes_id=Leave as-is', or by setting \"chosen\" in " + draft + ` (and deleting "stale" where it is set). Then run the command again.`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(f.dir, "0001_*")); len(files) != 0 {
		t.Errorf("a run that stopped wrote %v", files)
	}
}

// In a terminal, generate asks the question on stderr and saves the answer.
// The run's report names the answers it read, and the next run, with nobody
// to ask, reads the saved one.
func TestGenerateAsksInATerminal(t *testing.T) {
	f := newFixture(t)
	f.narrowNotes(t)
	draft := filepath.Join(f.dir, "draft.json")

	code, stdout, stderr := f.runWith(strings.NewReader("1\n"), "generate")
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"1 question needs an answer. Each answer is saved to " + draft,
		"Question 1 of 1 (alter_column_notes_id)",
		"  1) Apply alter\n  2) Leave as-is (default)\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	for _, want := range []string{"alter column notes.id", "Answers read from " + draft + ":\n  alter_column_notes_id: Apply alter"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = f.run("generate", "-confirm")
	if code != ExitOK || !strings.Contains(stdout, "alter column notes.id") || !strings.Contains(stdout, "removed from the draft") {
		t.Errorf("generate -confirm with the saved answer: exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// -no-input never asks, even with someone there to answer.
func TestGenerateWithNoInputDoesNotAsk(t *testing.T) {
	f := newFixture(t)
	f.narrowNotes(t)

	code, _, stderr := f.runWith(strings.NewReader("1\n"), "generate", "-no-input")
	if code != ExitFailure || strings.Contains(stderr, "Question 1 of 1") || !strings.Contains(stderr, "1 question has no answer") {
		t.Errorf("exit code = %d, stderr = %q; want the run stopped without asking", code, stderr)
	}
	if !strings.Contains(stderr, "Answer in a terminal without -no-input, with -answer") {
		t.Errorf("stderr = %q; want the way to be asked named", stderr)
	}
	if code, _, stderr := f.run("migrate", "-no-input"); code != ExitUsage || !strings.Contains(stderr, "flag provided but not defined") {
		t.Errorf("migrate -no-input: exit code = %d, stderr = %q; want a usage error, migrate asks nothing", code, stderr)
	}
}

// An interrupt while generate waits for an answer ends the command, and says
// that the answers given are kept.
func TestGenerateInterruptedWhileAsking(t *testing.T) {
	f := newFixture(t)
	f.narrowNotes(t)
	in, answers := io.Pipe()
	defer answers.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	var out, errOut bytes.Buffer
	code := Run(ctx, []string{"generate"}, in, &out, &errOut, f.prepare, f.backend)
	if code != ExitFailure || !strings.Contains(errOut.String(), "interrupted. The answers given so far are saved in") {
		t.Errorf("exit code = %d, stderr = %q", code, errOut.String())
	}
}

// Only a terminal is asked: not a pipe, a regular file or os.DevNull. A
// reader that is not a file is read as answers.
func TestAnswerer(t *testing.T) {
	if answerer(nil) != nil {
		t.Error("nil input: want nobody to ask")
	}
	r := strings.NewReader("1\n")
	if answerer(r) != io.Reader(r) {
		t.Error("a reader that is not a file: want it read")
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	regular, err := os.Create(filepath.Join(t.TempDir(), "answers"))
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	for name, f := range map[string]*os.File{"a pipe": pr, "a regular file": regular, "os.DevNull": null} {
		if answerer(f) != nil {
			t.Errorf("%s: want nobody to ask", name)
		}
	}
}

// The managed path: generate writes and leaves the database alone, migrate
// applies what generate wrote.
func TestGenerateThenMigrateOnTheManagedPath(t *testing.T) {
	f := newFixture(t)
	f.path = migration.PathManaged

	code, stdout, stderr := f.run("generate", "-confirm")
	if code != ExitOK || !strings.Contains(stdout, "create table notes") {
		t.Fatalf("generate -confirm: exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "0001_create_accounts_and_8_more.json")); err != nil {
		t.Fatalf("generate -confirm wrote no migration: %v", err)
	}
	if len(f.ledger) != 0 || len(f.live) != 0 {
		t.Fatalf("generate changed the database: ledger = %v, %d live table(s)", f.ledger, len(f.live))
	}

	// Without -confirm, migrate names the migration and applies nothing.
	code, stdout, stderr = f.run("migrate")
	if code != ExitOK || !strings.Contains(stdout, "0001") || len(f.ledger) != 0 {
		t.Fatalf("migrate: exit code = %d, stdout = %q, stderr = %q, ledger = %v", code, stdout, stderr, f.ledger)
	}

	code, stdout, stderr = f.run("migrate", "-confirm")
	if code != ExitOK || len(f.ledger) != 1 || f.ledger[0].ID != "0001" {
		t.Fatalf("migrate -confirm: exit code = %d, stdout = %q, stderr = %q, ledger = %v", code, stdout, stderr, f.ledger)
	}
	if _, ok := f.live["notes"]; !ok {
		t.Error("migrate -confirm did not create the application's table")
	}

	if code, stdout, _ = f.run("migrate", "-confirm"); code != ExitOK || !strings.Contains(stdout, "Nothing to apply") {
		t.Errorf("migrate again: exit code = %d, stdout = %q", code, stdout)
	}
}

// An application that applies its migrations with its own tool has nothing
// for migrate to do, and learns that before its database is opened.
func TestMigrateRefusesTheGenerateOnlyPath(t *testing.T) {
	f := newFixture(t)

	code, _, stderr := f.run("migrate", "-confirm")
	if code != ExitFailure || !strings.Contains(stderr, `MigrationConfig.Path is "generate_only"`) || !strings.Contains(stderr, "behemoth generate") {
		t.Errorf("exit code = %d, stderr = %q; want a failure that names the path and the other command", code, stderr)
	}
	if f.opened != 0 {
		t.Error("the database was opened for a command that could not run")
	}
}

func TestGenerateReportsFailures(t *testing.T) {
	f := newFixture(t)
	f.prepErr = errors.New("plugin \"x\" Declare failed")
	if code, _, stderr := f.run("generate"); code != ExitFailure || !strings.Contains(stderr, "Declare failed") {
		t.Errorf("failing Prepare: exit code = %d, stderr = %q", code, stderr)
	}

	f = newFixture(t)
	f.openErr = errors.New("connection refused")
	if code, _, stderr := f.run("generate"); code != ExitFailure || !strings.Contains(stderr, "connection refused") {
		t.Errorf("failing BackendFunc: exit code = %d, stderr = %q", code, stderr)
	}

	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"generate"}, nil, &out, &errOut, nil, nil); code != ExitFailure {
		t.Errorf("no functions: exit code = %d, want %d", code, ExitFailure)
	}
}

func TestDescribe(t *testing.T) {
	for want, op := range map[string]migration.SchemaOperation{
		"create table users":             {Kind: migration.OpCreateTable, Table: "users"},
		"add column users.plan":          {Kind: migration.OpAddColumn, Table: "users", Column: &schema.Column{Name: "plan"}},
		"alter column users.name":        {Kind: migration.OpAlterColumn, Table: "users", Column: &schema.Column{Name: "name"}},
		"drop column users.plan":         {Kind: migration.OpDropColumn, Table: "users", ColumnName: "plan"},
		"rename column users.a to b":     {Kind: migration.OpRenameColumn, Table: "users", ColumnName: "a", NewColumnName: "b"},
		"add index idx on users":         {Kind: migration.OpAddIndex, Table: "users", Index: &schema.Index{Name: "idx"}},
		"drop index idx on users":        {Kind: migration.OpDropIndex, Table: "users", IndexName: "idx"},
		"add foreign key fk on sessions": {Kind: migration.OpAddForeignKey, Table: "sessions", ForeignKey: &schema.ForeignKey{Name: "fk"}},
		"drop foreign key fk on users":   {Kind: migration.OpDropForeignKey, Table: "users", ForeignKeyName: "fk"},
		"drop table users":               {Kind: migration.OpDropTable, Table: "users"},
		"add_index users":                {Kind: migration.OpAddIndex, Table: "users"}, // a malformed operation still gets a line
	} {
		if got := describe(op); got != want {
			t.Errorf("describe(%s) = %q, want %q", op.Kind, got, want)
		}
	}
}

// -answer answers a question without a terminal, and the run goes on.
func TestGenerateWithAnswerFlags(t *testing.T) {
	f := newFixture(t)
	f.narrowNotes(t)

	code, stdout, stderr := f.run("generate", "-confirm", "-answer", "alter_column_notes_id=Apply alter")
	if code != ExitOK {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "alter column notes.id") || !strings.Contains(stdout, "removed from the draft") {
		t.Errorf("stdout = %q, want the alter written with the given answer", stdout)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "0001_alter_column_notes_id.json")); err != nil {
		t.Errorf("the migration was not written: %v", err)
	}
}

// A malformed or repeated -answer is a usage error, found before the
// database is opened. migrate asks nothing, so it has no -answer.
func TestAnswerFlagUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		inStderr string
	}{
		"no equals sign": {[]string{"generate", "-answer", "alter_column_notes_id"}, `"alter_column_notes_id" is not ID=LABEL`},
		"empty label":    {[]string{"generate", "-answer", "alter_column_notes_id= "}, "is not ID=LABEL"},
		"answered twice": {[]string{"generate", "-answer", "a=Apply alter", "-answer", "a=Leave as-is"}, "a is answered twice"},
		"on migrate":     {[]string{"migrate", "-answer", "a=Apply alter"}, "flag provided but not defined: -answer"},
	} {
		f := newFixture(t)
		code, _, stderr := f.run(tc.args...)
		if code != ExitUsage || !strings.Contains(stderr, tc.inStderr) {
			t.Errorf("%s: exit code = %d, stderr = %q; want %d and %q", name, code, stderr, ExitUsage, tc.inStderr)
		}
		if f.opened != 0 {
			t.Errorf("%s: the database was opened", name)
		}
	}

	f := newFixture(t)
	if code, _, stderr := f.run("generate", "-h"); code != ExitOK || !strings.Contains(stderr, "-answer ID=LABEL") {
		t.Errorf("generate -h: exit code = %d, stderr = %q; want -answer described", code, stderr)
	}
}

// An answer for a question the run does not raise, or with a label that is
// not one of its options, stops the run and says why.
func TestGenerateRefusesAnAnswerThatAnswersNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		answer   string
		inStderr string
	}{
		"unknown question": {"alter_column_notes_idd=Apply alter", `names a question this run does not raise: it raises alter_column_notes_id`},
		"unknown option":   {"alter_column_notes_id=Alter", `behemoth generate: the answer "Alter" for alter_column_notes_id is not one of its options: "Apply alter", "Leave as-is"`},
	} {
		f := newFixture(t)
		f.narrowNotes(t)
		code, _, stderr := f.run("generate", "-answer", tc.answer)
		if code != ExitFailure || !strings.Contains(stderr, tc.inStderr) {
			t.Errorf("%s: exit code = %d, stderr = %q; want %d and %q", name, code, stderr, ExitFailure, tc.inStderr)
		}
	}
}
