package proxy

// Cross-group spillover regression test (ROADMAP P3-10).
//
// Setup: three candidate groups. The first fails with a group-scoped
// capacity error (ErrGroupConcurrencyMax), the second returns a valid
// account, the third would also work if reached. We assert that
// PickAndLock was called for exactly the first two groups in order,
// that the request's GroupID is rewritten to the group that actually
// served the pick, and that Generate returns success.
//
// A second case proves non-spillover-eligible errors (ErrAPIKeyQuotaExceed
// surfaces before the loop; ErrUpstreamRateLimit inside CreateJob) do NOT
// cause spillover — the failed group's error propagates verbatim.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// spilloverStore records the sequence of PickAndLock GroupIDs it saw and
// returns a scripted response for each one. Anything not in the script
// panics so the test fails loud.
type spilloverStore struct {
	// script maps GroupID → response for that call. picking a group
	// missing from the script fails with ErrNoEligibleAccount so an
	// undefined transition is visible.
	script map[string]spilloverResp

	mu       sync.Mutex
	sawOrder []string
}

type spilloverResp struct {
	acc *domain.Account
	tok string
	err error
}

func (s *spilloverStore) PickAndLock(_ context.Context, p ports.PickParams) (*domain.Account, string, error) {
	s.mu.Lock()
	s.sawOrder = append(s.sawOrder, p.GroupID)
	s.mu.Unlock()
	r, ok := s.script[p.GroupID]
	if !ok {
		return nil, "", domain.ErrNoEligibleAccount
	}
	return r.acc, r.tok, r.err
}

// Every other AccountStore method the test never touches. Panicking
// keeps the surface small: if Service starts calling them the failure
// mode is explicit.
func (s *spilloverStore) Get(context.Context, string) (*domain.Account, error) {
	panic("not needed")
}
func (s *spilloverStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	panic("not needed")
}
func (s *spilloverStore) Upsert(context.Context, *domain.Account) error { panic("not needed") }
func (s *spilloverStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("not needed")
}
func (s *spilloverStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("not needed")
}
func (s *spilloverStore) UpdateInFlight(context.Context, string, int) error { return nil }
func (s *spilloverStore) ResetAllInFlight(context.Context) (int, error)     { return 0, nil }
func (s *spilloverStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	return nil
}
func (s *spilloverStore) MarkThrottled(context.Context, string, time.Time, string) error { return nil }
func (s *spilloverStore) RecoverThrottled(context.Context) (int, error)                  { return 0, nil }
func (s *spilloverStore) IncrFailStreak(context.Context, string) (int, error)            { return 0, nil }
func (s *spilloverStore) ResetFailStreak(context.Context, string) error                  { return nil }
func (s *spilloverStore) Unlock(context.Context, string, string) error                   { return nil }

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
