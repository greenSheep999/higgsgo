package notifier

import (
	"context"
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// stdoutNotifier logs notifications through slog. It is the zero-dependency
// default so failover / replenish always have a non-nil target even when no
// external channel is configured.
type stdoutNotifier struct {
	logger *slog.Logger
}

func newStdoutNotifier(logger *slog.Logger) *stdoutNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &stdoutNotifier{logger: logger}
}

func (s *stdoutNotifier) Name() string { return "stdout" }

func (s *stdoutNotifier) Send(_ context.Context, msg ports.Notification) error {
	attrs := []any{slog.String("title", msg.Title)}
	if msg.Body != "" {
		attrs = append(attrs, slog.String("body", msg.Body))
	}
	for k, v := range msg.Tags {
		attrs = append(attrs, slog.String(k, v))
	}
	s.logger.Log(context.Background(), slogLevel(msg.Level), "notification", attrs...)
	return nil
}

// slogLevel maps a NotificationLevel to a slog.Level. critical maps to
// Error+4 so it sorts above plain errors in level-aware backends.
func slogLevel(l ports.NotificationLevel) slog.Level {
	switch l {
	case ports.LevelInfo:
		return slog.LevelInfo
	case ports.LevelWarn:
		return slog.LevelWarn
	case ports.LevelError:
		return slog.LevelError
	case ports.LevelCritical:
		return slog.LevelError + 4
	default:
		return slog.LevelInfo
	}
}
