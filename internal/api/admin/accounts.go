package admin

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// AccountsHandler serves /admin/accounts endpoints. It never leaks
// password_enc / cookies_json values back to clients — those columns are
// sensitive at rest and only the pool internals should ever read them.
// Registry is optional; when set, /accounts/{id}/eligible-models can
// answer which models the account is entitled to run.
type AccountsHandler struct {
	Accounts ports.AccountStore
	Registry ports.ModelRegistry
	// Prober is optional; when set, POST /accounts/{id}/probe actively
	// pings the account through the upstream client. Nil = the endpoint
	// answers 503 probe_disabled, distinct from "500 something broke".
	// The upstream.Client's method set satisfies Prober directly.
	Prober Prober
}

// NewAccountsHandler wires an AccountsHandler over the given store.
func NewAccountsHandler(a ports.AccountStore) *AccountsHandler {
	return &AccountsHandler{Accounts: a}
}

// Register mounts the routes under /admin/accounts.
func (h *AccountsHandler) Register(r chi.Router) {
	r.Get("/accounts", h.List)
	// /accounts/export is registered ahead of /accounts/{id} so a request
	// for "export" resolves to the static handler rather than looking up an
	// account with id="export". chi v5 already prefers static routes, but
	// declaring them in this order keeps the intent obvious.
	r.Get("/accounts/export", h.Export)
	r.Post("/accounts", h.Import)
	r.Get("/accounts/{id}", h.Get)
	r.Patch("/accounts/{id}", h.Patch)
	r.Post("/accounts/{id}/pause", h.Pause)
	r.Post("/accounts/{id}/resume", h.Resume)
	r.Delete("/accounts/{id}", h.SoftDelete)
	r.Get("/accounts/{id}/eligible-models", h.EligibleModels)
	// /accounts/{id}/probe actively pings the account through the
	// upstream client. Nil h.Prober answers 503 probe_disabled so the
	// WebUI can distinguish "not configured" from "call failed".
	r.Post("/accounts/{id}/probe", h.Probe)
}

// EligibleModels returns the models this account is entitled to run,
// based on its plan_type + unlim/flex flags. Combines domain rules
// (PlanType.IsPaid; account.HasUnlim; account.HasFlexUnlim) with the
// model registry's per-spec gate flags (RequiresPaid / RequiresUltra
// / RequiresUnlim / StarterLocked). Read-only; expensive computation
// runs entirely in memory over the registry.
//
// Returns 503 when the registry is not wired.
func (h *AccountsHandler) EligibleModels(w http.ResponseWriter, r *http.Request) {
	if h.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model registry not configured")
		return
	}
	id := chi.URLParam(r, "id")
	acc, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	all := h.Registry.List(ports.ModelFilter{})
	eligible := make([]map[string]any, 0, len(all))
	var eligibleImage, eligibleVideo, eligibleAudio int
	for _, m := range all {
		ok, reason := accountCanRun(acc, m)
		if !ok {
			continue
		}
		_ = reason
		eligible = append(eligible, map[string]any{
			"alias":     m.Alias,
			"jst":       m.JST,
			"output":    m.Output,
			"est_cost":  float64(m.EstCostHundredths) / 100.0,
			"unstable":  m.Unstable,
		})
		switch m.Output {
		case "image":
			eligibleImage++
		case "video":
			eligibleVideo++
		case "audio":
			eligibleAudio++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":  acc.ID,
		"total":       len(all),
		"eligible":    len(eligible),
		"by_output": map[string]int{
			"image": eligibleImage,
			"video": eligibleVideo,
			"audio": eligibleAudio,
		},
		"data": eligible,
	})
}

