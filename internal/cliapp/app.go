package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type Deps struct {
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	HTTPClient     *http.Client
	HomeDir        string
	Platform       string
	Arch           string
	LookPath       config.LookPathFunc
	RunCommand     runCommandFunc
	SpawnDetached  spawnDetachedFunc
	KillProcess    killProcessFunc
	ReadFile       readFileFunc
	WriteFile      writeFileFunc
	RemoveFile     removeFileFunc
	MkdirAll       mkdirAllFunc
	Sleep          sleepFunc
	Getwd          getwdFunc
	ExecutablePath string

	// DaemonStartTimeout overrides how long daemon start/restart waits for the
	// spawned looperd process to become API-ready. It is primarily intended for
	// tests; production uses the CLI default.
	DaemonStartTimeout time.Duration

	// CLIChannel overrides the install-source channel used to decide whether
	// auto-upgrade may run. Defaults to version.Channel when empty. Tests that
	// need to exercise the upgrade path with mock binaries inject "stable"
	// here so the dev-build channel guard does not short-circuit them.
	CLIChannel string
}

type App struct {
	deps Deps
}

func New(deps Deps) *App {
	return &App{deps: deps}
}

func (a *App) Run(ctx context.Context, argv []string) int {
	root := a.newRootCommand(argv)
	root.SetArgs(argv)

	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(a.stderr(), "looper: %v\n", err)
		return exitCodeForError(err)
	}

	return 0

}

type commandSpec struct {
	use             string
	short           string
	args            cobra.PositionalArgs
	runE            func(cmd *cobra.Command, args []string) error
	persistentFlags []flagSpec
	localFlags      []flagSpec
	exampleLines    []string
	helpSubcommands []helpSubcommand
	helpWhenNoArgs  bool
	subcommands     []*cobra.Command
}

type helpSubcommand struct {
	name        string
	description string
}

type flagSpec struct {
	name        string
	valueName   string
	description string
	kind        flagKind
	hidden      bool
}

type flagKind int

const (
	flagKindBool flagKind = iota
	flagKindString
)

