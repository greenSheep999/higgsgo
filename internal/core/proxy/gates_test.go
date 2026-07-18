package proxy

import (
	"errors"
	"regexp"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestEnforceGroupGates_MatrixOfDenyAndAllow walks the four cases the
// gate covers (blocked, not-allowed, budget-exhausted, all-pass) plus
// nil-safety on empty policy. Each case names the expected sentinel so
// a failure output reads like a single-line spec.
func TestEnforceGroupGates_MatrixOfDenyAndAllow(t *testing.T) {
	s := &Service{}

	cases := []struct {
		name    string
		policy  groupPolicy
		alias   string
		estCost int64
		wantErr error
	}{
		{
			name:    "empty policy is nil-safe and allows",
			policy:  groupPolicy{},
			alias:   "seedance-2-0-mini",
			estCost: 1000,
			wantErr: nil,
		},
		{
			name: "blocked regex match returns ErrModelBlocked",
			policy: groupPolicy{
				BlockedModels: regexp.MustCompile(`^wan_.*`),
			},
			alias:   "wan_animate",
			estCost: 1000,
			wantErr: domain.ErrModelBlocked,
		},
		{
			name: "allowed regex with no match returns ErrModelNotAllowed",
			policy: groupPolicy{
				AllowedModels: regexp.MustCompile(`^seedance-.*`),
			},
			alias:   "wan_animate",
			estCost: 1000,
			wantErr: domain.ErrModelNotAllowed,
		},
		{
			name: "allowed regex with match passes through",
			policy: groupPolicy{
				AllowedModels: regexp.MustCompile(`^seedance-.*`),
			},
			alias:   "seedance-2-0-mini",
			estCost: 1000,
			wantErr: nil,
		},
		{
			name: "blocked wins over allowed when both match",
			policy: groupPolicy{
				AllowedModels: regexp.MustCompile(`.*`),
				BlockedModels: regexp.MustCompile(`^wan_.*`),
			},
			alias:   "wan_animate",
			estCost: 1000,
			wantErr: domain.ErrModelBlocked,
		},
		{
			name: "budget with headroom passes",
			policy: groupPolicy{
				MonthlyCreditBudget: 100000,
				MonthlyCreditUsed:   50000,
			},
			alias:   "seedance-2-0-mini",
			estCost: 40000,
			wantErr: nil,
		},
		{
			name: "budget exactly at limit passes",
			policy: groupPolicy{
				MonthlyCreditBudget: 100000,
				MonthlyCreditUsed:   50000,
			},
			alias:   "seedance-2-0-mini",
			estCost: 50000,
			wantErr: nil,
		},
		{
			name: "budget overrun returns ErrGroupQuotaExhausted",
			policy: groupPolicy{
				MonthlyCreditBudget: 100000,
				MonthlyCreditUsed:   90000,
			},
			alias:   "seedance-2-0-mini",
			estCost: 20000,
			wantErr: domain.ErrGroupQuotaExhausted,
		},
		{
			name: "zero budget disables the gate entirely",
			policy: groupPolicy{
				MonthlyCreditBudget: 0,
				MonthlyCreditUsed:   999999,
			},
			alias:   "seedance-2-0-mini",
			estCost: 999999,
			wantErr: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := s.enforceGroupGates(tc.policy, tc.alias, tc.estCost)
			if !errors.Is(got, tc.wantErr) {
				t.Fatalf("enforceGroupGates: got %v want %v", got, tc.wantErr)
			}
		})
	}
}

// TestEnforceKeyGates_QuotaMatrix covers the per-Key quota gate
// (ROADMAP P2-9): zero-quota = unlimited (no-op), remaining headroom
// passes, exactly-at-limit passes, overrun returns
// domain.ErrAPIKeyQuotaExceed. Matches the semantics of
// enforceGroupGates so both gates read the same way from a caller's
// perspective.
func TestEnforceKeyGates_QuotaMatrix(t *testing.T) {
	cases := []struct {
		name    string
		req     GenerationRequest
		estCost int64
		wantErr error
	}{
		{
			name:    "zero quota is unlimited",
			req:     GenerationRequest{APIKeyMonthlyQuota: 0, APIKeyMonthlyUsed: 999999},
			estCost: 999999,
			wantErr: nil,
		},
		{
			name:    "negative quota also treated as unlimited (defensive)",
			req:     GenerationRequest{APIKeyMonthlyQuota: -1, APIKeyMonthlyUsed: 999999},
			estCost: 999999,
			wantErr: nil,
		},
		{
			name:    "well under limit passes",
			req:     GenerationRequest{APIKeyMonthlyQuota: 100000, APIKeyMonthlyUsed: 20000},
			estCost: 30000,
			wantErr: nil,
		},
		{
			name:    "exactly at limit passes",
			req:     GenerationRequest{APIKeyMonthlyQuota: 100000, APIKeyMonthlyUsed: 50000},
			estCost: 50000,
			wantErr: nil,
		},
		{
			name:    "overrun by one credit rejected",
			req:     GenerationRequest{APIKeyMonthlyQuota: 100000, APIKeyMonthlyUsed: 50000},
			estCost: 50001,
			wantErr: domain.ErrAPIKeyQuotaExceed,
		},
		{
			name:    "already over limit rejected",
			req:     GenerationRequest{APIKeyMonthlyQuota: 100000, APIKeyMonthlyUsed: 100000},
			estCost: 1,
			wantErr: domain.ErrAPIKeyQuotaExceed,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := enforceKeyGates(tc.req, tc.estCost)
			if !errors.Is(got, tc.wantErr) {
				t.Fatalf("enforceKeyGates: got %v want %v", got, tc.wantErr)
			}
		})
	}
}

// TestCompileGroupRegex_InvalidPatternDegrades verifies the resolver's
// tolerance behavior: a malformed pattern on the group row must not
// crash the pick path. Returns nil (== "no filter"). Ensures that a
// misconfigured group only fails-open at the regex layer, not the
// budget or concurrency layers.
func TestCompileGroupRegex_InvalidPatternDegrades(t *testing.T) {
	got := compileGroupRegex("(unclosed", "grp_bad", "allowed", nil)
	if got != nil {
		t.Errorf("invalid pattern should return nil regex; got %v", got)
	}
	// Valid pattern still compiles.
	good := compileGroupRegex(`^allowed-.*$`, "grp_ok", "allowed", nil)
	if good == nil {
		t.Errorf("valid pattern should compile")
	}
	if !good.MatchString("allowed-thing") {
		t.Errorf("compiled regex should match")
	}
}