// accountCanRun encodes the same eligibility rules the pool router uses:
//   - MinPlan       → account plan must meet the explicit floor
//   - StarterLocked model requires a paid tier (PlanType.IsPaid())
//   - RequiresPaid  → PlanType.IsPaid()
//   - RequiresUltra → PlanType in {ultra, ultimate, scale, creator, team, enterprise}
//   - RequiresUnlim → HasUnlim (any flavour)
// Returns (true, "") when the account can run the model, or
// (false, reason) with a short human-readable reason otherwise.
func accountCanRun(a *domain.Account, m *domain.ModelSpec) (bool, string) {
	if m.Deprecated {
		return false, "deprecated"
	}
	if !a.PlanType.MeetsMinimum(m.MinPlan) {
		return false, "min_plan"
	}
	if m.StarterLocked && !a.PlanType.IsPaid() {
		return false, "starter_locked"
	}
	if m.RequiresPaid && !a.PlanType.IsPaid() {
		return false, "requires_paid"
	}
	if m.RequiresUltra {
		switch a.PlanType {
		case domain.PlanUltra, domain.PlanUltimate, domain.PlanScale,
			domain.PlanCreator, domain.PlanTeam, domain.PlanEnt:
			// ok
		default:
			return false, "requires_ultra"
		}
	}
	if m.RequiresUnlim && !a.HasUnlim && !a.HasFlexUnlim {
		return false, "requires_unlim"
	}
	return true, ""
}

// patchAccountRequest is the body shape for PATCH /admin/accounts/{id}.
// All fields are pointers so callers can send a partial update; a nil
// pointer leaves the existing value untouched.
type patchAccountRequest struct {
	Priority      *int    `json:"priority,omitempty"`
	BoundProxyURL *string `json:"bound_proxy_url,omitempty"`
	MaxConcurrent *int    `json:"max_concurrent,omitempty"`
	Note          *string `json:"note,omitempty"`
	Source        *string `json:"source,omitempty"`
}

// priorityRange bounds the operator-managed sort hint so a typo can't
// send the row to Int.MaxValue and monopolise the pool router.
const (
	priorityMin = -1000
	priorityMax = 1000
)

// Patch performs a partial update on the mutable admin-managed columns
// of an account row (priority, bound_proxy_url). Balances, entitlements
// and status flow through their own endpoints so a caller cannot leak
// unrelated state through this generic PATCH.
func (h *AccountsHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	existing, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req patchAccountRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}

	if req.Priority != nil {
		if *req.Priority < priorityMin || *req.Priority > priorityMax {
			writeErr(w, http.StatusBadRequest, "invalid_body",
				fmt.Sprintf("priority must be in [%d, %d]", priorityMin, priorityMax))
			return
		}
		existing.Priority = *req.Priority
	}
	if req.BoundProxyURL != nil {
		existing.BoundProxyURL = *req.BoundProxyURL
	}
	if req.MaxConcurrent != nil {
		if *req.MaxConcurrent < 0 || *req.MaxConcurrent > 20 {
			writeErr(w, http.StatusBadRequest, "invalid_body",
				"max_concurrent must be in [0, 20]")
			return
		}
		existing.MaxConcurrent = *req.MaxConcurrent
	}
	if req.Note != nil {
		existing.Note = *req.Note
	}
	if req.Source != nil {
		existing.Source = *req.Source
	}

	// Upsert rewrites every column, so the two writes above collide with
	// nothing else — the entitlement / balance refresher goroutines run on
	// different columns and don't race with this handler for the fields
	// we touch here.
	if err := h.Accounts.Upsert(r.Context(), existing); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountView(existing))
}

// List returns all accounts, optionally filtered by ?plan_type=, ?status=,
// ?min_balance= query parameters. Sensitive fields are stripped.
func (h *AccountsHandler) List(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseAccountFilter(w, r)
	if !ok {
		return
	}
	rows, err := h.Accounts.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, accountView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// parseAccountFilter pulls plan_type / status / min_balance from the query
// string. On invalid input it writes a 400 and returns ok=false so the
// caller can bail without hitting the store.
func parseAccountFilter(w http.ResponseWriter, r *http.Request) (ports.AccountFilter, bool) {
	q := r.URL.Query()
	filter := ports.AccountFilter{
		PlanType: domain.PlanType(q.Get("plan_type")),
		Status:   domain.AccountStatus(q.Get("status")),
	}
	if raw := q.Get("min_balance"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "min_balance must be an integer")
			return ports.AccountFilter{}, false
		}
		filter.MinBalance = v
	}
	return filter, true
}

// Get returns one account by id (sensitive fields stripped).
func (h *AccountsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountView(a))
}

// Pause flips the account status to "suspended". The pool skips suspended
// accounts on PickAndLock, so this is the safe way to take an account out of
// rotation without losing its credentials.
func (h *AccountsHandler) Pause(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusSuspended, "manual pause")
}