func (a *App) newRootCommand(argv []string) *cobra.Command {
	runtime := newCommandRuntime(a, argv)

	root := newCommand(commandSpec{
		use:             "looper",
		short:           "Looper command-line interface",
		helpSubcommands: []helpSubcommand{{name: "status", description: "Show service status"}, {name: "bootstrap", description: "Run first-time setup"}, {name: "version", description: "Show Looper version"}, {name: "project", description: "Project commands"}, {name: "config", description: "Config commands"}, {name: "prompt", description: "Prompt inspection commands"}, {name: "daemon", description: "Daemon commands"}, {name: "upgrade", description: "Check or upgrade Looper installations"}, {name: "labels", description: "GitHub label commands"}, {name: "queue", description: "Queue inspection and maintenance commands"}, {name: "loop", description: "Loop commands"}, {name: "work", description: "Create a worker run"}, {name: "plan", description: "Create a planner run"}, {name: "pr", description: "Pull request commands"}, {name: "review", description: "Create a reviewer task for a pull request"}, {name: "fix", description: "Create a fixer task for a pull request"}, {name: "feedback", description: "Submit feedback as a GitHub issue"}, {name: "ps", description: "Show running loops"}, {name: "jump", description: "Print shell command for a loop worktree"}, {name: "logs", description: "Show logs for a loop"}, {name: "pause", description: "Pause a loop by sequence number"}, {name: "unpause", description: "Resume a paused loop by sequence number"}, {name: "stop", description: "Stop an active loop"}, {name: "run", description: "Run commands"}},
		helpWhenNoArgs:  true,
		subcommands: []*cobra.Command{
			newCommand(commandSpec{use: "status", short: "Show service status", runE: runtime.status}),
			newCommand(commandSpec{
				use:   "bootstrap",
				short: "Run first-time setup",
				runE:  runtime.bootstrap,
				localFlags: []flagSpec{
					boolFlag("yes", "Run non-interactively with defaults"),
					boolFlag("force", "Reinstall the managed daemon binary even if it already exists"),
					stringFlag("agent-vendor", "vendor", "Agent vendor for generated config"),
					stringFlag("project-path", "path", "Add a default project from a local repository path"),
					boolFlag("enable-local-token", "Enable server.authMode=local-token for generated config"),
					boolFlag("disable-osascript", "Disable osascript notifications for generated config"),
				},
				exampleLines: []string{
					"$ looper bootstrap",
					"$ looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode",
				},
			}),
			newCommand(commandSpec{use: "version", short: "Show Looper version", runE: runtime.version}),
			newCommand(commandSpec{
				use:             "project",
				short:           "Project commands",
				helpSubcommands: []helpSubcommand{{name: "list", description: "List projects"}, {name: "add", description: "Add a project"}, {name: "remove", description: "Remove a project"}},
				helpWhenNoArgs:  true,
				persistentFlags: []flagSpec{
					stringFlag("repo-path", "path", "Repository path"),
					stringFlag("id", "id", "Project id"),
					stringFlag("name", "name", "Project name"),
					stringFlag("base-branch", "branch", "Base branch"),
					stringFlag("worktree-root", "path", "Worktree root"),
					stringFlag("repo", "repo", "Repository slug"),
					stringFlag("snapshot-mode", "mode", "Snapshot mode for project add: async, full, or off"),
				},
				exampleLines: []string{
					"$ looper project list",
					"$ looper project add /path/to/repo",
					"$ looper project remove project_1 --force",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "list", short: "List projects", runE: runtime.projectList}),
					newCommand(commandSpec{use: "add", short: "Add a project", args: cobra.MaximumNArgs(1), runE: runtime.projectAdd}),
					newCommand(commandSpec{use: "remove", short: "Remove a project", args: cobra.MaximumNArgs(1), runE: runtime.projectRemove, localFlags: []flagSpec{boolFlag("force", "Remove without prompting for confirmation")}}),
				},
			}),
			newCommand(commandSpec{
				use:             "config",
				short:           "Inspect and edit the active config file",
				helpSubcommands: []helpSubcommand{{name: "get", description: "Get a config value"}, {name: "set", description: "Set a config value"}, {name: "unset", description: "Unset a config value"}, {name: "validate", description: "Validate the active config file"}, {name: "lint", description: "Lint the active config file"}, {name: "show", description: "Show active config"}, {name: "edit", description: "Edit the active config file"}},
				helpWhenNoArgs:  true,
				exampleLines: []string{
					"$ looper config get roles.reviewer.behavior.reviewEvents.clean",
					"$ looper config set roles.reviewer.behavior.reviewEvents.clean APPROVE",
					"$ looper config unset roles.reviewer.behavior.reviewEvents.clean",
					"$ looper config validate",
					"$ looper config show --source",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "get <key>", short: "Get a config value", args: cobra.ExactArgs(1), runE: runtime.configGet}),
					newCommand(commandSpec{use: "set <key> <value>", short: "Set a config value", args: cobra.ExactArgs(2), runE: runtime.configSet}),
					newCommand(commandSpec{use: "unset <key>", short: "Unset a config value", args: cobra.ExactArgs(1), runE: runtime.configUnset}),
					newCommand(commandSpec{use: "validate", short: "Validate the active config file", runE: runtime.configValidate}),
					newCommand(commandSpec{use: "lint", short: "Lint the active config file", runE: runtime.configValidate}),
					newCommand(commandSpec{use: "show", short: "Show active config", runE: runtime.configShow, localFlags: []flagSpec{boolFlag("source", "Show config file values with their source layer")}}),
					newCommand(commandSpec{use: "edit", short: "Edit the active config file", runE: runtime.configEdit}),
				},
			}),
			newCommand(commandSpec{
				use:             "prompt",
				short:           "Prompt inspection commands",
				helpSubcommands: []helpSubcommand{{name: "preview", description: "Preview assembled prompt order"}},
				helpWhenNoArgs:  true,
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "preview", short: "Preview assembled prompt order", runE: runtime.promptPreview, localFlags: []flagSpec{stringFlag("project", "projectId", "Project id"), stringFlag("role", "role", "Role: planner, worker, reviewer, fixer, or sweeper")}}),
				},
			}),
			newCommand(commandSpec{
				use:             "daemon",
				short:           "Daemon commands",
				helpSubcommands: []helpSubcommand{{name: "install", description: "Install the managed daemon binary"}, {name: "status", description: "Show daemon status"}, {name: "start", description: "Start the daemon"}, {name: "stop", description: "Stop the daemon"}, {name: "restart", description: "Restart the daemon"}, {name: "logs", description: "Show daemon logs"}},
				helpWhenNoArgs:  true,
				persistentFlags: []flagSpec{
					stringFlag("lines", "count", "Line count"),
					boolFlag("full", "Show all retained daemon log lines, including rotated log files"),
					boolFlag("startup", "Show daemon startup logs instead of main logs"),
					boolFlag("force", "Overwrite existing installed daemon binary"),
				},
				exampleLines: []string{
					"$ looper daemon install",
					"$ looper daemon start",
					"$ looper daemon stop",
					"$ looper daemon restart",
					"$ looper daemon status",
					"$ looper daemon logs --lines 50",
					"$ looper daemon logs --full",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "install", short: "Install the managed daemon binary", runE: runtime.daemonInstall}),
					newCommand(commandSpec{use: "status", short: "Show daemon status", runE: runtime.daemonStatus}),
					newCommand(commandSpec{use: "start", short: "Start the daemon", runE: runtime.daemonStart}),
					newCommand(commandSpec{use: "stop", short: "Stop the daemon", runE: runtime.daemonStop}),
					newCommand(commandSpec{use: "restart", short: "Restart the daemon", runE: runtime.daemonRestart}),
					newCommand(commandSpec{use: "logs", short: "Show daemon logs", runE: runtime.daemonLogs}),
				},
			}),
			newCommand(commandSpec{
				use:   "upgrade",
				short: "Check or upgrade Looper installations",
				runE:  runtime.upgrade,
				localFlags: []flagSpec{
					boolFlag("check", "Check available CLI and daemon updates"),
					boolFlag("cli", "Upgrade the looper CLI binary when self-upgrade is allowed"),
					boolFlag("daemon", "Install or upgrade the managed daemon binary"),
					hiddenBoolFlag("background-auto", "Run the auto-upgrade worker in the background"),
				},
				exampleLines: []string{
					"$ looper upgrade --check",
					"$ looper upgrade --cli",
					"$ looper upgrade --daemon",
				},
			}),
			newCommand(commandSpec{
				use:             "labels",
				short:           "GitHub label commands",
				helpSubcommands: []helpSubcommand{{name: "init", description: "Initialize standard Looper GitHub labels"}},
				helpWhenNoArgs:  true,
				exampleLines: []string{
					"$ looper labels init",
					"$ looper labels init --repo acme/looper --dry-run",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{
						use:   "init",
						short: "Initialize standard Looper GitHub labels",
						runE:  runtime.labelsInit,
						localFlags: []flagSpec{
							stringFlag("repo", "owner/name", "GitHub repository slug"),
							boolFlag("dry-run", "Preview label changes without applying them"),
						},
					}),
				},
			}),
			newCommand(commandSpec{
				use:             "queue",
				short:           "Queue inspection and maintenance commands",
				helpSubcommands: []helpSubcommand{{name: "stats", description: "Show queue eligibility stats"}, {name: "list", description: "List queue items"}, {name: "cleanup", description: "Clean stale queue items"}},
				helpWhenNoArgs:  true,
				exampleLines: []string{
					"$ looper queue stats",
					"$ looper queue list --eligible",
					"$ looper queue cleanup --stale",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "stats", short: "Show queue eligibility stats", runE: runtime.queueStats}),
					newCommand(commandSpec{use: "list", short: "List queue items", runE: runtime.queueList, localFlags: []flagSpec{boolFlag("eligible", "Only list currently eligible queued items")}}),
					newCommand(commandSpec{use: "cleanup", short: "Clean stale queue items", runE: runtime.queueCleanup, localFlags: []flagSpec{boolFlag("stale", "Cancel queued items blocked by terminal loops")}}),
				},
			}),
			newCommand(commandSpec{
				use:             "loop",
				short:           "Loop commands",
				helpSubcommands: []helpSubcommand{{name: "list", description: "List loops"}, {name: "start", description: "Start a loop"}, {name: "pause", description: "Pause a loop"}},
				helpWhenNoArgs:  true,
				exampleLines: []string{
					"$ looper loop list",
					"$ looper loop start --type reviewer --pr acme/looper#42",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "list", short: "List loops", runE: runtime.loopList}),
					newCommand(commandSpec{use: "start", short: "Start a loop", runE: runtime.loopStart, localFlags: []flagSpec{stringFlag("type", "type", "Loop type"), stringFlag("pr", "repo#number", "Pull request reference"), stringFlag("project", "projectId", "Project id")}}),
					newCommand(commandSpec{use: "pause [id]", short: "Pause a loop", args: cobra.MaximumNArgs(1), runE: runtime.loopPause, localFlags: []flagSpec{stringFlag("id", "id", "Loop id")}}),
				},
			}),
			newCommand(commandSpec{
				use:   "work",
				short: "Create a worker run",
				runE:  runtime.workCreate,
				localFlags: []flagSpec{
					stringFlag("project", "projectId", "Project id"),
					stringFlag("title", "title", "Task title"),
					stringFlag("prompt", "text", "Implementation prompt"),
					stringFlag("issue", "number", "Issue number"),
					stringFlag("spec", "path", "Spec path"),
					stringFlag("repo", "repo", "Repository slug"),
					stringFlag("base-branch", "branch", "Base branch"),
				},
				exampleLines: []string{
					"$ looper work --project project_1 --title \"Ship CLI\" --spec specs/ship-cli.md",
					"$ looper work --project project_1 --issue 123",
				},
			}),
			newCommand(commandSpec{
				use:   "plan",
				short: "Create a planner run",
				runE:  runtime.planCreate,
				localFlags: []flagSpec{
					stringFlag("project", "projectId", "Project id"),
					stringFlag("issue", "number", "Issue number"),
				},
				exampleLines: []string{"$ looper plan --project project_1 --issue 123"},
			}),
			newCommand(commandSpec{
				use:             "pr",
				short:           "Pull request commands",
				helpSubcommands: []helpSubcommand{{name: "list", description: "List pull requests"}, {name: "show", description: "Show a pull request"}, {name: "status", description: "Show pull request status"}},
				helpWhenNoArgs:  true,
				exampleLines: []string{
					"$ looper pr list",
					"$ looper pr show acme/looper#42",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "list", short: "List pull requests", runE: runtime.pullRequestList}),
					newCommand(commandSpec{use: "show", short: "Show a pull request", args: cobra.ExactArgs(1), runE: runtime.pullRequestShow}),
					newCommand(commandSpec{use: "status", short: "Show pull request status", args: cobra.ExactArgs(1), runE: runtime.pullRequestStatus}),
				},
			}),
			newCommand(commandSpec{
				use:   "fix <pr>",
				short: "Create a fixer task for a pull request",
				args:  cobra.ExactArgs(1),
				runE:  runtime.fixCreate,
				localFlags: []flagSpec{
					stringFlag("project", "projectId", "Project id"),
				},
				exampleLines: []string{
					"$ looper fix 42",
					"$ looper fix acme/looper#42",
				},
			}),
			newCommand(commandSpec{
				use:             "review <pr>",
				short:           "Create a reviewer task for a pull request",
				args:            cobra.ExactArgs(1),
				runE:            runtime.reviewCreate,
				helpSubcommands: []helpSubcommand{{name: "submit", description: "Submit a validated PR review payload"}},
				localFlags: []flagSpec{
					stringFlag("project", "projectId", "Project id"),
					boolFlag("loop", "Keep reviewing when new commits are pushed"),
					boolFlag("no-loop", "Run only one review pass"),
					stringFlag("clean-review-event", "event", "Clean review event override: COMMENT or APPROVE"),
					stringFlag("blocking-review-event", "event", "Blocking review event override: COMMENT or REQUEST_CHANGES"),
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "submit <pr>", short: "Submit a validated PR review payload", args: cobra.ExactArgs(1), runE: runtime.reviewSubmit, localFlags: []flagSpec{stringFlag("event", "event", "Review event: COMMENT, APPROVE, or REQUEST_CHANGES"), stringFlag("commit-id", "sha", "Expected PR head commit SHA"), stringFlag("clean-review-event", "event", "Effective clean review event policy"), stringFlag("blocking-review-event", "event", "Effective blocking review event policy")}}),
				},
				exampleLines: []string{
					"$ looper review 123",
					"$ looper review acme/looper#42 --loop",
					"$ looper review submit acme/looper#42 --event COMMENT --commit-id abc123 < review.json",
				},
			}),
			newCommand(commandSpec{
				use:   "feedback [message...]",
				short: "Submit feedback as a GitHub issue",
				args:  cobra.ArbitraryArgs,
				runE:  runtime.feedback,
				localFlags: []flagSpec{
					stringFlag("title", "title", "Issue title hint"),
				},
				exampleLines: []string{
					"$ looper feedback The CLI should include feedback support",
					"$ looper feedback --title \"CLI UX\" add an interactive mode",
				},
			}),
			newCommand(commandSpec{
				use:   "ps",
				short: "Show running loops",
				runE:  runtime.activeRuns,
				localFlags: []flagSpec{
					boolFlag("all", "Show recent loops in any status"),
					stringFlag("status", "status", "Filter by loop or run status"),
					stringFlag("type", "type", "Filter by loop type"),
					stringFlag("project", "projectId", "Filter by project id"),
				},
				exampleLines: []string{
					"$ looper ps",
					"$ looper ps --status completed --type worker",
					"$ looper ps --all",
					"$ looper ps --type reviewer --project project_1",
				},
			}),
			newCommand(commandSpec{
				use:   "jump [id]",
				short: "Print shell command for a loop worktree",
				args:  cobra.MaximumNArgs(1),
				runE:  runtime.jump,
				localFlags: []flagSpec{
					boolFlag("print-path", "Print the worktree path only"),
					stringFlag("shell-integration", "shell", "Print shell integration helper"),
				},
				exampleLines: []string{
					"$ eval \"$(looper jump 12)\"",
					"$ looper jump 12 --print-path",
					"$ looper jump --shell-integration bash",
				},
			}),
			newCommand(commandSpec{
				use:   "logs <id>",
				short: "Show logs for a loop",
				args:  cobra.ExactArgs(1),
				runE:  runtime.loopLogs,
				localFlags: []flagSpec{
					boolFlag("stderr", "Show stderr instead of stdout"),
					stringFlag("tail", "count", "Show the last N lines"),
					boolFlag("full", "Show the full output"),
					boolFlag("follow", "Stream new log output until the run exits (human output only)"),
				},
				exampleLines: []string{
					"$ looper logs 12",
					"$ looper logs 12 --stderr --tail 50",
					"$ looper logs 12 --follow",
				},
			}),
			newCommand(commandSpec{use: "pause <seq>", short: "Pause a loop by sequence number", args: cobra.ExactArgs(1), runE: runtime.pauseLoopBySeq, exampleLines: []string{"$ looper pause 12"}}),
			newCommand(commandSpec{use: "unpause <seq>", short: "Resume a paused loop by sequence number", args: cobra.ExactArgs(1), runE: runtime.unpauseLoopBySeq, exampleLines: []string{"$ looper unpause 12"}}),
			newCommand(commandSpec{use: "stop <id|all>", short: "Stop an active loop or all active loops", args: cobra.ExactArgs(1), runE: runtime.stopLoop, exampleLines: []string{"$ looper stop 12", "$ looper stop all"}}),
			newCommand(commandSpec{
				use:             "run",
				short:           "Run commands",
				helpSubcommands: []helpSubcommand{{name: "list", description: "List runs"}, {name: "stats", description: "Show recent run stats"}},
				helpWhenNoArgs:  true,
				persistentFlags: []flagSpec{stringFlag("loop", "loopId", "Filter by loop id")},
				exampleLines: []string{
					"$ looper run list",
					"$ looper run stats --since 24h",
					"$ looper run list --loop loop_1",
				},
				subcommands: []*cobra.Command{
					newCommand(commandSpec{use: "list", short: "List runs", runE: runtime.runList}),
					newCommand(commandSpec{use: "stats", short: "Show recent run stats", args: cobra.NoArgs, runE: runtime.runStats, localFlags: []flagSpec{stringFlag("since", "duration", "Time window to aggregate (default 24h; supports h and d, for example 1h, 24h, 7d)"), stringFlag("role", "role", "Filter by role: planner, reviewer, worker, or fixer")}}),
				},
			}),
		},
	})

	addFlags(root.PersistentFlags(), globalFlags())
	root.PersistentPreRunE = runtime.maybeRunAutoUpgrade
	root.SetOut(a.stdout())
	root.SetErr(a.stderr())
	if a.deps.Stdin != nil {
		root.SetIn(a.deps.Stdin)
	}
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.CompletionOptions.DisableDefaultCmd = true

	return root
}

