package hostingidentity

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

const maxHostingCommandDuration = 4 * time.Minute

// RunGH executes a daemon-authored GitHub command with fresh credentials for
// the captured identity. Legacy invocations preserve their exact options.
func RunGH(ctx context.Context, options shell.Options, runner func(context.Context, shell.Options) (shell.Result, error)) (shell.Result, error) {
	if runner == nil {
		runner = shell.Run
	}
	session, selected := FromContext(ctx)
	if !selected {
		return runner(ctx, options)
	}
	if session.Kind() != config.HostingIdentityGitHubApp {
		return shell.Result{}, session.failure("gh", "GitHub CLI cannot use a Forgejo identity")
	}
	base, _ := url.Parse(session.Target().BaseURL)
	if err := validateGHCommand(session, options.Args); err != nil {
		return shell.Result{}, err
	}
	credential, err := session.Credentials(ctx)
	if err != nil {
		return shell.Result{}, err
	}
	options, err = boundCredentialCommand(session, credential, options)
	if err != nil {
		return shell.Result{}, err
	}
	dir, err := os.MkdirTemp("", "looper-host-gh-")
	if err != nil {
		return shell.Result{}, session.failure("gh", "cannot create private CLI configuration directory")
	}
	defer os.RemoveAll(dir)
	// TMPDIR may be relative or traverse symlinks. Resolve the directory we
	// actually created before making its child paths absolute for subprocesses.
	dir, err = filepath.EvalSymlinks(dir)
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		return shell.Result{}, session.failure("gh", "cannot resolve private CLI configuration directory")
	}
	// Even gh pr create with an explicit --head may invoke git status on
	// supported gh versions. Keep its credential-bearing children away from
	// repository configuration/helpers; all daemon targets are explicit or
	// supplied through GH_REPO below.
	options.CWD = dir
	options.Env = privilegedEnv(options.Env)
	// An explicit nonexistent Git directory disables repository discovery,
	// even if TMPDIR is inside a checkout. Unlike a discovery ceiling, this is
	// a single path and remains unambiguous when Unix directory names use ':'.
	options.Env["GIT_DIR"] = filepath.Join(dir, "no-repository")
	options.Env["HOME"] = dir
	options.Env["GH_CONFIG_DIR"] = dir
	options.Env["GH_HOST"] = base.Host
	options.Env["GH_REPO"] = base.Host + "/" + session.Target().Repo
	options.Env["GH_PROMPT_DISABLED"] = "1"
	options.Env["GH_PAGER"] = "cat"
	options.Env["PAGER"] = "cat"
	options.Env["GH_NO_UPDATE_NOTIFIER"] = "1"
	tokenVar := "GH_ENTERPRISE_TOKEN"
	if strings.EqualFold(base.Hostname(), "github.com") || strings.HasSuffix(strings.ToLower(base.Hostname()), ".ghe.com") {
		tokenVar = "GH_TOKEN"
	}
	options.Env[tokenVar] = credential.Token
	result, err := runner(ctx, options)
	return redactCommandResult(session, result, err)
}

func validateGHCommand(session *Session, args []string) error {
	if !safeGHCommand(args) {
		return session.failure("gh", "unsupported GitHub command for a bot identity")
	}
	base, _ := url.Parse(session.Target().BaseURL)
	start := 2
	if args[0] == "api" {
		start = 1
	}
	var positional []string
	for index := start; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flag, value, inline := strings.Cut(arg, "=")
		if strings.HasPrefix(arg, "-R") && len(arg) > 2 {
			flag, value, inline = "-R", strings.TrimPrefix(arg[2:], "="), true
		}
		// Consume the value of every option used by the daemon gateway before
		// inspecting the next argument. Publication text may itself start with
		// --repo, --web, or any other flag spelling. Unknown options fail here
		// instead of guessing their arity or forwarding a new CLI capability.
		takesValue, supported := true, false
		switch flag {
		case "--jq":
			supported = true
		case "--hostname", "-X", "--method", "-H", "--header", "-F", "--field", "-f", "--raw-field", "--input":
			supported = args[0] == "api"
		case "--repo", "-R", "--json", "--state", "--limit", "--label", "--author", "--base", "--assignee", "--reason", "--match-head-commit", "--body", "--head", "--title", "--color", "--description":
			supported = args[0] != "api"
		case "--paginate", "--slurp", "--include":
			takesValue, supported = false, args[0] == "api"
		case "--auto", "--merge", "--squash", "--rebase":
			takesValue, supported = false, args[0] == "pr" && args[1] == "merge"
		case "--approve", "--comment", "--request-changes":
			takesValue, supported = false, args[0] == "pr" && args[1] == "review"
		case "--draft":
			takesValue, supported = false, args[0] == "pr" && args[1] == "create"
		case "--force":
			takesValue, supported = false, args[0] == "label" && args[1] == "create"
		}
		if !supported || (!takesValue && inline) {
			return session.failure("gh", "unsupported GitHub option for a bot identity")
		}
		if !takesValue {
			continue
		}
		if !inline {
			index++
			if index == len(args) {
				return session.failure("gh", "GitHub option requires a value")
			}
			value = args[index]
		}
		if (flag == "--hostname" && !strings.EqualFold(value, base.Host)) || ((flag == "--repo" || flag == "-R") && !safeGHRepository(session, value)) {
			return session.failure("gh", "command target differs from the selected repository")
		}
	}
	if len(positional) > 1 {
		return session.failure("gh", "unsupported GitHub positional arguments for a bot identity")
	}
	if args[0] == "api" && (len(positional) != 1 || !safeGitHubEndpoint(session, positional[0])) {
		return session.failure("gh", "API target differs from the selected repository or instance")
	}
	if len(positional) == 1 {
		target := positional[0]
		if args[0] == "repo" && !safeGHRepository(session, target) {
			return session.failure("gh", "repository argument differs from the selected repository")
		}
		if (args[0] == "pr" || args[0] == "issue") && strings.Contains(target, "://") {
			parsed, err := url.Parse(target)
			if err != nil || parsed.User != nil || parsed.Scheme != base.Scheme || !strings.EqualFold(parsed.Host, base.Host) || !strings.HasPrefix(strings.ToLower(parsed.Path), "/"+strings.ToLower(session.Target().Repo)+"/") {
				return session.failure("gh", "command URL differs from the selected repository")
			}
		}
	}
	return nil
}

