//go:build linux

package notify

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/nexu-io/looper/internal/infra/shell"
)

type notifySendProvider struct {
	path       string
	runCommand RunCommandFunc
}

func newNotifySendProvider(path string, runCommand RunCommandFunc) *notifySendProvider {
	return &notifySendProvider{path: path, runCommand: runCommand}
}

func (p *notifySendProvider) Name() string { return "notify-send" }

func (p *notifySendProvider) IsAvailable() bool {
	_, err := exec.LookPath(p.path)
	return err == nil
}

func (p *notifySendProvider) Send(ctx context.Context, payload SystemNotificationPayload) error {
	urgency := "normal"
	switch payload.Level {
	case "failure", "action_required":
		urgency = "critical"
	case "success":
		urgency = "normal"
	case "info":
		urgency = "low"
	}

	args := []string{
		"--app-name", "Looper",
		"--urgency", urgency,
		payload.Title,
	}
	if payload.Body != "" {
		args = append(args, payload.Body)
	}

	_, err := p.runCommand(ctx, shell.Options{
		Command: p.path,
		Args:    args,
		Timeout: 10 * 1000 * 1000 * 1000,
	})
	if err != nil {
		return fmt.Errorf("notify-send: %w", err)
	}
	return nil
}

func defaultPlatformProviders() []Provider {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return []Provider{}
	}
	return []Provider{
		newNotifySendProvider(path, shell.Run),
	}
}
