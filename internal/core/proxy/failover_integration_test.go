package proxy

import (
	"testing"

	"github.com/greensheep999/higgsgo/internal/core/failover"
)

// TestServiceStruct_NilFailover confirms the Service's Failover field
// tolerates a nil pointer literal without panics — the proxy Generate
// path is fed nil in tests that don't wire the controller.
func TestServiceStruct_NilFailover(t *testing.T) {
	var ctl *failover.Controller
	svc := &Service{Failover: ctl}
	// Every observer method must be a no-op on nil.
	svc.Failover.RecordSuccess(t.Context(), "acc-1")
	svc.Failover.RecordFailure(t.Context(), "acc-1", 401, "boom")
	svc.Failover.RecordError(t.Context(), "acc-1", nil, "")
}