func safeGHRepository(session *Session, repository string) bool {
	base, _ := url.Parse(session.Target().BaseURL)
	if strings.EqualFold(repository, session.Target().Repo) || strings.EqualFold(repository, base.Host+"/"+session.Target().Repo) {
		return true
	}
	parsed, err := url.Parse(repository)
	return err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Scheme == base.Scheme && strings.EqualFold(parsed.Host, base.Host) && strings.EqualFold(strings.Trim(parsed.Path, "/"), session.Target().Repo)
}

func safeGitHubEndpoint(session *Session, endpoint string) bool {
	if endpoint == "" {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.IsAbs() || parsed.Host != "" {
		api, _ := url.Parse(session.APIURL())
		if parsed.Scheme != api.Scheme || !strings.EqualFold(parsed.Host, api.Host) {
			return false
		}
		parsed.Path = strings.TrimPrefix(parsed.Path, api.Path)
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	for _, component := range strings.Split(path, "/") {
		if component == "." || component == ".." || strings.ContainsAny(component, "%\\") {
			return false
		}
	}
	if strings.HasPrefix(strings.ToLower(path), "repos/") {
		prefix := "repos/" + strings.ToLower(session.Target().Repo)
		return strings.ToLower(path) == prefix || strings.HasPrefix(strings.ToLower(path), prefix+"/")
	}
	return path == "graphql" || path == "search/issues" || path == "search/code"
}

func safeGHCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "api" {
		return true
	}
	if len(args) < 2 {
		return false
	}
	// These are the daemon gateway's builtins. Commands that launch editors,
	// browsers, extensions, or a Git checkout/push are not credential consumers.
	switch args[0] {
	case "pr":
		switch args[1] {
		case "list", "view", "close", "merge", "diff", "review", "comment", "create", "edit", "checks":
			return true
		}
	case "issue":
		return args[1] == "list" || args[1] == "view" || args[1] == "close"
	case "repo":
		return args[1] == "view"
	case "label":
		return args[1] == "list" || args[1] == "create" || args[1] == "edit"
	}
	return false
}

// A CLI process cannot replace its token mid-pagination/transfer. Bound its
// lifetime beneath the captured token's expiry (and the manager's normal five
// minute refresh window). The context may still impose an earlier deadline.
func boundCredentialCommand(session *Session, credential Credential, options shell.Options) (shell.Options, error) {
	limit := maxHostingCommandDuration
	if session.Kind() == config.HostingIdentityGitHubApp {
		remaining := credential.ExpiresAt.Sub(session.manager.now()) - 5*time.Second
		if remaining <= 0 {
			return shell.Options{}, session.failure("subprocess", "installation credential has insufficient remaining lifetime")
		}
		if remaining < limit {
			limit = remaining
		}
	}
	if options.Timeout <= 0 || options.Timeout > limit {
		options.Timeout = limit
	}
	return options, nil
}

type redactedCommandError struct {
	message string
	cause   error
}

func (err *redactedCommandError) Error() string        { return err.message }
func (err *redactedCommandError) Is(target error) bool { return errors.Is(err.cause, target) }

func redactCommandResult(session *Session, result shell.Result, err error) (shell.Result, error) {
	clean := func(value shell.Result) shell.Result {
		value.Stdout, value.Stderr = session.Redact(value.Stdout), session.Redact(value.Stderr)
		return value
	}
	result = clean(result)
	if err == nil {
		return result, nil
	}
	var execution *shell.CommandExecutionError
	if errors.As(err, &execution) {
		return result, &shell.CommandExecutionError{Message: session.Redact(err.Error()), Result: clean(execution.Result)}
	}
	return result, &redactedCommandError{message: session.Redact(err.Error()), cause: err}
}
