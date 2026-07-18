package failover

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestRecoverer_FlipsExpiredThrottled feeds the recoverer a mock store
// with one throttled row whose deadline has passed and one whose
// deadline is in the future. Only the expired one should flip back.
func TestRecoverer_FlipsExpiredThrottled(t *testing.T) {
	accts := newMockAccountStore()

	// Two throttled accounts: one already recoverable, one still cooling.
	accts.mu.Lock()
	accts.statuses["acc-expired"] = domain.StatusThrottled
	accts.throttled["acc-expired"] = time.Now().Add(-1 * time.Minute)
	accts.reasons["acc-expired"] = "throttle"
	accts.statuses["acc-cool"] = domain.StatusThrottled
	accts.throttled["acc-cool"] = time.Now().Add(1 * time.Minute)
	accts.reasons["acc-cool"] = "throttle"
	accts.mu.Unlock()

	r := &Recoverer{
		Accounts: accts,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.once(context.Background())

	if got := accts.getStatus("acc-expired"); got != domain.StatusActive {
		t.Errorf("expired throttled account: got %s want active", got)
	}
	if got := accts.getStatus("acc-cool"); got != domain.StatusThrottled {
		t.Errorf("still-cooling account: got %s want throttled", got)
	}
}

func TestRecoverer_NilSafe(t *testing.T) {
	var r *Recoverer
	// Should not panic.
	r.Run(context.Background())
	r.once(context.Background())

	r2 := &Recoverer{}
	r2.Run(context.Background()) // Accounts nil → returns immediately
	r2.once(context.Background())
}
