package main

import (
	"context"
	"io"
	"os"

	"github.com/powerformer/looper/internal/cliapp"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	app := cliapp.New(cliapp.Deps{Stdout: stdout, Stderr: stderr})
	return app.Run(context.Background(), args)
}

func writeUsage(w io.Writer) {
	app := cliapp.New(cliapp.Deps{Stdout: w, Stderr: io.Discard})
	_ = app.Run(context.Background(), []string{"--help"})
}
