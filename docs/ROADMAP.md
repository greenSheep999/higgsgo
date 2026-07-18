# higgsgo Roadmap (Implementation Status + Priority)

> Last updated: 2026-07-18 after full audit sweep across pool / concurrency /
> priority / proxy / load-balancing / registrations / failover.
>
> This document is the **single source of truth for what actually works vs
> what is only defined on disk**. Every claim carries a `file:line`
> reference so you can verify it in one click. When code diverges from this
> doc, update this doc first.

---

## 1. Snapshot: What is Actually Wired

### ✅ Fully wired in the request path

| Feature | Where it lives | Notes |
|---|---|---|
| API Key → Group resolve | `internal/api/v1/handler.go:318-352` | 3-tier: explicit body > `api_key.group_id` > `apikey_group_bindings` |
| Group → Account filter | `internal/adapters/storage/sqlite/account_store.go:435-438` | `WHERE id IN (SELECT account_id FROM account_group_members ...)` |
| `RouteStrategy` ORDER BY | `account_store.go:445-479` | 5 strategies: `round_robin` / `least_used` / `cheapest_first` / `most_credits_first` / `priority` |
| `Account.Priority` sort | `account_store.go:475` | Only under `RoutePriority` strategy |
| `account_group_members.priority` sort | `account_store.go:471-473` | Backend reads it; WebUI cannot edit yet |
| Failover / disable / throttle / recover | `internal/core/failover/*`, `internal/api/admin/failover.go`, `webui/src/components/settings/failover-dialog.tsx` | Full stack, controller + recoverer goroutines wired in `main.go:144-153` |
| Admin bearer runtime rotation | `internal/core/bearer/manager.go`, `internal/api/admin/settings.go`, migration `014` | Rotate without restart |
| Version check | `internal/version/`, `internal/api/admin/version.go` | GitHub release compare, 1h cache |
| Model overrides | migration `015`, `internal/adapters/storage/sqlite/model_override_store.go`, `internal/api/admin/model_overrides.go` | Reload triggers `Registry.Reload()` |
| Default RouteStrategy setting | `internal/api/admin/routing_settings.go` | Affects new-group defaults only |
| Model health snapshot | `internal/api/admin/model_health.go`, regression ticker | UI has mock uptime fallback (see gaps) |
| Pool composition | `/admin/stats/pool` + `webui/src/components/dashboard/pool-composition.tsx` | Aggregates DB state, 30s refetch |

### ⚠️ Defined but only partially wired

| Feature | State | Missing |
|---|---|---|
| `account_group_members.priority` | Backend reads it; WebUI now exposes per-group priority in the account edit dialog (P0-1 landed). `addGroupMember` on the backend is an upsert (ON CONFLICT DO UPDATE), so the same call updates existing priorities. | — |
| `internal/core/resolver/` (config cascade) | Correct implementation of Key > Group > Account > Global precedence for concurrency / proxy / route / budget / regex / rate limit / markup | **Zero external importers** — never called from proxy path; see §3 |

### ❌ Stored but never enforced (silent no-op fields)

Fields you can set in the DB / admin API / WebUI that **have no runtime effect**:

