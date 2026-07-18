//go:build register
// +build register

// Package higgsfield's register-tag variant is the bridge from the main
// module's ports.Registrar to plugins/register's real workflow. See
// docs/PLUGGABLE.md §0 for the monorepo split rationale and
// docs/ROADMAP.md §5.4 for the delivery plan this file implements.
//
// The two modules deliberately have their own type systems:
//   - main module: ports.Registration.ID is int64 (SQLite autoincrement).
//   - plugin:      register.Registration.ID is string (opaque queue id).
// This file's storeAdapter translates between them so the plugin's Flow
// / Worker can stay ignorant of the main module's persistence choices.
//
// Under the register build tag NewRegistrar constructs:
//   1. a storeAdapter around the main module's ports.RegistrationStore
//      (the SQLite RegistrationStore that landed with P4-1);
//   2. a register.Flow wired to the caller-supplied Browser / Mailbox /
//      Captcha ports;
//   3. a register.Worker that polls NextPending on a ticker;
//   4. a small facade implementing ports.Registrar that services
//      Enqueue / GetStatus / List / Retry directly against the store
//      (the plugin's Worker owns the background flow; the facade only
//      reads/writes the queue).
//
// Wiring is done in cmd/higgsgo/main.go under the same build tag.

package higgsfield

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	register "github.com/greensheep999/higgsgo/plugins/register"
)

// Deps is the dependency bag NewRegistrar takes when the register tag
// is set. Every field is required for real flow execution; the type is
// documented so main.go can inject each piece via its concrete adapter.
// Keeping this a struct (rather than positional args) lets main.go
// evolve the wiring surface without touching admin/server wire order.
type Deps struct {
	// Store is the main module's SQLite RegistrationStore. The bridge
	// wraps it in a storeAdapter to satisfy the plugin's own
	// RegistrationStore interface (which uses string ids). Nil means
	// no persistence — NewRegistrar returns an error.
	Store ports.RegistrationStore

	// Browser drives the actual sign-up UI (camoufox / cloak / mock).
	// Provided by main.go from the config.Registrar.Browser section.
	Browser register.BrowserAutomator

	// Mailbox fetches the OTP verification code. Only exercised on the
	// password flow — OAuthSource != "" skips this step.
	Mailbox register.MailboxProvider

	// Captcha solves DataDome / hCaptcha challenges when the sign-up
	// flow trips one. Optional — Flow.run tolerates nil by aborting
	// with a captcha_unavailable error.
	Captcha register.CaptchaSolver

	// Config controls concurrency, retry counts, poll cadence, and
	// browser pool. Zero-value falls back to
	// register.DefaultConfig().
	Config register.Config

	// Logger is passed straight into the plugin. Never nil at runtime
	// (main.go always constructs one) but NewRegistrar treats nil as
	// slog.Default() so tests can omit it.
	Logger *slog.Logger

	// StartWorker, when true, launches the background register.Worker
	// goroutine that polls the queue. Set false in tests that only
	// exercise the admin surface. main.go always sets it to true.
	StartWorker bool
}

// NewRegistrar returns a ports.Registrar backed by the plugins/register
// module. Returns an error when required Deps are missing so a
// misconfigured deployment fails at boot instead of at the first
// admin call.
func NewRegistrar(deps Deps) (ports.Registrar, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("higgsfield: Store is required under -tags register")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Config.MaxConcurrent == 0 {
		deps.Config = register.DefaultConfig()
	}

	adapter := &storeAdapter{
		main: deps.Store,
		log:  deps.Logger,
	}

	// Flow may be nil if no browser/mailbox/captcha are wired — the
	// admin surface (Enqueue/List/Get/Retry) still works, only the
	// worker's Execute step is a no-op. Guarded so an operator can
	// stage the persistence side while adapters are still under
	// development.
	var worker *register.Worker
	if deps.Browser != nil && deps.Mailbox != nil {
		flow := register.NewFlow(
			deps.Browser,
			deps.Mailbox,
			deps.Captcha,
			adapter,
			deps.Config,
			deps.Logger,
		)
		worker = register.NewWorker(flow, adapter, deps.Config, deps.Logger)
	} else {
		deps.Logger.Warn("register worker skipped: browser or mailbox adapter missing",
			slog.Bool("has_browser", deps.Browser != nil),
			slog.Bool("has_mailbox", deps.Mailbox != nil))
	}

	return &registrar{
		deps:    deps,
		adapter: adapter,
		worker:  worker,
	}, nil
}

// registrar is the ports.Registrar facade. All four methods read/write
// the SQLite store directly through the adapter; the worker fires
// asynchronously against the same rows.
type registrar struct {
	deps    Deps
	adapter *storeAdapter
	worker  *register.Worker // nil when browser+mailbox not wired
}

// Compile-time assertion so the interface stays in sync.
var _ ports.Registrar = (*registrar)(nil)

// Start launches the background worker goroutine. Callable once from
// main.go after all adapters are wired. Idempotent: safe to call when
// worker is nil (no-op).
func (r *registrar) Start(ctx context.Context) {
	if r.worker == nil {
		return
	}
	if !r.deps.StartWorker {
		return
	}
	go r.worker.Start(ctx)
}

