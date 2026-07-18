package admin_test

// Integration test that drives RoutingSettingsHandler over a real
// SettingsStore (sqlite in-memory). Kept in the _test package so it
// can import both admin and the sqlite adapter without the sqlite
// package pulling admin transitively at build time.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/api/admin"
)

func TestRoutingSettings_SQLiteRoundtrip(t *testing.T) {
	db, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	store := sqlite.NewSettingsStore(db)
	h := admin.NewRoutingSettingsHandler(store)

	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.Register(r) })

	// GET before any PUT — must return default source.
	getBefore := httptest.NewRequest(http.MethodGet, "/admin/settings/routing", nil)
	recBefore := httptest.NewRecorder()
	r.ServeHTTP(recBefore, getBefore)
	if recBefore.Code != http.StatusOK {
		t.Fatalf("initial GET: got %d want 200; body=%s", recBefore.Code, recBefore.Body.String())
	}
	var bodyBefore map[string]any
	_ = json.Unmarshal(recBefore.Body.Bytes(), &bodyBefore)
	if bodyBefore["source"] != "default" {
		t.Errorf("initial source: got %v want default", bodyBefore["source"])
	}
	if bodyBefore["strategy"] != string(admin.DefaultRoutingPreference) {
		t.Errorf("initial strategy: got %v want %s", bodyBefore["strategy"], admin.DefaultRoutingPreference)
	}

	// PUT priority.
	putReq := httptest.NewRequest(http.MethodPut, "/admin/settings/routing",
		bytes.NewBufferString(`{"strategy":"priority"}`))
	putRec := httptest.NewRecorder()
	r.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT: got %d want 200; body=%s", putRec.Code, putRec.Body.String())
	}

	// GET after must reflect db source and the persisted value.
	getAfter := httptest.NewRequest(http.MethodGet, "/admin/settings/routing", nil)
	recAfter := httptest.NewRecorder()
	r.ServeHTTP(recAfter, getAfter)
	if recAfter.Code != http.StatusOK {
		t.Fatalf("GET after PUT: got %d want 200; body=%s", recAfter.Code, recAfter.Body.String())
	}
	var bodyAfter map[string]any
	_ = json.Unmarshal(recAfter.Body.Bytes(), &bodyAfter)
	if bodyAfter["source"] != "db" {
		t.Errorf("post-PUT source: got %v want db", bodyAfter["source"])
	}
	if bodyAfter["strategy"] != "priority" {
		t.Errorf("post-PUT strategy: got %v want priority", bodyAfter["strategy"])
	}
}