| Field | Storage | Actual runtime behavior |
|---|---|---|
| `account.bound_proxy_url` | `account_store.go:62,175,562` | ✅ **Enforced** (P1-5 landed). `utls.Pool` (`internal/adapters/httpclient/utls/pool.go`) caches one `*Client` per unique proxy URL and is wired as `upstream.Client.Resolver` in `main.go`. Empty `bound_proxy_url` falls back to the process-level `HIGGSGO_UPSTREAM_PROXY_URL`. |
| `group.max_concurrent_jobs` | `group_store.go` | ✅ **Enforced** (P0-3 landed). `proxy.Service.resolveGroupPolicy` reads it and passes `MaxGroupInFlight` to `PickAndLock`, which runs a `SUM(in_flight_jobs)` subquery inside the tx and returns `ErrGroupConcurrencyMax` when tripped. 429 `pool_saturated` at the HTTP layer. |
| `group.max_concurrent_per_account` | `group_store.go` | ✅ **Enforced** (P0-3 landed). Same policy hop feeds `MaxConcurrentPerAccount` into `PickAndLock` WHERE. Falls back to the historical `5` when the group hasn't set a value. |
| `group.allowed_models_regex` | `group_store.go` | ✅ **Enforced** (P1-4 landed). `proxy.Service.enforceGroupGates` runs the compiled regex against the resolved alias before pick; miss → `ErrModelNotAllowed` → HTTP 403 `model_not_allowed`. Invalid pattern logs WARN and fails open. |
| `group.blocked_models_regex` | `group_store.go` | ✅ **Enforced** (P1-4 landed). Same gate; match → `ErrModelBlocked` → HTTP 403 `model_blocked`. Blocked wins over allowed when both patterns match. |
| `group.monthly_credit_budget` / `monthly_credit_used` | `group_store.go` | ✅ **Enforced** (P1-4 landed). Pre-pick gate compares `MonthlyCreditUsed + EstCost` against `MonthlyCreditBudget`; over → `ErrGroupQuotaExhausted` → HTTP 402 `group_budget_exhausted`. `metering.Recorder.OnJobTerminal` now calls `GroupStore.IncrementUsed` at every non-zero terminal so the counter actually climbs. Zero budget disables the gate. |
| `account.max_concurrent` | migration `016` | **Ignored.** Never read by pick path. |
| `AvailableSlots()` | `domain/account.go:140-149` | Const `upstreamLimit = 6` diverges from the enforced literal `5`. |
| `cfg.Pool.MaxInFlightPerAccount` | `config.go:227,329` | **Ignored** by pick path. Only surfaced by config parser. |

### UI-only placeholders (resolved)

| Feature | UI location | Backend state |
|---|---|---|
| Account "probe" button | `webui/src/routes/accounts.tsx` | ✅ **Real** (P2-6 landed). `POST /admin/accounts/{id}/probe` calls `upstream.Client.FetchWallet` — exercises JWT mint + per-account proxy + TLS fingerprint. Response body carries `ok`/`latency_ms`/`balance` on success, or a classified error kind on failure (unauthorized / forbidden / rate_limit / upstream_5xx / timeout / network / internal). Handler always mounted; nil Prober answers 503 `probe_disabled` so the WebUI can tell "not configured" from "call failed". |
| Per-model uptime bars | `webui/src/routes/models.tsx` | ✅ **Honest** (P2-7 landed). No more `mockUptime` backfill. Rows without a `/admin/model-health` row show a muted "no data" bar + em-dash percentage; the detail sheet says "No probe data yet — run the regression ticker to populate". Real time-series probe data (per-slot success/fail counts) is still a P3 backend addition. |

---

## 2. Load-Balancing Reality Check

### 2.1 The names lie

- `round_robin` is **least-recently-used**, not round-robin. `ORDER BY last_used_at ASC LIMIT 1`. Under bursty concurrent picks the same row wins until the surrounding transaction commits — hot-spot risk.
- `least_used` sorts by lifetime consumed credits (`total_plan_credits - subscription_balance`), not month-to-date, despite what `POOL-AND-CPA.md` said in v0.3.

### 2.2 There is no inter-group balancing

`resolveGroup` picks **one** group up front and passes it as a hard filter. If you want spillover between groups, that logic doesn't exist yet.

### 2.3 No fair-share primitives

No weighted round-robin, no least-connections, no lease reservation, no in-memory counters. Everything is a single SQL `ORDER BY ... LIMIT 1` with `last_used_at` as tie-breaker.

**Cheapest upgrade** (deferred): seed an in-memory atomic pick-count table on process start (from `in_flight_jobs`), consult it as a jittered tiebreaker inside the transaction. No schema change required.

---

## 3. Config Cascade — the missing engine

`internal/core/resolver/resolver.go` implements exactly the precedence rule
we want:

