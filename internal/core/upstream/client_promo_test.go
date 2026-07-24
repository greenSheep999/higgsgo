package upstream

// Tests for the three promo/offer/cashback GET endpoints consumed by the
// promowatch ticker. Each uses the shared newTestClient/testAccount helpers
// (client_user_test.go) pointed at an httptest server serving a fixture.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func promoServer(path, body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if status != 0 && status != 200 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestClient_FetchPersonalPromo_Populated(t *testing.T) {
	exp := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"id":"promo_1","campaign_name":"welcome_back","promoCode":"WB20","max_display_percent_off":20,"expired_at":"` + exp + `","details":{"is_viewed":false}}`
	srv := promoServer("/user/personal-promo", body, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchPersonalPromo(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil {
		t.Fatal("expected a promo, got nil")
	}
	if got.ID != "promo_1" || got.PromoCode != "WB20" || got.Discount != 20 {
		t.Errorf("unexpected promo fields: %+v", got)
	}
	if got.IsViewed {
		t.Errorf("expected is_viewed=false")
	}
	if got.ExpiredAt.IsZero() {
		t.Errorf("expected expired_at parsed, got zero")
	}
}

func TestClient_FetchPersonalPromo_EmptyStarterState(t *testing.T) {
	srv := promoServer("/user/personal-promo", `{}`, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchPersonalPromo(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty {} state, got %+v", got)
	}
}

func TestClient_FetchCashbackChallenge_Progress(t *testing.T) {
	ends := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"status":"progress","credits_spent":150,"credits_cashback":30,"challenge_ends_at":"` + ends + `"}`
	srv := promoServer("/cashback-challenge", body, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchCashbackChallenge(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Status != "progress" {
		t.Fatalf("expected progress challenge, got %+v", got)
	}
	if got.ChallengeEndsAt.IsZero() {
		t.Errorf("expected challenge_ends_at parsed")
	}
	if got.CreditsSpent != 150 || got.CreditsCashback != 30 {
		t.Errorf("unexpected credits: %+v", got)
	}
}

func TestClient_FetchCashbackChallenge_HideReturnsValue(t *testing.T) {
	srv := promoServer("/cashback-challenge", `{"status":"hide"}`, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchCashbackChallenge(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Status != "hide" {
		t.Fatalf("expected hide status value, got %+v", got)
	}
}

func TestClient_FetchTwoDayOffer_Show(t *testing.T) {
	exp := time.Now().Add(40 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"status":"show","expires_at":"` + exp + `","modalData":null,"allowed_job_set_types":[]}`
	srv := promoServer("/two-day-offer", body, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchTwoDayOffer(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Status != "show" {
		t.Fatalf("expected show status, got %+v", got)
	}
	if got.ExpiresAt.IsZero() {
		t.Errorf("expected expires_at parsed")
	}
}

func TestClient_FetchTwoDayOffer_Hide(t *testing.T) {
	body := `{"status":"hide","modalData":null,"allowed_job_set_types":[],"expires_at":null,"purchase_expires_at":null,"quiz_type":null,"is_card_visible":null,"is_plan_visible":null}`
	srv := promoServer("/two-day-offer", body, 200)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchTwoDayOffer(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.Status != "hide" {
		t.Fatalf("expected hide status value, got %+v", got)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("expected zero expires_at for null, got %v", got.ExpiresAt)
	}
}

func TestClient_FetchTwoDayOffer_NotFoundIsBenign(t *testing.T) {
	srv := promoServer("/two-day-offer", ``, http.StatusNotFound)
	defer srv.Close()

	got, err := newTestClient(t, srv).FetchTwoDayOffer(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("expected benign nil on 404, got err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil offer on 404, got %+v", got)
	}
}
