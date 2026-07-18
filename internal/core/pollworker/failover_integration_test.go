package pollworker

import (
	"testing"

	"github.com/greensheep999/higgsgo/internal/core/failover"
)

// TestWorker_NilFailover confirms Worker.Failover is safe to leave nil.
// The pollworker's failure / success feedback both go through
// (*failover.Controller).methods, which short-circuit on nil.
func TestWorker_NilFailover(t *testing.T) {
	var ctl *failover.Controller
	w := &Worker{Failover: ctl}
	if w.Failover != nil {
		t.Fatal("guard: Worker.Failover should be nil for this test")
	}
	// Every observer method must be a no-op on nil.
	ctl.RecordSuccess(t.Context(), "acc-1")
	ctl.RecordFailure(t.Context(), "acc-1", 401, "")
	ctl.RecordError(t.Context(), "acc-1", nil, "")
}
