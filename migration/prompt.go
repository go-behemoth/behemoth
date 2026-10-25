package migration

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-behemoth/behemoth/errs"
)

// PromptPresenter asks the open questions of a run in a terminal, and saves
// each answer to the draft file as soon as it is given, so an interrupted
// session loses nothing. It is a front-end over the FilePresenter it embeds:
// the draft stays the record that a later run, a run with nobody to ask, and
// a reviewer read. Collect and Consume are the file's.
//
// A question is open when it has no answer, or when its answer no longer
// counts (see [Question.Open]). An answered question is not asked again; its
// answer is changed in the file.
type PromptPresenter struct {
	*FilePresenter
	in  *lineReader
	out io.Writer
}

// NewPromptPresenter returns a PromptPresenter that writes its questions to
// out, reads one answer per line from in, and keeps the answers in file.
//
// in is read as answers whatever it is: the caller decides that someone is
// there to answer, as the command line does by asking only on a terminal.
func NewPromptPresenter(file *FilePresenter, in io.Reader, out io.Writer) *PromptPresenter {
	return &PromptPresenter{FilePresenter: file, in: newLineReader(in), out: out}
}

// Present implements [ResolutionPresenter]. The file presenter updates the
// draft first. Each open question is then asked, and its answer saved,
// before the next one. An empty answer takes the question's default.
//
// When the input ends, the questions left stay open and the run stops on
// them, as a run with nobody to ask does. When ctx ends while it waits for an
// answer, it returns ctx's error; the answers already given are in the draft.
func (p *PromptPresenter) Present(ctx context.Context, issues []PlanIssue, prior map[string]int) error {
	if err := p.FilePresenter.Present(ctx, issues, prior); err != nil {
		return err
	}
	questions, err := p.Questions()
	if err != nil {
		return err
	}
	var open []Question
	for _, q := range questions {
		if q.Open() {
			open = append(open, q)
		}
	}
	if len(open) == 0 {
		return nil
	}

	fmt.Fprintf(p.out, "%s. Each answer is saved to %s as you give it.\n",
		plural(len(open), "question needs an answer", "questions need an answer"), p.path)
	for i, q := range open {
		fmt.Fprintf(p.out, "\nQuestion %d of %d (%s)\n", i+1, len(open), q.ID)
		p.show(q)
		index, err := p.ask(ctx, q)
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(p.out, "\nNo more input. The questions left stay open in %s.\n", p.path)
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.answer(q.ID, index); err != nil {
			return errs.NewMigrationError("PromptPresenter.Present", errs.CodeMigrationDraftWriteFailed, err)
		}
	}
	fmt.Fprintln(p.out)
	return nil
}

// show prints q: its table and description, why an answer it has does not
// count, and its options, numbered from 1, with the default marked.
func (p *PromptPresenter) show(q Question) {
	fmt.Fprintf(p.out, "%s: %s\n", q.Table, q.Description)
	switch {
	case q.Stale:
		fmt.Fprintln(p.out, "It was answered before, but its options have changed since. Answer it again.")
	case q.Chosen != nil:
		fmt.Fprintf(p.out, "The answer in the draft (\"chosen\": %d) is not one of its options. Answer it again.\n", *q.Chosen)
	}
	fmt.Fprintln(p.out)
	for i, label := range q.Options {
		mark := ""
		if i == q.Default {
			mark = " (default)"
		}
		fmt.Fprintf(p.out, "  %d) %s%s\n", i+1, label, mark)
	}
}

// ask reads answers until one names an option, and returns its index. An
// answer is the option's number, from 1, or nothing for the default.
func (p *PromptPresenter) ask(ctx context.Context, q Question) (int, error) {
	hasDefault := q.Default >= 0 && q.Default < len(q.Options)
	for {
		if hasDefault {
			fmt.Fprintf(p.out, "Answer [%d]: ", q.Default+1)
		} else {
			fmt.Fprint(p.out, "Answer: ")
		}
		line, err := p.in.readLine(ctx)
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" && hasDefault {
			return q.Default, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(q.Options) {
			return n - 1, nil
		}
		fmt.Fprintf(p.out, "Answer with a number from 1 to %d.\n", len(q.Options))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// lineReader reads the prompt's answers, one line at a time. Each read waits
// in a goroutine of its own, so that a prompt waiting for the terminal can
// give up when its context ends: an interrupt ends the command at once.
//
// At most one read is in flight, and only while an answer is asked for, so
// nothing is read ahead of the question. A read given up on stays pending
// and is the one the next call returns. In a command that is ending, as on
// an interrupt, nothing calls again.
type lineReader struct {
	r       *bufio.Reader
	pending chan lineResult // the read in flight, or nil
}

type lineResult struct {
	line string
	err  error
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: bufio.NewReader(r)} }

// readLine returns the next line without its line ending. A last line that
// has no line ending is returned before io.EOF.
func (lr *lineReader) readLine(ctx context.Context) (string, error) {
	if lr.pending == nil {
		result := make(chan lineResult, 1)
		lr.pending = result
		go func() {
			line, err := lr.r.ReadString('\n')
			result <- lineResult{line, err}
		}()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-lr.pending:
		lr.pending = nil
		if res.err != nil && (res.line == "" || !errors.Is(res.err, io.EOF)) {
			return "", res.err
		}
		return strings.TrimRight(res.line, "\r\n"), nil
	}
}
