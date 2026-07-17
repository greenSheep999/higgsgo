package ports

import "context"

// NotificationLevel classifies the severity of an outbound notification.
type NotificationLevel string

const (
	LevelInfo     NotificationLevel = "info"
	LevelWarn     NotificationLevel = "warn"
	LevelError    NotificationLevel = "error"
	LevelCritical NotificationLevel = "critical"
)

// Notifier delivers operational events to a human channel (Slack, Telegram,
// email, generic webhook). A Chain adapter fans out to multiple destinations.
type Notifier interface {
	Send(ctx context.Context, msg Notification) error
	Name() string
}

// Notification is a single message to deliver.
type Notification struct {
	Level NotificationLevel
	Title string
	Body  string
	Tags  map[string]string // e.g., {"account_id": "user_xxx", "model": "seedance-2"}
}
