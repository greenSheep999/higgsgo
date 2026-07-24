// Package upstream is the low-level client for higgsfield.ai's job API.
// It composes the JA3 HTTP client, the JWT minter, and per-account
// cookies/UA into ready-to-send authenticated requests.
//
// Higher-level orchestration (pool picking, retries across accounts, group
// quota checks) lives in core/proxy.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// MetricsSink is the narrow subset of the observability metrics surface the
// upstream client depends on. Defined locally to keep this package free of
// a hard dependency on internal/observability and to keep the unit tests
// self-contained (they can pass a fake without pulling in a Prometheus
// registry).
type MetricsSink interface {
	ObserveUpstreamDuration(endpoint, status string, seconds float64)
}

// Client submits jobs to fnf.higgsfield.ai and polls their status.
type Client struct {
	// http is the default UpstreamClient — used when no per-account
	// resolver is wired, or when the resolver returns nil for an
	// account (e.g. bound_proxy_url is empty).
	http ports.UpstreamClient

	// Resolver, when non-nil, is consulted before every request to pick
	// the UpstreamClient appropriate for the given account. This is how
	// account.bound_proxy_url gets honored per request (ROADMAP P1-5):
	// the resolver returns a client whose Transport dials via that
	// proxy, so the account's egress IP is the one Higgsfield sees —
	// preserving the sticky-IP promise it makes at registration time.
	//
	// Returning nil signals "use the default client" (empty proxy URL,
	// or a fallback the resolver decided is safer than a broken one).
	// Errors returned by the resolver are logged and treated as nil.
	Resolver AccountClientResolver

	jwt     *jwt.Minter
	baseURL string

	// timeouts maps a low-cardinality endpoint label (e.g. "create_job",
	// "fetch_status") to a per-request deadline enforced via context. When
	// the map is empty, no context wrapping happens — the underlying HTTP
	// client's transport-level Timeout (if any) still applies. Callers
	// that supply a specific endpoint key get that deadline; callers that
	// only supply the special "default" key get that fallback for every
	// otherwise-unmatched endpoint. Nothing is enforced when neither is
	// present, which keeps existing tests (that pass an empty map) fast
	// and unchanged.
	timeouts map[string]time.Duration

	// Metrics, when non-nil, receives one ObserveUpstreamDuration call
	// per terminal doWithRetry outcome. Wiring is optional: callers that
	// do not care about metrics leave this nil.
	Metrics MetricsSink

	// Logger, when non-nil, is used for resolver-failure warnings so a
	// broken per-account proxy degrades to the shared default instead
	// of silently redirecting every account to the shared egress IP.
	Logger *slog.Logger
}

// AccountClientResolver returns the UpstreamClient to use for one
// account's request. Return nil to fall back to Client.http. Errors are
// logged and treated identically to a nil return (the caller does not
// see the error; upstream traffic just falls back to default). See
// internal/adapters/httpclient/utls.Pool for the production
// implementation.
type AccountClientResolver interface {
	Resolve(ctx context.Context, account *domain.Account) (ports.UpstreamClient, error)
}

// Config controls Client construction.
type Config struct {
	// BaseURL defaults to "https://fnf.higgsfield.ai" when empty.
	BaseURL string

	// Timeouts, when non-empty, sets a per-endpoint request deadline
	// enforced via context.WithTimeout inside doWithRetry. Keys are the
	// endpoint labels used for metrics ("create_job", "fetch_status",
	// "fetch_job", "fetch_wallet", "fetch_user"). The special key
	// "default" is used as a fallback for endpoints not otherwise listed.
	// A nil / empty map disables per-endpoint timeouts entirely (the
	// underlying HTTP client's transport-level Timeout still applies).
	Timeouts map[string]time.Duration
}

// New builds a Client with the given HTTP client and JWT minter.
func New(httpClient ports.UpstreamClient, minter *jwt.Minter, cfg Config) *Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://fnf.higgsfield.ai"
	}
	var timeouts map[string]time.Duration
	if len(cfg.Timeouts) > 0 {
		timeouts = make(map[string]time.Duration, len(cfg.Timeouts))
		for k, v := range cfg.Timeouts {
			if v > 0 {
				timeouts[k] = v
			}
		}
	}
	return &Client{
		http:     httpClient,
		jwt:      minter,
		baseURL:  strings.TrimRight(baseURL, "/"),
		timeouts: timeouts,
	}
}

// timeoutFor returns the configured per-request timeout for the given
// endpoint label, or the "default" fallback, or (0, false) if neither is
// set. Callers use the second return value to decide whether to wrap the
// context with a deadline — an unset timeout preserves the caller's
// existing context unchanged, which is how tests avoid picking up an
// unwanted deadline.
func (c *Client) timeoutFor(endpoint string) (time.Duration, bool) {
	if len(c.timeouts) == 0 {
		return 0, false
	}
	if d, ok := c.timeouts[endpoint]; ok && d > 0 {
		return d, true
	}
	if d, ok := c.timeouts["default"]; ok && d > 0 {
		return d, true
	}
	return 0, false
}

// CreateRequest is a single job creation call.
type CreateRequest struct {
	Account  *domain.Account
	Endpoint string // relative path, e.g. "/jobs/v2/seedance_2_0"
	Body     any    // marshalled to JSON
}

// CreateResponse mirrors higgsfield's job creation reply.
type CreateResponse struct {
	JobSetID   string
	JobID      string
	Cost       int64
	Raw        json.RawMessage
	HTTPStatus int
}

// CreateJob POSTs the request body and returns the immediate response.
// Returns typed errors mapped from status codes.
//
// A single 401 triggers a JWT invalidate + one automatic retry (see
// doWithRetry). Job creation is safe to retry: upstream only accepts the
// payload once auth passes, so the first 401 attempt never mutates
// server state.
func (c *Client) CreateJob(ctx context.Context, r CreateRequest) (*CreateResponse, error) {
	body, err := json.Marshal(r.Body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+r.Endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, r.Account, token, true)
		return req, nil
	}

	resp, err := c.doWithRetry(ctx, "create_job", r.Account, build)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return parseCreateResponse(raw, resp.StatusCode)
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamForbidden, snip(raw))
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamRateLimit, snip(raw))
	case http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamBadBody, snip(raw))
	default:
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: %d %s", domain.ErrUpstreamServerError, resp.StatusCode, snip(raw))
		}
		return nil, fmt.Errorf("upstream unexpected status %d: %s", resp.StatusCode, snip(raw))
	}
}

// StatusResponse mirrors GET /jobs/{id}/status.
type StatusResponse struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	IPCheckFinished *bool  `json:"ip_check_finished"`
	IPDetected      *bool  `json:"ip_detected"`
	JobSetType      string `json:"job_set_type"`
}

// FetchResponse mirrors GET /jobs/{id} (full row).
type FetchResponse struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Meta      map[string]any `json:"meta"`
	Results   map[string]any `json:"results"`
	ResultURL string         `json:"-"`
	Refunded  bool           `json:"-"`
	Raw       json.RawMessage
}

