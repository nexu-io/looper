package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

type RunCommandFunc func(context.Context, shell.Options) (shell.Result, error)

type Options struct {
	Config       config.NotificationConfig
	Providers    []Provider
	LogFilePath  string
	Repositories *storage.Repositories
	Now          func() time.Time
	RunCommand   RunCommandFunc
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
	config       config.NotificationConfig
	providers    []Provider
	logFilePath  string
	repositories *storage.Repositories
	now          func() time.Time
	runCommand   RunCommandFunc
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

	providers := options.Providers
	if providers == nil {
		providers = defaultPlatformProviders()
	}

	return &Gateway{
		config:       options.Config,
		providers:    providers,
		logFilePath:  options.LogFilePath,
		repositories: options.Repositories,
		now:          now,
		runCommand:   runCommand,
	}
}

func (g *Gateway) Notify(ctx context.Context, payload SystemNotificationPayload) []storage.NotificationRecord {
	records := make([]storage.NotificationRecord, 0, 2)

	if record, ok := g.recordInApp(ctx, payload); ok {
		records = append(records, record)
	}

	for _, provider := range g.providers {
		if record, ok := g.sendWithProvider(ctx, payload, provider); ok {
			records = append(records, record)
		}
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

func (g *Gateway) sendWithProvider(ctx context.Context, payload SystemNotificationPayload, provider Provider) (storage.NotificationRecord, bool) {
	nowISO := eventlog.FormatJavaScriptISOString(g.now())
	id := eventlog.NewEventID("notification")
	channel := provider.Name()

	if payload.DedupeKey != "" && g.repositories != nil && g.repositories.Notifications != nil {
		dedupeRecord, err := g.repositories.Notifications.GetLatestByDedupe(ctx, channel, payload.DedupeKey)
		if err == nil && dedupeRecord != nil {
			createdAt, parseErr := time.Parse(time.RFC3339Nano, dedupeRecord.CreatedAt)
			if parseErr == nil {
				throttleWindow := time.Duration(g.config.Osascript.ThrottleWindowSeconds) * time.Second
				if g.now().UTC().Sub(createdAt.UTC()) < throttleWindow {
					record := buildRecord(payload, channel, id, nowISO, "skipped", "deduped")
					if err := g.persistNotification(ctx, record); err != nil {
						return storage.NotificationRecord{}, false
					}
					return record, true
				}
			}
		}
	}

	if !provider.IsAvailable() {
		record := buildRecord(payload, channel, id, nowISO, "skipped", "disabled")
		if err := g.persistNotification(ctx, record); err != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	if err := provider.Send(ctx, payload); err != nil {
		record := buildRecord(payload, channel, id, nowISO, "failed", err.Error())
		if persistErr := g.persistNotification(ctx, record); persistErr != nil {
			return storage.NotificationRecord{}, false
		}
		return record, true
	}

	record := buildRecord(payload, channel, id, nowISO, "success", "")
	record.SentAt = stringPointer(nowISO)
	if err := g.persistNotification(ctx, record); err != nil {
		return storage.NotificationRecord{}, false
	}
	return record, true
}

func buildRecord(payload SystemNotificationPayload, channel, id, nowISO, status, errorMsg string) storage.NotificationRecord {
	r := storage.NotificationRecord{
		ID:          id,
		ProjectID:   nilIfEmpty(payload.ProjectID),
		LoopID:      nilIfEmpty(payload.LoopID),
		RunID:       nilIfEmpty(payload.RunID),
		EntityType:  nilIfEmpty(payload.EntityType),
		EntityID:    nilIfEmpty(payload.EntityID),
		Channel:     channel,
		Level:       payload.Level,
		Title:       payload.Title,
		Subtitle:    nilIfEmpty(payload.Subtitle),
		Body:        payload.Body,
		Status:      status,
		DedupeKey:   nilIfEmpty(payload.DedupeKey),
		PayloadJSON: stringPointer(mustMarshalPayload(payload)),
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	if errorMsg != "" {
		r.ErrorMessage = stringPointer(errorMsg)
	}
	return r
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
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
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