```
MaxConcurrent : Key > Group.PerAccount > Account > Global
ProxyURL      : Account > Global
RouteStrategy : Group > Global (default round_robin)
MonthlyBudget : Key.MonthlyQuota > Group.MonthlyCreditBudget
Allowed/BlockedModels : Group only
RateLimit     : Key > Group > Global
MarkupPct     : Key only
```

`grep -rn "core/resolver"` currently returns **zero external importers** in
production code. The package compiles, tests pass in isolation, and it is
the single correct source of truth for six of the "stored but never
enforced" fields listed in §1. **Decision: wire it in, not delete it.**

Target call site: `internal/core/proxy/service.go` between `resolveGroup`
(v1/handler.go) and `Store.PickAndLock`. `Resolve()` returns a
`ResolvedConfig`; extend `PickParams` (`internal/ports/storage.go:85-99`)
to accept the fields the SQL WHERE / ORDER BY layer needs.

---

## 4. Priority Roadmap

### P0 — visible quick wins (backend already there)

1. **Group-member priority in WebUI** — ✅ **DONE**
   - `webui/src/lib/api.ts:537` — `addGroupMember(groupId, accountId, priority?)`.
   - `webui/src/components/accounts/edit-dialog.tsx` — per-group priority
     editor beside the multi-select chips, seeded from the new
     `members_detail` array on `/admin/groups/{id}/members`.
   - Backend: `ports.GroupStore.ListMembersWithPriority` + the enriched
     handler response; `AddMember` upsert semantics let the same call
     update priorities on rebind.

2. **`in_flight_jobs` leak fix** — ✅ **DONE**
   - `proxy/service.go` unlock is now `sync.Once`-wrapped with a top-level
     `defer unlock()`. Manual `unlock()` calls at the three original
     sites still fire eagerly (async release, error release, sync-poll
     release) but a panic between PickAndLock and any of them can no
     longer leak the counter.
   - `AccountStore.ResetAllInFlight` clears any leaked counters at boot
     (`cmd/higgsgo/main.go` reconciliation hop). Logged with a WARN so
     an operator sees "cleared N leaked in_flight counters on boot" if
     the previous process died mid-request.

3. **Enforce `group.max_concurrent_jobs`** — ✅ **DONE**
   - `ports.PickParams` now carries `MaxGroupInFlight` and
     `MaxConcurrentPerAccount`.
   - `PickAndLock` runs a `SUM(in_flight_jobs)` subquery inside the same
     tx as the account SELECT+UPDATE and returns
     `domain.ErrGroupConcurrencyMax` when the group cap has already been
     reached — atomic under SQLite's serialized writers.
   - `proxy.Service.resolveGroupPolicy` reads both caps from the Group
     row once per request and hands them to `PickAndLock`, replacing the
     hardcoded literal `5`.
   - HTTP layer (`internal/api/v1/videos.go:writeGenerationError`) maps
     the error to `429 pool_saturated` (retryable), distinct from `503
     no_account_available` (pool is dry).
   - Tests: `TestAccountStore_PickAndLock_GroupConcurrencyCap`,
     `TestAccountStore_PickAndLock_PerAccountCap`,
     `TestAccountStore_ResetAllInFlight`.

### P1 — correctness (drop silent no-ops)

