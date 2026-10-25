// Package cli is behemoth's command line as a library.
//
// The commands need an application's declarations, which exist only as Go
// code in that application's build: its plugins' Declare methods and its own
// schema function. A prebuilt binary cannot load them, so the application
// links this package and hands it two functions:
//
//	func main() { cli.Main(auth.Prepare, auth.MigrationBackend) }
//
// The first returns the result of Prepare, the second opens the database.
// Everything else comes from those two: the migrations folder, the path, the
// resolver and the draft file.
//
// That main is written once by hand, or generated on each run by the
// launcher in cmd/behemoth, which is what makes "behemoth generate" work in
// an application's directory.
//
// The commands follow what a developer does. generate writes the next
// migration and never changes the database, on either path. migrate applies
// what generate wrote, on PathManaged, and refuses an application that
// applies its migrations with its own tool.
//
// Some differences have more than one answer, such as a column that becomes
// shorter. generate asks them in a terminal and saves each answer to the
// draft file in the migrations folder. -answer ID=LABEL answers one on the
// command line. Without a terminal, or with -no-input, a run with an open
// question stops and lists it.
//
// Experimental: the command line is a first version. PostgreSQL and MySQL
// have a ready-made setup (postgres.MigrationBackend, mysql.MigrationBackend);
// for SQLite and SQL Server the application fills in migration.Backend
// itself. Its behavior can change between versions.
//
// Deferred: no --previous override and no schema snapshot command. See
// docs/internal/migrations/cli.md.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/go-behemoth/behemoth/engine"
	"github.com/go-behemoth/behemoth/errs"
	"github.com/go-behemoth/behemoth/identity"
	"github.com/go-behemoth/behemoth/migration"
	"github.com/go-behemoth/behemoth/telemetry"
)

// Exit codes of Run and Main.
const (
	ExitOK      = 0 // the command ran, whether or not it found changes
	ExitFailure = 1 // the command could not run or failed
	ExitUsage   = 2 // unknown command, unknown flag or stray argument
)

// program is the name the commands are documented under. It is the
// launcher's name, and reads the same when an application runs the commands
// from its own binary.
const program = "behemoth"

// PrepareFunc returns the application's declarations: engine.Prepare called
// with the plugins and the configuration the application boots with. It runs
// before any connection is opened, so it should need none, the same as
// Prepare itself.
type PrepareFunc func() (*engine.PreparedApp, error)

// BackendFunc opens the application's database and returns its migration
// backend, for example postgres.MigrationBackend(db, app.Resolver). The
// backend has to be built with app.Resolver. Its Close, when set, is called
// when the command ends.
type BackendFunc func(ctx context.Context, app *engine.PreparedApp) (migration.Backend, error)

// Main runs the command named by os.Args and exits the process with its
// exit code. An interrupt cancels the command's context. Questions are read
// from os.Stdin when it is a terminal.
func Main(prepare PrepareFunc, backend BackendFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, prepare, backend)
	stop()
	os.Exit(code)
}

// Run runs one command and returns its exit code. args are the arguments
// after the program name. Results go to stdout; errors, usage, warnings and
// the questions generate asks go to stderr.
//
// stdin is where generate reads the answers to its questions. It asks only
// when someone can answer there: stdin is not nil and, when it is an
// *os.File, a terminal. A pipe, a regular file and os.DevNull, which a
// container started without a TTY gets as its input, are not. Any other
// reader is read as answers. Without anyone to answer, a run with an open
// question stops and lists it.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, prepare PrepareFunc, backend BackendFunc) int {
	if len(args) == 0 {
		usage(stderr)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return ExitOK
	}
	// The commands are code in the process: they name themselves as the
	// caller of what they run.
	ctx = identity.WithCaller(ctx, identity.System(program))
	for _, cmd := range commands() {
		if cmd.name == args[0] {
			return cmd.exec(ctx, args[1:], stdin, stdout, stderr, prepare, backend)
		}
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n\n", program, args[0])
	usage(stderr)
	return ExitUsage
}

// command is one subcommand. Both have the same shape: a -confirm flag, the
// application's two functions, one call into core.
type command struct {
	name    string
	summary string
	confirm string // help text of -confirm
	asks    bool   // it can raise questions, so it takes -no-input and -answer

	// refuse returns why the command does not apply to an application on
	// path, or "". It is asked before the database is opened.
	refuse func(path migration.MigrationPath) string

	run func(context.Context, migration.MigrationConfig, migration.Declared, migration.Backend, migration.RunOptions) (*migration.RunResult, error)
}

