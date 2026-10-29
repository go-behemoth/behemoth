package behemoth_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// quickstartModule is the module path of examples/quickstart. The README
// shows the same code under the module path of the reader's own application.
const (
	quickstartModule = "github.com/go-behemoth/behemoth/examples/quickstart"
	readerModule     = "example.com/myapp"
)

// quickstartFiles are the files of examples/quickstart that the README's
// quick start shows in full.
var quickstartFiles = []string{
	"examples/quickstart/auth/behemoth.go",
	"examples/quickstart/main.go",
}

// TestReadmeQuickStart keeps the README's quick start compilable. Each file
// of examples/quickstart must appear in the README as a go code block, so
// the code a new user copies is the code CI builds. A README snippet that is
// only text drifts from the API without any build failing.
func TestReadmeQuickStart(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := goBlocks(string(readme))

	for _, name := range quickstartFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(string(src), quickstartModule, readerModule)
		if !slices.Contains(blocks, want) {
			t.Errorf("README.md has no go code block equal to %s, with %s in place of %s. "+
				"Copy the file into the quick start after you change it.", name, readerModule, quickstartModule)
		}
	}
}

var (
	openFence  = regexp.MustCompile("^(\\s*)```go\\s*$")
	closeFence = regexp.MustCompile("^\\s*```\\s*$")
)

// goBlocks returns the body of every ```go block in a Markdown document. A
// block inside a list item is indented, and its lines lose that indent.
func goBlocks(doc string) []string {
	var (
		blocks []string
		body   []string
		indent string
		inside bool
	)
	for line := range strings.SplitSeq(doc, "\n") {
		switch {
		case !inside:
			if m := openFence.FindStringSubmatch(line); m != nil {
				inside, indent, body = true, m[1], nil
			}
		case closeFence.MatchString(line):
			blocks = append(blocks, strings.Join(body, "\n")+"\n")
			inside = false
		default:
			body = append(body, strings.TrimPrefix(line, indent))
		}
	}
	return blocks
}
