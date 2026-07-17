package admin

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// exportSampleAccounts returns three accounts with all sensitive fields
// populated. Tests use this to verify that the no-secrets export scrubs
// session_id / cookies_json / user_agent / datadome and the secrets-on
// export includes them verbatim.
func exportSampleAccounts() []domain.Account {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	return []domain.Account{
		{
			ID:                  "user_1",
			Email:               "a@example.com",
			Password:            "pw-1",
			SessionID:           "sess_aaa",
			CookiesJSON:         `{"__session":"jwt-aaa"}`,
			UserAgent:           "Mozilla/5.0 (aaa)",
			DataDomeClientID:    "dd_aaa",
			WorkspaceID:         "ws_1",
			PlanType:            domain.PlanPlus,
			SubscriptionBalance: 12345,
			CreditsBalance:      1000,
			TotalPlanCredits:    50000,
			Status:              domain.StatusActive,
			RegisteredAt:        base,
		},
		{
			ID:                  "user_2",
			Email:               "b@example.com",
			Password:            "pw-2",
			SessionID:           "sess_bbb",
			CookiesJSON:         `{"__session":"jwt-bbb"}`,
			UserAgent:           "Mozilla/5.0 (bbb)",
			DataDomeClientID:    "dd_bbb",
			WorkspaceID:         "ws_2",
			PlanType:            domain.PlanPro,
			SubscriptionBalance: 6789,
			Status:              domain.StatusActive,
			RegisteredAt:        base.Add(time.Hour),
		},
		{
			ID:                  "user_3",
			Email:               "c@example.com",
			Password:            "pw-3",
			SessionID:           "sess_ccc",
			CookiesJSON:         `{"__session":"jwt-ccc"}`,
			UserAgent:           "Mozilla/5.0 (ccc)",
			DataDomeClientID:    "dd_ccc",
			WorkspaceID:         "ws_3",
			PlanType:            domain.PlanUltra,
			SubscriptionBalance: 100000,
			Status:              domain.StatusActive,
			RegisteredAt:        base.Add(2 * time.Hour),
		},
	}
}

// splitAccountJSONLines returns the non-empty lines of a jsonl body. Kept
// local so this test file stays independent of audit_test.go's helper.
func splitAccountJSONLines(t *testing.T, body string) []string {
	t.Helper()
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		t.Fatalf("scan jsonl: %v", err)
	}
	return lines
}

func TestExport_JSONL_NoSecrets(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/x-ndjson"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=accounts-") {
		t.Errorf("Content-Disposition: got %q want attachment prefix", cd)
	}
	if !strings.HasSuffix(cd, ".jsonl") {
		t.Errorf("Content-Disposition: got %q want .jsonl suffix", cd)
	}

	lines := splitAccountJSONLines(t, rec.Body.String())
	if got, want := len(lines), 3; got != want {
		t.Fatalf("line count: got %d want %d; body=%q", got, want, rec.Body.String())
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d decode: %v; raw=%q", i, err, line)
		}
		assertNoSensitiveFields(t, row)
	}
}

func TestExport_JSONL_WithSecrets(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export?include_secrets=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	lines := splitAccountJSONLines(t, rec.Body.String())
	if got, want := len(lines), 3; got != want {
		t.Fatalf("line count: got %d want %d; body=%q", got, want, rec.Body.String())
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d decode: %v; raw=%q", i, err, line)
		}
		for _, key := range []string{"session_id", "user_agent", "x_datadome_clientid", "cookies_json"} {
			if _, present := row[key]; !present {
				t.Errorf("line %d missing key %q (include_secrets=true): %v", i, key, row)
			}
		}
	}
	// First row's session id must match the fake source of truth.
	var first map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	if got, want := first["session_id"], "sess_aaa"; got != want {
		t.Errorf("row 0 session_id: got %v want %q", got, want)
	}
}

func TestExport_CSV_Header(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export?format=csv", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "text/csv; charset=utf-8"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasSuffix(got, ".csv") {
		t.Errorf("Content-Disposition: got %q want .csv suffix", got)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv decode: %v; body=%q", err, rec.Body.String())
	}
	if got, want := len(records), 4; got != want { // header + 3 rows
		t.Fatalf("csv rows: got %d want %d; records=%v", got, want, records)
	}
	// The first CSV row must be the header. Verify a stable subset so
	// re-ordering the doc'd column list breaks this test loudly.
	if got, want := records[0][0], "id"; got != want {
		t.Errorf("header col 0: got %q want %q", got, want)
	}
	if got, want := records[0][1], "email"; got != want {
		t.Errorf("header col 1: got %q want %q", got, want)
	}
	if got, want := records[0][len(records[0])-1], "imported_at"; got != want {
		t.Errorf("header last col: got %q want %q", got, want)
	}
	// Body rows must not carry a "session_id" cell (the no-secrets header
	// omits it entirely). Search the header for the key defensively so a
	// column re-order still catches leaks.
	for i, col := range records[0] {
		if col == "session_id" || col == "cookies_json" ||
			col == "user_agent" || col == "x_datadome_clientid" {
			t.Errorf("no-secrets CSV must not carry column %q at index %d", col, i)
		}
	}
}

func TestExport_JSON_Array(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export?format=json", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasSuffix(got, ".json") {
		t.Errorf("Content-Disposition: got %q want .json suffix", got)
	}

	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("body is not a JSON array: %v; body=%s", err, rec.Body.String())
	}
	if got, want := len(arr), 3; got != want {
		t.Fatalf("array len: got %d want %d", got, want)
	}
	// No-secrets by default.
	for i, row := range arr {
		assertNoSensitiveFields(t, row)
		if row["id"] == "" {
			t.Errorf("row %d missing id", i)
		}
	}
}

func TestExport_UnknownFormat(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export?format=xml", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.listCallCount != 0 {
		t.Errorf("List must not be called on unknown format; got calls=%d", store.listCallCount)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "invalid_query" {
		t.Errorf("error.type: got %v want invalid_query", errObj["type"])
	}
}

func TestExport_Filter(t *testing.T) {
	store := &fakeAccountStore{listRows: exportSampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/export?plan_type=plus", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.listCallCount != 1 {
		t.Fatalf("List calls: got %d want 1", store.listCallCount)
	}
	if got, want := store.lastFilter.PlanType, domain.PlanType("plus"); got != want {
		t.Errorf("filter.PlanType: got %q want %q", got, want)
	}
}
