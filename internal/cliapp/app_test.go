package cliapp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func runApp(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr})
	exitCode := app.Run(context.Background(), args)

	return exitCode, stdout.String(), stderr.String()
}

func TestCommandGroupHelpListsExpectedSubcommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args        []string
		subcommands []string
	}{
		{args: []string{"project", "--help"}, subcommands: []string{"list  List projects", "add   Add a project"}},
		{args: []string{"config", "--help"}, subcommands: []string{"show  Show active config"}},
		{args: []string{"daemon", "--help"}, subcommands: []string{"status  Show daemon status", "logs    Show daemon logs"}},
		{args: []string{"loop", "--help"}, subcommands: []string{"list   List loops", "start  Start a loop", "pause  Pause a loop"}},
		{args: []string{"pr", "--help"}, subcommands: []string{"list    List pull requests", "show    Show a pull request", "status  Show pull request status"}},
		{args: []string{"run", "--help"}, subcommands: []string{"list  List runs"}},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(strings.Join(testCase.args, "_"), func(t *testing.T) {
			t.Parallel()

			exitCode, stdout, stderr := runApp(t, testCase.args...)
			if exitCode != 0 {
				t.Fatalf("Run(%v) exit code = %d, want 0", testCase.args, exitCode)
			}
			if stderr != "" {
				t.Fatalf("Run(%v) stderr = %q, want empty string", testCase.args, stderr)
			}
			if !strings.Contains(stdout, "Subcommands:") {
				t.Fatalf("Run(%v) stdout = %q, want Subcommands section", testCase.args, stdout)
			}

			for _, subcommand := range testCase.subcommands {
				if !strings.Contains(stdout, subcommand) {
					t.Fatalf("Run(%v) stdout = %q, want to contain %q", testCase.args, stdout, subcommand)
				}
			}
		})
	}
}

func TestRootHelpIncludesGlobalFlagsWithFrozenSyntax(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "--help")
	if exitCode != 0 {
		t.Fatalf("Run([--help]) exit code = %d, want 0", exitCode)
	}
	if stderr != "" {
		t.Fatalf("Run([--help]) stderr = %q, want empty string", stderr)
	}

	for _, syntax := range []string{
		"--json",
		"--config <path>",
		"--host <host>",
		"--port <port>",
		"--db-path <path>",
		"--log-dir <path>",
		"--daemon-mode <mode>",
		"--bun-path <path>",
		"--git-path <path>",
		"--gh-path <path>",
		"--osascript-path <path>",
	} {
		if !strings.Contains(stdout, syntax) {
			t.Fatalf("Run([--help]) stdout = %q, want to contain %q", stdout, syntax)
		}
	}
}

func TestNestedCommandParsingReachesLeafCommands(t *testing.T) {
	t.Parallel()

	exitCode, stdout, stderr := runApp(t, "daemon", "logs", "--lines", "50", "--json")
	if exitCode != 2 {
		t.Fatalf("Run([daemon logs --lines 50 --json]) exit code = %d, want 2", exitCode)
	}
	if stdout != "" {
		t.Fatalf("Run([daemon logs --lines 50 --json]) stdout = %q, want empty string", stdout)
	}
	if got, want := stderr, "looper: command support has not been ported yet: daemon logs\n"; got != want {
		t.Fatalf("Run([daemon logs --lines 50 --json]) stderr = %q, want %q", got, want)
	}
}

func TestExtractConfigArgsForwardsOnlyConfigFlags(t *testing.T) {
	t.Parallel()

	got := ExtractConfigArgs([]string{
		"daemon",
		"start",
		"--json",
		"--config",
		"/tmp/looper.json",
		"--host",
		"127.0.0.2",
		"--port",
		"9999",
		"--db-path=/tmp/looper.sqlite",
		"--log-dir",
		"/tmp/looper-logs",
		"--daemon-mode",
		"minimal",
		"--bun-path",
		"/opt/bun",
		"--git-path",
		"/opt/git",
		"--gh-path",
		"/opt/gh",
		"--osascript-path",
		"/opt/osascript",
		"--force",
	})

	want := []string{
		"--config",
		"/tmp/looper.json",
		"--host",
		"127.0.0.2",
		"--port",
		"9999",
		"--db-path=/tmp/looper.sqlite",
		"--log-dir",
		"/tmp/looper-logs",
		"--daemon-mode",
		"minimal",
		"--bun-path",
		"/opt/bun",
		"--git-path",
		"/opt/git",
		"--gh-path",
		"/opt/gh",
		"--osascript-path",
		"/opt/osascript",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ExtractConfigArgs() = %#v, want %#v", got, want)
	}
}