4. **Wire group-level gates into proxy path** — ✅ **DONE (partial)**
   - `proxy.Service.resolveGroupPolicy` now compiles `AllowedModels` /
     `BlockedModels` and reads `MonthlyCreditBudget` /
     `MonthlyCreditUsed` alongside the strategy + concurrency caps
     already wired in P0-3.
   - `proxy.Service.enforceGroupGates` runs the three checks
     (blocked / allowed / budget) **before** `PickAndLock`, so a
     doomed request never consumes an in-flight slot. Invalid regex
     patterns are logged and treated as "no filter" — pre-existing
     rows with bad patterns don't 500 the caller.
   - `metering.Recorder.Groups` new field; `OnJobTerminal` calls
     `GroupStore.IncrementUsed` at every non-zero terminal so the
     budget gate self-limits. Wired in `main.go`.
   - HTTP error mapping in `v1/videos.writeGenerationError`:
     `ErrModelBlocked` → 403 `model_blocked`,
     `ErrModelNotAllowed` → 403 `model_not_allowed`,
     `ErrGroupQuotaExhausted` → 402 `group_budget_exhausted`.
   - Tests: `TestEnforceGroupGates_MatrixOfDenyAndAllow` (9 cases
     covering nil-safety, block wins over allow, exact-limit passes,
     zero-budget disables), `TestCompileGroupRegex_InvalidPatternDegrades`.
   - **Deferred** (still stored-but-not-enforced): per-key model
     regex, per-key rate limit fields (`KeyConfig.RateLimitRPS`,
     `RateLimitBurst`). Those live on `internal/core/resolver/` but
     the domain.APIKey struct doesn't carry them yet — adding them
     requires a migration + admin/keys UI, tracked outside the P0/P1
     scope.
   - `APIKey.MonthlyLimit` is now handled by P2-9 (below).
   - `internal/core/resolver/` remains uncalled: the P1-4 group
     gates + P2-9 key gate replicate the resolver's precedence for
     the fields that matter today. The resolver package stays for
     when we add per-key rate limits and model regex — same reason
     as before, just narrower scope.

5. **Per-account HTTP client honoring `bound_proxy_url`** — ✅ **DONE**
   - `internal/adapters/httpclient/utls/pool.go` (`utls.Pool`) caches
     one `*utls.Client` per unique bound_proxy_url. `sync.Mutex`-guarded
     map keyed by URL; malformed URLs are remembered so we don't retry
     the build on every request. `Invalidate(url)` drops one entry so
     the failover controller (or an operator) can force a rebuild
     after fixing a broken proxy.
   - `upstream.Client.Resolver` (new field, `AccountClientResolver`
     interface) is consulted in `doWithRetry` before every request. A
     `nil` return falls back to the default client (empty bound URL
     path). Errors are logged as warnings and fall back too — a broken
     per-account proxy degrades to shared egress rather than failing
     the request outright.
   - Wired in `cmd/higgsgo/main.go`: build the default client from
     `HIGGSGO_UPSTREAM_PROXY_URL` as before, then wrap it in
     `utls.NewPool(cfg, defaultClient)` and assign the pool to
     `upstreamClient.Resolver`. Boot log now includes
     `per_account_proxy_enabled=true`.
   - Tests: `TestPool_ResolveByBoundProxy` (empty URL → fallback;
     distinct URLs → distinct cached clients),
     `TestPool_MalformedURLReturnsErrorAndCaches` (bad URL doesn't
     flood the build path),
     `TestClient_Resolver_RoutesPerAccountClient` (proves the
     resolver's client actually receives the request instead of the
     shared default), `TestClient_Resolver_FallsBackOnResolverError`
     (resolver error → default client, request still succeeds).

### P2 — user-facing honesty

6. **Account probe endpoint** — ✅ **DONE**
   - `POST /admin/accounts/{id}/probe`
     (`internal/api/admin/accounts_probe.go`) actively fetches the
     account's wallet through `upstream.Client.FetchWallet` — same
     JWT minter, same per-account proxy (P1-5), same TLS
     fingerprint the pool uses in production. A green probe is
     therefore a strong signal.
   - Response: 200 with structured JSON on both success and
     failure. `ok=true` carries balance + latency; `ok=false`
     carries `error.kind` (unauthorized / forbidden / rate_limit /
     upstream_5xx / timeout / network / internal) + `error.message`.
     No HTTP error status on upstream failure — the WebUI renders
     both outcomes through the same code path.
   - Guards: 404 when the account is gone, 409 when the account is
     `banned` (deliberately soft-deleted — probing it would signal
     "just unban and it works"), 503 `probe_disabled` when no
     upstream client is wired (distinct from "call failed").
   - 15 s outer deadline so a wedged proxy can't hang an admin
     request.
   - WebUI: `admin.probeAccount()` in `webui/src/lib/api.ts`, wired
     to both card and table probe buttons in `routes/accounts.tsx`.
     Toast shows latency + balance on success, `[kind] message` on
     failure. Loading state via `probe.isPending`.
   - Tests: `TestProbe_SuccessReportsBalanceAndLatency`,
     `TestProbe_ErrorReturnsStructuredBody` (8-case matrix
     covering every error kind incl. context timeout),
     `TestProbe_NilProberReturns503`, `TestProbe_BannedAccountReturns409`.