// FetchStatus calls GET /jobs/{id}/status.
func (c *Client) FetchStatus(ctx context.Context, account *domain.Account, jobID string) (*StatusResponse, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/jobs/"+jobID+"/status", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_status", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, snip(raw))
	}
	var s StatusResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// FetchJob calls GET /jobs/{id} and derives ResultURL / Refunded.
func (c *Client) FetchJob(ctx context.Context, account *domain.Account, jobID string) (*FetchResponse, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/jobs/"+jobID, nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_job", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch job %s: HTTP %d: %s", jobID, resp.StatusCode, snip(raw))
	}
	var f FetchResponse
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	f.Raw = raw
	if results, ok := f.Results["raw"].(map[string]any); ok {
		if u, ok := results["url"].(string); ok {
			f.ResultURL = u
		}
	}
	if meta, ok := f.Meta["is_refunded"].(bool); ok {
		f.Refunded = meta
	}
	return &f, nil
}

// PollOptions controls PollUntilTerminal.
type PollOptions struct {
	Deadline     time.Duration // total wait before returning ErrUpstreamTimeout; default 5m
	Interval     time.Duration // default 4s
	OnTransition func(status string, elapsed time.Duration)
}

// PollUntilTerminal blocks until the job hits completed / failed / nsfw /
// terminated or the deadline passes.
func (c *Client) PollUntilTerminal(ctx context.Context, account *domain.Account, jobID string, opts PollOptions) (*FetchResponse, error) {
	if opts.Deadline == 0 {
		opts.Deadline = 5 * time.Minute
	}
	if opts.Interval == 0 {
		opts.Interval = 4 * time.Second
	}
	deadline := time.Now().Add(opts.Deadline)
	var last string
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return nil, domain.ErrUpstreamTimeout
		}
		s, err := c.FetchStatus(ctx, account, jobID)
		if err == nil {
			if s.Status != last && opts.OnTransition != nil {
				opts.OnTransition(s.Status, time.Until(deadline))
			}
			last = s.Status
			if isTerminal(s.Status) {
				return c.FetchJob(ctx, account, jobID)
			}
		}
		time.Sleep(opts.Interval)
	}
}

// Wallet mirrors GET /workspaces/wallet.
type Wallet struct {
	WorkspaceID         string  `json:"workspace_id"`
	SubscriptionBalance int64   `json:"subscription_balance"`
	CreditsBalance      int64   `json:"credits_balance"`
	TotalCredits        int64   `json:"total_credits"`
	OnDemandCredits     float64 `json:"on_demand_credits"`
}

// UserSnapshot mirrors GET /user. Values expressed in "dollars" here are
// credits in float form; balance-refresher logic converts them to the int64
// hundredths unit used by the accounts table.
//
// The seven `*_credits` / `*_count` fields at the tail are per-family
// free-generation counters granted monthly by the plan (see the /user
// sample in higgsfield-register/server/data/archived/spa-net-log.json).
// starter carries small non-zero grants (face_swap_credits: 2.0,
// qwen_camera_control_credits: 0.4). Persisted verbatim as REAL —
// operators route on strictly-positive values, so the fractional
// precision matters.
type UserSnapshot struct {
	ID                       string  `json:"id"`
	Email                    string  `json:"email"`
	PlanType                 string  `json:"plan_type"`
	SubscriptionCredits      float64 `json:"subscription_credits"`
	PackageCredits           float64 `json:"package_credits"`
	DailyCredits             float64 `json:"daily_credits"`
	TotalPlanCredits         float64 `json:"total_plan_credits"`
	BillingPeriod            string  `json:"billing_period"`
	PlanEndsAt               string  `json:"plan_ends_at"`
	HasUnlim                 bool    `json:"has_unlim"`
	HasFlexUnlim             bool    `json:"has_flex_unlim"`
	IsProVeo3Available       bool    `json:"is_pro_plan_veo3_available"`
	Cohort                   string  `json:"cohort"`
	WorkspaceID              string  `json:"workspace_id"`
	FaceSwapCredits          float64 `json:"face_swap_credits"`
	SoulCredits              float64 `json:"soul_credits"`
	CharacterSwapCredits     float64 `json:"character_swap_credits"`
	QwenCameraControlCredits float64 `json:"qwen_camera_control_credits"`
	Wan25VideoCredits        float64 `json:"wan2_5_video_credits"`
	Text2KeyframesCredits    float64 `json:"text2keyframes_credits"`
	Veo3FastGenerationsCount float64 `json:"veo3_fast_generations_count"`
	// Account-lifecycle signals. blocked_at / suspended_at are nullable
	// upstream timestamps (empty string when unset). is_pause_scheduled
	// covers "pause queued but not yet active". The refresher derives the
	// accounts.{blocked_at,suspended_at,is_paused} columns from these.
	BlockedAt        string `json:"blocked_at"`
	SuspendedAt      string `json:"suspended_at"`
	IsPaused         bool   `json:"is_paused"`
	IsPauseScheduled bool   `json:"is_pause_scheduled"`
}

