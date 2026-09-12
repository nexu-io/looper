package hostingidentity

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

var gitRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// RunGit keeps local operations credential-free. Only the supported network
// commands receive a fresh selected token, after their target and repository
// transport configuration have been checked without credentials.
func RunGit(ctx context.Context, options shell.Options, runner func(context.Context, shell.Options) (shell.Result, error)) (shell.Result, error) {
	if runner == nil {
		runner = shell.Run
	}
	session, selected := FromContext(ctx)
	if !selected {
		return runner(ctx, options)
	}
	commandIndex := 0
	for commandIndex < len(options.Args) && (options.Args[commandIndex] == "--literal-pathspecs" || options.Args[commandIndex] == "--no-pager") {
		commandIndex++
	}
	if commandIndex == len(options.Args) || !safeGitCommand(options.Args[commandIndex]) {
		return shell.Result{}, session.failure("git", "unsupported Git command or global configuration override")
	}
	options.Env = privilegedEnv(options.Env)
	command := options.Args[commandIndex]
	if command != "fetch" && command != "push" && command != "ls-remote" {
		if gitCreatesCommit(command) {
			attribution, err := GitAttributionEnv(ctx)
			if err != nil {
				return shell.Result{}, err
			}
			// Keep the transport prohibitions while applying nonsecret identity.
			options.Env["GIT_COMMITTER_NAME"] = attribution["GIT_COMMITTER_NAME"]
			options.Env["GIT_COMMITTER_EMAIL"] = attribution["GIT_COMMITTER_EMAIL"]
			gitConfigEnv(options.Env, [][2]string{{"user.name", attribution["GIT_COMMITTER_NAME"]}, {"user.email", attribution["GIT_COMMITTER_EMAIL"]}})
		}
		result, err := runner(ctx, options)
		return redactCommandResult(session, result, err)
	}
	remote, err := gitNetworkRemote(session, options.Args[commandIndex:])
	if err != nil {
		return shell.Result{}, err
	}
	base, _ := url.Parse(session.Target().BaseURL)
	if base.Scheme != "https" {
		return shell.Result{}, session.failure("git", "bot Git transport requires an HTTPS hosting instance")
	}
	if options.Timeout <= 0 || options.Timeout > maxHostingCommandDuration {
		options.Timeout = maxHostingCommandDuration
	}
	dir, err := os.MkdirTemp("", "looper-host-git-")
	if err != nil {
		return shell.Result{}, session.failure("git", "cannot create private transport home directory")
	}
	defer os.RemoveAll(dir)
	// libcurl otherwise also reads ~/.netrc, independently of Git's disabled
	// credential helpers and global config. No personal authentication source
	// participates in this credential-bearing subprocess.
	options.Env["HOME"] = dir
	selectedURL := strings.TrimRight(session.Target().BaseURL, "/") + "/" + session.Target().Repo + ".git"
	probe := options
	// Include worktree config and conditional includes as well as .git/config.
	// Suppress only our command-scope defaults in this credential-free probe;
	// global/system files remain disabled by the sanitized environment.
	probe.Env = make(map[string]string, len(options.Env))
	for key, value := range options.Env {
		if key != "GIT_CONFIG_COUNT" && !strings.HasPrefix(key, "GIT_CONFIG_KEY_") && !strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			probe.Env[key] = value
		}
	}
	probe.Args = []string{"config", "--includes", "--null", "--list"}
	probe.Stdin = ""
	probeResult, probeErr := runner(ctx, probe)
	if probeErr != nil || probeResult.StdoutTruncated || probeResult.StderrTruncated {
		return shell.Result{}, session.failure("git", "cannot inspect repository transport configuration")
	}
	named := gitRemoteName.MatchString(remote)
	found := false
	var remoteURLs []string
	for _, entry := range strings.Split(probeResult.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		key, value, _ := strings.Cut(entry, "\n")
		lower := strings.ToLower(key)
		if unsafeNetworkGitConfig(lower) {
			return shell.Result{}, session.failure("git", "repository transport overrides are incompatible with bot authentication (URL rewrites, HTTP/credential settings, or custom helpers)")
		}
		if named && (lower == "remote."+strings.ToLower(remote)+".url" || lower == "remote."+strings.ToLower(remote)+".pushurl") {
			if !gitRemoteMatches(session, value) {
				return shell.Result{}, session.failure("git", "configured remote points outside the selected hosting repository")
			}
			found = true
			remoteURLs = append(remoteURLs, value)
		}
	}
	if (named && !found) || (!named && !gitRemoteMatches(session, remote)) {
		return shell.Result{}, session.failure("git", "remote must identify the selected hosting repository")
	}
	if named {
		// A remote URL is multivalued: overriding it would append a URL, and
		// empty-value resets require recent Git. Rewrite only these verified
		// exact URLs in this process instead. All repository rewrites above are
		// rejected, so a longer untrusted rewrite cannot win. Retaining the
		// named remote preserves tracking refs and -u semantics on older Git.
		for _, remoteURL := range remoteURLs {
			if remoteURL != selectedURL {
				gitConfigEnv(options.Env, [][2]string{{"url." + selectedURL + ".insteadOf", remoteURL}})
			}
		}
	} else {
		options.Args = append([]string(nil), options.Args...)
		for index, arg := range options.Args {
			if arg == remote {
				options.Args[index] = selectedURL
				break
			}
		}
	}
	credential, err := session.Credentials(ctx)
	if err != nil {
		return shell.Result{}, err
	}
	options, err = boundCredentialCommand(session, credential, options)
	if err != nil {
		return shell.Result{}, err
	}
	username := "x-access-token"
	if session.Kind() == config.HostingIdentityForgejoToken {
		username = credential.Login
	}
	gitConfigEnv(options.Env, [][2]string{
		{"protocol.https.allow", "always"}, {"http.proxy", ""}, {"http.followRedirects", "false"}, {"http.sslVerify", "true"},
		{"http.extraHeader", ""}, {"http." + selectedURL + ".extraHeader", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+credential.Token))},
		{"core.fsmonitor", "false"}, {"push.gpgSign", "false"}, {"gc.auto", "0"}, {"maintenance.auto", "false"},
	})
	result, err := runner(ctx, options)
	return redactCommandResult(session, result, err)
}

