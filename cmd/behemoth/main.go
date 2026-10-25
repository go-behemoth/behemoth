// Command behemoth runs behemoth's command line for the application in the
// current directory:
//
//	behemoth generate           # show the next migration
//	behemoth generate -confirm  # write it to the migrations folder
//
// The commands live in the cli package and need the application's own Go
// code: its plugins and its schema. This program holds none of it. It finds
// the package with the application's setup, builds a main that hands that
// setup to cli.Main, and runs the result:
//
//	func main() { cli.Main(app.Prepare, app.MigrationBackend) }
//
// The package is the one with a file named behemoth.go, in the current
// directory or below, or the directory given with -app. It has to export
//
//	func Prepare() (*engine.PreparedApp, error)
//	func MigrationBackend(context.Context, *engine.PreparedApp) (migration.Backend, error)
//
// The main is compiled inside the application's module through the go
// command's -overlay flag, so it may import an internal package and nothing
// is written into the application's tree. The commands that run are those of
// the behemoth version in the application's go.mod, not of the version this
// launcher was installed from. It links no database driver.
//
// An application can skip the launcher by writing that main itself, which
// is also how the commands run where there is no Go toolchain.
//
// Experimental: the command line is a first version, with a ready-made setup
// for PostgreSQL and MySQL. Its behavior can change between versions.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	// entryFile marks the package that holds an application's setup.
	entryFile = "behemoth.go"

	// The two functions that package exports, and the package they are
	// handed to.
	prepareFunc = "Prepare"
	backendFunc = "MigrationBackend"
	cliPackage  = "github.com/go-behemoth/behemoth/cli"

	// appEnv names the setup package's directory when -app is not given.
	appEnv = "BEHEMOTH_APP"

	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the exit. Everything after the launcher's own flags
// goes to the application's command line untouched, and its exit code is
// returned.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("behemoth", flag.ContinueOnError)
	flags.SetOutput(stderr)
	appDir := flags.String("app", os.Getenv(appEnv), "directory of the package with the application's setup (default: the package with a "+entryFile+" file; $"+appEnv+" sets it too)")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: behemoth [-app dir] <command> [flags]\n\n"+
			"Runs behemoth's command line against the application in the current directory.\n"+
			"\"behemoth help\" lists the commands.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	// Parse stops at the first argument that is not a flag: the command.
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}

	fail := func(err error) int {
		fmt.Fprintf(stderr, "behemoth: %v\n", err)
		return exitFailure
	}

	goTool, err := exec.LookPath("go")
	if err != nil {
		return fail(errors.New("the go command is not on PATH. The launcher builds your application's command line with it"))
	}
	wd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}

	dir := *appDir
	if dir == "" {
		if dir, err = findApp(wd); err != nil {
			return fail(err)
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return fail(err)
	}

	pkg, err := loadPackage(goTool, dir)
	if err != nil {
		return fail(err)
	}
	if err := checkEntryPoints(pkg); err != nil {
		return fail(err)
	}

	tmp, err := os.MkdirTemp("", "behemoth-cli-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)

	bin, err := build(goTool, pkg, tmp, stderr)
	if err != nil {
		return fail(err)
	}
	return execute(bin, flags.Args(), stdin, stdout, stderr)
}

// findApp returns the directory under root, root included, that holds the
// entry file. It does not enter directories the go command ignores (names
// starting with "." or "_", testdata), vendor, or another module.
func findApp(root string) (string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that can't be read (a database volume owned by
			// another user, say) holds no package of this application.
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			return err
		}
		if !d.IsDir() {
			if d.Name() == entryFile {
				found = append(found, filepath.Dir(path))
			}
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return filepath.SkipDir // another module: its packages are not this one's
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no %s in %s or below.\n"+
			"The launcher finds the package with your application's setup by that file name. Name the file that\n"+
			"exports %s and %s that way, or point at its directory with -app", entryFile, root, prepareFunc, backendFunc)
	default:
		sort.Strings(found)
		return "", fmt.Errorf("%d packages have a %s:\n  %s\nChoose one with -app", len(found), entryFile, strings.Join(found, "\n  "))
	}
}

// goPackage is what the launcher reads from "go list -json".
type goPackage struct {
	Dir        string
	ImportPath string
	Name       string
	GoFiles    []string
	CgoFiles   []string // files that import "C", which go list keeps apart
}

