// Package promowatch is a background ticker that scans active accounts for
// time-sensitive promo/offer/cashback surfaces and alerts an operator when
// one needs attention. It mirrors the repo's other tickers (claimer,
// invoicewatch, refresher, failover.Recoverer): one struct, one ticker, one
// goroutine launched by main.go.
//
// MVP scope: alert only, no automatic action. Per tick (default cadence 30m)
// it does List(active) → fan out over accounts bounded by Concurrency → for
// each account pull three endpoints and evaluate alert conditions:
//   - personal-promo: expired_at within the near-expiry window (<24h) AND
//     not yet viewed → Warn (the account has an unclaimed discount about to
//     lapse).
//   - cashback-challenge: status=progress AND challenge_ends_at <24h → Warn
//     (a cashback challenge is about to end mid-progress).
//   - two-day-offer: showable AND value-gated (discount >40% OR
//     allowed_job_set_types non-empty) → Warn (an offer with operator
//     value; body carries discount + price info so the operator can decide).
//     Alert-only — never auto-purchases.
//
// Alerts are deduplicated in-memory (mirrors invoicewatch's states map /
// replenish's muted map): each (account, surface) alert fires at most once
// per mutedWindow, keyed by a per-surface identity token so a NEW promo /
// challenge / offer re-arms the alert. A process restart clears the mute
// map, which is an acceptable trade for zero schema cost (MVP does not
// persist).
package promowatch

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

const (
	defaultInterval    = 30 * time.Minute
	defaultConcurrency = 3
	// nearExpiry is the window before a deadline within which a promo /
	// cashback challenge is considered "about to lapse" and worth an alert.
	nearExpiry = 24 * time.Hour
	// mutedWindow is how long a fired (account, surface-identity) alert
	// stays deduplicated before it may fire again.
	mutedWindow = 24 * time.Hour
	// deepDiscountPct is the threshold above which a two-day offer's
	// discount is considered operator-actionable on price alone. Chosen at
	// 40 so bundle-hardcoded tiers (44% / 44%) trip but the 40% tier does
	// not (avoiding an alert-on-every-plan-refresh). MVP hardcoded — if
	// operators want to tune, promote to config.
	deepDiscountPct = 40
)

// Watcher scans active accounts for time-sensitive promo/offer/cashback
// surfaces and alerts (Warn/Info) via the notifier chain. Alert-only: it
// never mutates account state or the upstream.
type Watcher struct {
	Accounts    ports.AccountStore
	Upstream    *upstream.Client
	Notifier    ports.Notifier
	Logger      *slog.Logger
	Interval    time.Duration // zero selects defaultInterval
	Concurrency int           // zero selects defaultConcurrency

	mu    sync.Mutex
	muted map[string]time.Time // dedup key -> time the alert last fired
	now   func() time.Time     // injectable clock for tests
}

// New builds a Watcher with defaults filled. Interval/Concurrency can be
// overridden by the caller after construction.
func New(accounts ports.AccountStore, up *upstream.Client, ntf ports.Notifier, logger *slog.Logger) *Watcher {
	return &Watcher{
		Accounts:    accounts,
		Upstream:    up,
		Notifier:    ntf,
		Logger:      logger,
		Interval:    defaultInterval,
		Concurrency: defaultConcurrency,
		muted:       make(map[string]time.Time),
		now:         time.Now,
	}
}

// Run blocks until ctx is canceled. Intended as `go w.Run(ctx)`. A nil
// receiver or unwired dependency returns immediately so callers need not
// guard the launch.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil || w.Accounts == nil || w.Upstream == nil || w.Notifier == nil {
		return
	}
	if w.Interval <= 0 {
		w.Interval = defaultInterval
	}
	if w.Concurrency <= 0 {
		w.Concurrency = defaultConcurrency
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.muted == nil {
		w.muted = make(map[string]time.Time)
	}
	if w.Logger != nil {
		w.Logger.Info("promowatch starting", slog.Duration("interval", w.Interval))
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	w.once(ctx) // immediate first pass so a near-expiry promo isn't stalled a full interval
	for {
		select {
		case <-ctx.Done():
			if w.Logger != nil {
				w.Logger.Info("promowatch stopping")
			}
			return
		case <-ticker.C:
			w.once(ctx)
		}
	}
}

// TriggerOnce runs a single scan synchronously. Used by tests and
// (optionally) an admin trigger endpoint.
func (w *Watcher) TriggerOnce(ctx context.Context) { w.once(ctx) }

// once fans out watchOne over every active account, bounded by Concurrency.
// One failing account never blocks the others.
func (w *Watcher) once(ctx context.Context) {
	if w == nil || w.Accounts == nil || w.Upstream == nil || w.Notifier == nil {
		return
	}
	accounts, err := w.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("promowatch list accounts", slog.String("err", err.Error()))
		}
		return
	}
	if len(accounts) == 0 {
		return
	}
	conc := w.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range accounts {
		acc := accounts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.watchOne(ctx, &acc)
		}()
	}
	wg.Wait()
}

