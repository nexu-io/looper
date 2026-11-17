package cliapp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/spf13/cobra"
)

func isForgejoManagedWebhookRepo(repo string) bool {
	repo = strings.ToLower(repo)
	return strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "http://")
}

func normalizeManagedWebhookRepo(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !isForgejoManagedWebhookRepo(value) {
		return normalizeWebhookRepo(value)
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Count(strings.Trim(u.Path, "/"), "/") < 1 {
		return "", fmt.Errorf("Forgejo webhook repo must be its full repository URL")
	}
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	// Repository names are case-insensitive; the installation subpath is not.
	parts[len(parts)-2] = strings.ToLower(parts[len(parts)-2])
	parts[len(parts)-1] = strings.ToLower(parts[len(parts)-1])
	u.Path, u.RawPath = strings.Join(parts, "/"), ""
	return u.String(), nil
}

func (r *commandRuntime) getManagedWebhookHook(ctx context.Context, cfg config.Config, repo string, id int64) (webhookHook, bool, error) {
	if isForgejoManagedWebhookRepo(repo) {
		client, err := forge.NewForgejoWebhookClient(ctx, cfg, repo)
		if err != nil {
			return webhookHook{}, false, err
		}
		hook, found, err := client.GetWebhook(ctx, id)
		result := webhookHook{ID: hook.ID, Type: hook.Type, Active: hook.Active, Events: hook.Events}
		result.Config.URL = hook.Config.URL
		return result, found, err
	}
	ghPath, err := r.resolveGHPath(cfg)
	if err != nil {
		return webhookHook{}, false, err
	}
	return r.getWebhookHook(ctx, ghPath, repo, id)
}

func (r *commandRuntime) deleteManagedWebhookHook(ctx context.Context, cfg config.Config, repo string, id int64) error {
	if isForgejoManagedWebhookRepo(repo) {
		client, err := forge.NewForgejoWebhookClient(ctx, cfg, repo)
		if err != nil {
			return err
		}
		return client.DeleteWebhook(ctx, id)
	}
	ghPath, err := r.resolveGHPath(cfg)
	if err != nil {
		return err
	}
	return r.deleteWebhookHook(ctx, ghPath, repo, id)
}

func (r *commandRuntime) rotateForgejoWebhook(cmd *cobra.Command, cfg config.Config, repos *storage.Repositories, record storage.WebhookTunnelHookRecord) error {
	ctx := cmd.Context()
	client, err := forge.NewForgejoWebhookClient(ctx, cfg, record.Repo)
	if err != nil {
		return err
	}
	old, found, err := client.GetWebhook(ctx, record.HookID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(old.Config.URL) != strings.TrimSpace(record.ManagedURL) {
		return fmt.Errorf("refusing to rotate hook %d for %s: remote hook is missing or URL differs from managed URL", record.HookID, record.Repo)
	}
	secret, err := generateWebhookTunnelSecret()
	if err != nil {
		return err
	}
	// A distinct existing secret_ref lets the old hook remain usable until
	// both its replacement and local authority record are ready.
	file, err := os.CreateTemp(filepath.Dir(webhookTunnelSecretPathForCLI(cfg.Storage.DBPath, record.SecretRef)), "webhook-forgejo-*.key")
	if err != nil {
		return err
	}
	secretPath := file.Name()
	secretRef := filepath.Base(secretPath)
	keepSecret := false
	defer func() {
		if !keepSecret {
			_ = os.Remove(secretPath)
		}
	}()
	if _, err := file.WriteString(secret); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	hook, err := client.CreateWebhook(ctx, record.ManagedURL, secret)
	if err != nil {
		return err
	}
	updated := record
	updated.HookID, updated.SecretRef, updated.UpdatedAt = hook.ID, secretRef, time.Now().UnixNano()
	updated.ConsecutiveDisables, updated.LastDisableAt, updated.LastPingAt = 0, nil, nil
	if err := repos.WebhookTunnelHooks.SaveIfCurrentHookID(ctx, updated, record.HookID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := client.DeleteWebhook(cleanupCtx, hook.ID); cleanupErr != nil {
			return fmt.Errorf("persist replacement: %w; delete replacement hook %d: %v", err, hook.ID, cleanupErr)
		}
		return err
	}
	keepSecret = true
	if err := client.DeleteWebhook(ctx, record.HookID); err != nil {
		return fmt.Errorf("secret rotated to hook %d; remove previous remote hook %d manually: %w", hook.ID, record.HookID, err)
	}
	_ = os.Remove(webhookTunnelSecretPathForCLI(cfg.Storage.DBPath, record.SecretRef))
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Rotated managed tunnel webhook secret for %s (hookId=%d, replaced=%d).\n", record.Repo, hook.ID, record.HookID)
	return err
}
