package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/config"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/infra/shell"
	"github.com/powerformer/looper/internal/storage"
)

const osascriptTimeout = 35 * time.Second

type RunCommandFunc func(context.Context, shell.Options) (shell.Result, error)

type Options struct {
	Config        config.NotificationConfig
	OsascriptPath string
	LogFilePath   string
	Repositories  *storage.Repositories
	Now           func() time.Time
	RunCommand    RunCommandFunc
}

type SystemNotificationPayload struct {
	ID         string
	ProjectID  string
	LoopID     string
	RunID      string
	Level      string
	Title      string
	Subtitle   string
	Body       string
	Sound      string
	Group      string
	EntityType string
	EntityID   string
	DedupeKey  string
}

type Gateway struct {
	config        config.NotificationConfig
	osascriptPath string
	logFilePath   string
	repositories  *storage.Repositories
	now           func() time.Time
	runCommand    RunCommandFunc
}

func NewGateway(options Options) *Gateway {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	runCommand := options.RunCommand
	if runCommand == nil {
		runCommand = shell.Run
	}

	return &Gateway{
		config:        options.Config,
		osascriptPath: options.OsascriptPath,
		logFilePath:   options.LogFilePath,
		repositories:  options.Repositories,
		now:           now,
		runCommand:    runCommand,
	}
}

func (g *Gateway) Notify(ctx context.Context, payload SystemNotificationPayload) []storage.NotificationRecord {
	records := make([]storage.NotificationRecord, 0, 2)

	if record, ok := g.recordInApp(ctx, payload); ok {
		records = append(records, record)
	}

	if record, ok := g.recordOsascript(ctx, payload); ok {
		records = append(records, record)
	}

	return records
}