// Resume flips the account status back to "active".
func (h *AccountsHandler) Resume(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusActive, "manual resume")
}

// SoftDelete flips the account status to "banned". Rows are never physically
// removed — this keeps the audit trail (jobs, usage_events) intact.
func (h *AccountsHandler) SoftDelete(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusBanned, "manual delete")
}

func (h *AccountsHandler) markStatus(w http.ResponseWriter, r *http.Request, status domain.AccountStatus, reason string) {
	id := chi.URLParam(r, "id")
	if err := h.Accounts.MarkStatus(r.Context(), id, status, reason); err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": string(status),
		"reason": reason,
	})
}

// --- Import (POST /admin/accounts) --------------------------------------
//
// Import accepts one of three payload formats keyed by the top-level
// "format" field:
//
//   - "session_paste": raw fields captured from a browser session (email,
//     user_id, session_id, cookies_json, user_agent, x_datadome_clientid,
//     plan/credits).
//   - "higgsfield_register_json": the JSON shape emitted by the Node
//     higgsfield-register tool (see scripts/import-node-accounts). Accepts
//     both an object payload under "raw" and a JSON-encoded string.
//   - "raw_cookies": a Chrome "Cookie" header string plus an email. The
//     handler splits the header into name=value pairs and reverse-engineers
//     the Clerk session_id from clerk_active_context / __client / any
//     sess_-prefixed cookie value.
//
// Conflict handling: by default (?upsert=false, the omitted default) an
// existing account is rejected with 409. Passing ?upsert=true forces the
// import to overwrite the existing row via AccountStore.Upsert.

// importRequest is the outer envelope for POST /admin/accounts. Only the
// discriminator lives here; each sub-format decodes the raw body a second
// time into its own struct.
type importRequest struct {
	Format string `json:"format"`
}

// sessionPastePayload is the "session_paste" body shape.
type sessionPastePayload struct {
	Format              string          `json:"format"`
	Email               string          `json:"email"`
	UserID              string          `json:"user_id"`
	SessionID           string          `json:"session_id"`
	WorkspaceID         string          `json:"workspace_id"`
	CookiesJSON         json.RawMessage `json:"cookies_json"`
	UserAgent           string          `json:"user_agent"`
	XDataDomeClientID   string          `json:"x_datadome_clientid"`
	PlanType            string          `json:"plan_type"`
	CreditsBalance      int64           `json:"credits_balance"`
	SubscriptionBalance int64           `json:"subscription_balance"`
	TotalCredits        int64           `json:"total_credits"`
	Password            string          `json:"password"`
}

// higgsfieldRegisterPayload is the "higgsfield_register_json" body shape.
// Raw is decoded lazily because it may be either an object or a JSON string.
type higgsfieldRegisterPayload struct {
	Format string          `json:"format"`
	Raw    json.RawMessage `json:"raw"`
}

// rawCookiesPayload is the "raw_cookies" body shape.
type rawCookiesPayload struct {
	Format            string `json:"format"`
	Email             string `json:"email"`
	CookiesHeader     string `json:"cookies_header"`
	UserAgent         string `json:"user_agent"`
	XDataDomeClientID string `json:"x_datadome_clientid"`
	PlanType          string `json:"plan_type"`
	UserID            string `json:"user_id"`
}

// nodeRegisterJSON mirrors the schema emitted by higgsfield-register (see
// scripts/import-node-accounts). Kept in-package (not exported) so this
// handler stays self-contained and the CLI importer can keep its copy.
type nodeRegisterJSON struct {
	Type              string            `json:"type"`
	Email             string            `json:"email"`
	Password          string            `json:"password"`
	UserID            string            `json:"user_id"`
	SessionID         string            `json:"session_id"`
	PlanType          string            `json:"plan_type"`
	Cookies           map[string]string `json:"cookies"`
	XDataDomeClientID string            `json:"x_datadome_clientid"`
	CapturedUserAgent string            `json:"captured_user_agent"`
	ImportedAt        string            `json:"imported_at"`
	CreditsSnapshot   struct {
		SubscriptionCredits float64 `json:"subscription_credits"`
		PackageCredits      float64 `json:"package_credits"`
		DailyCredits        float64 `json:"daily_credits"`
		TotalPlanCredits    float64 `json:"total_plan_credits"`
		CapturedAt          string  `json:"captured_at"`
	} `json:"credits_snapshot"`
}