// watchOne pulls the three promo surfaces for one account and evaluates the
// alert conditions. Each sub-step isolates its own errors — a failure on one
// endpoint never blocks the others.
func (w *Watcher) watchOne(ctx context.Context, acc *domain.Account) {
	w.checkPersonalPromo(ctx, acc)
	w.checkCashbackChallenge(ctx, acc)
	w.checkTwoDayOffer(ctx, acc)
}

// checkPersonalPromo alerts (Warn) when the account has an active personal
// promo expiring within nearExpiry that has not yet been viewed — an unclaimed
// discount about to lapse.
func (w *Watcher) checkPersonalPromo(ctx context.Context, acc *domain.Account) {
	promo, err := w.Upstream.FetchPersonalPromo(ctx, acc)
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("promowatch fetch personal-promo",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if promo == nil {
		return // no active promo
	}
	if promo.IsViewed {
		return // already seen by the account — nothing to nudge
	}
	if promo.ExpiredAt.IsZero() || !w.withinNearExpiry(promo.ExpiredAt) {
		return // no deadline, or not close enough yet
	}
	// Dedup by promo id so a brand-new promo re-arms the alert.
	if !w.reserveAlert("promo:" + acc.ID + ":" + promo.ID) {
		return
	}
	w.notify(ctx, ports.Notification{
		Level: ports.LevelWarn,
		Title: "Personal promo expiring soon",
		Body:  "An active account has an unviewed personal promo expiring within 24h; claim/use it before it lapses.",
		Tags: map[string]string{
			"account_id": acc.ID,
			"email":      acc.Email,
			"promo_id":   promo.ID,
			"promo_code": promo.PromoCode,
			"campaign":   promo.CampaignName,
			"expires_at": promo.ExpiredAt.Format(time.RFC3339),
		},
	})
}

// checkCashbackChallenge alerts (Warn) when a cashback challenge is in
// progress and ends within nearExpiry — the account risks leaving cashback on
// the table.
func (w *Watcher) checkCashbackChallenge(ctx context.Context, acc *domain.Account) {
	cb, err := w.Upstream.FetchCashbackChallenge(ctx, acc)
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("promowatch fetch cashback-challenge",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if cb == nil || cb.Status != "progress" {
		return // no challenge, or not in a progress state
	}
	if cb.ChallengeEndsAt.IsZero() || !w.withinNearExpiry(cb.ChallengeEndsAt) {
		return
	}
	// Dedup by end-time so a new challenge window re-arms the alert.
	if !w.reserveAlert("cashback:" + acc.ID + ":" + cb.ChallengeEndsAt.Format(time.RFC3339)) {
		return
	}
	w.notify(ctx, ports.Notification{
		Level: ports.LevelWarn,
		Title: "Cashback challenge ending soon",
		Body:  "An active account has a cashback challenge in progress ending within 24h; complete it before it closes.",
		Tags: map[string]string{
			"account_id":       acc.ID,
			"email":            acc.Email,
			"credits_spent":    ftoa(cb.CreditsSpent),
			"credits_cashback": ftoa(cb.CreditsCashback),
			"ends_at":          cb.ChallengeEndsAt.Format(time.RFC3339),
		},
	})
}

// checkTwoDayOffer alerts (Warn) when a two-day offer is showable AND has
// operator value — either a deep discount (>deepDiscountPct off) or it unlocks
// job-set-types the operator may want. Bare "offer is showing" is not enough:
// a value-gate keeps the alert stream signal-heavy so an operator can act on
// the discount info in the body.
//
// MVP simplification: relevance is a coarse "any non-empty allowed_job_set_types"
// rather than intersecting with the account's recent-usage JST set. Rationale:
// the recent-usage query adds coupling to usage_events for a signal that is
// already high-precision (upstream only lists JSTs relevant to the plan tier);
// operators can filter false positives via the allowed_count / allowed_models
// tags. If the alert stream grows noisy, swap in a usage-events intersection.
//
// Alert never auto-purchases — MVP is inform-only. The referral half of the
// original P2-3 spec is out-of-scope for higgsgo (belongs to the
// higgsfield-register plugin as a separate task).
func (w *Watcher) checkTwoDayOffer(ctx context.Context, acc *domain.Account) {
	offer, err := w.Upstream.FetchTwoDayOffer(ctx, acc)
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("promowatch fetch two-day-offer",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if offer == nil || offer.Status == "" || offer.Status == "hide" {
		return // no offer or idle
	}
	if offer.ModalData == nil {
		return // showable status but no pricing surface — nothing actionable
	}

	// Value gate: deep discount OR unlocks any JST.
	discountPct := twoDayOfferDiscountPct(offer.ModalData)
	triggersDeep := discountPct > deepDiscountPct
	triggersRelevant := len(offer.AllowedJobSetTypes) > 0
	if !triggersDeep && !triggersRelevant {
		return // showable but not valuable enough — skip
	}

	// Dedup per-account for the mutedWindow (24h). No status suffix: a re-fire
	// on tier transition every tick would be noise; operators can trigger a
	// fresh alert after acting by restarting the process, and the offer is
	// short-lived anyway.
	if !w.reserveAlert("twodayoffer:" + acc.ID) {
		return
	}

	tags := map[string]string{
		"account_id":     acc.ID,
		"email":          acc.Email,
		"status":         offer.Status,
		"discount_pct":   strconv.FormatInt(int64(discountPct), 10),
		"final_price":    strconv.FormatInt(offer.ModalData.FinalPrice, 10),
		"original_price": strconv.FormatInt(offer.ModalData.OriginalPrice, 10),
		"currency":       offer.ModalData.Currency,
		"allowed_count":  strconv.Itoa(len(offer.AllowedJobSetTypes)),
	}
	if !offer.ExpiresAt.IsZero() {
		tags["expires_at"] = offer.ExpiresAt.Format(time.RFC3339)
	}
	if models := formatAllowedJSTs(offer.AllowedJobSetTypes, 5); models != "" {
		tags["allowed_models"] = models
	}

	w.notify(ctx, ports.Notification{
		Level: ports.LevelWarn,
		Title: "Two-day offer available (" + strconv.FormatInt(int64(discountPct), 10) + "% off)",
		Body: "An active account has a two-day offer with " +
			strconv.FormatInt(int64(discountPct), 10) + "% off (" +
			formatPrice(offer.ModalData.OriginalPrice, offer.ModalData.Currency) + " → " +
			formatPrice(offer.ModalData.FinalPrice, offer.ModalData.Currency) + "); " +
			"review whether to purchase manually — this is an alert only.",
		Tags: tags,
	})
}

// twoDayOfferDiscountPct returns the whole-percent discount of a modalData
// block, guarding zero/negative originals (→ 0%). Rounds down (truncates).
func twoDayOfferDiscountPct(m *upstream.TwoDayOfferModal) int {
	if m == nil || m.OriginalPrice <= 0 {
		return 0
	}
	off := m.OriginalPrice - m.FinalPrice
	if off <= 0 {
		return 0
	}
	return int((off * 100) / m.OriginalPrice)
}

// formatAllowedJSTs joins the first n JobSetType strings from the allowed
// list with commas; returns "" when the list is empty. Used in the alert
// tags so an operator can see at a glance which models the offer unlocks.
func formatAllowedJSTs(jsts []upstream.TwoDayOfferAllowedJST, n int) string {
	if len(jsts) == 0 {
		return ""
	}
	if n > len(jsts) {
		n = len(jsts)
	}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if jsts[i].JobSetType != "" {
			names = append(names, jsts[i].JobSetType)
		}
	}
	return strings.Join(names, ",")
}

// formatPrice renders a cents amount as "$29.00" or "29.00 EUR" style for
// the alert body. Currency defaults to USD when empty (bundle default).
func formatPrice(cents int64, currency string) string {
	if currency == "" {
		currency = "USD"
	}
	whole := cents / 100
	frac := cents % 100
	if frac < 0 {
		frac = -frac
	}
	amt := strconv.FormatInt(whole, 10) + "." + fmt.Sprintf("%02d", frac)
	if currency == "USD" {
		return "$" + amt
	}
	return amt + " " + currency
}

// withinNearExpiry reports whether deadline is in the future but no more than
// nearExpiry away. A deadline already in the past is NOT alerted (a lapsed
// promo is no longer actionable).
func (w *Watcher) withinNearExpiry(deadline time.Time) bool {
	now := w.now()
	if !deadline.After(now) {
		return false
	}
	return deadline.Sub(now) <= nearExpiry
}

// reserveAlert returns true (and records the fire time) when the given dedup
// key has not fired within mutedWindow; false when it is still muted. Keeps
// the muted map from growing unbounded by pruning expired entries lazily.
func (w *Watcher) reserveAlert(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if last, ok := w.muted[key]; ok && now.Sub(last) < mutedWindow {
		return false
	}
	// Prune stale entries so the map doesn't grow without bound.
	for k, t := range w.muted {
		if now.Sub(t) >= mutedWindow {
			delete(w.muted, k)
		}
	}
	w.muted[key] = now
	return true
}

// notify sends a notification, logging (not failing) on delivery error.
func (w *Watcher) notify(ctx context.Context, msg ports.Notification) {
	if err := w.Notifier.Send(ctx, msg); err != nil && w.Logger != nil {
		w.Logger.Warn("promowatch notify failed",
			slog.String("title", msg.Title),
			slog.String("err", err.Error()))
	}
}

// ftoa formats a credit amount compactly for a notification tag.
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