7. **Mock uptime removed** — ✅ **DONE**
   - `webui/src/routes/models.tsx` no longer imports or calls
     `mockUptime`. Rows without a `/admin/model-health` entry show
     an em dash and a muted "no data" bar (`generateEmptySlots`).
   - Detail sheet's uptime area shows "No probe data yet — run the
     regression ticker to populate" when the JST has no health row,
     instead of a fabricated 95-100% number.
   - `generateMockSlots` in `uptime-bar.tsx` renamed to
     `generateEmptySlots`; produces total=0 slots the bar renders
     in muted gray with "No data" tooltips. The bar stays visible
     to keep column widths stable, but the data it renders is
     honest.
   - **Update**: real time-series data now available via P3-13 below.
     Table view keeps the aggregate percentage for scanning; the
     detail sheet fetches real slots.
8. **LRU → jittered tiebreaker** — ✅ **DONE**
   - `PickAndLock` ORDER BY tail on every strategy is now
     `, in_flight_jobs ASC, RANDOM() LIMIT 1`. Primary sort keys
     that tie under real load (identical `last_used_at`, same
     `plan_type`, same `priority`) fall through to a least-loaded
     preference, then a random tiebreaker. Spreads concurrent picks
     instead of hitting the same row until its `last_used_at`
     commit advances.
   - Test: `TestPickAndLock_JitterSpreadsAcrossTiedAccounts` seeds
     three identical rows and proves 30 picks land on at least 2 of
     them (previously all 30 hit row 0).

9. **Per-Key monthly quota pre-pick gate** — ✅ **DONE (P2-9)**
   - `GenerationRequest` grew `APIKeyMonthlyQuota` and
     `APIKeyMonthlyUsed` fields. `/v1` handlers (`videos.go`,
     `images.go`) forward them from
     `middleware.APIKeyFromContext()`; `internal/api/cpaplugin/*`
     keeps recorder-time accounting since its plane has its own
     upstream quota model.
   - `proxy.Service.Generate` now calls `enforceKeyGates` right
     after `enforceGroupGates`. Overrun returns
     `domain.ErrAPIKeyQuotaExceed` before `PickAndLock` runs — so
     a downstream integration that hit its cap gets a fast, honest
     402 `quota_exhausted` instead of consuming a pool slot and
     only failing when metering reconciles credits after the job.
   - Same "reserve headroom" semantics as the group budget gate:
     the last credit of the month is spendable, the recorder-side
     IncrementUsage is what actually retires it. Zero quota =
     unlimited (unchanged historical default).
   - HTTP mapping in `v1/videos.writeGenerationError`:
     `ErrAPIKeyQuotaExceed` → 402 `quota_exhausted` — same shape
     as the middleware's post-hoc check so callers don't need to
     tell "rejected before pool touched you" from "drained last
     credit after".
   - Test: `TestEnforceKeyGates_QuotaMatrix` covers 6 cases
     (unlimited via 0 quota, defensive negative, well-under,
     exactly-at, one-over, already-over).

### P3 — new capability