func newCommand(spec commandSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Args:  spec.args,
	}

	if spec.helpWhenNoArgs {
		cmd.RunE = helpCommand
	} else {
		cmd.RunE = notPortedCommand
	}
	if spec.runE != nil {
		cmd.RunE = spec.runE
	}

	if len(spec.exampleLines) > 0 {
		cmd.Example = strings.Join(spec.exampleLines, "\n")
	}

	cmd.SetHelpFunc(func(command *cobra.Command, _ []string) {
		renderHelp(command.OutOrStdout(), command, spec.helpSubcommands)
	})

	addFlags(cmd.PersistentFlags(), spec.persistentFlags)
	addFlags(cmd.Flags(), spec.localFlags)
	cmd.AddCommand(spec.subcommands...)
	return cmd
}

func addFlags(flagSet *pflag.FlagSet, flags []flagSpec) {
	for _, flag := range flags {
		switch flag.kind {
		case flagKindBool:
			flagSet.Bool(flag.name, false, flag.description)
		case flagKindString:
			flagSet.String(flag.name, "", flag.description)
		}

		defined := flagSet.Lookup(flag.name)
		if defined != nil && flag.valueName != "" {
			defined.Annotations = map[string][]string{"looperValueName": {flag.valueName}}
		}
		if defined != nil {
			defined.Hidden = flag.hidden
		}
	}
}

