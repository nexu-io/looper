package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestWebhookRotateRefusesURLMismatchBeforePatch(t *testing.T) {
	t.Parallel()

	configPath, repo := writeWebhookCommandConfigWithRecord(t, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: "https://example.com/webhook/acme/looper", SecretRef: "webhook_acme_looper.key", CreatedAt: 1, UpdatedAt: 1})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	commands := []string{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		LookPath: func(command string) (string, error) {
			if command == "gh" {
				return "/usr/bin/gh", nil
			}
			return command, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			commands = append(commands, command+" "+strings.Join(args, " "))
			if len(commands) != 1 {
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
			}
			return commandExecutionResult{Stdout: `{"id":42,"active":true,"events":["pull_request"],"config":{"url":"https://example.com/other","content_type":"json","insecure_ssl":"0"}}`, ExitCode: 0}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "rotate", repo, "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run(webhook rotate) exit code = %d, want non-zero", exitCode)
	}
	if len(commands) != 1 || !strings.HasSuffix(commands[0], " api repos/acme/looper/hooks/42") {
		t.Fatalf("commands = %q, want a single GET preflight", commands)
	}
	if !strings.Contains(stderr.String(), "refusing to rotate hook 42 for acme/looper") {
		t.Fatalf("stderr = %q, want URL mismatch refusal", stderr.String())
	}
}

func TestWebhookRotateFailsWhenExistingSecretCannotBeRead(t *testing.T) {
	t.Parallel()

	configPath, repo := writeWebhookCommandConfigWithRecord(t, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: "https://example.com/webhook/acme/looper", SecretRef: "webhook_acme_looper.key", CreatedAt: 1, UpdatedAt: 1})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	commands := []string{}
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		LookPath: func(command string) (string, error) {
			if command == "gh" {
				return "/usr/bin/gh", nil
			}
			return command, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			commands = append(commands, command+" "+strings.Join(args, " "))
			if len(commands) != 1 {
				t.Fatalf("unexpected RunCommand call %d: %s %q", len(commands), command, args)
			}
			return commandExecutionResult{Stdout: `{"id":42,"active":true,"events":["pull_request"],"config":{"url":"https://example.com/webhook/acme/looper","content_type":"json","insecure_ssl":"0"}}`, ExitCode: 0}, nil
		},
	})

	secretPath := webhookTunnelSecretPathForCLI(filepath.Join(filepath.Dir(configPath), "looper.sqlite"), "webhook_acme_looper.key")
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want not exists", secretPath, err)
	}

	exitCode := app.Run(context.Background(), []string{"webhook", "rotate", repo, "--config", configPath})
	if exitCode == 0 {
		t.Fatalf("Run(webhook rotate) exit code = %d, want non-zero", exitCode)
	}
	if len(commands) != 1 || !strings.HasSuffix(commands[0], " api repos/acme/looper/hooks/42") {
		t.Fatalf("commands = %q, want a single GET preflight before the local secret read failure", commands)
	}
	if !strings.Contains(stderr.String(), "read existing tunnel webhook secret") {
		t.Fatalf("stderr = %q, want secret read failure", stderr.String())
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) error = %v, want secret file to remain missing", secretPath, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
}

func TestWebhookDeleteConfirmForgetRemovesLocalRecordWithoutRunningGH(t *testing.T) {
	t.Parallel()

	configPath, repoStoreKey := writeWebhookCommandConfigWithRecord(t, storage.WebhookTunnelHookRecord{Repo: "acme/looper", HookID: 42, ManagedURL: "https://example.com/webhook/acme/looper", SecretRef: "webhook_acme_looper.key", Orphaned: true, CreatedAt: 1, UpdatedAt: 1})
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	runCalled := false
	app := New(Deps{
		Stdout: stdout,
		Stderr: stderr,
		LookPath: func(command string) (string, error) {
			if command == "gh" {
				return "/usr/bin/gh", nil
			}
			return command, nil
		},
		RunCommand: func(ctx context.Context, command string, args []string, timeout time.Duration) (commandExecutionResult, error) {
			runCalled = true
			return commandExecutionResult{}, nil
		},
	})

	exitCode := app.Run(context.Background(), []string{"webhook", "delete", repoStoreKey, "--confirm", "--forget", "--config", configPath})
	if exitCode != 0 {
		t.Fatalf("Run(webhook delete --confirm --forget) exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if runCalled {
		t.Fatal("RunCommand() called, want no gh invocation")
	}
	if !strings.Contains(stdout.String(), "Forgot local tunnel webhook record for acme/looper") {
		t.Fatalf("stdout = %q, want forget confirmation", stdout.String())
	}
	db, err := storage.OpenSQLiteDB(context.Background(), filepath.Join(filepath.Dir(configPath), "looper.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLiteDB() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	_, ok, err := storage.NewRepositories(db).WebhookTunnelHooks.Get(context.Background(), "acme/looper")
	if err != nil {
		t.Fatalf("WebhookTunnelHooks.Get() error = %v", err)
	}
	if ok {
		t.Fatal("WebhookTunnelHooks.Get() found record, want deleted")
	}
}

func writeWebhookCommandConfigWithRecord(t *testing.T, record storage.WebhookTunnelHookRecord) (string, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "looper.sqlite")
	coordinator := openMigratedCLIWebhookCoordinator(t, dbPath)
	defer func() { _ = coordinator.Close() }()
	if err := storage.NewRepositories(coordinator.DB()).WebhookTunnelHooks.Upsert(context.Background(), record); err != nil {
		t.Fatalf("WebhookTunnelHooks.Upsert() error = %v", err)
	}
	raw, err := json.Marshal(map[string]any{
		"storage": map[string]any{"dbPath": dbPath},
		"tools":   map[string]any{"ghPath": "/usr/bin/gh"},
		"notifications": map[string]any{
			"osascript": map[string]any{"enabled": false},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath, record.Repo
}

func openMigratedCLIWebhookCoordinator(t *testing.T, dbPath string) *storage.SQLiteCoordinator {
	t.Helper()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), dbPath, storage.SQLiteCoordinatorOptions{BackupDir: filepath.Join(filepath.Dir(dbPath), "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	return coordinator
}
