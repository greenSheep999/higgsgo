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
	"net/http"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Client submits jobs to fnf.higgsfield.ai and polls their status.
type Client struct {
	http    ports.UpstreamClient
	jwt     *jwt.Minter
	baseURL string
}

// Config controls Client construction.
type Config struct {
	// BaseURL defaults to "https://fnf.higgsfield.ai" when empty.
	BaseURL string
}

// New builds a Client with the given HTTP client and JWT minter.
func New(httpClient ports.UpstreamClient, minter *jwt.Minter, cfg Config) *Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://fnf.higgsfield.ai"
	}
	return &Client{http: httpClient, jwt: minter, baseURL: strings.TrimRight(baseURL, "/")}
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
func (c *Client) CreateJob(ctx context.Context, r CreateRequest) (*CreateResponse, error) {
	tok, err := c.jwt.Get(ctx, r.Account)
	if err != nil {
		return nil, fmt.Errorf("mint jwt: %w", err)
	}

	body, err := json.Marshal(r.Body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+r.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setStdHeaders(req, r.Account, tok.JWT, true)

	resp, err := c.http.Do(ctx, req)
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
	tok, err := c.jwt.Get(ctx, account)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/jobs/"+jobID+"/status", nil)
	if err != nil {
		return nil, err
	}
	c.setStdHeaders(req, account, tok.JWT, false)
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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
	tok, err := c.jwt.Get(ctx, account)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/jobs/"+jobID, nil)
	if err != nil {
		return nil, err
	}
	c.setStdHeaders(req, account, tok.JWT, false)
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
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

// FetchWallet calls GET /workspaces/wallet.
func (c *Client) FetchWallet(ctx context.Context, account *domain.Account) (*Wallet, error) {
	tok, err := c.jwt.Get(ctx, account)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/workspaces/wallet", nil)
	if err != nil {
		return nil, err
	}
	c.setStdHeaders(req, account, tok.JWT, false)
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("wallet: HTTP %d: %s", resp.StatusCode, snip(raw))
	}
	var w Wallet
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	return &w, nil
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
			ID   string `json:"id"`
			Cost int64  `json:"cost"`
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
	return &CreateResponse{
		JobSetID:   body.JobSets[0].ID,
		JobID:      body.JobSets[0].Jobs[0].ID,
		Cost:       body.JobSets[0].Cost,
		Raw:        raw,
		HTTPStatus: httpStatus,
	}, nil
}
