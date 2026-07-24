package notifier

import (
	"context"
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// Chain fans a Notification out to several notifiers, dropping any message
// below minLevel. A single destination failing never blocks the others —
// alerts are best-effort — but the first error is returned so callers can
// log it. Chain itself satisfies ports.Notifier.
type Chain struct {
	notifiers []ports.Notifier
	minLevel  ports.NotificationLevel
	logger    *slog.Logger
}

func (c *Chain) Name() string { return "chain" }

func (c *Chain) Send(ctx context.Context, msg ports.Notification) error {
	if levelRank(msg.Level) < levelRank(c.minLevel) {
		return nil
	}
	var firstErr error
	for _, n := range c.notifiers {
		if err := n.Send(ctx, msg); err != nil {
			if c.logger != nil {
				c.logger.Warn("notifier send failed",
					slog.String("notifier", n.Name()),
					slog.String("err", err.Error()))
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// levelRank orders severities so minLevel filtering is a simple compare.
func levelRank(l ports.NotificationLevel) int {
	switch l {
	case ports.LevelInfo:
		return 0
	case ports.LevelWarn:
		return 1
	case ports.LevelError:
		return 2
	case ports.LevelCritical:
		return 3
	default:
		return 0 // unset level treated as info so nothing is silently dropped
	}
}