// loadPackage asks the go command about the package in dir: its import
// path, and the files of the current build configuration.
func loadPackage(goTool, dir string) (goPackage, error) {
	cmd := exec.Command(goTool, "list", "-json", ".")
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return goPackage{}, fmt.Errorf("go list in %s: %w\n%s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var pkg goPackage
	if err := json.Unmarshal(out, &pkg); err != nil {
		return goPackage{}, fmt.Errorf("go list in %s: %w", dir, err)
	}
	return pkg, nil
}

// checkEntryPoints reports a package the generated main could not use, in
// terms of the application's code. Without it the developer would read a
// compiler error about a file they never wrote. It checks that the functions
// exist, and leaves their types to the compiler.
func checkEntryPoints(pkg goPackage) error {
	if pkg.Name == "main" {
		return fmt.Errorf("%s is package main, which no other package can import.\n"+
			"Move %s and %s to a package of their own, or call cli.Main from your own main", pkg.Dir, prepareFunc, backendFunc)
	}

	missing := map[string]bool{prepareFunc: true, backendFunc: true}
	fset := token.NewFileSet()
	for _, name := range append(append([]string(nil), pkg.GoFiles...), pkg.CgoFiles...) {
		file, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				delete(missing, fn.Name.Name)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var names []string
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("package %s does not export %s. The launcher needs both of\n"+
		"  func %s() (*engine.PreparedApp, error)\n"+
		"  func %s(context.Context, *engine.PreparedApp) (migration.Backend, error)",
		pkg.ImportPath, strings.Join(names, " and "), prepareFunc, backendFunc)
}

// mainSource is the program the launcher builds: the application's two
// functions handed to cli.Main.
func mainSource(importPath string) ([]byte, error) {
	src := fmt.Sprintf(`// Code generated by the behemoth launcher. DO NOT EDIT.

package main

import (
	%q

	app %q
)

func main() { cli.Main(app.%s, app.%s) }
`, cliPackage, importPath, prepareFunc, backendFunc)
	return format.Source([]byte(src))
}

// build compiles the generated main into tmp and returns the binary's path.
//
// The main has to be part of the application's module: that is where the
// module's requirements apply, and the only place an internal package can be
// imported from. The overlay places it in a directory below the setup
// package that does not exist on disk, so the build sees it there and the
// application's tree is left as it was.
func build(goTool string, pkg goPackage, tmp string, stderr io.Writer) (string, error) {
	src, err := mainSource(pkg.ImportPath)
	if err != nil {
		return "", err
	}
	mainFile := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(mainFile, src, 0o644); err != nil {
		return "", err
	}

	virtual := virtualDir(pkg.Dir)
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {filepath.Join(virtual, "main.go"): mainFile},
	})
	if err != nil {
		return "", err
	}
	overlayFile := filepath.Join(tmp, "overlay.json")
	if err := os.WriteFile(overlayFile, overlay, 0o644); err != nil {
		return "", err
	}

	bin := filepath.Join(tmp, "behemoth-cli")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command(goTool, "build", "-overlay", overlayFile, "-o", bin, virtual)
	cmd.Dir = pkg.Dir
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("could not build the command line for %s: %w\n"+
			"The program is one call, cli.Main(app.%s, app.%s).\n"+
			"  - If the errors above name those functions, compare their types with cli.PrepareFunc and cli.BackendFunc.\n"+
			"  - If they name the package %s, the behemoth version in your go.mod has no\n"+
			"    command line yet, or your module vendors its dependencies. A vendor directory holds that package only\n"+
			"    once your own code imports it, so write the main by hand there",
			pkg.ImportPath, err, prepareFunc, backendFunc, cliPackage)
	}
	return bin, nil
}

// virtualDir is a directory below dir that does not exist, for the overlay
// to put the generated main in.
func virtualDir(dir string) string {
	for i := 0; ; i++ {
		name := "behemoth_cli_main"
		if i > 0 {
			name = fmt.Sprintf("%s_%d", name, i)
		}
		candidate := filepath.Join(dir, name)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// execute runs the built command line in the current directory, where the
// application's relative paths (its migrations folder) resolve, and returns
// its exit code.
func execute(bin string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr

	// An interrupt from the terminal reaches the child as well, and the
	// child handles it by cancelling its command. The launcher only has to
	// outlive it, to report its exit code and remove the temporary files.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	err := cmd.Run()
	if err == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() >= 0 {
		return exit.ExitCode()
	}
	fmt.Fprintf(stderr, "behemoth: %v\n", err)
	return exitFailure
}
