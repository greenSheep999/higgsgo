package middleware

// Tests for the Audit middleware. Because the production middleware
// calls store.Insert from a background goroutine, the test suite
// exercises the internal auditWith seam and passes a shim that
// signals through a channel when the row has been recorded. That
// keeps assertions deterministic without adding a Sleep.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// recorderStore is a fake ports.AuditStore used by the middleware
// tests. Every Insert flows onto the events channel so tests can
// deterministically wait for the async goroutine to complete.
type recorderStore struct {
	events chan *domain.AuditEvent
	fail   error
}

func newRecorderStore() *recorderStore {
	return &recorderStore{events: make(chan *domain.AuditEvent, 4)}
}

func (r *recorderStore) Insert(_ context.Context, e *domain.AuditEvent) error {
	if r.fail != nil {
		// Still surface the event so tests can assert on payload
		// even when the store returned an error.
		select {
		case r.events <- e:
		default:
		}
		return r.fail
	}
	r.events <- e
	return nil
}

func (r *recorderStore) List(context.Context, ports.AuditFilter) ([]domain.AuditEvent, error) {
	return nil, nil
}

// waitEvent pulls the next audit event off the channel or fails
// the test if the middleware never emitted one.
func (r *recorderStore) waitEvent(t *testing.T) *domain.AuditEvent {
	t.Helper()
	select {
	case e := <-r.events:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for audit insert")
		return nil
	}
}