9. **Async job lifecycle in_flight tracking** — ✅ **DONE**
   - `proxy.Service.Generate`'s async branch now sets a `handedOff`
     flag before returning so the deferred `unlock()` becomes a
     no-op. The reserved slot stays counted against the account
     for the whole job lifetime instead of being released the
     moment CreateJob returns.
   - `pollworker.Worker.releaseInFlight` calls
     `AccountStore.UpdateInFlight(-1)` at every terminal path:
     successful terminal (after `UpdateStatus`), timeout, and any
     other path that permanently retires the job from
     `ListPending`. Failures are logged and never block the
     terminal transition — a stray leak is cleaned up by
     `ResetAllInFlight` on next boot (P0-2).
   - Fetch-terminal stall path deliberately does NOT release: if
     upstream reports terminal but `FetchJob` fails we'll retry
     next tick, and the slot is still notionally in use.
   - Group + per-account concurrency caps (P0-3) now enforce
     across BOTH sync and async work — before this, an async
     burst could oversubscribe an account because its slots freed
     at CreateJob time even though upstream jobs were still
     running.
   - Tests: `TestPollworker_ReleasesInFlightOnTerminalSuccess`,
     `TestPollworker_ReleasesInFlightOnTimeout`,
     `TestPollworker_HoldsInFlightWhileFetchTerminalRetries`.