// Import creates an account from one of the supported payload formats.
// See the block comment above for the wire schema of each format.
func (h *AccountsHandler) Import(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "request body is empty")
		return
	}
	var head importRequest
	if err := json.Unmarshal(raw, &head); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	var (
		acct     *domain.Account
		parseErr error
	)
	switch head.Format {
	case "session_paste":
		acct, parseErr = parseSessionPaste(raw)
	case "higgsfield_register_json":
		acct, parseErr = parseHiggsfieldRegisterJSON(raw)
	case "raw_cookies":
		acct, parseErr = parseRawCookies(raw)
	case "":
		writeErr(w, http.StatusBadRequest, "invalid_body", "format is required")
		return
	default:
		writeErr(w, http.StatusBadRequest, "invalid_body", fmt.Sprintf("unknown format %q", head.Format))
		return
	}
	if parseErr != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", parseErr.Error())
		return
	}

	upsert := r.URL.Query().Get("upsert") == "true"
	if !upsert {
		existing, err := h.Accounts.Get(r.Context(), acct.ID)
		if err != nil && !errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if existing != nil {
			writeErr(w, http.StatusConflict, "already_exists",
				fmt.Sprintf("account %s already imported", acct.ID))
			return
		}
	}

	if err := h.Accounts.Upsert(r.Context(), acct); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          acct.ID,
		"email":       acct.Email,
		"plan_type":   string(acct.PlanType),
		"imported_at": acct.ImportedAt.UTC().Format(time.RFC3339),
	})
}

// parseSessionPaste validates and converts a "session_paste" body into a
// domain.Account. session_id / cookies_json / user_agent / datadome are all
// mandatory since without them the pool cannot mint a JWT nor pass upstream
// bot checks.
func parseSessionPaste(raw []byte) (*domain.Account, error) {
	var p sessionPastePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.UserID == "" {
		return nil, fmt.Errorf("user_id is required")
	}
	if p.Email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if len(p.CookiesJSON) == 0 || bytesTrim(p.CookiesJSON) == "null" {
		return nil, fmt.Errorf("cookies_json is required")
	}
	// Verify cookies_json is a JSON object, not just any blob.
	var probe map[string]any
	if err := json.Unmarshal(p.CookiesJSON, &probe); err != nil {
		return nil, fmt.Errorf("cookies_json must be a JSON object: %w", err)
	}
	if len(probe) == 0 {
		return nil, fmt.Errorf("cookies_json is empty")
	}
	if p.UserAgent == "" {
		return nil, fmt.Errorf("user_agent is required")
	}
	if p.XDataDomeClientID == "" {
		return nil, fmt.Errorf("x_datadome_clientid is required")
	}

	now := time.Now().UTC()
	return &domain.Account{
		ID:                  p.UserID,
		Email:               p.Email,
		Password:            p.Password,
		SessionID:           p.SessionID,
		WorkspaceID:         p.WorkspaceID,
		CookiesJSON:         string(p.CookiesJSON),
		UserAgent:           p.UserAgent,
		DataDomeClientID:    p.XDataDomeClientID,
		PlanType:            domain.PlanType(p.PlanType),
		SubscriptionBalance: p.SubscriptionBalance,
		CreditsBalance:      p.CreditsBalance,
		TotalPlanCredits:    p.TotalCredits,
		Status:              domain.StatusActive,
		RegisteredAt:        now,
		ImportedAt:          now,
	}, nil
}