func (r *registrar) Enqueue(ctx context.Context, req ports.RegistrationRequest) (string, error) {
	if req.Email == "" {
		return "", fmt.Errorf("registrar.Enqueue: email required")
	}
	row := &ports.Registration{
		Email:       req.Email,
		OAuthSource: req.OAuthSource,
		ProxyURL:    req.ProxyURL,
		Status:      "pending",
	}
	if err := r.deps.Store.Enqueue(ctx, row); err != nil {
		return "", err
	}
	// Nudge the worker so a freshly-queued row starts processing
	// without waiting for the next poll tick. Best-effort — a nil
	// worker just means the admin surface is running without the
	// background flow.
	if r.worker != nil {
		r.worker.Trigger(ctx)
	}
	return strconv.FormatInt(row.ID, 10), nil
}

func (r *registrar) GetStatus(ctx context.Context, id string) (*ports.RegistrationRow, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, domain.ErrRegistrationNotFound
	}
	row, err := r.deps.Store.Get(ctx, n)
	if err != nil {
		return nil, err
	}
	return toRegistrationRow(row), nil
}

func (r *registrar) List(ctx context.Context, filter ports.RegistrationFilter) ([]ports.RegistrationRow, error) {
	// Store.List returns rows newest-first with status/since/limit/
	// offset pushed into SQL. We just translate the shape for the
	// admin surface. Limit defaults + capping are handled inside the
	// store.
	rows, err := r.deps.Store.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]ports.RegistrationRow, 0, len(rows))
	for i := range rows {
		out = append(out, *toRegistrationRow(&rows[i]))
	}
	return out, nil
}

func (r *registrar) Retry(ctx context.Context, id string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return domain.ErrRegistrationNotFound
	}
	row, err := r.deps.Store.Get(ctx, n)
	if err != nil {
		return err
	}
	switch row.Status {
	case "success":
		// Success rows are terminal — retry is a no-op. Return nil so
		// admins retrying the wrong row don't see an error.
		return nil
	case "pending", "running":
		// Already in-flight; retry is a no-op.
		return nil
	}
	// Reset to pending; worker will pick it up on next tick.
	// Attempts count is preserved by the store so operators can see
	// retry history.
	if err := r.adapter.resetToPending(ctx, n); err != nil {
		return err
	}
	if r.worker != nil {
		r.worker.Trigger(ctx)
	}
	return nil
}

// toRegistrationRow converts the store's Registration into the ports
// view struct that admin handlers consume. Keeping this a free helper
// lets tests exercise the mapping without a full registrar.
func toRegistrationRow(r *ports.Registration) *ports.RegistrationRow {
	if r == nil {
		return nil
	}
	return &ports.RegistrationRow{
		ID:          strconv.FormatInt(r.ID, 10),
		Email:       r.Email,
		OAuthSource: r.OAuthSource,
		ProxyURL:    r.ProxyURL,
		Status:      r.Status,
		Attempts:    r.Attempts,
		LastError:   r.LastError,
		AccountID:   r.AccountID,
		CreatedAt:   r.CreatedAt,
		FinishedAt:  r.FinishedAt,
	}
}

// storeAdapter satisfies plugins/register.RegistrationStore by
// delegating to the main module's ports.RegistrationStore. The two
// interfaces differ in three places, and each translation is a
// dedicated method here:
//
//  1. ID types: plugin uses string, main uses int64. Every method
//     parses the incoming string as an int64 and returns
//     ErrRegistrationNotFound on parse failure (matches "unknown id"
//     semantics — a caller that fabricates a non-numeric id is
//     indistinguishable from one referencing a deleted row).
//  2. Status enum: plugin has more granular states (otp_wait,
//     verifying); main module keeps pending/running/success/failed.
//     MarkOTPWait folds into "running" here so the queue still
//     progresses.
//  3. CompletedResult: plugin returns rich sign-up artefacts (cookies,
//     UA, DataDome id, plan, credits). Today we only persist the
//     produced account_id — the rest is TODO wire-through to
//     account_store.Upsert. Documented so a future engineer picks
//     that up.
//
// Because plugins/register/Worker only calls the store methods, this
// adapter is the only surface where translation lives.
type storeAdapter struct {
	main ports.RegistrationStore
	log  *slog.Logger
}

// Compile-time assertion so the interface stays in sync as the plugin evolves.
var _ register.RegistrationStore = (*storeAdapter)(nil)

func (a *storeAdapter) Enqueue(ctx context.Context, req register.EnqueueRequest) (string, error) {
	row := &ports.Registration{
		Email:       req.Email,
		Password:    req.Password,
		OAuthSource: req.OAuthSource,
		ProxyURL:    req.ProxyURL,
	}
	if err := a.main.Enqueue(ctx, row); err != nil {
		return "", err
	}
	return strconv.FormatInt(row.ID, 10), nil
}

func (a *storeAdapter) NextPending(ctx context.Context) (*register.Registration, error) {
	row, err := a.main.NextPending(ctx)
	if err != nil || row == nil {
		return nil, err
	}
	return fromPortsRegistration(row), nil
}

