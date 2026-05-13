package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/spf13/cobra"
)

const feedbackRepo = "nexu-io/looper"

const feedbackCommandTimeout = 5 * time.Minute

var feedbackIssueURLPattern = regexp.MustCompile(`https://github\.com/` + regexp.QuoteMeta(feedbackRepo) + `/issues/\d+`)

type feedbackOutput struct {
	Repo      string `json:"repo"`
	TitleHint string `json:"titleHint"`
	Message   string `json:"message"`
	IssueURL  string `json:"issueUrl"`
	Summary   string `json:"summary"`
}

func (r *commandRuntime) feedback(cmd *cobra.Command, args []string) error {
	message := strings.TrimSpace(strings.Join(args, " "))
	if message == "" {
		return fmt.Errorf("feedback message is required")
	}
	titleHint := strings.TrimSpace(getStringFlag(cmd, "title"))

	loaded, err := r.loadConfig()
	if err != nil {
		return err
	}
	if loaded.Config.Agent.Vendor == nil || strings.TrimSpace(string(*loaded.Config.Agent.Vendor)) == "" {
		return fmt.Errorf("feedback requires agent.vendor to be configured")
	}

	prompt := buildFeedbackPrompt(titleHint, message, disclosure.FromConfig(loaded.Config))
	agentCfg := agent.ExecutorConfig{
		Vendor: *loaded.Config.Agent.Vendor,
		Model:  loaded.Config.Agent.Model,
		Params: loaded.Config.Agent.Params,
		Env:    loaded.Config.Agent.Env,
	}
	cwd, err := r.getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	command, commandArgs := agent.ResolveSpawn(agentCfg, cwd, prompt)

	runCtx, cancel := context.WithTimeout(cmd.Context(), feedbackCommandTimeout)
	defer cancel()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	process := exec.CommandContext(runCtx, command, commandArgs...)
	process.Dir = cwd
	process.Stdout = stdout
	process.Stderr = stderr
	process.Env = agent.BuildCommandEnv(cwd, prompt, loaded.Config.Agent.Env)

	if err := process.Run(); err != nil {
		return feedbackRunError(err, stdout.String(), stderr.String())
	}

	issueURL := extractFeedbackIssueURL(stdout.String(), stderr.String())
	summary := parseFeedbackCompletionSummary(stdout.String(), stderr.String())
	if issueURL == "" && summary == "" {
		return fmt.Errorf("feedback agent completed without reporting an issue URL or summary")
	}

	output := feedbackOutput{
		Repo:      feedbackRepo,
		TitleHint: titleHint,
		Message:   message,
		IssueURL:  issueURL,
		Summary:   summary,
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	if issueURL != "" {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), issueURL)
		return err
	}
	if summary != "" {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), summary)
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Feedback submitted successfully.")
	return err
}

func buildFeedbackPrompt(titleHint, message string, stamper disclosure.Stamper) string {
	sections := []string{
		"Create a new GitHub issue in the repository nexu-io/looper for the user feedback below.",
		"Write the issue title and body in English.",
		"Use the local GitHub CLI (`gh`) if needed.",
	}
	if footer := stamper.Markdown("", "feedback", disclosure.ChannelIssueComment); footer != "" {
		sections = append(sections, "Before creating the issue, append this exact disclosure footer to the generated issue body and do not include hostname, username, local paths, IP/MAC addresses, env vars, tokens, endpoints, or machine identifiers:\n"+footer)
	}
	if titleHint != "" {
		sections = append(sections, "Preferred title hint: "+titleHint)
	}
	sections = append(sections,
		"Feedback message:\n"+message,
		"After creating the issue, print the issue URL (https://github.com/nexu-io/looper/issues/<number>) before the final completion marker line.",
	)
	return agent.AppendCompletionInstruction(strings.Join(sections, "\n\n"))
}

func extractFeedbackIssueURL(stdout, stderr string) string {
	joined := strings.TrimSpace(stdout + "\n" + stderr)
	if joined == "" {
		return ""
	}
	matches := feedbackIssueURLPattern.FindAllString(joined, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

func parseFeedbackCompletionSummary(stdout, stderr string) string {
	lines := strings.Split(strings.TrimSpace(stdout+"\n"+stderr), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if !strings.HasPrefix(line, agent.CompletionMarkerPrefix) {
			continue
		}
		payload := strings.TrimPrefix(line, agent.CompletionMarkerPrefix)
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			return ""
		}
		summary, _ := parsed["summary"].(string)
		return strings.TrimSpace(summary)
	}
	return ""
}

func feedbackRunError(err error, stdout string, stderr string) error {
	summary := parseFeedbackCompletionSummary(stdout, stderr)
	if summary != "" {
		return fmt.Errorf("run feedback agent: %w (%s)", err, summary)
	}
	return fmt.Errorf("run feedback agent: %w", err)
}
