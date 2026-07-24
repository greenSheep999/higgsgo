package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeNoticeStatus(t *testing.T) {
	cases := map[string]string{
		"add_card_grace_notice":     "grace",
		"enforcement-notice":        "enforcement",
		"access-lose-notice":        "access_lose",
		"card-decline-credit-offer": "card_declined",
		"add_backup_card_notice":    "backup_card",
		"hide-notice":               "",
		"soft-notice":               "",
		"warning-notice":            "",
		"winback-offer":             "",
		"":                          "",
		"totally-unknown":           "",
	}
	for raw, want := range cases {
		if got := NormalizeNoticeStatus(raw); got != want {
			t.Errorf("NormalizeNoticeStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestClient_FetchWorkspaceNotice(t *testing.T) {
	t.Run("returns status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/workspaces/notice" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"status":"add_card_grace_notice","modal_data":null}`))
		}))
		defer srv.Close()
		got, err := newTestClient(t, srv).FetchWorkspaceNotice(context.Background(), testAccount())
		if err != nil {
			t.Fatalf("FetchWorkspaceNotice: %v", err)
		}
		if got != "add_card_grace_notice" {
			t.Errorf("got %q want add_card_grace_notice", got)
		}
	})

	t.Run("404 is empty non-error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no notice", http.StatusNotFound)
		}))
		defer srv.Close()
		got, err := newTestClient(t, srv).FetchWorkspaceNotice(context.Background(), testAccount())
		if err != nil {
			t.Fatalf("expected nil err on 404, got %v", err)
		}
		if got != "" {
			t.Errorf("got %q want empty", got)
		}
	})

	t.Run("empty body is empty non-error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		got, err := newTestClient(t, srv).FetchWorkspaceNotice(context.Background(), testAccount())
		if err != nil || got != "" {
			t.Errorf("got (%q, %v) want (empty, nil)", got, err)
		}
	})
}
