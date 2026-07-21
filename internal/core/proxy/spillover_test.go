package proxy

// Cross-group spillover unit coverage for the ROADMAP P3-10 predicate.
//
// A previous iteration of this file also carried a large `spilloverStore`
// fake with 15 stub methods that was never wired into a full-Service
// integration test; staticcheck (U1000) surfaced it as dead weight. The
// only assertion that actually ran was TestIsSpilloverEligible, so we
// keep that and drop the rest — reintroduce the fake if / when a real
// end-to-end spillover test lands.

import (
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestIsSpilloverEligible walks the error list explicitly so future
// domain additions have to make an explicit choice about whether they
// trigger spillover. Cheap change-detector on isSpilloverEligible.
func TestIsSpilloverEligible(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"group concurrency max eligible", domain.ErrGroupConcurrencyMax, true},
		{"group quota exhausted eligible", domain.ErrGroupQuotaExhausted, true},
		{"no eligible account eligible", domain.ErrNoEligibleAccount, true},
		{"model blocked eligible", domain.ErrModelBlocked, true},
		{"model not allowed eligible", domain.ErrModelNotAllowed, true},
		{"api key quota NOT eligible", domain.ErrAPIKeyQuotaExceed, false},
		{"upstream rate limit NOT eligible", domain.ErrUpstreamRateLimit, false},
		{"random unknown error NOT eligible", errors.New("boom"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isSpilloverEligible(tc.err); got != tc.want {
				t.Errorf("isSpilloverEligible(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