func boolFlag(name, description string) flagSpec {
	return flagSpec{name: name, description: description, kind: flagKindBool}
}

func hiddenBoolFlag(name, description string) flagSpec {
	return flagSpec{name: name, description: description, kind: flagKindBool, hidden: true}
}

func stringFlag(name, valueName, description string) flagSpec {
	return flagSpec{name: name, valueName: valueName, description: description, kind: flagKindString}
}

func hiddenStringFlag(name, valueName, description string) flagSpec {
	return flagSpec{name: name, valueName: valueName, description: description, kind: flagKindString, hidden: true}
}

func renderHelp(w io.Writer, cmd *cobra.Command, listedSubcommands []helpSubcommand) {
	var output strings.Builder

	_, _ = fmt.Fprintf(&output, "Usage:\n  %s\n", cmd.UseLine())
	if cmd.Parent() == nil {
		_, _ = fmt.Fprintf(&output, "\nVersion:\n  %s\n", version.Current().Version)
	}

	subcommands := listedSubcommands
	if len(subcommands) == 0 {
		subcommands = actualSubcommands(cmd)
	}
	if len(subcommands) > 0 {
		longestName := 0
		for _, subcommand := range subcommands {
			if len(subcommand.name) > longestName {
				longestName = len(subcommand.name)
			}
		}

		_, _ = fmt.Fprintf(&output, "\nSubcommands:\n")
		for _, subcommand := range subcommands {
			_, _ = fmt.Fprintf(&output, "  %-*s  %s\n", longestName, subcommand.name, subcommand.description)
		}
	}

	localFlags := collectFlags(cmd.LocalFlags())
	if len(localFlags) > 0 {
		_, _ = fmt.Fprintf(&output, "\nFlags:\n")
		for _, line := range localFlags {
			_, _ = fmt.Fprintf(&output, "%s\n", line)
		}
	}

	inheritedFlags := collectFlags(cmd.InheritedFlags())
	if len(inheritedFlags) > 0 {
		_, _ = fmt.Fprintf(&output, "\nGlobal Flags:\n")
		for _, line := range inheritedFlags {
			_, _ = fmt.Fprintf(&output, "%s\n", line)
		}
	}

	if example := strings.TrimSpace(cmd.Example); example != "" {
		_, _ = fmt.Fprintf(&output, "\nExamples:\n%s\n", example)
	}

	_, _ = io.WriteString(w, output.String())
}

