package notifier

import (
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// New builds a Chain notifier from the config slice. Each entry becomes a
// concrete notifier keyed by Type; unknown or unconfigured types are
// skipped with a warning. When the resulting chain would be empty (no
// config, or every entry invalid) a stdout notifier is used so downstream
// consumers (failover, replenish) always have a non-nil target.
//
// MinLevel is taken from the first entry that sets one; a chain-wide
// filter is simpler than per-notifier levels and matches how the single
// [notifiers] block is used in practice.
func New(cfgs []config.NotifierConfig, logger *slog.Logger) *Chain {
	if logger == nil {
		logger = slog.Default()
	}
	chain := &Chain{minLevel: ports.LevelInfo, logger: logger}
	levelSet := false
	for _, c := range cfgs {
		if !levelSet && c.MinLevel != "" {
			chain.minLevel = ports.NotificationLevel(c.MinLevel)
			levelSet = true
		}
		switch c.Type {
		case "slack":
			if c.Slack.Webhook == "" {
				logger.Warn("notifier slack configured without webhook; skipping")
				continue
			}
			chain.notifiers = append(chain.notifiers, newSlackNotifier(c.Slack.Webhook))
		case "stdout", "":
			chain.notifiers = append(chain.notifiers, newStdoutNotifier(logger))
		default:
			logger.Warn("unknown notifier type; skipping", slog.String("type", c.Type))
		}
	}
	if len(chain.notifiers) == 0 {
		chain.notifiers = append(chain.notifiers, newStdoutNotifier(logger))
	}
	return chain
}
