package promowatch

// Tests for the promowatch tick. A real upstream.Client is pointed at an
// httptest server serving the three promo surfaces; a fake store returns one
// active account and a fake notifier records alerts. Assertions cover each
// alert condition firing (and its negative case not firing), plus dedup.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only List — the
// watcher touches nothing else, so any other call would panic (the desired
// tripwire if the deps ever grow silently).
type fakeStore struct {
	ports.AccountStore
	accounts []domain.Account
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}

// fakeNotifier records sent notifications.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []ports.Notification
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(_ context.Context, m ports.Notification) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}
func (f *fakeNotifier) byTitle(title string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.sent {
		if m.Title == title {
			n++
		}
	}
	return n
}

// fakeHTTPClient short-circuits the clerk JWT mint and forwards everything
// else to the real transport.
type fakeHTTPClient struct{ mintJWT string }

func (f *fakeHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"jwt":%q}`, f.mintJWT))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}
func (f *fakeHTTPClient) Fingerprint() string { return "fake" }
func (f *fakeHTTPClient) Name() string        { return "fake" }

func newFakeJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub": "user_test", "email": "t@example.com",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func mkAccount(id string) domain.Account {
	return domain.Account{
		ID: id, Email: id + "@example.com", SessionID: "sess_" + id,
		CookiesJSON: `{"__session":"stub"}`, UserAgent: "test", Status: domain.StatusActive,
	}
}

// promoBodies bundles the JSON each of the three surfaces returns. An empty
// string ⇒ the handler returns a benign empty object for that path.
type promoBodies struct {
	personalPromo string
	cashback      string
	twoDayOffer   string
}

func mkServer(b promoBodies) *httptest.Server {
	write := func(w http.ResponseWriter, body, fallback string) {
		if body == "" {
			body = fallback
		}
		_, _ = w.Write([]byte(body))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/personal-promo":
			write(w, b.personalPromo, `{}`)
		case "/cashback-challenge":
			write(w, b.cashback, `{"status":"hide"}`)
		case "/two-day-offer":
			write(w, b.twoDayOffer, `{"status":"hide"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func newWatcher(srv *httptest.Server, store ports.AccountStore, ntf ports.Notifier) *Watcher {
	fake := &fakeHTTPClient{mintJWT: newFakeJWT()}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	w := New(store, up, ntf, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Concurrency = 1
	return w
}

func rfc(d time.Duration) string {
	return time.Now().Add(d).UTC().Format(time.RFC3339)
}

func TestPromoWatch_PersonalPromoNearExpiryUnviewed_Alerts(t *testing.T) {
	b := promoBodies{
		personalPromo: `{"id":"p1","promoCode":"X","expired_at":"` + rfc(6*time.Hour) + `","details":{"is_viewed":false}}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.byTitle("Personal promo expiring soon"); got != 1 {
		t.Fatalf("expected 1 personal-promo alert, got %d (total=%d)", got, ntf.count())
	}
}

func TestPromoWatch_PersonalPromoViewed_NoAlert(t *testing.T) {
	b := promoBodies{
		personalPromo: `{"id":"p1","expired_at":"` + rfc(6*time.Hour) + `","details":{"is_viewed":true}}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for viewed promo, got %d", got)
	}
}

func TestPromoWatch_PersonalPromoFarExpiry_NoAlert(t *testing.T) {
	b := promoBodies{
		personalPromo: `{"id":"p1","expired_at":"` + rfc(72*time.Hour) + `","details":{"is_viewed":false}}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for far-off expiry, got %d", got)
	}
}

func TestPromoWatch_CashbackProgressEndingSoon_Alerts(t *testing.T) {
	b := promoBodies{
		cashback: `{"status":"progress","credits_spent":100,"credits_cashback":20,"challenge_ends_at":"` + rfc(3*time.Hour) + `"}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.byTitle("Cashback challenge ending soon"); got != 1 {
		t.Fatalf("expected 1 cashback alert, got %d (total=%d)", got, ntf.count())
	}
}

func TestPromoWatch_CashbackHide_NoAlert(t *testing.T) {
	b := promoBodies{cashback: `{"status":"hide"}`}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for hide cashback, got %d", got)
	}
}

func TestPromoWatch_CashbackProgressFarEnd_NoAlert(t *testing.T) {
	b := promoBodies{
		cashback: `{"status":"progress","challenge_ends_at":"` + rfc(72*time.Hour) + `"}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for far-off cashback end, got %d", got)
	}
}

// tier1Body builds a showable two-day-offer JSON body with a controllable
// discount and allowed_job_set_types list. The bundle-hardcoded 1-day tier
// (900→500 cents = 44% off) is the go-to "deep discount" fixture.
func tier1Body(finalPrice, originalPrice int, allowedJSTs []string) string {
	jsts := "["
	for i, j := range allowedJSTs {
		if i > 0 {
			jsts += ","
		}
		jsts += `{"job_set_type":"` + j + `","resolution":"720p","resolutions":["720p"],"max_duration":5}`
	}
	jsts += "]"
	return fmt.Sprintf(
		`{"status":"tier_1","expires_at":"%s","modalData":{"final_price":%d,"original_price":%d,"currency":"USD","features":["a","b"]},"allowed_job_set_types":%s}`,
		rfc(40*time.Hour), finalPrice, originalPrice, jsts,
	)
}

func TestPromoWatch_TwoDayOfferHide_NoAlert(t *testing.T) {
	b := promoBodies{twoDayOffer: `{"status":"hide","modalData":null,"allowed_job_set_types":[],"expires_at":null}`}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for hide offer, got %d", got)
	}
}

func TestPromoWatch_TwoDayOfferDeepDiscount_AlertsWarn(t *testing.T) {
	// 900 → 500 cents = 44% off > deepDiscountPct=40 → deep trigger.
	b := promoBodies{twoDayOffer: tier1Body(500, 900, nil)}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if ntf.count() != 1 {
		t.Fatalf("expected 1 alert, got %d", ntf.count())
	}
	ntf.mu.Lock()
	got := ntf.sent[0]
	ntf.mu.Unlock()
	if got.Level != ports.LevelWarn {
		t.Errorf("expected Warn level, got %q", got.Level)
	}
	if !strings.Contains(got.Title, "44% off") {
		t.Errorf("expected title to include 44%% off, got %q", got.Title)
	}
	if got.Tags["discount_pct"] != "44" {
		t.Errorf("expected discount_pct tag=44, got %q", got.Tags["discount_pct"])
	}
	if got.Tags["final_price"] != "500" || got.Tags["original_price"] != "900" {
		t.Errorf("expected price tags 500/900, got %q/%q",
			got.Tags["final_price"], got.Tags["original_price"])
	}
	if got.Tags["currency"] != "USD" {
		t.Errorf("expected currency=USD, got %q", got.Tags["currency"])
	}
}

func TestPromoWatch_TwoDayOfferShallowDiscountNoJSTs_NoAlert(t *testing.T) {
	// 20% off, empty allowed_job_set_types → neither deep nor relevant → skip.
	b := promoBodies{twoDayOffer: tier1Body(800, 1000, nil)}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for shallow-no-jsts offer, got %d", got)
	}
}

func TestPromoWatch_TwoDayOfferShallowDiscountWithJSTs_AlertsWarn(t *testing.T) {
	// 20% off but allowed_job_set_types non-empty → relevance triggers.
	b := promoBodies{twoDayOffer: tier1Body(800, 1000, []string{"seedance-2", "veo-3"})}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if ntf.count() != 1 {
		t.Fatalf("expected 1 alert (relevance trigger), got %d", ntf.count())
	}
	ntf.mu.Lock()
	got := ntf.sent[0]
	ntf.mu.Unlock()
	if got.Level != ports.LevelWarn {
		t.Errorf("expected Warn level, got %q", got.Level)
	}
	if got.Tags["allowed_count"] != "2" {
		t.Errorf("expected allowed_count=2, got %q", got.Tags["allowed_count"])
	}
	if !strings.Contains(got.Tags["allowed_models"], "seedance-2") {
		t.Errorf("expected allowed_models to include seedance-2, got %q", got.Tags["allowed_models"])
	}
}

func TestPromoWatch_TwoDayOfferNilModalData_NoAlert(t *testing.T) {
	// Showable status but modalData=null (edge case: server flap between hide
	// and tier states) — no pricing → nothing actionable → skip.
	b := promoBodies{
		twoDayOffer: `{"status":"tier_1","modalData":null,"allowed_job_set_types":[],"expires_at":null}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Fatalf("expected no alert for nil modalData, got %d", got)
	}
}

func TestPromoWatch_TwoDayOfferDedupSameAccount(t *testing.T) {
	b := promoBodies{twoDayOffer: tier1Body(500, 900, nil)}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())
	w.TriggerOnce(context.Background()) // second tick, same account — muted

	if got := ntf.count(); got != 1 {
		t.Fatalf("expected dedup to 1 alert across two ticks, got %d", got)
	}
}

func TestPromoWatch_DedupSameSurfaceAcrossTicks(t *testing.T) {
	b := promoBodies{
		personalPromo: `{"id":"p1","expired_at":"` + rfc(6*time.Hour) + `","details":{"is_viewed":false}}`,
	}
	srv := mkServer(b)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, ntf)
	w.TriggerOnce(context.Background())
	w.TriggerOnce(context.Background()) // second tick, same promo — muted

	if got := ntf.byTitle("Personal promo expiring soon"); got != 1 {
		t.Fatalf("expected dedup to 1 alert across two ticks, got %d", got)
	}
}

func TestPromoWatch_NoAccounts_NoPanic(t *testing.T) {
	srv := mkServer(promoBodies{})
	defer srv.Close()
	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: nil}, ntf)
	w.TriggerOnce(context.Background())
	if ntf.count() != 0 {
		t.Fatalf("expected no alerts with no accounts")
	}
}