func safeGitCommand(command string) bool {
	switch command {
	case "fetch", "push", "ls-remote", "status", "rev-parse", "rev-list", "show-ref", "for-each-ref", "diff", "diff-tree", "show", "log", "cat-file", "ls-files", "check-ref-format", "merge-base", "merge-tree", "worktree", "branch", "symbolic-ref", "reflog", "config", "add", "commit", "checkout", "restore", "switch", "reset", "clean", "merge", "rebase", "cherry-pick", "revert", "update-ref", "submodule":
		return true
	default:
		return false
	}
}

func gitCreatesCommit(command string) bool {
	switch command {
	case "commit", "merge", "rebase", "cherry-pick", "revert":
		return true
	default:
		return false
	}
}

func gitNetworkRemote(session *Session, args []string) (string, error) {
	remote := ""
	refCount := 0
	for index := 1; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			allowed := false
			switch args[0] {
			case "fetch":
				allowed = arg == "--no-tags" || arg == "--no-recurse-submodules" || arg == "--no-write-fetch-head" || arg == "--refmap=" || arg == "--prune" || arg == "--force" || arg == "-f" || arg == "--quiet" || arg == "-q"
				if arg == "--depth" || strings.HasPrefix(arg, "--depth=") {
					depth := strings.TrimPrefix(arg, "--depth=")
					if arg == "--depth" && index+1 < len(args) {
						index++
						depth = args[index]
					}
					value, err := strconv.Atoi(depth)
					allowed = err == nil && value > 0
				}
			case "push":
				allowed = arg == "-u" || arg == "--set-upstream" || arg == "--porcelain" || arg == "--force-with-lease" || strings.HasPrefix(arg, "--force-with-lease=refs/heads/")
			case "ls-remote":
				allowed = arg == "--heads" || arg == "--refs" || arg == "--tags" || arg == "--exit-code"
			}
			if !allowed || strings.ContainsAny(arg, "\x00\r\n") {
				return "", session.failure("git", "unsupported network option or remote-helper override")
			}
			continue
		}
		if remote == "" {
			remote = arg
		} else {
			if strings.ContainsAny(arg, "\x00\r\n") {
				return "", session.failure("git", "invalid network refspec")
			}
			refCount++
		}
	}
	if remote == "" || (args[0] != "ls-remote" && refCount == 0) {
		return "", session.failure("git", "network command requires an explicit remote and refspec")
	}
	return remote, nil
}

func unsafeNetworkGitConfig(key string) bool {
	if strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "http.") || strings.HasPrefix(key, "credential.") || key == "core.alternaterefscommand" || key == "extensions.partialclone" {
		return true
	}
	if strings.HasPrefix(key, "remote.") {
		for _, suffix := range []string{".vcs", ".uploadpack", ".receivepack", ".proxy", ".proxyauthmethod", ".promisor"} {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	return false
}

func gitRemoteMatches(session *Session, remote string) bool {
	base, _ := url.Parse(session.Target().BaseURL)
	var host, path string
	ssh := false
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return false
		}
		ssh = parsed.Scheme == "ssh"
		if !ssh && (parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.User != nil) {
			return false
		}
		host, path = parsed.Hostname(), parsed.Path
		if !ssh && parsed.Host != base.Host {
			return false
		}
	} else {
		left, right, ok := strings.Cut(remote, ":")
		if !ok || strings.ContainsAny(left, "/\\") {
			return false
		}
		_, host, ok = strings.Cut(left, "@")
		if !ok {
			host = left
		}
		path, ssh = right, true
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	wantPath := strings.Trim(strings.TrimRight(base.Path, "/")+"/"+session.Target().Repo, "/")
	return strings.EqualFold(host, base.Hostname()) && (strings.EqualFold(path, wantPath) || ssh && strings.EqualFold(path, session.Target().Repo))
}
