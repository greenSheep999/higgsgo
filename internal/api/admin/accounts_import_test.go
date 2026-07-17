package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// postImport POSTs the given body to /accounts with an optional query
// suffix (e.g. "?upsert=true") and returns the recorded response. Kept
// tiny so each test case reads top-down.
func postImport(store *fakeAccountStore, body string, query string) *httptest.ResponseRecorder {
	r := newAccountsRouter(store)
	req := httptest.NewRequest(http.MethodPost, "/accounts"+query, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// decodeJSON unmarshals a recorder body into a map, failing the test on
// any parse error. Response bodies from this handler are always JSON so
// this keeps the assertion boilerplate down.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode json: %v; raw=%s", err, rec.Body.String())
	}
	return out
}

// sessionPasteBody returns a well-formed session_paste body. Individual
// fields are overridden through the mutator so each test can set up its
// own bad-field scenario.
func sessionPasteBody(mut func(m map[string]any)) string {
	m := map[string]any{
		"format":               "session_paste",
		"email":                "u@example.com",
		"user_id":              "user_abc",
		"session_id":           "sess_abc",
		"workspace_id":         "ws_1",
		"cookies_json":         map[string]any{"__session": "jwt", "datadome": "dd"},
		"user_agent":           "Mozilla/5.0",
		"x_datadome_clientid":  "dd_client",
		"plan_type":            "plus",
		"credits_balance":      1000,
		"subscription_balance": 5000,
		"total_credits":        120000,
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestImport_SessionPaste_Success(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body := sessionPasteBody(nil)

	rec := postImport(store, body, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d want 1", len(store.upsertCalls))
	}
	got := store.upsertCalls[0]
	if got.ID != "user_abc" {
		t.Errorf("Upsert.ID: got %q want user_abc", got.ID)
	}
	if got.SessionID != "sess_abc" {
		t.Errorf("Upsert.SessionID: got %q want sess_abc", got.SessionID)
	}
	if got.UserAgent != "Mozilla/5.0" {
		t.Errorf("Upsert.UserAgent: got %q", got.UserAgent)
	}
	if got.DataDomeClientID != "dd_client" {
		t.Errorf("Upsert.DataDomeClientID: got %q", got.DataDomeClientID)
	}
	if got.PlanType != domain.PlanPlus {
		t.Errorf("Upsert.PlanType: got %q want plus", got.PlanType)
	}
	if got.SubscriptionBalance != 5000 {
		t.Errorf("Upsert.SubscriptionBalance: got %d want 5000", got.SubscriptionBalance)
	}
	if got.CookiesJSON == "" {
		t.Errorf("Upsert.CookiesJSON is empty")
	}
	if got.Status != domain.StatusActive {
		t.Errorf("Upsert.Status: got %q want active", got.Status)
	}

	resp := decodeJSON(t, rec)
	if resp["id"] != "user_abc" {
		t.Errorf("response.id: got %v want user_abc", resp["id"])
	}
	if resp["email"] != "u@example.com" {
		t.Errorf("response.email: got %v", resp["email"])
	}
	if resp["plan_type"] != "plus" {
		t.Errorf("response.plan_type: got %v want plus", resp["plan_type"])
	}
	if _, ok := resp["imported_at"].(string); !ok {
		t.Errorf("response.imported_at: got %v (want RFC3339 string)", resp["imported_at"])
	}
}

func TestImport_SessionPaste_MissingSessionID(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body := sessionPasteBody(func(m map[string]any) { delete(m, "session_id") })

	rec := postImport(store, body, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 0 {
		t.Errorf("Upsert must not be called when session_id is missing; got %d calls", len(store.upsertCalls))
	}
	resp := decodeJSON(t, rec)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object missing: %v", resp)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "session_id") {
		t.Errorf("error message should mention session_id; got %q", msg)
	}
}

func TestImport_SessionPaste_MissingCookies(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body := sessionPasteBody(func(m map[string]any) { delete(m, "cookies_json") })

	rec := postImport(store, body, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 0 {
		t.Errorf("Upsert must not be called; got %d", len(store.upsertCalls))
	}
}

// higgsfieldRawObject returns a minimal but complete higgsfield-register
// JSON payload as an object. Individual fields are overridable so the
// same helper drives both the "object" and "string" cases.
func higgsfieldRawObject() map[string]any {
	return map[string]any{
		"type":                "higgsfield",
		"email":               "hf@example.com",
		"password":            "pw",
		"user_id":             "user_hf",
		"session_id":          "sess_hf",
		"plan_type":           "pro",
		"cookies":             map[string]string{"__session": "jwt", "datadome": "dd"},
		"x_datadome_clientid": "dd_client",
		"captured_user_agent": "Mozilla/5.0 (X11)",
		"imported_at":         "2026-07-10T12:00:00Z",
		"credits_snapshot": map[string]any{
			"subscription_credits": 100.0,
			"package_credits":      5.0,
			"total_plan_credits":   1200.0,
			"captured_at":          "2026-07-10T12:00:00Z",
		},
	}
}

func TestImport_HiggsfieldRegisterJSON_ObjectPayload(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body, _ := json.Marshal(map[string]any{
		"format": "higgsfield_register_json",
		"raw":    higgsfieldRawObject(),
	})

	rec := postImport(store, string(body), "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d want 1", len(store.upsertCalls))
	}
	got := store.upsertCalls[0]
	if got.ID != "user_hf" {
		t.Errorf("Upsert.ID: got %q want user_hf", got.ID)
	}
	if got.SessionID != "sess_hf" {
		t.Errorf("Upsert.SessionID: got %q", got.SessionID)
	}
	if got.PlanType != domain.PlanPro {
		t.Errorf("Upsert.PlanType: got %q want pro", got.PlanType)
	}
	if got.SubscriptionBalance != 10000 {
		t.Errorf("Upsert.SubscriptionBalance: got %d want 10000 (100 * 100)", got.SubscriptionBalance)
	}
	if got.TotalPlanCredits != 120000 {
		t.Errorf("Upsert.TotalPlanCredits: got %d want 120000 (1200 * 100)", got.TotalPlanCredits)
	}
}

func TestImport_HiggsfieldRegisterJSON_StringPayload(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	rawBytes, _ := json.Marshal(higgsfieldRawObject())
	// Wrap the pre-encoded JSON as a JSON string.
	body, _ := json.Marshal(map[string]any{
		"format": "higgsfield_register_json",
		"raw":    string(rawBytes),
	})

	rec := postImport(store, string(body), "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d want 1", len(store.upsertCalls))
	}
	if store.upsertCalls[0].ID != "user_hf" {
		t.Errorf("Upsert.ID: got %q want user_hf", store.upsertCalls[0].ID)
	}
}

func TestImport_RawCookies_ParsedSession(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	// Include the DevTools "Cookie: " prefix to cover the strip path,
	// plus clerk_active_context that leads with a sess_ id.
	header := "Cookie: __client=eyJhbGc; clerk_active_context=sess_2XYZabc:org_1; datadome=dd_value; other=misc"
	body, _ := json.Marshal(map[string]any{
		"format":              "raw_cookies",
		"email":               "raw@example.com",
		"cookies_header":      header,
		"x_datadome_clientid": "dd_client_override",
		"user_agent":          "Mozilla/5.0",
	})

	rec := postImport(store, string(body), "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d want 1", len(store.upsertCalls))
	}
	got := store.upsertCalls[0]
	if got.SessionID != "sess_2XYZabc" {
		t.Errorf("SessionID from cookies: got %q want sess_2XYZabc", got.SessionID)
	}
	if got.Email != "raw@example.com" {
		t.Errorf("Email: got %q", got.Email)
	}
	if got.DataDomeClientID != "dd_client_override" {
		t.Errorf("DataDomeClientID should honor explicit override: got %q", got.DataDomeClientID)
	}
	// Cookies map serialized as JSON. Verify a couple of representative keys
	// survive the round-trip.
	var parsed map[string]string
	if err := json.Unmarshal([]byte(got.CookiesJSON), &parsed); err != nil {
		t.Fatalf("cookies_json decode: %v", err)
	}
	if parsed["__client"] == "" {
		t.Errorf("__client cookie missing from stored CookiesJSON: %v", parsed)
	}
	if parsed["datadome"] != "dd_value" {
		t.Errorf("datadome cookie: got %q want dd_value", parsed["datadome"])
	}
}

