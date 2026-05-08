package github

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/infra/shell"
)

type TransientError struct {
	Err error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient GitHub error: %v", e.Err)
}

func (e *TransientError) Unwrap() error {
	return e.Err
}

// IsTransientError reports whether a GitHub CLI/API failure is likely caused by
// network or service flakiness and should be retried instead of treated as a
// terminal loop failure.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	var transientErr *TransientError
	if errors.As(err, &transientErr) {
		return true
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		message := strings.Join([]string{commandErr.Message, commandErr.Result.Stdout, commandErr.Result.Stderr}, "\n")
		return (looksLikeGitHubFailure(message) && isTransientGitHubMessage(message)) || isExplicitTransientGitHubStatus(message)
	}
	message := err.Error()
	if !looksLikeGitHubFailure(message) && !isExplicitTransientGitHubStatus(message) {
		return false
	}
	return isTransientGitHubMessage(message)
}

// ErrorMessage returns the most useful user-facing text for a GitHub CLI/API
// error, including captured command output when the shell error message is only
// a generic exit-code summary.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		return strings.TrimSpace(strings.Join(nonEmptyStrings(commandErr.Message, commandErr.Result.Stdout, commandErr.Result.Stderr), "\n"))
	}
	return err.Error()
}

// IsPullRequestNotFoundError reports whether GitHub could not resolve the PR
// object, which usually means a queued reviewer loop is stale.
func IsPullRequestNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) {
		message := strings.Join([]string{commandErr.Message, commandErr.Result.Stdout, commandErr.Result.Stderr}, "\n")
		return isPullRequestNotFoundMessage(message)
	}
	return isPullRequestNotFoundMessage(err.Error())
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func looksLikeGitHubFailure(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"github",
		"api.github.com",
		"graphql",
		"gh api",
		"gh pr",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isPullRequestNotFoundMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "could not resolve to a pullrequest")
}

func isTransientGitHubMessage(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"tls handshake timeout",
		"unexpected eof",
		"connection reset by peer",
		"connection refused",
		"connection timed out",
		"i/o timeout",
		"temporary failure in name resolution",
		"no such host",
		"network is unreachable",
		"stream error",
		"http2: server sent goaway",
		"http 502",
		"502 bad gateway",
		"http 503",
		"503 service unavailable",
		"http 504",
		"504 gateway timeout",
		"secondary rate limit",
		"rate limit exceeded",
		"api rate limit exceeded",
		"graphql: something went wrong",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isExplicitTransientGitHubStatus(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"http 502",
		"http 503",
		"http 504",
		"502 bad gateway",
		"503 service unavailable",
		"504 gateway timeout",
		"secondary rate limit",
		"rate limit exceeded",
		"api rate limit exceeded",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