func commands() []command {
	return []command{
		{
			name:    "generate",
			summary: "Show the next migration. With -confirm, write it to the migrations folder.",
			confirm: "write the migration to the migrations folder",
			asks:    true,
			refuse:  func(migration.MigrationPath) string { return "" },
			run:     migration.Generate,
		},
		{
			name:    "migrate",
			summary: "Show the migration that is waiting to be applied. With -confirm, apply it.",
			confirm: "apply the migration to the database",
			refuse: func(path migration.MigrationPath) string {
				if path == migration.PathManaged {
					return ""
				}
				return fmt.Sprintf("MigrationConfig.Path is %q: this application applies its migrations with its own tool, from the files \"%s generate\" writes. Set the path to %q to have %s apply them",
					path, program, migration.PathManaged, program)
			},
			run: migration.Migrate,
		},
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", program)
	for _, cmd := range commands() {
		fmt.Fprintf(w, "  %-9s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintf(w, "\ngenerate never changes the database. migrate does, and is for applications\nwhose MigrationConfig.Path is %q.\n", migration.PathManaged)
}

func (cmd command) exec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, prepare PrepareFunc, backend BackendFunc) int {
	name := program + " " + cmd.name

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	confirm := fs.Bool("confirm", false, cmd.confirm)
	noInput := new(bool)
	given := answers{}
	flags := "[-confirm]"
	if cmd.asks {
		noInput = fs.Bool("no-input", false, "never ask a question, even in a terminal; stop when one has no answer")
		fs.Var(given, "answer", "answer a question, as `ID=LABEL`: the question's id and the label of one of its options. Repeat it for several")
		flags += " [-no-input] [-answer ID=LABEL]..."
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s %s\n\n%s\n\nFlags:\n", name, flags, cmd.summary)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage // Parse has printed the problem and the usage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fs.Arg(0))
		return ExitUsage
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return ExitFailure
	}
	if prepare == nil || backend == nil {
		return fail(errors.New("the command line needs both a PrepareFunc and a BackendFunc"))
	}

	app, err := prepare()
	if err != nil {
		return fail(err)
	}
	if app == nil {
		return fail(errors.New("the PrepareFunc returned no application"))
	}
	// Asked before the database is opened: the answer needs no connection.
	if reason := cmd.refuse(app.Migration.Path); reason != "" {
		return fail(errors.New(reason))
	}

	be, err := backend(ctx, app)
	if err != nil {
		return fail(err)
	}
	if be.Close != nil {
		defer be.Close()
	}

	draft := migration.DraftPath(app.Migration)
	opts := migration.RunOptions{
		Confirm: *confirm,
		// The managed path's warnings (a script that could not be written, a
		// driver that is not fully atomic) are lines on stderr. No audit
		// recorder is set, so an applied migration writes no audit event
		// here: the audit table may be the one this migration creates.
		Telemetry: telemetry.New(telemetry.NewTextLogger(stderr, slog.LevelWarn), nil, nil),
	}
	asked := false
	if cmd.asks {
		file := migration.NewFilePresenter(draft)
		file.Given = given
		opts.Presenter = file
		if in := answerer(stdin); !*noInput && in != nil {
			// The questions go to stderr with the other text that is not
			// a result, so they still show when stdout is redirected.
			opts.Presenter = migration.NewPromptPresenter(file, in, stderr)
			asked = true
		}
	}

	res, err := cmd.run(ctx, app.Migration, app.Declared(), be, opts)
	if err != nil {
		if asked && errors.Is(err, context.Canceled) {
			fmt.Fprintf(stderr, "\n%s: interrupted. The answers given so far are saved in %s.\n", name, draft)
			return ExitFailure
		}
		if errs.IsCode(err, errs.CodeMigrationUnresolvedIssues) && openQuestions(stderr, name, draft, *noInput) {
			return ExitFailure
		}
		if de, ok := errors.AsType[*errs.DomainError](err); ok && de.Code == errs.CodeMigrationInvalidAnswer && de.Original != nil {
			// The reason alone: the developer fixes a flag, and the
			// operation that refused it says nothing to them.
			return fail(de.Original)
		}
		return fail(err)
	}

	report(stdout, app.Migration, res)
	return ExitOK
}

// answers is the -answer flag: repeatable, each value ID=LABEL, kept by ID.
type answers map[string]string

func (a answers) String() string {
	ids := make([]string, 0, len(a))
	for id := range a {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = id + "=" + a[id]
	}
	return strings.Join(values, ", ")
}

func (a answers) Set(value string) error {
	id, label, ok := strings.Cut(value, "=")
	id, label = strings.TrimSpace(id), strings.TrimSpace(label)
	if !ok || id == "" || label == "" {
		return fmt.Errorf("%q is not ID=LABEL, such as \"alter_column_users_plan=Apply alter\"", value)
	}
	if _, twice := a[id]; twice {
		return fmt.Errorf("%s is answered twice", id)
	}
	a[id] = label
	return nil
}

// answerer returns stdin when someone can answer there, and nil otherwise:
// no input, or a file that is not a terminal. A terminal is a character
// device other than os.DevNull, which a container without a TTY gets as its
// input. A check with golang.org/x/term would be exact, but the launcher
// builds this package inside the application's module, whose go.sum has no
// checksum for a module only this package imports.
func answerer(stdin io.Reader) io.Reader {
	if stdin == nil {
		return nil
	}
	f, ok := stdin.(*os.File)
	if !ok {
		return stdin
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return nil
	}
	return f
}

// openQuestions reports a run that stopped on open questions: it lists them,
// with the "chosen" value of each option, and says how to answer them, with
// an -answer flag for the first one as an example. noInput says that
// -no-input kept a terminal from being asked. It returns false when the
// draft lists none, for the caller to report the error as it is.
func openQuestions(w io.Writer, name, draft string, noInput bool) bool {
	questions, err := migration.NewFilePresenter(draft).Questions()
	if err != nil {
		return false
	}
	var open []migration.Question
	for _, q := range questions {
		if q.Open() {
			open = append(open, q)
		}
	}
	switch len(open) {
	case 0:
		return false
	case 1:
		fmt.Fprintf(w, "%s: 1 question has no answer:\n", name)
	default:
		fmt.Fprintf(w, "%s: %d questions have no answer:\n", name, len(open))
	}
	for _, q := range open {
		fmt.Fprintf(w, "\n  %s\n  %s: %s\n", q.ID, q.Table, q.Description)
		if q.Stale {
			fmt.Fprintln(w, "  It was answered before, but its options have changed since.")
		}
		for i, label := range q.Options {
			mark := ""
			if i == q.Default {
				mark = " (default)"
			}
			fmt.Fprintf(w, "    \"chosen\": %d  %s%s\n", i, label, mark)
		}
	}
	terminal := "in a terminal"
	if noInput {
		terminal = "in a terminal without -no-input"
	}
	first := open[0]
	label := first.Options[0]
	if first.Default >= 0 && first.Default < len(first.Options) {
		label = first.Options[first.Default]
	}
	fmt.Fprintf(w, "\nAnswer %s, with -answer, such as -answer '%s=%s', or by setting \"chosen\" in %s (and deleting \"stale\" where it is set). Then run the command again.\n",
		terminal, first.ID, label, draft)
	return true
}

// report prints a run's outcome: core's message, the operations of the
// migration it concerns, and the answers it read from the draft.
func report(w io.Writer, cfg migration.MigrationConfig, res *migration.RunResult) {
	fmt.Fprintln(w, res.Message)

	if m := res.Migration; m != nil && len(m.Up) > 0 {
		fmt.Fprintf(w, "\n%s, %d operation(s):\n", m.ID, len(m.Up))
		for _, op := range m.Up {
			fmt.Fprintf(w, "  %s\n", describe(op))
		}
	}

	// The answers decided what the migration holds. A "Leave as-is" keeps a
	// difference out of it, and the run can then report no changes while a
	// column still differs, so each answer is named.
	draft := migration.DraftPath(cfg)
	questions, err := migration.NewFilePresenter(draft).Questions()
	if err != nil {
		return
	}
	var answered []migration.Question
	for _, q := range questions {
		if !q.Open() {
			answered = append(answered, q)
		}
	}
	if len(answered) == 0 {
		return
	}
	fmt.Fprintf(w, "\nAnswers read from %s:\n", draft)
	for _, q := range answered {
		fmt.Fprintf(w, "  %s: %s\n", q.ID, q.Options[*q.Chosen])
	}
	fmt.Fprintln(w, "To change an answer, edit the file and run the command again.")
}

// describe is one operation as a line of the report.
func describe(op migration.SchemaOperation) string {
	switch op.Kind {
	case migration.OpCreateTable:
		return "create table " + op.Table
	case migration.OpDropTable:
		return "drop table " + op.Table
	case migration.OpAddColumn:
		return "add column " + op.Table + "." + columnName(op)
	case migration.OpAlterColumn:
		return "alter column " + op.Table + "." + columnName(op)
	case migration.OpDropColumn:
		return "drop column " + op.Table + "." + op.ColumnName
	case migration.OpRenameColumn:
		return "rename column " + op.Table + "." + op.ColumnName + " to " + op.NewColumnName
	case migration.OpAddIndex:
		if op.Index != nil {
			return "add index " + op.Index.Name + " on " + op.Table
		}
	case migration.OpDropIndex:
		return "drop index " + op.IndexName + " on " + op.Table
	case migration.OpAddForeignKey:
		if op.ForeignKey != nil {
			return "add foreign key " + op.ForeignKey.Name + " on " + op.Table
		}
	case migration.OpDropForeignKey:
		return "drop foreign key " + op.ForeignKeyName + " on " + op.Table
	}
	return string(op.Kind) + " " + op.Table
}

func columnName(op migration.SchemaOperation) string {
	if op.Column != nil {
		return op.Column.Name
	}
	return op.ColumnName
}
