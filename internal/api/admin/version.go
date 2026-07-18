package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/version"
)

// VersionHandler serves /admin/version endpoints. It reports the running
// binary's build metadata and (optionally) polls GitHub Releases to surface
// an "update available" hint to the WebUI.
//
// The GitHub check is intentionally NOT a background ticker: the pool
// deployment is single-process, and the WebUI only asks for this on load
// and manual refresh — so a lazy in-process cache is enough. The cache
// avoids hammering the anonymous GitHub rate limit (60/h) when someone
// hard-refreshes.
type VersionHandler struct {
	// GitHubOwner / GitHubRepo pick the repo whose latest release we
	// poll. Left empty they disable the outbound call (Check returns
	// the current version with update_available=false).
	GitHubOwner string
	GitHubRepo  string

	// CheckEnabled short-circuits Check to a no-network response when
	// false. Used when the operator wants to keep the endpoint mounted
	// for the WebUI plumbing but not talk to github.com at all.
	CheckEnabled bool

	// HTTPClient is a plain net/http client scoped to this handler.
	// Kept configurable so the tests can point it at httptest.
	HTTPClient *http.Client

	// GitHubBaseURL overrides https://api.github.com (tests).
	GitHubBaseURL string

	// CacheTTL controls how long a successful upstream response is
	// reused. Defaults to 1h; tests set it lower.
	CacheTTL time.Duration

	mu    sync.Mutex
	cache versionCacheEntry
}

type versionCacheEntry struct {
	body   *checkResponse
	expiry time.Time
}

// NewVersionHandler builds a VersionHandler with production defaults.
func NewVersionHandler(owner, repo string, checkEnabled bool) *VersionHandler {
	return &VersionHandler{
		GitHubOwner:  owner,
		GitHubRepo:   repo,
		CheckEnabled: checkEnabled,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		GitHubBaseURL: "https://api.github.com",
		CacheTTL:      1 * time.Hour,
	}
}

// Register mounts the routes under /admin/version.
func (h *VersionHandler) Register(r chi.Router) {
	r.Get("/version", h.Current)
	r.Get("/version/check", h.Check)
}

// Current returns the running binary's build metadata. Cheap and offline —
// no upstream calls, no cache. Auth: /admin bearer (mounted under /admin).
func (h *VersionHandler) Current(w http.ResponseWriter, r *http.Request) {
	info := version.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    info.Version,
		"commit":     info.Commit,
		"build_time": info.BuildTime,
		"go_version": runtime.Version(),
		"os_arch":    runtime.GOOS + "/" + runtime.GOARCH,
	})
}

// checkResponse is the shape returned by Check. Exported field names would
// leak into JSON so we use lowercase tags to keep the wire schema stable
// even if the struct is renamed.
type checkResponse struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	Dev             bool   `json:"dev"`
	Error           string `json:"error,omitempty"`
}

// githubReleaseAPI is the minimal projection of api.github.com/repos/.../releases/latest.
type githubReleaseAPI struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

// Check reports whether a newer release exists on GitHub. It caches the
// upstream response in-process for CacheTTL to avoid triggering the anonymous
// rate limit. On any upstream error (timeout, 5xx, malformed body) it
// returns 200 with an "error" field so the WebUI badge degrades to
// "unknown" rather than "broken".
func (h *VersionHandler) Check(w http.ResponseWriter, r *http.Request) {
	info := version.Info()

	// Dev short-circuit: no injected tag means we can't tell what "newer"
	// even means, and we don't want to spam GitHub from every dev laptop.
	if info.Version == "dev" {
		writeJSON(w, http.StatusOK, checkResponse{
			Current:         "dev",
			Dev:             true,
			UpdateAvailable: false,
		})
		return
	}

	if !h.CheckEnabled || h.GitHubOwner == "" || h.GitHubRepo == "" {
		writeJSON(w, http.StatusOK, checkResponse{
			Current:         info.Version,
			Dev:             false,
			UpdateAvailable: false,
		})
		return
	}

	// Cache check.
	h.mu.Lock()
	if h.cache.body != nil && time.Now().Before(h.cache.expiry) {
		body := *h.cache.body
		h.mu.Unlock()
		// Refresh Current in case the running version was changed
		// out from under us (e.g. tests) — the cached upstream data
		// stays valid even if we compare against a different tag.
		body.Current = info.Version
		body.UpdateAvailable = version.IsNewer(body.Latest, info.Version)
		writeJSON(w, http.StatusOK, body)
		return
	}
	h.mu.Unlock()

	body := h.fetchLatest(r.Context(), info.Version)

	// Cache successful lookups only — errors should be retried on the
	// next request so we don't paper over a transient outage for a
	// full hour.
	if body.Error == "" {
		h.mu.Lock()
		h.cache = versionCacheEntry{
			body:   &body,
			expiry: time.Now().Add(h.cacheTTL()),
		}
		h.mu.Unlock()
	}

	writeJSON(w, http.StatusOK, body)
}

func (h *VersionHandler) cacheTTL() time.Duration {
	if h.CacheTTL > 0 {
		return h.CacheTTL
	}
	return 1 * time.Hour
}

// fetchLatest issues the GET to api.github.com/repos/{owner}/{repo}/releases/latest.
// It ALWAYS returns a populated checkResponse — network failures land as
// error="upstream_unavailable" rather than propagating out to the caller,
// so the JSON envelope is stable regardless of GitHub's mood.
func (h *VersionHandler) fetchLatest(ctx context.Context, current string) checkResponse {
	base := h.GitHubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	url := base + "/repos/" + h.GitHubOwner + "/" + h.GitHubRepo + "/releases/latest"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return checkResponse{Current: current, Error: "upstream_unavailable"}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "higgsgo/"+current)

	client := h.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return checkResponse{Current: current, Error: "upstream_unavailable"}
	}
	defer resp.Body.Close()

	// Anything above 299 (rate limit → 403, missing repo → 404,
	// GitHub outage → 5xx) is a soft failure. The badge should just
	// go dark, not red.
	if resp.StatusCode >= 300 {
		return checkResponse{Current: current, Error: "upstream_unavailable"}
	}

	var payload githubReleaseAPI
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return checkResponse{Current: current, Error: "upstream_unavailable"}
	}
	if payload.TagName == "" {
		return checkResponse{Current: current, Error: "upstream_unavailable"}
	}

	return checkResponse{
		Current:         current,
		Latest:          payload.TagName,
		UpdateAvailable: version.IsNewer(payload.TagName, current),
		ReleaseURL:      payload.HTMLURL,
		PublishedAt:     payload.PublishedAt,
	}
}