func (g *Gateway) recordInApp(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	record := storage.NotificationRecord{
		ID:           firstNonEmpty(payload.ID, eventlog.NewEventID("notification")),
		ProjectID:    nilIfEmpty(payload.ProjectID),
		LoopID:       nilIfEmpty(payload.LoopID),
		RunID:        nilIfEmpty(payload.RunID),
		EntityType:   nilIfEmpty(payload.EntityType),
		EntityID:     nilIfEmpty(payload.EntityID),
		Channel:      "in_app",
		Level:        payload.Level,
		Title:        payload.Title,
		Subtitle:     nilIfEmpty(payload.Subtitle),
		Body:         payload.Body,
		Status:       ternaryString(g.config.InApp, "success", "skipped"),
		DedupeKey:    nilIfEmpty(payload.DedupeKey),
		ErrorMessage: ternaryPointer(!g.config.InApp, "disabled"),
		PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
		SentAt:       ternaryTimePointer(g.config.InApp, nowISO),
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}

	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) recordOsascript(ctx context.Context, payload SystemNotificationPayload) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, "osascript", payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Osascript.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					record := storage.NotificationRecord{
						ID:           id,
						ProjectID:    nilIfEmpty(payload.ProjectID),
						LoopID:       nilIfEmpty(payload.LoopID),
						RunID:        nilIfEmpty(payload.RunID),
						EntityType:   nilIfEmpty(payload.EntityType),
						EntityID:     nilIfEmpty(payload.EntityID),
						Channel:      "osascript",
						Level:        payload.Level,
						Title:        payload.Title,
						Subtitle:     nilIfEmpty(payload.Subtitle),
						Body:         payload.Body,
						Status:       "skipped",
						DedupeKey:    nilIfEmpty(payload.DedupeKey),
						ErrorMessage: stringPointer("deduped"),
						PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
						CreatedAt:    nowISO,
						UpdatedAt:    nowISO,
					}
					if err := g.persistNotification(ctx, record); err != nil {
						return storage.NotificationRecord{}, false
					}
					return record, true
				}
			}
		}
	}

	if !g.config.Osascript.Enabled || strings.TrimSpace(g.osascriptPath) == "" {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "skipped",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer("disabled"),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	_, err := g.runCommand(ctx, shell.Options{
		Command: g.osascriptPath,
		Args:    []string{"-e", buildAppleScript(payload, g.config, g.logFilePath)},
		Timeout: osascriptTimeout,
	})
	if err != nil {
		record := storage.NotificationRecord{
			ID:           id,
			ProjectID:    nilIfEmpty(payload.ProjectID),
			LoopID:       nilIfEmpty(payload.LoopID),
			RunID:        nilIfEmpty(payload.RunID),
			EntityType:   nilIfEmpty(payload.EntityType),
			EntityID:     nilIfEmpty(payload.EntityID),
			Channel:      "osascript",
			Level:        payload.Level,
			Title:        payload.Title,
			Subtitle:     nilIfEmpty(payload.Subtitle),
			Body:         payload.Body,
			Status:       "failed",
			DedupeKey:    nilIfEmpty(payload.DedupeKey),
			ErrorMessage: stringPointer(err.Error()),
			PayloadJSON:  stringPointer(mustMarshalPayload(payload)),
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if persistErr := g.persistNotification(ctx, record); persistErr != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	record := storage.NotificationRecord{
		ID:          id,
		ProjectID:   nilIfEmpty(payload.ProjectID),
		LoopID:      nilIfEmpty(payload.LoopID),
		RunID:       nilIfEmpty(payload.RunID),
		EntityType:  nilIfEmpty(payload.EntityType),
		EntityID:    nilIfEmpty(payload.EntityID),
		Channel:     "osascript",
		Level:       payload.Level,
		Title:       payload.Title,
		Subtitle:    nilIfEmpty(payload.Subtitle),
		Body:        payload.Body,
		Status:      "success",
		DedupeKey:   nilIfEmpty(payload.DedupeKey),
		PayloadJSON: stringPointer(mustMarshalPayload(payload)),
		SentAt:      stringPointer(nowISO),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}

	return record, true
}

func (g *Gateway) persistNotification(ctx context.Context, record storage.NotificationRecord) error {
	if g.repositories == nil || g.repositories.Notifications == nil || g.repositories.Events == nil {
		return fmt.Errorf("notification repositories are not configured")
	}

	if err := g.repositories.Notifications.Upsert(ctx, record); err != nil {
		return err
	}

	return eventlog.Append(ctx, g.repositories, eventlog.AppendInput{
		ID:         eventlog.NewEventID("event"),
		EventType:  "notification.sent",
		ProjectID:  record.ProjectID,
		LoopID:     record.LoopID,
		RunID:      record.RunID,
		EntityType: firstPointer(record.EntityType, stringPointer("notification")),
		EntityID:   firstPointer(record.EntityID, &record.ID),
		Payload: map[string]any{
			"channel":   record.Channel,
			"level":     record.Level,
			"status":    record.Status,
			"dedupeKey": record.DedupeKey,
			"title":     record.Title,
		},
		CreatedAt: mustParseJSISOString(record.CreatedAt),
	})
}

func buildAppleScript(payload SystemNotificationPayload, cfg config.NotificationConfig, logFilePath string) string {
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
	if payload.Sound != "" && isSoundEnabledForLevel(cfg, payload.Level) {
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

func isSoundEnabledForLevel(cfg config.NotificationConfig, level string) bool {
	for _, candidate := range cfg.Osascript.SoundForLevels {
		if string(candidate) == level {
			return true
		}
	}

	return false
}

func mustMarshalPayload(payload SystemNotificationPayload) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}

	return string(encoded)
}

func mustParseJSISOString(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UTC()
	}

	return parsed
}

func nilIfEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	return &value
}

func stringPointer(value string) *string {
	return &value
}

func firstPointer(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}

	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func ternaryString(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}

	return whenFalse
}

func ternaryPointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}

func ternaryTimePointer(condition bool, value string) *string {
	if !condition {
		return nil
	}

	return &value
}