// FetchUser calls GET /user and returns the per-account entitlement snapshot.
// This is the canonical source for plan_type, has_unlim, has_flex_unlim,
// is_pro_plan_veo3_available and cohort — /workspaces/wallet only exposes
// balances, not entitlement flags.
func (c *Client) FetchUser(ctx context.Context, account *domain.Account) (*UserSnapshot, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/user", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_user", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("user: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var u UserSnapshot
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// freeGensV2Response mirrors GET /user/free-gens/v2. The endpoint carries
// the authoritative per-family free-generation counters keyed by
// job_set_type — a strictly more precise surface than the seven flat
// `*_credits` fields on GET /user, which report a stale 0 for the soul
// family in every captured tier snapshot (starter/pro/plus) even though
// the account actually holds 298 / 3000 / 300 soul generations.
//
// Field names job_set_type / counter / config are sample-confirmed against
// server/data/user-entitlements.json (starter/pro/plus). counter is a plain
// number; config is null in every captured snapshot and is ignored here.
type freeGensV2Response struct {
	Items []struct {
		JobSetType string  `json:"job_set_type"`
		Counter    float64 `json:"counter"`
	} `json:"items"`
}

// FetchFreeGensV2 calls GET /user/free-gens/v2 and returns a map of
// job_set_type → counter (the number of free generations remaining for
// that family). Mirrors the FetchWallet GET template. An empty items array
// is a non-error nil-map result; a 404 (endpoint absent for this account)
// is likewise treated as "no free-gens surface" — a benign nil-map —
// matching the SPA's 404→null handling.
//
// The refresher uses this to calibrate the accounts free_quota columns: the
// three soul job_set_types (text2image_soul_v2 / soul_cinematic /
// soul_location) all map to the soul_credits column, so their counters
// override the stale /user value on every tick.
func (c *Client) FetchFreeGensV2(ctx context.Context, account *domain.Account) (map[string]float64, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/user/free-gens/v2", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_free_gens_v2", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no free-gens surface for this account — benign
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("free-gens/v2: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var envelope freeGensV2Response
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Items) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(envelope.Items))
	for _, it := range envelope.Items {
		if it.JobSetType == "" {
			continue
		}
		out[it.JobSetType] = it.Counter
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// UnlimActivationsResponse mirrors GET /workspaces/unlim-activations.
// The response wraps a flat "activations" array whose items each carry
// the operator-activated bundle plus a nested `models` list — one entry
// per unlim endpoint that bundle unlocks. Both the bundle type and the
// nested job_set_type are required for the load-balance router to
// match a candidate account to a model's UnlimJobSetType.
//
// A `null` expires_at means the activation is permanent (no auto-
// revocation); the client passes it through as an empty string and the
// refresher normalises to zero time.
type UnlimActivationsResponse struct {
	Activations []UnlimActivationRaw `json:"activations"`
}

// UnlimActivationRaw mirrors one item inside the response envelope.
// `is_claimed` distinguishes bundles the platform granted but the user
// has not yet activated — the claimer (internal/core/claimer) picks
// these up and POSTs the claim so the window starts counting. Pricing /
// seat data the SPA modal uses is still ignored.
type UnlimActivationRaw struct {
	ID          string                 `json:"id"`
	BundleType  string                 `json:"bundle_type"`
	Models      []UnlimActivationModel `json:"models"`
	ExpiresAt   string                 `json:"expires_at"`
	StartedAt   string                 `json:"started_at"`
	ActivatedAt string                 `json:"activated_at"`
	IsClaimed   bool                   `json:"is_claimed"`
}

// UnlimActivationModel is one row of the nested `models` array. The
// bundle → endpoint relationship is many-to-many (see the "all_above"
// bundle in the sample response), so one activation record can produce
// multiple domain.UnlimActivation rows keyed by (bundle_type, job_set_type).
type UnlimActivationModel struct {
	JobSetType     string   `json:"job_set_type"`
	GenerationType string   `json:"generation_type"`
	Resolutions    []string `json:"resolutions"`
	MaxDuration    *int     `json:"max_duration"`
}

// FetchUnlimActivations calls GET /workspaces/unlim-activations and
// returns the account's currently-active unlim bundles flattened to
// the domain-side shape. Each response row can produce one-or-more
// UnlimActivation entries because a bundle can unlock multiple unlim
// endpoints (the "all_above" case), but the AccountStore keys by
// (account_id, bundle_type) so we collapse to one row per bundle by
// picking the first job_set_type — this preserves the sort-first-on-
// bundle-holder semantics without needing a compound primary key.
//
// Actually: to correctly support the "all_above" case where one
// bundle unlocks multiple unlim endpoints, we emit one row per
// (bundle_type, job_set_type) pair, prefixing the storage key with
// the JST index. This preserves the join semantics in PickAndLock
// (any activation for the requested JST wins).
//
// The endpoint responds with `{"activations":[]}` for accounts that
// haven't purchased any unlim bundles — a non-error nil-slice result.
func (c *Client) FetchUnlimActivations(ctx context.Context, account *domain.Account) ([]domain.UnlimActivation, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/workspaces/unlim-activations", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_unlim_activations", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unlim-activations: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var envelope UnlimActivationsResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Activations) == 0 {
		return nil, nil
	}
	// Flatten each activation's `models` array to a domain-side row per
	// (bundle_type, job_set_type). The account_unlim_activations PK is
	// (account_id, bundle_type) so a bundle that unlocks N endpoints
	// would collide on the second insert — we prefix bundle_type with
	// the JST index (`<bundle>@<jst>`) for the N>1 case so every
	// endpoint gets its own row. The prefer_unlim ORDER BY joins on
	// job_set_type, so the compound key is invisible to the router.
	//
	// Rows with no `models` field are dropped: an activation that
	// doesn't unlock any endpoint carries no routing signal.
	out := make([]domain.UnlimActivation, 0, len(envelope.Activations))
	for _, a := range envelope.Activations {
		if len(a.Models) == 0 {
			continue
		}
		expires := parseUnlimTime(a.ExpiresAt)
		activated := parseUnlimTime(a.ActivatedAt)
		if activated.IsZero() {
			activated = parseUnlimTime(a.StartedAt)
		}
		for _, m := range a.Models {
			bundle := a.BundleType
			// Compound key when a bundle unlocks multiple endpoints —
			// the router still matches on job_set_type via the
			// separate column, so downstream consumers are unaffected.
			if len(a.Models) > 1 {
				bundle = a.BundleType + "@" + m.JobSetType
			}
			out = append(out, domain.UnlimActivation{
				ID:          a.ID,
				BundleType:  bundle,
				JobSetType:  m.JobSetType,
				Resolutions: append([]string(nil), m.Resolutions...),
				ExpiresAt:   expires,
				ActivatedAt: activated,
				IsClaimed:   a.IsClaimed,
			})
		}
	}
	return out, nil
}

// parseUnlimTime accepts higgsfield's RFC3339-ish timestamp variants
// and returns the zero time.Time on parse failure. An empty / "null"
// string is treated as absent (zero value) so callers can distinguish
// "never expires" from a real deadline.
func parseUnlimTime(s string) time.Time {
	if s == "" || s == "null" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// UploadImage reserves a media slot, PUTs the raw bytes to the presigned
// S3 URL, then commits the reservation. Returns the media_id that
// higgsfield later accepts as `media_id` in a job body.
//
// Three-step protocol (see docs/OPENAI-VIDEO-COMPAT.md Appendix C and
// higgsfield-register/src/upstream/media.mjs):
//
//  1. POST /media  {"content_type":"..."}  ->  {id, url, upload_url}
//  2. PUT  upload_url   raw bytes           ->  S3 direct, no higgsfield auth
//  3. POST /media/{id}/upload  {}           ->  flips to "uploaded"
//
// Failures at any step are wrapped so the caller can distinguish the
// phase (media_reserve_failed / media_upload_failed / media_commit_failed).
// The body is streamed once into memory because S3 needs the raw bytes
// (with matching Content-Type) as a single PUT payload; callers with
// very large images should chunk upstream of us.
func (c *Client) UploadImage(ctx context.Context, account *domain.Account, contentType string, body io.Reader) (string, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("read image bytes: %w", err)
	}
	// Step 1: reserve.
	reserveBody, err := json.Marshal(map[string]string{"content_type": contentType})
	if err != nil {
		return "", fmt.Errorf("marshal reserve body: %w", err)
	}
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/media", bytes.NewReader(reserveBody))
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, true)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "media_reserve", account, build)
	if err != nil {
		return "", fmt.Errorf("media_reserve_failed: %w", err)
	}
	defer resp.Body.Close()
	respRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("media_reserve_failed: HTTP %d: %s", resp.StatusCode, snip(respRaw))
	}
	var reserved struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		UploadURL string `json:"upload_url"`
	}
	if err := json.Unmarshal(respRaw, &reserved); err != nil {
		return "", fmt.Errorf("media_reserve_failed: parse: %w", err)
	}
	if reserved.ID == "" || reserved.UploadURL == "" {
		return "", fmt.Errorf("media_reserve_failed: missing id / upload_url")
	}

	// Step 2: PUT bytes to S3 directly. No higgsfield auth headers —
	// this URL is a presigned S3 upload_url which carries its own sig
	// in the query string.
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, reserved.UploadURL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("media_upload_failed: build: %w", err)
	}
	putReq.Header.Set("Content-Type", contentType)
	putReq.ContentLength = int64(len(raw))
	putResp, err := c.http.Do(ctx, putReq)
	if err != nil {
		return "", fmt.Errorf("media_upload_failed: %w", err)
	}
	putBody, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		return "", fmt.Errorf("media_upload_failed: HTTP %d: %s", putResp.StatusCode, snip(putBody))
	}

	// Step 3: commit.
	commitBuild := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/media/"+reserved.ID+"/upload", strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, true)
		return req, nil
	}
	commitResp, err := c.doWithRetry(ctx, "media_commit", account, commitBuild)
	if err != nil {
		return "", fmt.Errorf("media_commit_failed: %w", err)
	}
	commitRaw, _ := io.ReadAll(commitResp.Body)
	_ = commitResp.Body.Close()
	if commitResp.StatusCode < 200 || commitResp.StatusCode >= 300 {
		return "", fmt.Errorf("media_commit_failed: HTTP %d: %s", commitResp.StatusCode, snip(commitRaw))
	}
	return reserved.ID, nil
}

