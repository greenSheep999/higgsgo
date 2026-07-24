package claimer

// Tests for the auto-claim tick. A real upstream.Client is pointed at an
// httptest server that records which claim endpoints get hit; a fake
// store returns one active account. Assertions cover: unclaimed gift is
// claimed, already-claimed gift is skipped, unclaimed activation is
// claimed (with job_set_type body), multi-model activations dedupe to a
// single claim, and benign 400/409 gift responses don't abort the tick.

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
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only List — the
// claimer touches nothing else, so the embedded nil interface stays
// unused. A call to any other method would panic, which is the desired
// tripwire if the claimer's dependencies ever grow silently.
type fakeStore struct {
	ports.AccountStore
	accounts []domain.Account
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}

// fakeHTTPClient short-circuits the clerk JWT mint and forwards
// everything else to the real transport (so the httptest server serves
// the gift / activation endpoints).
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

func newClaimer(srv *httptest.Server, store ports.AccountStore) *Claimer {
	fake := &fakeHTTPClient{mintJWT: newFakeJWT()}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	return &Claimer{
		Accounts:    store,
		Upstream:    up,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Concurrency: 1,
	}
}

// recorder captures the claim POSTs the tick fires.
type recorder struct {
	mu             sync.Mutex
	giftClaims     []string          // gift ids POSTed to /claim
	unlimClaims    map[string]string // activation id -> job_set_type body
}

func newRecorder() *recorder { return &recorder{unlimClaims: map[string]string{}} }

// mkServer builds a test server serving the given gifts + activations
// JSON on GET, and recording claim POSTs into rec.
func mkServer(rec *recorder, giftsJSON, actsJSON string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/gifts":
			_, _ = w.Write([]byte(giftsJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/workspaces/unlim-activations":
			_, _ = w.Write([]byte(actsJSON))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/gifts/") && strings.HasSuffix(r.URL.Path, "/claim"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/gifts/"), "/claim")
			rec.mu.Lock()
			rec.giftClaims = append(rec.giftClaims, id)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/workspaces/unlim-activations/"):
			id := strings.TrimPrefix(r.URL.Path, "/workspaces/unlim-activations/")
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				JobSetType string `json:"job_set_type"`
			}
			_ = json.Unmarshal(body, &payload)
			rec.mu.Lock()
			rec.unlimClaims[id] = payload.JobSetType
			rec.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestClaimer_ClaimsUnclaimedGift(t *testing.T) {
	rec := newRecorder()
	gifts := `{"items":[
		{"id":"gift_a","plan":"basic","duration":"monthly","claimed":false,"status":"pending"},
		{"id":"gift_b","plan":"pro","duration":"monthly","claimed":true,"status":"claimed"}
	],"total":2}`
	srv := mkServer(rec, gifts, `{"activations":[]}`)
	defer srv.Close()

	cl := newClaimer(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cl.TriggerOnce(ctx)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.giftClaims) != 1 {
		t.Fatalf("expected 1 gift claim, got %d: %v", len(rec.giftClaims), rec.giftClaims)
	}
	if rec.giftClaims[0] != "gift_a" {
		t.Errorf("claimed wrong gift: got %q want gift_a (gift_b was already claimed)", rec.giftClaims[0])
	}
}

func TestClaimer_ClaimsUnclaimedActivationOncePerID(t *testing.T) {
	rec := newRecorder()
	// act_multi has two models (dedupe target); act_done is already claimed.
	acts := `{"activations":[
		{"id":"act_multi","bundle_type":"all_above","expires_at":null,"activated_at":"2026-07-01T00:00:00Z","is_claimed":false,
		 "models":[
			{"job_set_type":"kling_3_unlimited","generation_type":"video","resolutions":["4k"],"max_duration":15},
			{"job_set_type":"seedance_2_unlimited","generation_type":"video","resolutions":["1080p"],"max_duration":15}
		 ]},
		{"id":"act_done","bundle_type":"nano_banana_2_4k","expires_at":null,"activated_at":"2026-07-01T00:00:00Z","is_claimed":true,
		 "models":[{"job_set_type":"nano_banana_pro_unlimited","generation_type":"image","resolutions":["4k"],"max_duration":null}]}
	]}`
	srv := mkServer(rec, `{"items":[]}`, acts)
	defer srv.Close()

	cl := newClaimer(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cl.TriggerOnce(ctx)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.unlimClaims) != 1 {
		t.Fatalf("expected 1 activation claim (deduped), got %d: %v", len(rec.unlimClaims), rec.unlimClaims)
	}
	jst, ok := rec.unlimClaims["act_multi"]
	if !ok {
		t.Fatalf("act_multi not claimed; claims=%v", rec.unlimClaims)
	}
	// First model's job_set_type is used as the claim body.
	if jst != "kling_3_unlimited" {
		t.Errorf("claim body job_set_type: got %q want kling_3_unlimited (first model)", jst)
	}
	if _, done := rec.unlimClaims["act_done"]; done {
		t.Errorf("already-claimed activation should not be re-claimed")
	}
}

func TestClaimer_BenignGiftErrorsDoNotAbortTick(t *testing.T) {
	rec := newRecorder()
	// Server returns 400 (already claimed) for the gift, then the tick
	// must still proceed to claim the activation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gifts":
			_, _ = w.Write([]byte(`{"items":[{"id":"gift_x","claimed":false}],"total":1}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			w.WriteHeader(http.StatusBadRequest) // ErrGiftAlreadyClaimed
		case r.URL.Path == "/workspaces/unlim-activations":
			_, _ = w.Write([]byte(`{"activations":[
				{"id":"act_1","bundle_type":"b","expires_at":null,"activated_at":"2026-07-01T00:00:00Z","is_claimed":false,
				 "models":[{"job_set_type":"kling_3_unlimited","generation_type":"video","resolutions":["4k"],"max_duration":15}]}
			]}`))
		case strings.HasPrefix(r.URL.Path, "/workspaces/unlim-activations/"):
			rec.mu.Lock()
			rec.unlimClaims[strings.TrimPrefix(r.URL.Path, "/workspaces/unlim-activations/")] = "ok"
			rec.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cl := newClaimer(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cl.TriggerOnce(ctx)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.unlimClaims["act_1"]; !ok {
		t.Errorf("400 on gift claim aborted the tick; activation was never claimed: %v", rec.unlimClaims)
	}
}
