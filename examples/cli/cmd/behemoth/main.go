// Command behemoth is this application's own copy of the behemoth command
// line: the program the launcher generates, written by hand.
//
//	go run ./cmd/behemoth generate
//
// An application that has the launcher installed does not need this file.
// It is kept here to show the other way in, which also works where there is
// no Go toolchain: build it into the image and run the binary, as the
// migrate stage of ../../Dockerfile does.
package main

import (
	"github.com/go-behemoth/behemoth/cli"
	"github.com/go-behemoth/behemoth/examples/cli/auth"
)

func main() { cli.Main(auth.Prepare, auth.MigrationBackend) }