// parseHiggsfieldRegisterJSON handles the "higgsfield_register_json" format.
// The "raw" field may be either a JSON object or a JSON-encoded string; both
// shapes are unwrapped to the nodeRegisterJSON struct below.
func parseHiggsfieldRegisterJSON(raw []byte) (*domain.Account, error) {
	var p higgsfieldRegisterPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Raw) == 0 {
		return nil, fmt.Errorf("raw is required")
	}
	// If Raw is a JSON string, decode once to get the inner JSON bytes.
	body := []byte(p.Raw)
	trimmed := bytesTrim(p.Raw)
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(p.Raw, &s); err != nil {
			return nil, fmt.Errorf("raw string decode: %w", err)
		}
		body = []byte(s)
	}
	var na nodeRegisterJSON
	if err := json.Unmarshal(body, &na); err != nil {
		return nil, fmt.Errorf("raw decode: %w", err)
	}
	if na.Type != "" && na.Type != "higgsfield" {
		return nil, fmt.Errorf("unexpected raw.type=%q; want \"higgsfield\"", na.Type)
	}
	if na.UserID == "" {
		return nil, fmt.Errorf("raw.user_id is required")
	}
	if na.Email == "" {
		return nil, fmt.Errorf("raw.email is required")
	}
	if na.SessionID == "" {
		return nil, fmt.Errorf("raw.session_id is required")
	}
	if len(na.Cookies) == 0 {
		return nil, fmt.Errorf("raw.cookies is required")
	}
	if na.CapturedUserAgent == "" {
		return nil, fmt.Errorf("raw.captured_user_agent is required")
	}
	if na.XDataDomeClientID == "" {
		return nil, fmt.Errorf("raw.x_datadome_clientid is required")
	}

	cookiesJSON, err := json.Marshal(na.Cookies)
	if err != nil {
		cookiesJSON = []byte("{}")
	}
	// Credits arrive as floats in the higgsfield credits units — the storage
	// layer keeps them in credits*100 to avoid float drift.
	subH := int64(na.CreditsSnapshot.SubscriptionCredits * 100)
	pkgH := int64(na.CreditsSnapshot.PackageCredits * 100)
	totalH := int64(na.CreditsSnapshot.TotalPlanCredits * 100)

	importedAt := parseImportTime(na.ImportedAt)
	if importedAt.IsZero() {
		importedAt = time.Now().UTC()
	}
	regAt := parseImportTime(na.CreditsSnapshot.CapturedAt)
	if regAt.IsZero() {
		regAt = importedAt
	}

	return &domain.Account{
		ID:                  na.UserID,
		Email:               na.Email,
		Password:            na.Password,
		SessionID:           na.SessionID,
		CookiesJSON:         string(cookiesJSON),
		UserAgent:           na.CapturedUserAgent,
		DataDomeClientID:    na.XDataDomeClientID,
		PlanType:            domain.PlanType(na.PlanType),
		SubscriptionBalance: subH,
		CreditsBalance:      pkgH,
		TotalPlanCredits:    totalH,
		Status:              domain.StatusActive,
		RegisteredAt:        regAt,
		ImportedAt:          importedAt,
	}, nil
}

// parseRawCookies handles a Chrome "Cookie" header string. The header can
// arrive either with the "Cookie: " prefix (as DevTools copies it) or as a
// bare "name=val; name2=val2" list. The session_id is reverse-engineered
// from clerk_active_context (leading "sess_XXX:...") first, then from any
// cookie value with the "sess_" prefix; failing that we surface a 400.
func parseRawCookies(raw []byte) (*domain.Account, error) {
	var p rawCookiesPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if p.CookiesHeader == "" {
		return nil, fmt.Errorf("cookies_header is required")
	}

	cookies := parseCookieHeader(p.CookiesHeader)
	if len(cookies) == 0 {
		return nil, fmt.Errorf("cookies_header parsed to zero cookies")
	}

	sessionID := extractSessionID(cookies)
	if sessionID == "" {
		return nil, fmt.Errorf("cookies_header does not carry a Clerk session id " +
			"(looked at clerk_active_context, __client, and any sess_ prefixed value)")
	}

	userID := p.UserID
	if userID == "" {
		// Best-effort: use the session id as the account id when the caller
		// did not supply one. The operator can update it later once /user is
		// hit by the balance refresher; treating session_id as a stable local
		// id is fine because we deduplicate on ID = clerk user_id when the
		// caller provides it.
		userID = sessionID
	}

	dd := p.XDataDomeClientID
	if dd == "" {
		dd = cookies["datadome"]
	}

	cookiesJSON, err := json.Marshal(cookies)
	if err != nil {
		cookiesJSON = []byte("{}")
	}
	now := time.Now().UTC()
	return &domain.Account{
		ID:               userID,
		Email:            p.Email,
		SessionID:        sessionID,
		CookiesJSON:      string(cookiesJSON),
		UserAgent:        p.UserAgent,
		DataDomeClientID: dd,
		PlanType:         domain.PlanType(p.PlanType),
		Status:           domain.StatusActive,
		RegisteredAt:     now,
		ImportedAt:       now,
	}, nil
}

