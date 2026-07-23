package refresher

// Tests for the refresher's task-6 wiring: per-tick fan-out of
// GET /workspaces/unlim-activations + GET /user's per-family free-quota
// fields. Both writes are non-fatal — a fetch failure on either endpoint
// logs and moves on, so the tests only assert the store receives the
// expected calls when the upstream cooperates.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// unlimActivationsJSON mirrors the shape returned by higgsfield's
// /workspaces/unlim-activations endpoint. See
// higgsfield-register/server/data/archived/spa-net-log.json for the
// empty-response reference; the SPA loader in the JS bundle also
// documents the fields (id, is_claimed, models[]).
const unlimActivationsJSON = `{
  "activations": [
    {
      "id": "act_1",
      "bundle_type": "nano_banana_2_4k",
      "expires_at": null,
      "started_at": "2026-07-01T00:00:00Z",
      "activated_at": "2026-07-01T00:00:00Z",
      "models": [
        {"job_set_type": "nano_banana_pro_unlimited", "generation_type": "image", "resolutions": ["1k","2k","4k"], "max_duration": null}
      ]
    },
    {
      "id": "act_2",
      "bundle_type": "all_above",
      "expires_at": "2026-08-30T00:00:00Z",
      "started_at": "2026-07-15T00:00:00Z",
      "activated_at": "2026-07-15T00:00:00Z",
      "models": [
        {"job_set_type": "kling_3_unlimited", "generation_type": "video", "resolutions": ["720p","1080p","4k"], "max_duration": 15},
        {"job_set_type": "seedance_2_unlimited", "generation_type": "video", "resolutions": ["480p","720p","1080p"], "max_duration": 15}
      ]
    }
  ]
}`

// userWithQuotaJSON extends the existing userJSON fixture with the seven
// per-family free-quota fields. Values mirror what a starter account
// returns in the real /user response (see spa-net-log.json).
const userWithQuotaJSON = `{
  "id": "user_x",
  "email": "e",
  "plan_type": "plus",
  "has_unlim": true,
  "has_flex_unlim": false,
  "is_pro_plan_veo3_available": true,
  "cohort": "c1",
  "total_plan_credits": 100.0,
  "plan_ends_at": "2026-08-17T10:00:00Z",
  "workspace_id": "ws_1",
  "face_swap_credits": 2.0,
  "soul_credits": 0.0,
  "character_swap_credits": 0.0,
  "qwen_camera_control_credits": 0.4,
  "wan2_5_video_credits": 0.0,
  "text2keyframes_credits": 0.0,
  "veo3_fast_generations_count": 0.0
}`

// TestRefresher_SyncsUnlimActivations wires a fake upstream that returns
// two activation records — one with a single model, one with two models
// (the "all_above" case) — and asserts the store's Replace path
// receives three rows total. The compound-key strategy for multi-model
// bundles is verified via the emitted BundleType values.
func TestRefresher_SyncsUnlimActivations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userWithQuotaJSON))
		case "/workspaces/unlim-activations":
			_, _ = w.Write([]byte(unlimActivationsJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{mkAccount("acc_1", "m1")},
	}
	r := newRefresher(t, srv, store, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	// One ReplaceUnlimActivations call per active account.
	if got := len(store.unlimReplaces); got != 1 {
		t.Fatalf("expected 1 ReplaceUnlimActivations call, got %d", got)
	}
	call := store.unlimReplaces[0]
	if call.ID != "acc_1" {
		t.Errorf("replace ID: got %q want acc_1", call.ID)
	}
	// Three flattened rows: 1 for nano_banana + 2 from all_above.
	if len(call.Activations) != 3 {
		t.Fatalf("expected 3 activations, got %d", len(call.Activations))
	}

	// The single-model bundle keeps its bare bundle_type.
	// The all_above bundle expands to two compound-keyed rows to avoid
	// PK collision on (account_id, bundle_type).
	found := map[string]string{}
	for _, a := range call.Activations {
		found[a.BundleType] = a.JobSetType
	}
	if jst, ok := found["nano_banana_2_4k"]; !ok || jst != "nano_banana_pro_unlimited" {
		t.Errorf("nano_banana_2_4k not linked to nano_banana_pro_unlimited: found %v", found)
	}
	if jst, ok := found["all_above@kling_3_unlimited"]; !ok || jst != "kling_3_unlimited" {
		t.Errorf("all_above@kling_3_unlimited row missing: found %v", found)
	}
	if jst, ok := found["all_above@seedance_2_unlimited"]; !ok || jst != "seedance_2_unlimited" {
		t.Errorf("all_above@seedance_2_unlimited row missing: found %v", found)
	}
}

// TestRefresher_SyncsFreeQuota asserts the seven per-family counters
// from /user land on UpdateFreeQuota verbatim (including the fractional
// qwen value that must not be rounded).
func TestRefresher_SyncsFreeQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userWithQuotaJSON))
		case "/workspaces/unlim-activations":
			_, _ = w.Write([]byte(`{"activations":[]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{mkAccount("acc_1", "m1")},
	}
	r := newRefresher(t, srv, store, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	if got := len(store.quotaCalls); got != 1 {
		t.Fatalf("expected 1 UpdateFreeQuota call, got %d", got)
	}
	q := store.quotaCalls[0].Q
	if q.FaceSwapCredits != 2.0 {
		t.Errorf("face_swap_credits: got %v want 2.0", q.FaceSwapCredits)
	}
	if q.QwenCameraControlCredits != 0.4 {
		t.Errorf("qwen_camera_control_credits (fractional): got %v want 0.4", q.QwenCameraControlCredits)
	}
	if q.SoulCredits != 0.0 || q.CharacterSwapCredits != 0.0 || q.Wan25VideoCredits != 0.0 || q.Text2KeyframesCredits != 0.0 || q.Veo3FastGenerationsCount != 0.0 {
		t.Errorf("expected zero fields to be zero, got %+v", q)
	}
}

// TestRefresher_DerivesUpstreamStatus wires a /user carrying blocked_at +
// is_paused and a /workspaces/notice returning a grace notice, asserting
// the refresher records both derived writes.
func TestRefresher_DerivesUpstreamStatus(t *testing.T) {
	userBlocked := `{"id":"user_x","email":"e","plan_type":"plus","has_unlim":false,"cohort":"c1","total_plan_credits":100.0,"plan_ends_at":"2026-08-17T10:00:00Z","workspace_id":"ws_1","blocked_at":"2026-07-20T00:00:00Z","is_paused":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userBlocked))
		case "/workspaces/unlim-activations":
			_, _ = w.Write([]byte(`{"activations":[]}`))
		case "/workspaces/notice":
			_, _ = w.Write([]byte(`{"status":"add_card_grace_notice"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{accounts: []domain.Account{mkAccount("acc_1", "m1")}}
	r := newRefresher(t, srv, store, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	if len(store.upstreamStat) != 1 {
		t.Fatalf("expected 1 UpdateUpstreamStatus, got %d", len(store.upstreamStat))
	}
	u := store.upstreamStat[0]
	if u.BlockedAt != "2026-07-20T00:00:00Z" || !u.IsPaused {
		t.Errorf("derived upstream status wrong: %+v", u)
	}
	if len(store.graceStatuses) != 1 || store.graceStatuses[0] != "grace" {
		t.Errorf("grace status: got %v want [grace]", store.graceStatuses)
	}
}
