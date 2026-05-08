//go:build darwin

package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

type osascriptProvider struct {
	path       string
	throttle   int
	soundFor   []string
	runCommand RunCommandFunc
}

func newOsascriptProvider(path string, throttleWindow int, soundForLevels []string, runCommand RunCommandFunc) *osascriptProvider {
	return &osascriptProvider{
		path:       path,
		throttle:   throttleWindow,
		soundFor:   soundForLevels,
		runCommand: runCommand,
	}
}

func (p *osascriptProvider) Name() string { return "osascript" }

func (p *osascriptProvider) IsAvailable() bool {
	return strings.TrimSpace(p.path) != ""
}

func (p *osascriptProvider) Send(ctx context.Context, payload SystemNotificationPayload) error {
	_, err := p.runCommand(ctx, shell.Options{
		Command: p.path,
		Args:    []string{"-e", buildAppleScript(payload, p.soundFor, p.path)},
		Timeout: 35 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("osascript: %w", err)
	}
	return nil
}

func defaultPlatformProviders() []Provider {
	return []Provider{}
}

func buildAppleScript(payload SystemNotificationPayload, soundForLevels []string, logFilePath string) string {
	body := escapeAppleScriptString(payload.Body)
	title := escapeAppleScriptString(payload.Title)

	if payload.Level == "failure" && strings.TrimSpace(logFilePath) != "" {
		openLogPath := escapeAppleScriptString(logFilePath)
		return fmt.Sprintf(`set dialogResult to display dialog %q with title %q buttons {"Open Log", "Dismiss"} default button "Dismiss" cancel button "Dismiss" giving up after 30
if gave up of dialogResult is false and button returned of dialogResult is "Open Log" then
  do shell script "open " & quoted form of %q
end if`, body, title, openLogPath)
	}

	subtitle := ""
	if payload.Subtitle != "" {
		subtitle = fmt.Sprintf(` subtitle %q`, escapeAppleScriptString(payload.Subtitle))
	}

	sound := ""
	if payload.Sound != "" && isSoundEnabledForLevel(soundForLevels, payload.Level) {
		sound = fmt.Sprintf(` sound name %q`, escapeAppleScriptString(payload.Sound))
	}

	return fmt.Sprintf(`display notification %q with title %q%s%s`, body, title, subtitle, sound)
}

func escapeAppleScriptString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func isSoundEnabledForLevel(soundForLevels []string, level string) bool {
	for _, candidate := range soundForLevels {
		if candidate == level {
			return true
		}
	}
	return false
}