func TestImport_RawCookies_DataDomeFallback(t *testing.T) {
	// When x_datadome_clientid is absent, the handler should fall back to
	// the datadome cookie value so the pool has a usable client id.
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	header := "clerk_active_context=sess_XX:; datadome=dd_from_cookie"
	body, _ := json.Marshal(map[string]any{
		"format":         "raw_cookies",
		"email":          "raw@example.com",
		"cookies_header": header,
	})

	rec := postImport(store, string(body), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d", len(store.upsertCalls))
	}
	if got := store.upsertCalls[0].DataDomeClientID; got != "dd_from_cookie" {
		t.Errorf("DataDomeClientID fallback: got %q want dd_from_cookie", got)
	}
}

func TestImport_RawCookies_MissingSession(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	// No sess_ substring anywhere in the header.
	header := "datadome=dd_value; foo=bar; baz=quux"
	body, _ := json.Marshal(map[string]any{
		"format":         "raw_cookies",
		"email":          "raw@example.com",
		"cookies_header": header,
	})

	rec := postImport(store, string(body), "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 0 {
		t.Errorf("Upsert must not fire when session_id cannot be extracted; got %d calls", len(store.upsertCalls))
	}
	resp := decodeJSON(t, rec)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object missing: %v", resp)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "session") {
		t.Errorf("error message should mention session; got %q", msg)
	}
}

