package notify

import "context"

type Provider interface {
	Send(ctx context.Context, payload SystemNotificationPayload) error
	Name() string
	IsAvailable() bool
}