// parseCookieHeader splits a Chrome-style "Cookie" header into a map. The
// optional "Cookie: " prefix is stripped. Values are kept verbatim (no
// URL-decoding) so we can preserve the exact bytes Clerk / DataDome sent.
func parseCookieHeader(header string) map[string]string {
	s := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(s), "cookie:") {
		s = strings.TrimSpace(s[len("cookie:"):])
	}
	out := make(map[string]string)
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:eq])
		val := strings.TrimSpace(part[eq+1:])
		if name == "" {
			continue
		}
		out[name] = val
	}
	return out
}

// extractSessionID walks the cookie map for a Clerk session id. The
// canonical source is clerk_active_context (format "sess_XXX:optionalOrg");
// __client and any other value that starts with "sess_" are consulted as
// fallbacks so operators pasting partial cookies still get through.
func extractSessionID(cookies map[string]string) string {
	if v := cookies["clerk_active_context"]; v != "" {
		if id := extractSessPrefix(v); id != "" {
			return id
		}
	}
	if v := cookies["__client"]; v != "" {
		if id := extractSessPrefix(v); id != "" {
			return id
		}
	}
	for name, v := range cookies {
		if name == "clerk_active_context" || name == "__client" {
			continue
		}
		if id := extractSessPrefix(v); id != "" {
			return id
		}
	}
	return ""
}

// extractSessPrefix returns the substring that starts with "sess_" and
// runs until the next non [A-Za-z0-9] byte. Returns "" when no such
// substring exists.
func extractSessPrefix(s string) string {
	idx := strings.Index(s, "sess_")
	if idx < 0 {
		return ""
	}
	tail := s[idx:]
	end := len(tail)
	for i, r := range tail {
		if !(r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			end = i
			break
		}
	}
	id := tail[:end]
	if id == "sess_" {
		return ""
	}
	return id
}

// parseImportTime is a best-effort RFC3339-ish decoder for the mixed set of
// timestamp shapes the Node registrar can emit.
func parseImportTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// bytesTrim is a tiny helper that returns the trimmed form of a json.RawMessage
// as a string, so callers can cheaply inspect the first non-whitespace byte.
func bytesTrim(b []byte) string {
	return strings.TrimSpace(string(b))
}

