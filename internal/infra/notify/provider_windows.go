//go:build windows

package notify

import (
	"context"
	"fmt"

	"github.com/nexu-io/looper/internal/infra/shell"
)

type toastProvider struct {
	runCommand RunCommandFunc
}

func newToastProvider(runCommand RunCommandFunc) *toastProvider {
	return &toastProvider{runCommand: runCommand}
}

func (p *toastProvider) Name() string { return "toast" }

func (p *toastProvider) IsAvailable() bool { return true }

func (p *toastProvider) Send(ctx context.Context, payload SystemNotificationPayload) error {
	_, err := p.runCommand(ctx, shell.Options{
		Command: "powershell",
		Args: []string{
			"-NoProfile",
			"-Command",
			fmt.Sprintf(
				`New-BurntToastNotification -AppLogo "Looper" -Text "%s", "%s"`,
				payload.Title,
				payload.Body,
			),
		},
		Timeout: 15 * 1000 * 1000 * 1000,
	})
	if err != nil {
		return fmt.Errorf("toast: %w", err)
	}
	return nil
}

func defaultPlatformProviders() []Provider {
	return []Provider{
		newToastProvider(shell.Run),
	}
}