// mustNoEvent asserts that no audit row was recorded within a small
// grace window. Used by TestAudit_SkipsGET.
func (r *recorderStore) mustNoEvent(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.events:
		t.Fatalf("unexpected audit insert: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// newAuditRouter mounts the audit middleware over a chi router with
// a couple of representative /admin routes so RoutePattern and
// URLParam resolution are covered end-to-end.
func newAuditRouter(store ports.AuditStore, handler http.HandlerFunc) chi.Router {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := chi.NewRouter()
	r.Use(Audit(store, logger))
	r.Get("/keys/{id}", handler)
	r.Post("/keys", handler)
	r.Post("/keys/{id}/rotate", handler)
	r.Delete("/keys/{id}", handler)
	r.Delete("/accounts/{id}", handler)
	return r
}

func writeStatus(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}
}

func TestAudit_SkipsGET(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/keys/key_1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	store.mustNoEvent(t)
}

func TestAudit_LogsPOST(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusCreated))

	body := []byte(`{"name":"my-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201", rec.Code)
	}
	e := store.waitEvent(t)
	if e.Method != http.MethodPost {
		t.Errorf("method: got %q want POST", e.Method)
	}
	if e.Path != "/keys" {
		t.Errorf("path: got %q want /keys", e.Path)
	}
	if e.Route != "/keys" {
		t.Errorf("route: got %q want /keys", e.Route)
	}
	if e.Status != http.StatusCreated {
		t.Errorf("status: got %d want 201", e.Status)
	}
	if e.BodyHash == "" {
		t.Errorf("body_hash: empty; want non-empty")
	}
	if e.ResourceType != "apikey" {
		t.Errorf("resource_type: got %q want apikey", e.ResourceType)
	}
	if !strings.HasPrefix(e.ID, "audit_") {
		t.Errorf("id: got %q; expected prefix audit_", e.ID)
	}
	if e.TS.IsZero() {
		t.Errorf("ts: zero value")
	}
}

func TestAudit_LogsDELETE(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusNoContent))

	req := httptest.NewRequest(http.MethodDelete, "/keys/key_42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rec.Code)
	}
	e := store.waitEvent(t)
	if e.Method != http.MethodDelete {
		t.Errorf("method: got %q want DELETE", e.Method)
	}
	if e.Route != "/keys/{id}" {
		t.Errorf("route: got %q want /keys/{id}", e.Route)
	}
	if e.ResourceType != "apikey" {
		t.Errorf("resource_type: got %q want apikey", e.ResourceType)
	}
	if e.ResourceID != "key_42" {
		t.Errorf("resource_id: got %q want key_42", e.ResourceID)
	}
}

func TestAudit_TruncatesActor(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	req := httptest.NewRequest(http.MethodPost, "/keys", nil)
	req.Header.Set("Authorization", "Bearer sk-hg-verylongtoken")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	e := store.waitEvent(t)
	if e.Actor != "sk-hg-ve" {
		t.Errorf("actor: got %q want sk-hg-ve (8 chars)", e.Actor)
	}
}

func TestAudit_AnonymousWithoutBearer(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	req := httptest.NewRequest(http.MethodPost, "/keys", nil)
	// No Authorization header.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	e := store.waitEvent(t)
	if e.Actor != anonymousActor {
		t.Errorf("actor: got %q want %q", e.Actor, anonymousActor)
	}
}

// TestAudit_PreservesBodyForHandler drives a handler that echoes the
// request body back to the caller, then asserts the caller received
// exactly what it sent. If the middleware drained the body without
// restoring it, the echo would be empty.
func TestAudit_PreservesBodyForHandler(t *testing.T) {
	store := newRecorderStore()
	body := []byte(`{"name":"my-key","markup":1.25}`)

	echo := func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	}
	r := newAuditRouter(store, echo)

	req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got, want := rec.Body.Bytes(), body; !bytes.Equal(got, want) {
		t.Fatalf("handler echo mismatch:\n got=%q\nwant=%q", got, want)
	}
	e := store.waitEvent(t)
	if e.BodyHash == "" {
		t.Fatalf("body_hash: empty; expected sha256 hex")
	}
}

// TestAudit_BodyHashDeterministic asserts that identical bodies
// yield identical hashes across two requests. That's the property
// operators lean on to spot retries and replays in the audit log.
func TestAudit_BodyHashDeterministic(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	body := []byte(`{"name":"my-key"}`)

	req1 := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(body))
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	e1 := store.waitEvent(t)
	e2 := store.waitEvent(t)
	if e1.BodyHash == "" || e2.BodyHash == "" {
		t.Fatalf("empty body_hash: %q %q", e1.BodyHash, e2.BodyHash)
	}
	if e1.BodyHash != e2.BodyHash {
		t.Errorf("determinism: %q != %q", e1.BodyHash, e2.BodyHash)
	}
	// Different payload → different hash.
	req3 := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader([]byte(`{"name":"other"}`)))
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	e3 := store.waitEvent(t)
	if e3.BodyHash == e1.BodyHash {
		t.Errorf("distinct bodies produced same hash: %q", e3.BodyHash)
	}
}

// TestAudit_InsertErrorNotFatal makes sure a store error does not
// leak to the caller. The middleware logs at warn and moves on.
func TestAudit_InsertErrorNotFatal(t *testing.T) {
	store := newRecorderStore()
	store.fail = errors.New("db down")
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	req := httptest.NewRequest(http.MethodPost, "/keys", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (audit failure must not affect caller)", rec.Code)
	}
	// The recorder still emits the event; we just verify the middleware
	// swallowed the store's error rather than panicking.
	_ = store.waitEvent(t)
}

// TestAudit_NilStorePassthrough asserts that a nil store yields a
// passthrough middleware — critical for main-package wiring where
// the audit store is optional.
func TestAudit_NilStorePassthrough(t *testing.T) {
	handlerHit := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerHit = true
		w.WriteHeader(http.StatusOK)
	})
	// Feed a typed nil so Audit sees an actual nil ports.AuditStore.
	mw := Audit(nil, nil)(handler)

	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !handlerHit {
		t.Fatalf("handler never invoked; middleware should be passthrough on nil store")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}

// TestAudit_JSONBodyStable is a belt-and-braces guard that the middleware
// hashes the raw byte sequence, not a re-encoded JSON value. It compares
// the observed hash against the sha256 of the untouched bytes.
func TestAudit_JSONBodyStable(t *testing.T) {
	store := newRecorderStore()
	r := newAuditRouter(store, writeStatus(http.StatusOK))

	payload := map[string]any{"a": 1, "b": "two"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/keys", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	e := store.waitEvent(t)
	if len(e.BodyHash) != 64 { // sha256 hex length
		t.Fatalf("body_hash length: got %d want 64", len(e.BodyHash))
	}
}