func TestImport_UnknownFormat(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body := `{"format":"weird","email":"u@example.com"}`

	rec := postImport(store, body, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 0 {
		t.Errorf("Upsert must not fire for unknown format; got %d", len(store.upsertCalls))
	}
	resp := decodeJSON(t, rec)
	errObj, _ := resp["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "weird") {
		t.Errorf("error message should quote the bad format value; got %q", msg)
	}
}

func TestImport_MissingFormat(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	body := `{"email":"u@example.com"}`
	rec := postImport(store, body, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImport_Conflict(t *testing.T) {
	// Existing account with the same ID -> 409, no Upsert.
	existing := &domain.Account{ID: "user_abc", Email: "old@example.com"}
	store := &fakeAccountStore{getResult: existing}

	body := sessionPasteBody(nil)
	rec := postImport(store, body, "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 0 {
		t.Errorf("Upsert must not fire on conflict; got %d calls", len(store.upsertCalls))
	}
	resp := decodeJSON(t, rec)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("error object missing: %v", resp)
	}
	if errObj["type"] != "already_exists" {
		t.Errorf("error.type: got %v want already_exists", errObj["type"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "user_abc") {
		t.Errorf("error message should include the id; got %q", msg)
	}
}

func TestImport_UpsertMode(t *testing.T) {
	// Even when Get would find an existing row, ?upsert=true skips the
	// pre-check and calls Upsert directly. We simulate this by making
	// Get return a hit and asserting the handler never inspects it.
	existing := &domain.Account{ID: "user_abc"}
	store := &fakeAccountStore{getResult: existing}

	body := sessionPasteBody(nil)
	rec := postImport(store, body, "?upsert=true")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.upsertCalls) != 1 {
		t.Fatalf("Upsert calls: got %d want 1", len(store.upsertCalls))
	}
	if store.lastGetID != "" {
		t.Errorf("upsert=true must skip the Get pre-check; lastGetID=%q", store.lastGetID)
	}
}

func TestImport_UpsertError(t *testing.T) {
	// Upsert failing surfaces a 500 with the internal error message.
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound, upsertErr: errors.New("db down")}
	body := sessionPasteBody(nil)

	rec := postImport(store, body, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImport_EmptyBody(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	rec := postImport(store, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImport_InvalidJSON(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	rec := postImport(store, "{not-json", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// Sanity guard: streaming path over a very large body still returns a
// bounded error rather than blowing memory. The 1 MiB reader cap should
// short-circuit before decode.
func TestImport_LargeBodyBounded(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	// Craft a body of ~2 MiB of whitespace inside a valid JSON envelope
	// so the io.LimitReader truncates and json.Unmarshal fails cleanly.
	pad := bytes.Repeat([]byte(" "), 2<<20)
	body := fmt.Sprintf(`{"format":"session_paste","email":"u@example.com"%s}`, pad)

	r := newAccountsRouter(store)
	req := httptest.NewRequest(http.MethodPost, "/accounts", io.NopCloser(bytes.NewReader([]byte(body))))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}
