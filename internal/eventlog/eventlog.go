package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

type AppendInput struct {
	ID               string
	EventType        string
	ProjectID        *string
	LoopID           *string
	RunID            *string
	EntityType       *string
	EntityID         *string
	CorrelationID    *string
	CausationID      *string
	ActorType        *string
	ActorID          *string
	ActorDisplayName *string
	Payload          any
	PayloadJSON      *string
	CreatedAt        time.Time
}

func FormatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

func NewEventID(prefix string) string {
	if prefix == "" {
		prefix = "event"
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(raw)
}

func Append(ctx context.Context, repositories *storage.Repositories, input AppendInput) error {
	if repositories == nil || repositories.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}

	payloadJSON := "{}"
	if input.PayloadJSON != nil {
		payloadJSON = *input.PayloadJSON
	} else if input.Payload != nil {
		encoded, err := json.Marshal(input.Payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		payloadJSON = string(encoded)
	}

	id := input.ID
	if id == "" {
		id = NewEventID("event")
	}

	createdAt := input.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	actorType := input.ActorType
	actorID := input.ActorID
	actorDisplayName := input.ActorDisplayName
	if actorType == nil {
		actorType = stringPointer("system")
	}
	if actorID == nil {
		actorID = stringPointer("looperd")
	}
	if actorDisplayName == nil {
		actorDisplayName = stringPointer("looperd")
	}

	return repositories.Events.Append(ctx, storage.EventLogRecord{
		ID:               id,
		EventType:        input.EventType,
		ProjectID:        input.ProjectID,
		LoopID:           input.LoopID,
		RunID:            input.RunID,
		EntityType:       input.EntityType,
		EntityID:         input.EntityID,
		CorrelationID:    input.CorrelationID,
		CausationID:      input.CausationID,
		ActorType:        actorType,
		ActorID:          actorID,
		ActorDisplayName: actorDisplayName,
		PayloadJSON:      payloadJSON,
		CreatedAt:        FormatJavaScriptISOString(createdAt),
	})
}

func stringPointer(value string) *string {
	return &value
}
