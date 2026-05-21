package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/spf13/cobra"
)

type netadminRepoStatusOutput struct {
	Repo           string        `json:"repo"`
	DeliveryURL    string        `json:"deliveryUrl"`
	LabelResult    any           `json:"labelResult,omitempty"`
	Hooks          []webhookHook `json:"hooks,omitempty"`
	RemovedHookIDs []int64       `json:"removedHookIds,omitempty"`
}

func (r *commandRuntime) netadminOnboardRepo(cmd *cobra.Command, args []string) error {
	repo, ghPath, cwd, deliveryURL, err := r.netadminRepoContext(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	secret, err := r.fetchLoopernetWebhookSecret(cmd.Context(), deliveryURL)
	if err != nil {
		return err
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: cwd, GHRun: r.runGHCommand})
	result, err := gh.InitializeLabels(cmd.Context(), githubinfra.InitializeLabelsInput{Repo: repo, CWD: cwd})
	if err != nil {
		return err
	}
	hook, err := r.ensureNetadminWebhook(cmd.Context(), ghPath, repo, deliveryURL, secret)
	if err != nil {
		return err
	}
	output := netadminRepoStatusOutput{Repo: repo, DeliveryURL: deliveryURL, LabelResult: result, Hooks: []webhookHook{hook}}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Onboarded %s to %s (hookId=%d).\n", repo, deliveryURL, hook.ID)
	return err
}

func (r *commandRuntime) netadminOffboardRepo(cmd *cobra.Command, args []string) error {
	repo, ghPath, _, deliveryURL, err := r.netadminRepoContext(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	hooks, err := r.listWebhookHooks(cmd.Context(), ghPath, repo)
	if err != nil {
		return err
	}
	removed := make([]int64, 0)
	for _, hook := range hooks {
		if strings.TrimSpace(hook.Config.URL) != deliveryURL {
			continue
		}
		if err := r.deleteWebhookHook(cmd.Context(), ghPath, repo, hook.ID); err != nil {
			return err
		}
		removed = append(removed, hook.ID)
	}
	output := netadminRepoStatusOutput{Repo: repo, DeliveryURL: deliveryURL, RemovedHookIDs: removed}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Offboarded %s from %s (%d hook(s) removed).\n", repo, deliveryURL, len(removed))
	return err
}

func (r *commandRuntime) netadminRepoStatus(cmd *cobra.Command, args []string) error {
	repo, ghPath, _, deliveryURL, err := r.netadminRepoContext(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	hooks, err := r.listWebhookHooks(cmd.Context(), ghPath, repo)
	if err != nil {
		return err
	}
	matched := make([]webhookHook, 0)
	for _, hook := range hooks {
		if strings.TrimSpace(hook.Config.URL) == deliveryURL {
			matched = append(matched, hook)
		}
	}
	output := netadminRepoStatusOutput{Repo: repo, DeliveryURL: deliveryURL, Hooks: matched}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s webhook hooks targeting %s: %d\n", repo, deliveryURL, len(matched))
	return err
}

func (r *commandRuntime) netadminRepoContext(ctx context.Context, rawRepo string) (string, string, string, string, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return "", "", "", "", err
	}
	ghPath, err := r.resolveGHPath(loaded.Config)
	if err != nil {
		return "", "", "", "", err
	}
	repo, err := normalizeWebhookRepo(rawRepo)
	if err != nil {
		return "", "", "", "", err
	}
	hostname := labelsAuthHostname(repo)
	cwd, err := r.getwd()
	if err != nil {
		return "", "", "", "", err
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: cwd, GHRun: r.runGHCommand})
	if authenticated, err := gh.IsAuthenticated(ctx, cwd, hostname); err != nil {
		return "", "", "", "", fmt.Errorf("check gh authentication: %w", err)
	} else if !authenticated {
		return "", "", "", "", fmt.Errorf("gh is not authenticated; run `gh auth login` and retry")
	}
	deliveryBaseURL := loaded.Config.Network.LoopernetBaseURL
	if strings.TrimSpace(deliveryBaseURL) == "" && loaded.Config.Server.BaseURL != nil {
		deliveryBaseURL = *loaded.Config.Server.BaseURL
	}
	deliveryURL, err := loopernetWebhookDeliveryURL(deliveryBaseURL)
	if err != nil {
		return "", "", "", "", err
	}
	return repo, ghPath, cwd, deliveryURL, nil
}

func loopernetWebhookDeliveryURL(baseURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return "", fmt.Errorf("network.loopernetBaseUrl is required for netadmin repo operations")
	}
	return trimmed + "/v1/github/webhook", nil
}

func (r *commandRuntime) fetchLoopernetWebhookSecret(ctx context.Context, deliveryURL string) (string, error) {
	adminToken := strings.TrimSpace(os.Getenv("LOOPERNET_ADMIN_TOKEN"))
	if adminToken == "" {
		return "", fmt.Errorf("LOOPERNET_ADMIN_TOKEN is required for netadmin onboard-repo")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(deliveryURL, "/v1/github/webhook")+"/v1/github/webhook-secret", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch loopernet webhook secret: request failed with status %d", resp.StatusCode)
	}
	var payload struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Secret) == "" {
		return "", fmt.Errorf("fetch loopernet webhook secret: empty secret")
	}
	return strings.TrimSpace(payload.Secret), nil
}

func (r *commandRuntime) ensureNetadminWebhook(ctx context.Context, ghPath, repo, deliveryURL, secret string) (webhookHook, error) {
	hooks, err := r.listWebhookHooks(ctx, ghPath, repo)
	if err != nil {
		return webhookHook{}, err
	}
	for _, hook := range hooks {
		if strings.TrimSpace(hook.Config.URL) == deliveryURL {
			if err := r.updateWebhookHookSecret(ctx, ghPath, repo, hook.ID, deliveryURL, secret); err != nil {
				return webhookHook{}, err
			}
			return hook, nil
		}
	}
	return r.createWebhookHook(ctx, ghPath, repo, deliveryURL, secret)
}

func (r *commandRuntime) createWebhookHook(ctx context.Context, ghPath, repo, deliveryURL string, secret string) (webhookHook, error) {
	hostname, repoPath := splitWebhookRepoHostname(repo)
	body := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"check_run", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "push"},
		"config": map[string]string{"url": deliveryURL, "content_type": "json", "insecure_ssl": "0", "secret": secret},
	}
	raw, _ := json.Marshal(body)
	tmp, err := os.CreateTemp("", "looper-netadmin-hook-*.json")
	if err != nil {
		return webhookHook{}, err
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return webhookHook{}, err
	}
	if err := tmp.Close(); err != nil {
		return webhookHook{}, err
	}
	args := []string{"api", fmt.Sprintf("repos/%s/hooks", repoPath), "--method", "POST", "--input", path}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := r.runCommand(ctx, ghPath, args, 15*time.Second)
	if err != nil {
		return webhookHook{}, fmt.Errorf("create webhook hook for %s: %w", repo, err)
	}
	if result.ExitCode != 0 {
		output := strings.TrimSpace(strings.Join([]string{result.Stderr, result.Stdout}, "\n"))
		if output == "" {
			output = fmt.Sprintf("exit code %d", result.ExitCode)
		}
		return webhookHook{}, fmt.Errorf("create webhook hook for %s: %s", repo, output)
	}
	var hook webhookHook
	if err := json.Unmarshal([]byte(result.Stdout), &hook); err != nil {
		return webhookHook{}, fmt.Errorf("decode created webhook hook for %s: %w", repo, err)
	}
	return hook, nil
}