func actualSubcommands(cmd *cobra.Command) []helpSubcommand {
	subcommands := make([]helpSubcommand, 0)
	for _, child := range cmd.Commands() {
		if !child.IsAvailableCommand() || child.Hidden || child.Name() == "help" {
			continue
		}
		subcommands = append(subcommands, helpSubcommand{name: child.Name(), description: child.Short})
	}
	return subcommands
}

func collectFlags(flagSet *pflag.FlagSet) []string {
	flags := make([]string, 0)
	flagSet.VisitAll(func(flag *pflag.Flag) {
		if flag.Name == "help" || flag.Hidden {
			return
		}

		syntax := "--" + flag.Name
		if flag.Value.Type() != "bool" {
			valueName := "value"
			if values := flag.Annotations["looperValueName"]; len(values) > 0 && values[0] != "" {
				valueName = values[0]
			}
			syntax += " <" + valueName + ">"
		}

		flags = append(flags, fmt.Sprintf("  %-27s %s", syntax, flag.Usage))
	})
	return flags
}

func globalFlags() []flagSpec {
	return []flagSpec{
		boolFlag("json", "Emit JSON output"),
		hiddenBoolFlag("no-auto-upgrade", "Disable automatic upgrade checks for this command"),
		stringFlag("package-auto-upgrade-enabled", "bool", "Enable automatic upgrade checks for this command"),
		stringFlag("config", "path", "Config path (`~/.looper/config.toml` by default; also supports .yaml, .yml, and .json)"),
		stringFlag("host", "host", "Server host"),
		stringFlag("port", "port", "Server port"),
		stringFlag("db-path", "path", "Database path"),
		stringFlag("log-dir", "path", "Daemon log directory"),
		stringFlag("daemon-mode", "mode", "Daemon mode"),
		stringFlag("daemon-restart-policy", "policy", "Daemon supervisor restart policy: never, on-failure, or always"),
		stringFlag("daemon-restart-throttle-seconds", "seconds", "Daemon supervisor restart throttle"),
		stringFlag("git-path", "path", "Git binary path"),
		stringFlag("gh-path", "path", "GitHub CLI path"),
		stringFlag("looper-path", "path", "Looper CLI path"),
		stringFlag("osascript-path", "path", "osascript binary path"),
		stringFlag("planner-agent-timeout-seconds", "seconds", "Planner agent execution timeout"),
		stringFlag("worker-agent-timeout-seconds", "seconds", "Worker agent execution timeout"),
		stringFlag("reviewer-agent-timeout-seconds", "seconds", "Reviewer agent execution timeout"),
		stringFlag("fixer-agent-timeout-seconds", "seconds", "Fixer agent execution timeout"),
		stringFlag("roles-fixer-triggers-author-filter", "filter", "Fixer author filter: current-user or any"),
		hiddenStringFlag("fix-all-pull-requests", "bool", "Allow fixer to inspect and fix PRs created by any author"),
		stringFlag("roles-reviewer-behavior-loop-enabled-by-default", "bool", "Enable reviewer follow-up loops by default"),
		hiddenStringFlag("reviewer-loop-enabled", "bool", "Enable reviewer follow-up loops by default"),
		stringFlag("roles-reviewer-discovery-triggers-enable-self-review", "bool", "Allow reviewer triggers to process self-authored pull requests"),
		hiddenStringFlag("reviewer-enable-self-review", "bool", "Allow reviewer triggers to process self-authored pull requests"),
		stringFlag("roles-reviewer-behavior-review-events-clean", "event", "Reviewer event for clean runs: COMMENT or APPROVE"),
		hiddenStringFlag("allow-auto-approve", "bool", "Legacy alias for reviewer clean approvals"),
		hiddenStringFlag("reviewer-clean-review-event", "event", "Reviewer event for clean runs: COMMENT or APPROVE"),
		stringFlag("roles-reviewer-behavior-review-events-blocking", "event", "Reviewer event for blocking findings: COMMENT or REQUEST_CHANGES"),
		hiddenStringFlag("reviewer-blocking-review-event", "event", "Reviewer event for blocking findings: COMMENT or REQUEST_CHANGES"),
		stringFlag("roles-reviewer-behavior-loop-quiet-period-seconds", "seconds", "Reviewer loop quiet period"),
		hiddenStringFlag("reviewer-quiet-period-seconds", "seconds", "Reviewer loop quiet period"),
		stringFlag("roles-reviewer-behavior-loop-min-publish-interval-seconds", "seconds", "Reviewer loop minimum publish interval"),
		hiddenStringFlag("reviewer-min-publish-interval-seconds", "seconds", "Reviewer loop minimum publish interval"),
		stringFlag("roles-reviewer-behavior-loop-max-iterations-per-pr", "count", "Deprecated; ignored by reviewer loop filtering"),
		hiddenStringFlag("reviewer-max-iterations-per-pr", "count", "Deprecated; ignored by reviewer loop filtering"),
		stringFlag("roles-reviewer-behavior-loop-max-iterations-per-head", "count", "Deprecated; ignored by reviewer loop filtering"),
		hiddenStringFlag("reviewer-max-iterations-per-head", "count", "Deprecated; ignored by reviewer loop filtering"),
		stringFlag("instructions-enabled", "bool", "Enable custom instructions"),
		hiddenBoolFlag("no-custom-instructions", "Disable custom instructions for debugging"),
	}
}