// accountView is the public-safe representation of an Account. Password and
// cookies are never included; UA / DataDome / session id are also treated as
// secrets since together they let an attacker impersonate the account.
func accountView(a *domain.Account) map[string]any {
	v := map[string]any{
		"id":                   a.ID,
		"email":                a.Email,
		"workspace_id":         a.WorkspaceID,
		"plan_type":            string(a.PlanType),
		"has_unlim":            a.HasUnlim,
		"has_flex_unlim":       a.HasFlexUnlim,
		"is_pro_veo3":          a.IsProVeo3Available,
		"cohort":               a.Cohort,
		"subscription_balance": a.SubscriptionBalance,
		"credits_balance":      a.CreditsBalance,
		"total_plan_credits":   a.TotalPlanCredits,
		"status":               string(a.Status),
		"in_flight_jobs":       a.InFlightJobs,
		"fail_streak":          a.FailStreak,
		"bound_proxy_url":      a.BoundProxyURL,
		"priority":             a.Priority,
		"max_concurrent":       a.MaxConcurrent,
		"note":                 a.Note,
		"source":               a.Source,
	}
	if !a.PlanEndsAt.IsZero() {
		v["plan_ends_at"] = a.PlanEndsAt.UTC().Format(time.RFC3339)
	}
	if !a.LastBalanceAt.IsZero() {
		v["last_balance_at"] = a.LastBalanceAt.UTC().Format(time.RFC3339)
	}
	if !a.LastUsedAt.IsZero() {
		v["last_used_at"] = a.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if !a.LastFailedAt.IsZero() {
		v["last_failed_at"] = a.LastFailedAt.UTC().Format(time.RFC3339)
	}
	if !a.RegisteredAt.IsZero() {
		v["registered_at"] = a.RegisteredAt.UTC().Format(time.RFC3339)
	}
	if !a.ImportedAt.IsZero() {
		v["imported_at"] = a.ImportedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// --- Export (GET /admin/accounts/export) --------------------------------
//
// Export streams the account pool for offline backup / migration. The same
// filter grammar as List is supported (plan_type / status / min_balance).
// Three formats are offered — JSONL (default), JSON array, and CSV — and
// are selected via ?format=.
//
// The endpoint is *doubly* sensitive: the operator already has a valid
// admin bearer, but an exported file can be copied off-host and outlive
// its short-lived transport. By default we scrub the same secret fields
// that accountView omits: password_enc / cookies_json / session_id /
// user_agent / datadome_id. Callers that legitimately need the raw
// credentials — e.g. migrating to a new deployment — must opt in with
// ?include_secrets=true.

// accountExportBatchSize is the chunk size used when streaming account
// rows to the wire. AccountStore.List has no LIMIT / OFFSET surface, so
// we pull the full slice into memory once and then hand out sub-slices of
// this size. Kept small so the JSON encoder / CSV writer flushes cadence
// stays comparable to the streaming audit exporter.
const accountExportBatchSize = 500

// accountExportBaseCSVHeader is the ordered CSV header written for the
// no-secrets export. Column order matches the "CSV fields" list documented
// on the /admin/accounts/export endpoint.
var accountExportBaseCSVHeader = []string{
	"id", "email", "workspace_id", "plan_type",
	"has_unlim", "has_flex_unlim", "is_pro_veo3", "cohort",
	"subscription_balance", "credits_balance", "total_plan_credits",
	"status", "in_flight_jobs", "fail_streak",
	"plan_ends_at", "last_balance_at", "last_used_at", "last_failed_at",
	"registered_at", "imported_at",
}

// accountExportSecretCSVHeader is appended to the base header when the
// caller passes ?include_secrets=true. Keeping the secret columns at the
// end means downstream consumers of the safe format can splice on the
// extra columns without re-indexing their existing importers.
var accountExportSecretCSVHeader = []string{
	"session_id", "user_agent", "x_datadome_clientid", "cookies_json",
}

// Export serves GET /admin/accounts/export. Streams the account pool in
// JSONL (default), JSON array, or CSV form. Filter params match List
// (plan_type / status / min_balance). Sensitive fields (session_id,
// cookies_json, user_agent, x_datadome_clientid) are stripped unless the
// caller sets ?include_secrets=true.
func (h *AccountsHandler) Export(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "jsonl"
	}
	if format != "jsonl" && format != "json" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "invalid_query",
			"format must be one of: jsonl, json, csv")
		return
	}
	includeSecrets := r.URL.Query().Get("include_secrets") == "true"

	filter, ok := parseAccountFilter(w, r)
	if !ok {
		return
	}

	// AccountStore.List has no pagination surface; we accept that we hold
	// the full slice in memory once and chunk from there. The sqlite pool
	// tops out well below 10k rows in practice, so ~a few MB peak is fine.
	rows, err := h.Accounts.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	ext := format
	contentType := "application/x-ndjson"
	switch format {
	case "json":
		contentType = "application/json"
	case "csv":
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		"attachment; filename="+accountExportFilename(ext))
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	switch format {
	case "csv":
		writeAccountsExportCSV(w, rows, includeSecrets, flusher)
	case "json":
		writeAccountsExportJSONArray(w, rows, includeSecrets, flusher)
	case "jsonl":
		writeAccountsExportJSONL(w, rows, includeSecrets, flusher)
	}
}

