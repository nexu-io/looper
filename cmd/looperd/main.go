package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) || args[0] == "help" {
		writeUsage(stdout)
		return 0
	}

	_, _ = fmt.Fprintf(stderr, "looperd: command support has not been ported yet: %s\n\n", args[0])
	writeUsage(stderr)
	return 2
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help"
}

func writeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `looperd (Go port bootstrap)

Usage:
  looperd help

Status:
  The Go daemon entrypoint exists, but runtime behavior has not been ported yet.
`)
}
