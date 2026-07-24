package upstream

// Tests for FetchCreditLedgerStatistics — the /workspaces/credit-ledger/
// statistics GET used by the monthly reconciler (core/creditrecon). Covers
// the happy path (populated aggregate), the empty {} response (must return
// a zero-value struct, not nil), and query-param wiring.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_FetchCreditLedgerStatistics_Populated(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/credit-ledger/statistics" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`{
			"total_credits_spent": 1234,
			"total_credits_refunded": 50,
			"jobs_created": 42,
			"currency": "USD",
			"spending_by_model": {"veo3": 500}
		}`))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchCreditLedgerStatistics(
		context.Background(), testAccount(), "2026-06-01", "2026-06-30")
	if err != nil {
		t.Fatalf("FetchCreditLedgerStatistics: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil result")
	}
	if got.TotalCreditsSpent != 1234 || got.TotalCreditsRefunded != 50 {
		t.Errorf("credit totals wrong: %+v", got)
	}
	if got.JobsCreated != 42 || got.Currency != "USD" {
		t.Errorf("jobs/currency wrong: %+v", got)
	}
	// Wire-check on the query string so a refactor that forgets one of
	// the params fails a test rather than silently misreporting.
	if !containsAll(gotURL, "start_date=2026-06-01", "end_date=2026-06-30") {
		t.Errorf("query string missing date params: %s", gotURL)
	}
}

func TestClient_FetchCreditLedgerStatistics_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchCreditLedgerStatistics(
		context.Background(), testAccount(), "2026-06-01", "2026-06-30")
	if err != nil {
		t.Fatalf("FetchCreditLedgerStatistics: %v", err)
	}
	// Zero-value struct, not nil — callers rely on being able to read
	// TotalCreditsSpent without a nil check.
	if got == nil {
		t.Fatal("expected zero-value struct on empty body, got nil")
	}
	if got.TotalCreditsSpent != 0 {
		t.Errorf("expected zero totals, got %+v", got)
	}
}

func TestClient_FetchCreditLedgerStatistics_TrulyEmpty(t *testing.T) {
	// Server returns a truly empty body (no JSON at all). Client must
	// still return a zero-value struct rather than a JSON parse error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write nothing.
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchCreditLedgerStatistics(
		context.Background(), testAccount(), "2026-06-01", "2026-06-30")
	if err != nil {
		t.Fatalf("FetchCreditLedgerStatistics: %v", err)
	}
	if got == nil {
		t.Fatal("expected zero-value struct on empty body, got nil")
	}
	if got.TotalCreditsSpent != 0 {
		t.Errorf("expected zero totals, got %+v", got)
	}
}

// containsAll returns true iff every needle is a substring of hay.
func containsAll(hay string, needles ...string) bool {
	for _, n := range needles {
		if !contains(hay, n) {
			return false
		}
	}
	return true
}

// contains is a tiny substring helper so this test file needs no extra imports.
func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