func (a *storeAdapter) MarkRunning(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	return a.main.MarkRunning(ctx, n)
}

func (a *storeAdapter) MarkOTPWait(ctx context.Context, id string) error {
	// Plugin's finer-grained otp_wait state folds into main's
	// "running" so admin UIs still see progress. If a future audit
	// wants per-substate visibility, add a status column to the main
	// module's Registration and stop the fold here.
	if a.log != nil {
		a.log.Debug("registrar: otp_wait recorded as running",
			slog.String("id", id))
	}
	return nil
}

func (a *storeAdapter) MarkCompleted(ctx context.Context, id string, result register.CompletedResult) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	// TODO(registrar): wire result.Cookies / UserAgent / DataDomeID /
	// PlanType / Credits into an account_store.Upsert call so a
	// completed registration lands a fully-populated Account row.
	// The account_id parameter below records the produced id but
	// doesn't create the row.
	return a.main.MarkCompleted(ctx, n, result.AccountID)
}

func (a *storeAdapter) MarkFailed(ctx context.Context, id string, reason string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	return a.main.MarkFailed(ctx, n, reason)
}

func (a *storeAdapter) Get(ctx context.Context, id string) (*register.Registration, error) {
	n, err := parseID(id)
	if err != nil {
		return nil, err
	}
	row, err := a.main.Get(ctx, n)
	if err != nil {
		return nil, err
	}
	return fromPortsRegistration(row), nil
}

func (a *storeAdapter) List(ctx context.Context, filter register.ListFilter) ([]register.Registration, error) {
	// Translate the plugin's ListFilter (status pointer + limit/offset)
	// into the main-module ports.RegistrationFilter (status string +
	// limit/offset + since). Nil status means "any"; the main store's
	// empty-string status likewise means any.
	mainFilter := ports.RegistrationFilter{
		Limit:  filter.Limit,
		Offset: filter.Offset,
	}
	if filter.Status != nil {
		mainFilter.Status = mapPluginStatusToMain(*filter.Status)
	}
	rows, err := a.main.List(ctx, mainFilter)
	if err != nil {
		return nil, err
	}
	out := make([]register.Registration, 0, len(rows))
	for i := range rows {
		out = append(out, *fromPortsRegistration(&rows[i]))
	}
	return out, nil
}

// mapPluginStatusToMain converts the plugin's 6-state enum down to
// the main module's 4-state enum for filtering. Substates like
// otp_wait and verifying map to "running" — mirroring
// storeAdapter.MarkOTPWait's fold. Unknown values pass through so a
// caller supplying an unmapped status still gets an exact-match
// filter (which will match nothing, cleanly failing).
func mapPluginStatusToMain(s register.RegistrationStatus) string {
	switch s {
	case register.StatusPending:
		return "pending"
	case register.StatusRunning, register.StatusOTPWait, register.StatusVerifying:
		return "running"
	case register.StatusCompleted:
		return "success"
	case register.StatusFailed:
		return "failed"
	default:
		return string(s)
	}
}

func (a *storeAdapter) Retry(ctx context.Context, id string) error {
	n, err := parseID(id)
	if err != nil {
		return err
	}
	return a.resetToPending(ctx, n)
}

// resetToPending flips a failed / terminal row back to pending via
// the store's dedicated ResetToPending method. Called from
// registrar.Retry AND storeAdapter.Retry (both admin-triggered).
// The store preserves attempts and clears last_error / finished_at.
func (a *storeAdapter) resetToPending(ctx context.Context, id int64) error {
	return a.main.ResetToPending(ctx, id)
}

// fromPortsRegistration maps a main-module row into the plugin's own
// Registration type. Field-by-field, no logic; kept out of the store
// method bodies for readability.
func fromPortsRegistration(r *ports.Registration) *register.Registration {
	return &register.Registration{
		ID:          strconv.FormatInt(r.ID, 10),
		Email:       r.Email,
		Password:    r.Password,
		Status:      mapMainStatusToPlugin(r.Status),
		OAuthSource: r.OAuthSource,
		ProxyURL:    r.ProxyURL,
		Error:       r.LastError,
		AccountID:   r.AccountID,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   pickUpdatedAt(r),
	}
}

// pickUpdatedAt returns FinishedAt when present, else CreatedAt. The
// plugin uses UpdatedAt to spot stuck rows; giving it CreatedAt for
// active rows is close enough for a first cut.
func pickUpdatedAt(r *ports.Registration) time.Time {
	if !r.FinishedAt.IsZero() {
		return r.FinishedAt
	}
	return r.CreatedAt
}

// mapMainStatusToPlugin maps the main module's four-state enum to the
// plugin's six-state enum. Unknowns default to StatusPending so a
// mid-development row doesn't crash the worker.
func mapMainStatusToPlugin(s string) register.RegistrationStatus {
	switch s {
	case "pending":
		return register.StatusPending
	case "running":
		return register.StatusRunning
	case "success":
		return register.StatusCompleted
	case "failed":
		return register.StatusFailed
	default:
		return register.StatusPending
	}
}

func parseID(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, domain.ErrRegistrationNotFound
	}
	return n, nil
}