func helpCommand(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

func notPortedCommand(cmd *cobra.Command, _ []string) error {
	path := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), "looper"))
	if path == "" {
		path = cmd.Name()
	}
	return notPortedError{Path: strings.TrimSpace(path)}
}

type notPortedError struct {
	Path string
}

func (e notPortedError) Error() string {
	return fmt.Sprintf("command support has not been ported yet: %s", e.Path)
}

func exitCodeForError(err error) int {
	var notPorted notPortedError
	if errors.As(err, &notPorted) {
		return 2
	}
	return 1
}

func (a *App) stdout() io.Writer {
	if a.deps.Stdout != nil {
		return a.deps.Stdout
	}
	return io.Discard
}

func (a *App) stderr() io.Writer {
	if a.deps.Stderr != nil {
		return a.deps.Stderr
	}
	return io.Discard
}

var configFlagNames = map[string]struct{}{
	"config":                             {},
	"no-auto-upgrade":                    {},
	"package-auto-upgrade-enabled":       {},
	"host":                               {},
	"port":                               {},
	"db-path":                            {},
	"log-dir":                            {},
	"daemon-mode":                        {},
	"daemon-restart-policy":              {},
	"daemon-restart-throttle-seconds":    {},
	"git-path":                           {},
	"gh-path":                            {},
	"looper-path":                        {},
	"osascript-path":                     {},
	"planner-agent-timeout-seconds":      {},
	"worker-agent-timeout-seconds":       {},
	"reviewer-agent-timeout-seconds":     {},
	"fixer-agent-timeout-seconds":        {},
	"roles-fixer-triggers-author-filter": {},
	"fix-all-pull-requests":              {},
	"allow-auto-approve":                 {},
	"roles-reviewer-behavior-review-events-clean":               {},
	"reviewer-clean-review-event":                               {},
	"roles-reviewer-behavior-review-events-blocking":            {},
	"reviewer-blocking-review-event":                            {},
	"roles-reviewer-behavior-loop-enabled-by-default":           {},
	"reviewer-loop-enabled":                                     {},
	"roles-reviewer-discovery-triggers-enable-self-review":      {},
	"reviewer-enable-self-review":                               {},
	"roles-reviewer-behavior-loop-quiet-period-seconds":         {},
	"reviewer-quiet-period-seconds":                             {},
	"roles-reviewer-behavior-loop-min-publish-interval-seconds": {},
	"reviewer-min-publish-interval-seconds":                     {},
	"roles-reviewer-behavior-loop-max-iterations-per-pr":        {},
	"reviewer-max-iterations-per-pr":                            {},
	"roles-reviewer-behavior-loop-max-iterations-per-head":      {},
	"reviewer-max-iterations-per-head":                          {},
	"instructions-enabled":                                      {},
	"no-custom-instructions":                                    {},
}

var configBoolFlagNames = map[string]struct{}{
	"no-auto-upgrade":        {},
	"no-custom-instructions": {},
}

func ExtractConfigArgs(argv []string) []string {
	extracted := make([]string, 0)
	for index := 0; index < len(argv); index++ {
		arg := argv[index]
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		trimmed := strings.TrimPrefix(arg, "--")
		name := trimmed
		if equals := strings.Index(trimmed, "="); equals >= 0 {
			name = trimmed[:equals]
		}

		if _, ok := configFlagNames[name]; !ok {
			continue
		}

		extracted = append(extracted, arg)
		if _, isBool := configBoolFlagNames[name]; isBool {
			if !strings.Contains(trimmed, "=") && index+1 < len(argv) {
				next := argv[index+1]
				if isConfigBoolLiteral(next) {
					extracted = append(extracted, next)
					index++
				}
			}
			continue
		}
		if !strings.Contains(trimmed, "=") && index+1 < len(argv) {
			next := argv[index+1]
			if !strings.HasPrefix(next, "--") {
				extracted = append(extracted, next)
				index++
			}
		}
	}

	return extracted
}

func isConfigBoolLiteral(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on", "0", "false", "no", "off":
		return true
	default:
		return false
	}
}
