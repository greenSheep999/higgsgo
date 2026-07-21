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
// Only fields the refresher needs are decoded — the SPA also uses
// `is_claimed` and pricing / seat data for its own modal, which the
// server doesn't care about.
type UnlimActivationRaw struct {
	ID          string                 `json:"id"`
	BundleType  string                 `json:"bundle_type"`
	Models      []UnlimActivationModel `json:"models"`
	ExpiresAt   string                 `json:"expires_at"`
	StartedAt   string                 `json:"started_at"`
	ActivatedAt string                 `json:"activated_at"`
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
				BundleType:  bundle,
				JobSetType:  m.JobSetType,
				Resolutions: append([]string(nil), m.Resolutions...),
				ExpiresAt:   expires,
				ActivatedAt: activated,
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