// FetchWallet calls GET /workspaces/wallet.
func (c *Client) FetchWallet(ctx context.Context, account *domain.Account) (*Wallet, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/workspaces/wallet", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_wallet", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wallet: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var w Wallet
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// doWithRetry mints a JWT, builds and sends the request via buildReq, and
// retries once when the response is HTTP 401. The retry invalidates the
// cached JWT for the account so the second attempt mints a fresh token from
// clerk.higgsfield.ai. Only a single retry is attempted; a second 401 is
// returned to the caller (which maps it to domain.ErrUpstreamUnauthorized).
//
// buildReq receives the JWT to attach and MUST return a fresh *http.Request
// on each call — the body may have been consumed by the first send.
//
// Non-401 responses (200, 4xx other than 401, 5xx) are returned as-is with
// no retry. Transport errors are also returned as-is.
//
// This makes the client self-healing when clerk revokes a cached JWT before
// its exp claim (account bans, key rotation, clock drift): the first call
// sees 401, we invalidate and re-mint, and the second call succeeds.
//
// endpoint is a low-cardinality label used only for Prometheus histogram
// observations (e.g. "fetch_wallet"). Timing covers the full logical call
// including a retry: a 401 -> remint -> 200 sequence records exactly one
// observation with status="200" and the total elapsed wall time.
func (c *Client) doWithRetry(ctx context.Context, endpoint string, account *domain.Account, buildReq func(token string) (*http.Request, error)) (*http.Response, error) {
	start := time.Now()
	var finalStatus string
	defer func() {
		if c.Metrics != nil {
			c.Metrics.ObserveUpstreamDuration(endpoint, finalStatus, time.Since(start).Seconds())
		}
	}()

	// Wrap the caller's context with a per-endpoint deadline when one is
	// configured. The deadline covers the full logical call — JWT mint,
	// the first send, an optional 401 remint, and the retry — which
	// matches how the histogram observation is scoped. Nothing is wrapped
	// when the client was built without a timeouts map, so tests that
	// omit Config.Timeouts keep their unbounded context.
	if d, ok := c.timeoutFor(endpoint); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	// Per-account HTTP client selection (ROADMAP P1-5). When a resolver
	// is wired and returns a non-nil client, requests for this account
	// egress through the account's bound proxy so Higgsfield sees the
	// sticky IP promised at registration time. A nil return or resolver
	// error falls back to the default shared client — safer than
	// failing the request, but logged so operators can spot broken
	// per-account proxies.
	httpClient := c.http
	if c.Resolver != nil {
		alt, err := c.Resolver.Resolve(ctx, account)
		if err != nil {
			if c.Logger != nil {
				c.Logger.Warn("upstream client resolver failed; using default",
					slog.String("account_id", account.ID),
					slog.String("err", err.Error()))
			}
		} else if alt != nil {
			httpClient = alt
		}
	}

	tok, err := c.jwt.Get(ctx, account)
	if err != nil {
		finalStatus = "network_error"
		return nil, fmt.Errorf("mint jwt: %w", err)
	}
	req, err := buildReq(tok.JWT)
	if err != nil {
		finalStatus = "network_error"
		return nil, err
	}
	resp, err := httpClient.Do(ctx, req)
	if err != nil {
		finalStatus = "network_error"
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		finalStatus = statusCodeString(resp.StatusCode)
		return resp, nil
	}

	// Drain + close the first response body before retrying so the
	// underlying connection can be reused by the JA3 client.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Invalidate the cached JWT and mint a fresh one.
	c.jwt.Invalidate(account.ID)
	tok2, err := c.jwt.Get(ctx, account)
	if err != nil {
		finalStatus = "network_error"
		return nil, fmt.Errorf("remint jwt after 401: %w", err)
	}
	req2, err := buildReq(tok2.JWT)
	if err != nil {
		finalStatus = "network_error"
		return nil, err
	}
	resp2, err := httpClient.Do(ctx, req2)
	if err != nil {
		finalStatus = "network_error"
		return nil, err
	}
	finalStatus = statusCodeString(resp2.StatusCode)
	return resp2, nil
}

// statusCodeString renders an HTTP status code as a low-cardinality label
// value for the upstream latency histogram. Common codes get pre-interned
// strings; anything else falls back to strconv.Itoa. Keeping this list short
// intentionally bounds the label cardinality.
func statusCodeString(code int) string {
	switch code {
	case 200:
		return "200"
	case 201:
		return "201"
	case 400:
		return "400"
	case 401:
		return "401"
	case 403:
		return "403"
	case 404:
		return "404"
	case 429:
		return "429"
	case 500:
		return "500"
	case 502:
		return "502"
	case 503:
		return "503"
	}
	return strconv.Itoa(code)
}

// setStdHeaders applies the exact header set a real Chrome browser sends
// when calling fnf.higgsfield.ai from higgsfield.ai. Matches the Node
// impit client in higgsfield-register/src/upstream/client.mjs.
//
// Any deviation (missing sec-ch-ua headers, different accept value) can
// trigger a DataDome challenge redirect.
func (c *Client) setStdHeaders(req *http.Request, account *domain.Account, token string, isJSON bool) {
	// Body content type.
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}

	// Chrome-shaped fetch headers.
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://higgsfield.ai")
	req.Header.Set("Referer", "https://higgsfield.ai/")
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="145", "Not_A Brand";v="99"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")

	if account.UserAgent != "" {
		req.Header.Set("User-Agent", account.UserAgent)
	}
	if account.DataDomeClientID != "" && account.DataDomeClientID != ".keep" {
		req.Header.Set("X-Datadome-Clientid", account.DataDomeClientID)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	if account.CookiesJSON != "" {
		if h, err := buildCookieHeader(account.CookiesJSON); err == nil && h != "" {
			req.Header.Set("Cookie", h)
		}
	}
}

func buildCookieHeader(cookiesJSON string) (string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(cookiesJSON), &m); err != nil {
		return "", err
	}
	var b strings.Builder
	first := true
	for k, v := range m {
		if !first {
			b.WriteString("; ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		first = false
	}
	return b.String(), nil
}

func snip(b []byte) string {
	const max = 240
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "failed", "nsfw", "terminated":
		return true
	}
	return false
}

// parseCreateResponse extracts the first job_set + job id from higgsfield's
// creation reply.
func parseCreateResponse(raw []byte, httpStatus int) (*CreateResponse, error) {
	var body struct {
		ID      string `json:"id"`
		JobSets []struct {
			ID   string      `json:"id"`
			Cost json.Number `json:"cost"`
			Jobs []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		} `json:"job_sets"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}
	if len(body.JobSets) == 0 || len(body.JobSets[0].Jobs) == 0 {
		return nil, errors.New("create response missing job_sets[0].jobs[0]")
	}
	cost, err := parseCreateCost(body.JobSets[0].Cost)
	if err != nil {
		return nil, err
	}
	return &CreateResponse{
		JobSetID:   body.JobSets[0].ID,
		JobID:      body.JobSets[0].Jobs[0].ID,
		Cost:       cost,
		Raw:        raw,
		HTTPStatus: httpStatus,
	}, nil
}

func parseCreateCost(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if v, err := n.Int64(); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return 0, fmt.Errorf("parse create response cost %q: %w", n.String(), err)
	}
	if f < 0 || math.Trunc(f) != f {
		return 0, fmt.Errorf("parse create response cost %q: expected whole number", n.String())
	}
	return int64(f), nil
}

// Gift is one inbound gift item from GET /gifts. Only the fields the
// claimer needs are decoded; the SPA modal reads sender / background /
// invoice data we don't care about.
type Gift struct {
	ID       string `json:"id"`
	Plan     string `json:"plan"`
	Duration string `json:"duration"`
	Claimed  bool   `json:"claimed"`
	Status   string `json:"status"`
}

type giftsResponse struct {
	Items []Gift `json:"items"`
	Total int    `json:"total"`
}

// Sentinel errors for ClaimGift. Both are benign: the claimer skips them
// rather than treating them as account-health failures.
var (
	// ErrGiftAlreadyClaimed maps HTTP 400 — the gift was already claimed
	// (a prior tick, or another operator). Idempotent no-op.
	ErrGiftAlreadyClaimed = errors.New("gift already claimed")
	// ErrGiftActiveSubscription maps HTTP 409 — the account already holds
	// an active subscription, so the gift cannot stack right now.
	ErrGiftActiveSubscription = errors.New("gift claim blocked by active subscription")
)

// FetchGifts calls GET /gifts?type=inbound&activated=false and returns
// the account's unclaimed inbound gifts. Mirrors the FetchWallet GET
// template. An empty items array is a non-error nil-slice result.
func (c *Client) FetchGifts(ctx context.Context, account *domain.Account) ([]Gift, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/gifts?size=20&type=inbound&activated=false", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_gifts", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gifts: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var envelope giftsResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Items) == 0 {
		return nil, nil
	}
	return envelope.Items, nil
}

// ClaimGift POSTs /gifts/{giftID}/claim. Fire-and-forget: the response
// body is ignored, only the status code matters. Returns nil on 2xx,
// ErrGiftAlreadyClaimed on 400, ErrGiftActiveSubscription on 409, and a
// generic error otherwise. No request body (matches the SPA).
func (c *Client) ClaimGift(ctx context.Context, account *domain.Account, giftID string) error {
	if giftID == "" {
		return fmt.Errorf("claim gift: empty gift id")
	}
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/gifts/"+giftID+"/claim", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "claim_gift", account, build)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest:
		return ErrGiftAlreadyClaimed
	case resp.StatusCode == http.StatusConflict:
		return ErrGiftActiveSubscription
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	default:
		return fmt.Errorf("claim gift %s: HTTP %d: %s", giftID, resp.StatusCode, snip(raw))
	}
}

// ClaimUnlimActivation POSTs /workspaces/unlim-activations/{id} with body
// {"job_set_type": jobSetType} to activate a platform-granted unlim
// bundle. Returns nil on 2xx, an error otherwise. Once claimed, the next
// refresher tick stores the (now is_claimed) activation so the proxy's
// _unlimited routing (P0-1) can match it.
func (c *Client) ClaimUnlimActivation(ctx context.Context, account *domain.Account, activationID, jobSetType string) error {
	if activationID == "" || jobSetType == "" {
		return fmt.Errorf("claim unlim: empty activation id or job_set_type")
	}
	body, err := json.Marshal(map[string]string{"job_set_type": jobSetType})
	if err != nil {
		return fmt.Errorf("claim unlim: marshal body: %w", err)
	}
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/workspaces/unlim-activations/"+activationID, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, true)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "claim_unlim", account, build)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("claim unlim %s: HTTP %d: %s", activationID, resp.StatusCode, snip(raw))
	}
	return nil
}

// noticeResponse decodes only the status field of GET /workspaces/notice;
// the rest of the envelope (modal_data, subscription, discount…) is for
// the SPA modal and irrelevant to the refresher's grace derivation.
type noticeResponse struct {
	Status string `json:"status"`
}

// FetchWorkspaceNotice returns the raw status of GET /workspaces/notice
// (e.g. "hide-notice", "add_card_grace_notice", "enforcement-notice").
// A 404 or empty body yields "" (no active notice) rather than an error —
// most accounts have no notice most of the time. Pass the raw result to
// NormalizeNoticeStatus to get the grace_status token.
func (c *Client) FetchWorkspaceNotice(ctx context.Context, account *domain.Account) (string, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/workspaces/notice", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_notice", account, build)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("notice: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	if len(raw) == 0 {
		return "", nil
	}
	var n noticeResponse
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", err
	}
	return n.Status, nil
}

// NormalizeNoticeStatus maps the raw /workspaces/notice.status enum to a
// small grace_status token. Payment-risk notices become a stable
// non-empty token (the replenish S3 signal fires on any non-empty
// PendingInvoice is one unpaid invoice returned by GET
// /workspaces/pending-invoices. Only id + status are load-bearing for the
// invoicewatch ticker (non-empty list → attempt a retry); the remaining
// fields are decoded opportunistically for logging. Field shape is
// bundle-inferred (no live JSON sample of a real pending-invoices response
// exists in the repo — only the SPA bundle mapper reveals it), so amount
// is a plain STRING (dollars ×100 encoded upstream, client would divide by
// 100) and there is NO amount_cents / error_code field. TODO: confirm
// against a real captured response.
type PendingInvoice struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Amount   string `json:"amount"`   // string dollars (bundle-inferred; 待真实样本确认)
	Currency string `json:"currency"` // 待真实样本确认
}

// pendingInvoicesResponse is the server envelope. hasMore/total are
// client-derived in the SPA (total = items.length, hasMore = cursor!=null),
// so the server returns just {items, cursor}. Empty state = {items:[],
// cursor:null} (inferred).
type pendingInvoicesResponse struct {
	Items  []PendingInvoice `json:"items"`
	Cursor *string          `json:"cursor"`
}

// FetchPendingInvoices calls GET /workspaces/pending-invoices and returns
// the account's unpaid invoices. Mirrors the FetchGifts GET template. An
// empty items array is a non-error nil-slice result. A 404 (endpoint not
// present for this account) is treated as "no pending invoices" — a benign
// nil-slice — rather than an error, matching the SPA's 404→null handling.
func (c *Client) FetchPendingInvoices(ctx context.Context, account *domain.Account) ([]PendingInvoice, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/workspaces/pending-invoices", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_pending_invoices", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no pending-invoices surface for this account — benign
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pending-invoices: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var envelope pendingInvoicesResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Items) == 0 {
		return nil, nil
	}
	return envelope.Items, nil
}

// RetryAutoTopUp POSTs /v2/auto-top-ups/retry-payment to re-attempt the
// account's failed auto-top-up charge. Fire-and-forget: no request body,
// the response body is ignored, only the status code matters (matches the
// SPA, which passes the raw json through with no transform). Returns nil on
// 2xx. A 404 (retry surface absent / nothing outstanding) is treated as a
// benign no-op (nil) rather than an error. Any other non-2xx is an error.
func (c *Client) RetryAutoTopUp(ctx context.Context, account *domain.Account) error {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, c.baseURL+"/v2/auto-top-ups/retry-payment", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "retry_auto_top_up", account, build)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return nil // no outstanding auto-top-up to retry — benign
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	default:
		return fmt.Errorf("retry auto-top-up: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
}

// jobSetCostsResponse is the GET /job-sets/costs envelope. The SPA
// unwraps it as ("data" in n ? n.data : n), so the server always nests the
// per-model entries under a top-level "data" key. Each entry carries a
// job_set_type and a `cost` array whose item shape VARIES by model (four
// known variants — see reduceJobSetCost). Costs are quoted in CREDITS
// (fractional allowed, e.g. 1.5), NOT credit-hundredths.
type jobSetCostsResponse struct {
	Data []JobSetCostEntry `json:"data"`
}

// JobSetCostEntry is one model entry from GET /job-sets/costs.
type JobSetCostEntry struct {
	JobSetType string            `json:"job_set_type"`
	Cost       []json.RawMessage `json:"cost"`
}

// JobSetCostCatalog preserves the raw upstream payload and both derived
// views: a minimum affordability floor and normalized priced rules.
type JobSetCostCatalog struct {
	RawJSON  string
	MinCosts map[string]int64
	Rules    []domain.ModelCostRule
}

// FetchJobSetCosts calls GET /job-sets/costs and returns a map of
// job_set_type → representative cost in credit-hundredths (credits ×100,
// matching the accounts/registry unit). The endpoint works unauthenticated
// but we send the standard headers anyway so DataDome doesn't challenge.
//
// The raw `cost` array is a union of four shapes (per-resolution video with
// cost_per_second, per-model wrapper with nested resolutions, mode+audio
// with audio.on/off, and flat image `credits`). We collapse each model's
// array to a single number by taking the MINIMUM strictly-positive cost
// found across every variant field, then ×100 → hundredths. Minimum is the
// conservative floor a credit-gate check needs ("can this account afford
// the cheapest generation of this model"); it also matches the locally-
// derived cost-map.json summary for the video models.
//
// 待真实样本确认: the reduction heuristic (min-positive ×100) is a working
// choice, not a product-confirmed rule — if the registry later needs a
// per-resolution / per-mode breakdown, extend reduceJobSetCost rather than
// changing the map contract.
func (c *Client) FetchJobSetCosts(ctx context.Context, account *domain.Account) (map[string]int64, error) {
	catalog, err := c.FetchJobSetCostCatalog(ctx, account)
	if err != nil || catalog == nil {
		return nil, err
	}
	return catalog.MinCosts, nil
}

// FetchJobSetCostCatalog returns the lossless upstream payload together with
// normalized pricing rules. Callers that only need the affordability floor
// should use FetchJobSetCosts.
func (c *Client) FetchJobSetCostCatalog(ctx context.Context, account *domain.Account) (*JobSetCostCatalog, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/job-sets/costs", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_job_set_costs", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("job-sets/costs: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var envelope jobSetCostsResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Data) == 0 {
		return nil, nil
	}
	out := &JobSetCostCatalog{
		RawJSON:  string(raw),
		MinCosts: make(map[string]int64, len(envelope.Data)),
	}
	for _, e := range envelope.Data {
		if e.JobSetType == "" {
			continue
		}
		if h, ok := reduceJobSetCost(e.Cost); ok {
			out.MinCosts[e.JobSetType] = h
		}
		out.Rules = append(out.Rules, normalizeJobSetCostRules(e.JobSetType, e.Cost)...)
	}
	if len(out.MinCosts) == 0 {
		return nil, nil
	}
	return out, nil
}

func normalizeJobSetCostRules(jst string, items []json.RawMessage) []domain.ModelCostRule {
	var out []domain.ModelCostRule
	for _, item := range items {
		var node map[string]any
		if err := json.Unmarshal(item, &node); err != nil {
			continue
		}
		out = append(out, normalizeCostNode(jst, node, nil)...)
	}
	return out
}

func normalizeCostNode(jst string, node, inherited map[string]any) []domain.ModelCostRule {
	dimensions := make(map[string]any, len(inherited)+len(node))
	for key, value := range inherited {
		dimensions[key] = value
	}
	for key, value := range node {
		switch key {
		case "cost_per_second", "original_cost_per_second", "credits", "base_credits", "audio", "resolutions":
			continue
		default:
			dimensions[key] = value
		}
	}

	var out []domain.ModelCostRule
	add := func(unit, component, audio string, credits, originalCredits float64) {
		if credits <= 0 {
			return
		}
		dims := make(map[string]any, len(dimensions)+1)
		for key, value := range dimensions {
			dims[key] = value
		}
		if audio != "" {
			dims["audio"] = audio
		}
		dimensionsJSON, _ := json.Marshal(dims)
		rule := domain.ModelCostRule{
			JST:                       jst,
			Unit:                      unit,
			Component:                 component,
			CreditsHundredths:         int64(math.Round(credits * 100)),
			OriginalCreditsHundredths: int64(math.Round(originalCredits * 100)),
			Resolution:                stringDimension(dims, "resolution"),
			DurationSeconds:           intDimension(dims, "duration", "duration_seconds", "seconds"),
			Mode:                      stringDimension(dims, "mode"),
			Audio:                     audio,
			DimensionsJSON:            string(dimensionsJSON),
		}
		out = append(out, rule)
	}
	if value, ok := node["cost_per_second"].(float64); ok {
		original, _ := node["original_cost_per_second"].(float64)
		add("per_second", "cost_per_second", "", value, original)
	}
	if value, ok := node["credits"].(float64); ok {
		add("per_request", "credits", "", value, 0)
	}
	if value, ok := node["base_credits"].(float64); ok {
		add("per_request", "base_credits", "", value, 0)
	}
	if audio, ok := node["audio"].(map[string]any); ok {
		for _, state := range []string{"off", "on"} {
			if value, ok := audio[state].(float64); ok {
				add("upstream_unspecified", "audio_state", state, value, 0)
			}
		}
	}
	if resolutions, ok := node["resolutions"].([]any); ok {
		for _, value := range resolutions {
			if child, ok := value.(map[string]any); ok {
				out = append(out, normalizeCostNode(jst, child, dimensions)...)
			}
		}
	}
	return out
}

func stringDimension(dimensions map[string]any, key string) string {
	value, _ := dimensions[key].(string)
	return value
}

func intDimension(dimensions map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := dimensions[key].(type) {
		case float64:
			return int(value)
		case string:
			if parsed, err := strconv.Atoi(value); err == nil {
				return parsed
			}
		}
	}
	return 0
}

// reduceJobSetCost collapses one model's `cost` array to a single value in
// credit-hundredths, returning ok=false when no positive cost field is
// present. It walks every cost item as a generic tree and collects the
// numeric fields known to carry a price — cost_per_second, credits,
// base_credits, and audio.on/off — plus any nested `resolutions` array
// (the per-model wrapper variant). The minimum strictly-positive value
// across all of them ×100 is returned.
func reduceJobSetCost(items []json.RawMessage) (int64, bool) {
	best := math.Inf(1)
	found := false
	consider := func(v float64) {
		if v > 0 && v < best {
			best = v
			found = true
		}
	}
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		for _, key := range []string{"cost_per_second", "credits", "base_credits"} {
			if f, ok := m[key].(float64); ok {
				consider(f)
			}
		}
		if audio, ok := m["audio"].(map[string]any); ok {
			for _, key := range []string{"on", "off"} {
				if f, ok := audio[key].(float64); ok {
					consider(f)
				}
			}
		}
		if res, ok := m["resolutions"].([]any); ok {
			for _, r := range res {
				if rm, ok := r.(map[string]any); ok {
					walk(rm)
				}
			}
		}
	}
	for _, it := range items {
		var m map[string]any
		if err := json.Unmarshal(it, &m); err != nil {
			continue
		}
		walk(m)
	}
	if !found {
		return 0, false
	}
	// Round to the nearest hundredth to absorb float noise (e.g. 1.75 → 175).
	return int64(math.Round(best * 100)), true
}

// grace_status); marketing / hide / unknown notices become "" so they
// never trigger an alert. soft-notice / warning-notice are deliberately
// mapped to "" — no real sample distinguishes payment dunning from
// marketing there, so we stay conservative to avoid false alarms.
func NormalizeNoticeStatus(raw string) string {
	switch raw {
	case "add_card_grace_notice":
		return "grace"
	case "enforcement-notice":
		return "enforcement"
	case "access-lose-notice":
		return "access_lose"
	case "card-decline-credit-offer":
		return "card_declined"
	case "add_backup_card_notice":
		return "backup_card"
	default:
		return ""
	}
}

// PersonalPromo is the account's active personal promo returned by GET
// /user/personal-promo. Only ID / ExpiredAt / IsViewed are load-bearing for
// the promowatch ticker (near-expiry + unviewed → alert); the remaining
// fields are decoded for logging. Empty starter state is a literal `{}`
// (no id) → FetchPersonalPromo returns nil.
//
// Field shape is bundle-inferred: the empty {} state is live-confirmed, but
// the populated field NAMES come from the SPA mapper (raw uses expired_at /
// max_display_percent_off / details.is_viewed; promoCode is camelCase in the
// raw body). TODO 待真实样本确认: confirm against a captured populated response.
type PersonalPromo struct {
	ID           string    // top-level `id`; absent ⇒ no active promo
	CampaignName string    // raw campaign_name (logging only)
	PromoCode    string    // raw promoCode (camelCase in body)
	Discount     float64   // raw max_display_percent_off
	ExpiredAt    time.Time // raw expired_at (RFC3339); zero when absent/unparseable
	IsViewed     bool      // raw details.is_viewed
}

// personalPromoRaw mirrors the raw /user/personal-promo body. Minimal field
// set (待真实样本确认 for the rest of the object).
type personalPromoRaw struct {
	ID               string  `json:"id"`
	CampaignName     string  `json:"campaign_name"`
	PromoCode        string  `json:"promoCode"`
	MaxDisplayPctOff float64 `json:"max_display_percent_off"`
	ExpiredAt        string  `json:"expired_at"`
	Details          struct {
		IsViewed bool `json:"is_viewed"`
	} `json:"details"`
}

// FetchPersonalPromo calls GET /user/personal-promo and returns the account's
// active personal promo, or nil when none is active. Mirrors the FetchWallet
// GET template. The empty starter state is a literal `{}` (no id) → nil; a
// 404 is likewise treated as "no promo" (benign nil), matching the SPA's
// 404→null handling.
func (c *Client) FetchPersonalPromo(ctx context.Context, account *domain.Account) (*PersonalPromo, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/user/personal-promo", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_personal_promo", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no personal-promo surface for this account — benign
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("personal-promo: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var r personalPromoRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, nil // empty {} starter state — no active promo
	}
	return &PersonalPromo{
		ID:           r.ID,
		CampaignName: r.CampaignName,
		PromoCode:    r.PromoCode,
		Discount:     r.MaxDisplayPctOff,
		ExpiredAt:    parseUnlimTime(r.ExpiredAt),
		IsViewed:     r.Details.IsViewed,
	}, nil
}

// CashbackChallenge is the account's cashback challenge returned by GET
// /cashback-challenge. Status / ChallengeEndsAt are load-bearing for the
// promowatch ticker (status=progress + ends<24h → alert). The status enum
// {hide|progress|summary} is bundle-inferred from the SPA UI switch; only
// "hide" is live-confirmed. Field names credits_spent / credits_cashback /
// challenge_ends_at are confirmed as raw names in the JS mapper.
type CashbackChallenge struct {
	Status          string    // hide | progress | summary (bundle-inferred enum)
	CreditsSpent    float64   // raw credits_spent (logging)
	CreditsCashback float64   // raw credits_cashback (logging)
	ChallengeEndsAt time.Time // raw challenge_ends_at (RFC3339); zero when absent
}

// cashbackChallengeRaw mirrors the raw /cashback-challenge body.
type cashbackChallengeRaw struct {
	Status          string  `json:"status"`
	CreditsSpent    float64 `json:"credits_spent"`
	CreditsCashback float64 `json:"credits_cashback"`
	ChallengeEndsAt string  `json:"challenge_ends_at"`
}

// FetchCashbackChallenge calls GET /cashback-challenge and returns the
// account's cashback challenge, or nil when the surface is absent. The
// live-confirmed idle state is {"status":"hide"} (returned as a real
// value, not nil, so callers can inspect the status). A 404 is treated as
// benign nil.
func (c *Client) FetchCashbackChallenge(ctx context.Context, account *domain.Account) (*CashbackChallenge, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/cashback-challenge", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_cashback_challenge", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no cashback surface — benign
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cashback-challenge: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var r cashbackChallengeRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Status == "" {
		return nil, nil
	}
	return &CashbackChallenge{
		Status:          r.Status,
		CreditsSpent:    r.CreditsSpent,
		CreditsCashback: r.CreditsCashback,
		ChallengeEndsAt: parseUnlimTime(r.ChallengeEndsAt),
	}, nil
}

// TwoDayOffer is the account's two-day offer returned by GET /two-day-offer.
// Status / ExpiresAt / ModalData / AllowedJobSetTypes are load-bearing for
// the promowatch ticker: a value-gated alert compares the offer's discount
// (final vs original price) and the models it unlocks against the account's
// interests. The full body shape is live-confirmed (idle sample: status="hide"
// with null modalData / empty allowed_job_set_types). Non-hide statuses
// observed in bundle enums: tier_1, tier_2, tier_3, tier_{1..3}_active,
// upgrade_premium.
type TwoDayOffer struct {
	Status             string                  // "hide" (idle) | tier_* (showable)
	ExpiresAt          time.Time               // raw expires_at (RFC3339|null → zero)
	ModalData          *TwoDayOfferModal       // nil when status=hide or modalData=null
	AllowedJobSetTypes []TwoDayOfferAllowedJST // JSTs unlocked by the offer; empty on idle
}

// TwoDayOfferModal is the modalData block of a showable two-day offer. Prices
// are in cents (bundle-confirmed hardcoded tiers: 1-day 900→500, 7-day
// 4900→2900, 14-day 8900→4900). Currency defaults to USD upstream. Features
// is a human-readable bullet list the SPA renders.
type TwoDayOfferModal struct {
	FinalPrice    int64    `json:"final_price"`
	OriginalPrice int64    `json:"original_price"`
	Currency      string   `json:"currency"`
	Features      []string `json:"features"`
}

// TwoDayOfferAllowedJST is one entry of allowed_job_set_types — a job-set-type
// the offer would unlock. Resolutions/MaxDuration are informational; the
// value-gate only inspects JobSetType.
type TwoDayOfferAllowedJST struct {
	JobSetType  string   `json:"job_set_type"`
	Resolution  string   `json:"resolution"`
	Resolutions []string `json:"resolutions"`
	MaxDuration int64    `json:"max_duration"`
}

// twoDayOfferRaw mirrors the live-confirmed raw /two-day-offer body. The
// SPA-only fields (quiz_type, is_card_visible, is_plan_visible,
// purchase_expires_at) are intentionally omitted — promowatch has no use for
// them.
type twoDayOfferRaw struct {
	Status             string                  `json:"status"`
	ExpiresAt          string                  `json:"expires_at"`
	ModalData          *TwoDayOfferModal       `json:"modalData"`
	AllowedJobSetTypes []TwoDayOfferAllowedJST `json:"allowed_job_set_types"`
}

// FetchTwoDayOffer calls GET /two-day-offer and returns the account's
// two-day offer, or nil when the surface is absent (404/204). The idle state
// {"status":"hide",...} is returned as a real value so callers can inspect
// the status rather than mistaking it for "no offer".
func (c *Client) FetchTwoDayOffer(ctx context.Context, account *domain.Account) (*TwoDayOffer, error) {
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/two-day-offer", nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_two_day_offer", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, nil // no two-day-offer surface — benign
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("two-day-offer: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var r twoDayOfferRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.Status == "" {
		return nil, nil
	}
	return &TwoDayOffer{
		Status:             r.Status,
		ExpiresAt:          parseUnlimTime(r.ExpiresAt),
		ModalData:          r.ModalData,
		AllowedJobSetTypes: r.AllowedJobSetTypes,
	}, nil
}

// CreditLedgerStatistics mirrors GET /workspaces/credit-ledger/statistics.
// The upstream endpoint returns an aggregate over a [start_date, end_date]
// window (inclusive on both ends per higgsfield convention). It is the
// canonical "how much did this account actually spend upstream" source and
// is what the monthly reconciler (core/creditrecon) compares against the
// locally-recorded usage_events.charged_credits_h sum.
//
// Units are "credits" (integer), same as /workspaces/wallet.credits_balance;
// higgsgo's internal charged_credits_h column stores hundredths (×100) —
// callers must multiply the upstream credit count by 100 before comparing.
//
// Extra fields (spending_by_model, ...) exposed by upstream are ignored:
// the reconciler only needs the aggregate totals.
type CreditLedgerStatistics struct {
	TotalCreditsSpent    int64  `json:"total_credits_spent"`
	TotalCreditsRefunded int64  `json:"total_credits_refunded"`
	JobsCreated          int64  `json:"jobs_created"`
	Currency             string `json:"currency"`
}

// FetchCreditLedgerStatistics calls GET /workspaces/credit-ledger/statistics
// with the given [start_date, end_date] window (each ISO YYYY-MM-DD). An
// empty 200 body ("{}") returns a zero-valued struct (not nil) so the
// reconciler can compare against a well-defined "no upstream spend" number
// without extra nil-guards on the caller side.
func (c *Client) FetchCreditLedgerStatistics(ctx context.Context, account *domain.Account, startDate, endDate string) (*CreditLedgerStatistics, error) {
	// Query string is trivial (YYYY-MM-DD) so string concatenation matches
	// the rest of the file's style; no url.Values allocation needed.
	path := c.baseURL + "/workspaces/credit-ledger/statistics?start_date=" + startDate + "&end_date=" + endDate
	build := func(token string) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		c.setStdHeaders(req, account, token, false)
		return req, nil
	}
	resp, err := c.doWithRetry(ctx, "fetch_credit_ledger_statistics", account, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(raw))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("credit_ledger_statistics: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	// Empty body or {} — return a zero-valued struct so callers can proceed
	// without a nil-check (the reconciler needs a real "upstream says 0 credits
	// spent" answer, which is meaningfully different from "no data").
	if len(raw) == 0 {
		return &CreditLedgerStatistics{}, nil
	}
	var s CreditLedgerStatistics
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
