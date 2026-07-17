// Package jwt mints and caches Clerk session JWTs for higgsfield accounts.
//
// Clerk-issued JWTs expire after 60 seconds. The persistent session cookie
// (__session / __client) lives 7-30 days, so we can mint a fresh JWT on
// demand by POSTing to clerk.higgsfield.ai with the cookie attached.
//
// The cache serializes concurrent mint requests per account (no thundering
// herd) and returns the cached token when it still has slackSeconds of life
// remaining.
package jwt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Claims is the subset of Clerk JWT claims we care about.
type Claims struct {
	Sub         string `json:"sub"`
	Email       string `json:"email"`
	Exp         int64  `json:"exp"`
	Iat         int64  `json:"iat"`
	WorkspaceID string `json:"workspace_id"`
}

// Token is a JWT together with its parsed claims.
type Token struct {
	JWT    string
	Claims Claims
}

// Expiry returns the JWT's exp claim as a time.Time.
func (t Token) Expiry() time.Time { return time.Unix(t.Claims.Exp, 0) }

// Config controls the Minter.
type Config struct {
	// SlackSeconds is the safety window before expiry — a cached token
	// with less than SlackSeconds of life will be refreshed. Default 15.
	SlackSeconds int

	// APIVersion / JSVersion are Clerk query-string parameters. They can
	// stay defaulted unless higgsfield rotates them.
	APIVersion string
	JSVersion  string
}

func (c *Config) applyDefaults() {
	if c.SlackSeconds == 0 {
		c.SlackSeconds = 15
	}
	if c.APIVersion == "" {
		c.APIVersion = "2025-11-10"
	}
	if c.JSVersion == "" {
		c.JSVersion = "5.127.0"
	}
}

// Minter mints fresh Clerk JWTs and caches them per account.
//
// It uses an UpstreamClient to talk to clerk.higgsfield.ai; the same client
// should be used across the app so the JA3 fingerprint stays consistent.
type Minter struct {
	client ports.UpstreamClient
	clock  ports.Clock
	cfg    Config

	mu    sync.Mutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	mu    sync.Mutex
	token Token
}

// New returns a Minter that mints via the given HTTP client.
func New(client ports.UpstreamClient, clock ports.Clock, cfg Config) *Minter {
	cfg.applyDefaults()
	return &Minter{
		client: client,
		clock:  clock,
		cache:  make(map[string]*cacheEntry),
		cfg:    cfg,
	}
}

// Get returns a valid JWT for the account, minting a fresh one when the
// cache is empty or the cached token is within SlackSeconds of expiry.
// Concurrent Get calls for the same account share a single mint attempt.
func (m *Minter) Get(ctx context.Context, acc *domain.Account) (Token, error) {
	if acc == nil {
		return Token{}, errors.New("jwt: nil account")
	}
	entry := m.getEntry(acc.ID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := m.clock.Now()
	if entry.token.JWT != "" {
		remaining := entry.token.Expiry().Sub(now)
		if remaining > time.Duration(m.cfg.SlackSeconds)*time.Second {
			return entry.token, nil
		}
	}

	tok, err := m.mint(ctx, acc)
	if err != nil {
		return Token{}, err
	}
	entry.token = tok
	return tok, nil
}

// Invalidate drops the cached token for an account. Next Get will re-mint.
func (m *Minter) Invalidate(accountID string) {
	entry := m.getEntry(accountID)
	entry.mu.Lock()
	entry.token = Token{}
	entry.mu.Unlock()
}

func (m *Minter) getEntry(id string) *cacheEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[id]
	if !ok {
		e = &cacheEntry{}
		m.cache[id] = e
	}
	return e
}

// mint performs the actual clerk.higgsfield.ai POST. Callers must hold the
// entry lock.
func (m *Minter) mint(ctx context.Context, acc *domain.Account) (Token, error) {
	if acc.SessionID == "" {
		return Token{}, fmt.Errorf("jwt: account %s missing session_id", acc.ID)
	}
	if acc.CookiesJSON == "" {
		return Token{}, fmt.Errorf("jwt: account %s missing cookies", acc.ID)
	}

	url := fmt.Sprintf(
		"https://clerk.higgsfield.ai/v1/client/sessions/%s/tokens?__clerk_api_version=%s&_clerk_js_version=%s",
		acc.SessionID, m.cfg.APIVersion, m.cfg.JSVersion,
	)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("organization_id="))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://higgsfield.ai")
	req.Header.Set("Referer", "https://higgsfield.ai/")
	if acc.UserAgent != "" {
		req.Header.Set("User-Agent", acc.UserAgent)
	}
	cookieHeader, err := buildCookieHeader(acc.CookiesJSON)
	if err != nil {
		return Token{}, fmt.Errorf("cookie header: %w", err)
	}
	req.Header.Set("Cookie", cookieHeader)

	resp, err := m.client.Do(ctx, req)
	if err != nil {
		return Token{}, fmt.Errorf("mint jwt: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		return Token{}, fmt.Errorf("%w: %s", domain.ErrUpstreamUnauthorized, snip(body, 200))
	}
	if resp.StatusCode >= 400 {
		return Token{}, fmt.Errorf("mint jwt: HTTP %d: %s", resp.StatusCode, snip(body, 200))
	}

	var raw struct {
		JWT string `json:"jwt"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Token{}, fmt.Errorf("parse jwt response: %w", err)
	}
	if raw.JWT == "" {
		return Token{}, errors.New("mint jwt: empty jwt in response")
	}

	claims, err := decodeClaims(raw.JWT)
	if err != nil {
		return Token{}, fmt.Errorf("decode jwt claims: %w", err)
	}
	return Token{JWT: raw.JWT, Claims: claims}, nil
}

// decodeClaims parses the payload segment of a JWT (base64-url, unverified).
func decodeClaims(jwt string) (Claims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("jwt: expected 3 segments")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some tokens use standard base64 with padding; retry with StdEncoding.
		raw2, err2 := base64.StdEncoding.DecodeString(parts[1])
		if err2 != nil {
			return Claims{}, err
		}
		raw = raw2
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// buildCookieHeader converts the JSON blob stored in accounts.cookies_json
// into a single Cookie header value.
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

func snip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