// writeAccountsExportCSV walks rows in batches and emits one CSV row per
// account. The header is written first (with optional secret columns) so
// downstream tooling can pin column positions.
func writeAccountsExportCSV(w http.ResponseWriter, rows []domain.Account, includeSecrets bool, flusher http.Flusher) {
	cw := csv.NewWriter(w)
	header := accountExportBaseCSVHeader
	if includeSecrets {
		header = append(append([]string{}, accountExportBaseCSVHeader...), accountExportSecretCSVHeader...)
	}
	_ = cw.Write(header)
	for start := 0; start < len(rows); start += accountExportBatchSize {
		end := start + accountExportBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		for i := start; i < end; i++ {
			_ = cw.Write(accountExportCSVRow(&rows[i], includeSecrets))
		}
		cw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// writeAccountsExportJSONL emits one JSON object per line, batching flushes
// so a large export starts trickling to the client immediately.
func writeAccountsExportJSONL(w http.ResponseWriter, rows []domain.Account, includeSecrets bool, flusher http.Flusher) {
	enc := json.NewEncoder(w)
	for start := 0; start < len(rows); start += accountExportBatchSize {
		end := start + accountExportBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		for i := start; i < end; i++ {
			_ = enc.Encode(accountExportView(&rows[i], includeSecrets))
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// writeAccountsExportJSONArray emits a single JSON array. Elements are
// written one at a time with commas between them so the batch flush cadence
// still applies; the outer "[" / "]" bracket the whole payload.
func writeAccountsExportJSONArray(w http.ResponseWriter, rows []domain.Account, includeSecrets bool, flusher http.Flusher) {
	_, _ = w.Write([]byte("["))
	first := true
	for start := 0; start < len(rows); start += accountExportBatchSize {
		end := start + accountExportBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		for i := start; i < end; i++ {
			if !first {
				_, _ = w.Write([]byte(","))
			}
			first = false
			buf, err := json.Marshal(accountExportView(&rows[i], includeSecrets))
			if err != nil {
				// json.Marshal on a map[string]any of primitives cannot fail;
				// if it did the payload is already committed so we just drop
				// the row rather than corrupt the array.
				continue
			}
			_, _ = w.Write(buf)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("]"))
	if flusher != nil {
		flusher.Flush()
	}
}

// accountExportView is the JSON-shaped view of an account for export. It
// reuses accountView for the safe fields and layers on the secret columns
// only when the caller opted in via ?include_secrets=true.
func accountExportView(a *domain.Account, includeSecrets bool) map[string]any {
	v := accountView(a)
	if includeSecrets {
		v["session_id"] = a.SessionID
		v["user_agent"] = a.UserAgent
		v["x_datadome_clientid"] = a.DataDomeClientID
		v["cookies_json"] = a.CookiesJSON
	}
	return v
}

// accountExportCSVRow flattens an account into the ordered string slice
// expected by encoding/csv. Column order matches accountExportBaseCSVHeader
// (+ accountExportSecretCSVHeader when includeSecrets is true).
func accountExportCSVRow(a *domain.Account, includeSecrets bool) []string {
	row := []string{
		a.ID,
		a.Email,
		a.WorkspaceID,
		string(a.PlanType),
		strconv.FormatBool(a.HasUnlim),
		strconv.FormatBool(a.HasFlexUnlim),
		strconv.FormatBool(a.IsProVeo3Available),
		a.Cohort,
		strconv.FormatInt(a.SubscriptionBalance, 10),
		strconv.FormatInt(a.CreditsBalance, 10),
		strconv.FormatInt(a.TotalPlanCredits, 10),
		string(a.Status),
		strconv.FormatInt(int64(a.InFlightJobs), 10),
		strconv.Itoa(a.FailStreak),
		formatOptionalTime(a.PlanEndsAt),
		formatOptionalTime(a.LastBalanceAt),
		formatOptionalTime(a.LastUsedAt),
		formatOptionalTime(a.LastFailedAt),
		formatOptionalTime(a.RegisteredAt),
		formatOptionalTime(a.ImportedAt),
	}
	if includeSecrets {
		row = append(row,
			a.SessionID,
			a.UserAgent,
			a.DataDomeClientID,
			a.CookiesJSON,
		)
	}
	return row
}

// formatOptionalTime renders a time as RFC3339 UTC or "" for the zero value.
// CSV importers expect empty strings for missing values; RFC3339 lines up
// with the JSON view emitted by accountView.
func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// accountExportFilename shapes the Content-Disposition filename. A UTC
// wall-clock timestamp keeps two exports run seconds apart from colliding
// on disk.
func accountExportFilename(ext string) string {
	const stamp = "20060102T150405Z"
	return fmt.Sprintf("accounts-%s.%s", time.Now().UTC().Format(stamp), ext)
}