10. **Inter-group spillover** — ✅ **DONE (P3-10)**
    - `resolveGroup` returns `[]string` — the ordered spillover
      candidate list — instead of a single group or `ambiguous_group`
      error. Multi-binding keys used to 400; now they sort their
      bindings by name ascending so operators can drive priority
      with a naming convention (`primary`, `fallback-1`,
      `fallback-2`).
    - `GenerationRequest.GroupCandidates []string` carries the list
      to `proxy.Service.Generate`. Legacy callers (CPA plugin,
      tests) that leave it empty degenerate to a one-shot with the
      pinned `GroupID` — behavior unchanged.
    - `Generate`'s pick section loops through candidates. On each
      iteration it runs `enforceGroupGates` + `PickAndLock`; a
      failure that satisfies `isSpilloverEligible` (group cap,
      group budget, no eligible account, blocked/allowed regex
      mismatch) triggers the next candidate. Non-eligible errors
      (`ErrAPIKeyQuotaExceed`, upstream errors) short-circuit
      immediately. `req.GroupID` is rewritten to the group that
      actually served the pick so downstream accounting (Job row,
      metering event, webhook) is honest.
    - Pre-pick key gate (`enforceKeyGates`) runs BEFORE the loop
      because a per-key overrun cannot be helped by trying a
      different group.
    - Tests: `TestIsSpilloverEligible` (8 cases covering every
      sentinel that should / shouldn't be eligible),
      `TestPickAndLock_SpilloverContract` (proves the pool layer
      returns the right sentinel for the loop to switch on), plus
      the refreshed `group_resolve_test.go` matrix now asserts on
      the returned slice shape instead of a single string.
    - Explicit `group_id` in the request body still pins one group
      (tier 1 of resolveGroup) — spillover only kicks in when the
      key's M:N binding table is the source of truth.
11. **Weighted or lease-based load balancing** — deferred until we have
    QPS data showing LRU hot-spots are a real problem in production.
12. **Per-slot time-series probe data** — ✅ **DONE (P3-13)**
    - `ModelHealthStore.SlotsByJST(ctx, jst, count, slotSec)` buckets
      the existing `model_health` rows (already storing per-check
      verdicts) into fixed-width slots. No new table required —
      the regression ticker was already writing what we needed;
      we just weren't reading it as a time series. Returns oldest
      -first so the frontend iterates left-to-right without a
      reverse. Empty windows come back as `total=0` so gap slots
      render as muted "no data" instead of forcing the frontend to
      backfill.
    - `GET /admin/model-health/{jst}/slots?count=N&slot_sec=S`
      admin endpoint (`internal/api/admin/model_health.go:Slots`).
      Count capped at 168 to bound the per-slot query fan-out.
    - WebUI: `admin.getModelHealthSlots()` + the `UptimeBarDetail`
      component in `webui/src/routes/models.tsx` now consumes the
      real slot response when the operator opens the model detail
      sheet. Empty response falls back to
      `generateEmptySlots(48)` so freshly-added models still
      render cleanly. Table view (`UptimeCell`) keeps the aggregate
      percentage — one detail-only fetch is enough, no N+1 query.
    - Tests: `TestModelHealthStore_SlotsByJST` covers the happy
      path (probes bucketed correctly, empty jst returns
      count=count of `total=0` slots) and the guard-rail
      (`count=0` returns nil).
    - Coda for P2-7: the "no data" placeholder we introduced when
      we killed `mockUptime` is now what fills the gap when the
      regression ticker hasn't run yet — an honest, self-fixing
      default. Once the ticker fires, real slots overwrite the
      placeholder on the next refetch.

---

## 5. Registration Plugin — Monorepo Split

### 5.1 Decision

The registration flow is being moved **out of `internal/`** and into a
sibling **`plugins/register/`** sub-module. Public reverse-proxy builds
compile only the interface + stub; the real automation code lives in a
separately-versioned module gated by `-tags register`.

Rationale: the sensitive scraping / captcha / cookie code should not ship
in the public higgsgo binary. Operators who need registration compile the
full variant themselves.

### 5.2 Layout

```
higgsgo/
├── go.work                              # binds main + plugins/register
├── go.mod                               # github.com/greensheep999/higgsgo
├── internal/
│   ├── ports/registrar.go               # interface (always compiled)
│   └── adapters/registrar/higgsfield/
│       ├── higgsfield_disabled.go       # default build: stub → 503
│       └── higgsfield.go (//go:build register)
│                                        # -tags register: bridge to plugin
└── plugins/
    └── register/
        └── go.mod                       # github.com/greensheep999/higgsgo/plugins/register
                                         # separately-taggable version
```

### 5.3 Current state (2026-07-18)

- ✅ Interface defined (`internal/ports/registrar.go`).
- ✅ Admin API routes mounted (`internal/api/admin/registrations.go`,
  `server.go:386`).
- ✅ Default stub returns 503 (`higgsfield_disabled.go`).
- ❌ `higgsfield.go` under `-tags register` is `panic("TODO")` for all
  four methods. Bridge not wired.
- ❌ `plugins/register/go.mod` module path is
  `github.com/higgsgo/higgsgo/plugins/register` — mismatched with main
  module `github.com/greensheep999/higgsgo`. Needs realignment + a
  top-level `go.work` file.
- ❌ `plugins/register/adapters/{camoufox,cloak}` are `not implemented`
  placeholders.
- ❌ No sqlite registration store; migration `001` creates the table but
  no CRUD adapter reads/writes it.

### 5.4 Path to working registration

1. Add top-level `go.work` binding both modules.
2. Rewrite `plugins/register/go.mod` module path to
   `github.com/greensheep999/higgsgo/plugins/register`.
3. Import `plugins/register` from `higgsfield.go` under `-tags register`
   and delegate `Enqueue`/`GetStatus`/`List`/`Retry`.
4. Write `internal/adapters/storage/sqlite/registration_store.go` +
   inject into `plugin.Deps`.
5. Fill in one working browser adapter (camoufox first — has existing
   binary; cloak later).

---

## 6. Documentation Debt

The audit found stale claims in these docs, corrected in this pass:

- `docs/POOL-AND-CPA.md` §"Concurrency" and §"Route strategies" — see this
  file's §1 and §2 for the actual state.
- `docs/ARCHITECTURE.md` §8 "sticky per-account proxy" — false; see §1
  above.
- `docs/PLUGGABLE.md` — was written assuming `internal/plugins/*`. Rewritten
  around the monorepo split in §5.
- `docs/CHANGELOG.md` — was 13 commits behind. Refreshed.
- `docs/API_REFERENCE.md` — missing 12 admin endpoints. Backfilled.

If you find a doc that contradicts the code, update the doc *and* file an
entry here in §1's "stored but never enforced" table, then decide P0/P1/P2.
