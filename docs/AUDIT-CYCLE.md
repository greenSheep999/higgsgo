# Audit → Delivery: July 2026

> A running log of the audit that ran through the pool / admin / WebUI
> surfaces in July 2026 and the fixes that closed every finding. Kept
> as a milestone document so future audits (and future engineers) can
> see what "one honest cycle" looks like: what we found, what we
> shipped, and what we deliberately left for later.
>
> For live status of individual items see `docs/ROADMAP.md`. This file
> is the narrative; ROADMAP is the ledger.

---

## 1. Why the audit ran

The WebUI had just landed and the operator-facing story was: log in,
click through, configure the pool. The concern was that many of those
click-throughs — group concurrency caps, monthly credit budgets,
bound proxy URLs — might be doing nothing at runtime. If an operator
set `group.max_concurrent_jobs = 5` and the pool ignored it, the tool
was actively misleading, not merely under-documented.

So we ran a systematic sweep across four axes:

1. **Priority** — is `account.priority` /
   `account_group_members.priority` actually consulted?
2. **Concurrency** — do the group / per-account caps enforce anything?
3. **Proxy** — does `bound_proxy_url` route requests, or is it
   cosmetic?
4. **Load balancing** — do the five `RouteStrategy` values behave the
   way their names imply?

We also spot-checked the failover controller and the registration
plugin path since those were the two most recent additions.

## 2. What the audit found

The findings, sorted worst first. Numbers are pre-fix.

### 2.1 Storage-and-ignore fields (the "silent no-op" class)

Nine columns / struct fields that the admin API accepted writes for,
the WebUI let operators edit, and the runtime never read:

| Field | Storage | Runtime behaviour before fix |
|---|---|---|
| `account.bound_proxy_url` | ✅ persisted, PATCH works | 🚫 Every request went through the process-level `HIGGSGO_UPSTREAM_PROXY_URL`. All accounts shared one egress IP. |
| `group.max_concurrent_jobs` | ✅ persisted | 🚫 `PickAndLock` never consulted it. |
| `group.max_concurrent_per_account` | ✅ persisted | 🚫 Hardcoded literal `5` won. |
| `group.allowed_models_regex` | ✅ persisted | 🚫 Only read by dead `resolver` code. |
| `group.blocked_models_regex` | ✅ persisted | 🚫 Same. |
| `group.monthly_credit_budget` | ✅ persisted | 🚫 `IncrementUsed` had no caller. |
| `group.monthly_credit_used` | ✅ persisted | 🚫 Same. |
| `account_group_members.priority` | ✅ persisted, sort key applied | ⚠️ Backend read it, but WebUI couldn't edit — every row landed on DB default 100. |
| `apikey.monthly_quota` | ✅ persisted | ⚠️ Only checked in `metering.Recorder` AFTER the job completed. Over-quota keys kept firing pool slots. |

### 2.2 Fake data in the WebUI

- **Account probe button** was a `toast.success("Health check triggered")`.
  No backend endpoint existed.
- **Uptime bars on the models page** called a `mockUptime()` helper
  that fabricated 95–100% uptime for every model with no
  `/admin/model-health` row. `generateMockSlots` produced pseudo-random
  green/yellow/red blocks seeded from the JST string. A model that had
  never been probed still looked ~97% healthy.

### 2.3 Semantic bugs

- **`in_flight_jobs` leaked on panic.** The proxy had three
  hand-rolled `unlock()` sites without a `defer`. A panic anywhere
  between `PickAndLock` and any of them stranded the counter — and
  since it lived in SQLite, a restart didn't clear it either.
- **Async jobs released their slot too early.** The `unlock()` fired
  the moment `CreateJob` returned. The pollworker never touched
  `in_flight`, so an async video job upstream ran for 30-40 minutes
  while the pool thought the account had zero in-flight work. This
  made P0-3's group concurrency caps useless for the exact case they
  were meant to solve: async bursts.
- **`round_robin` was actually LRU.** No round-tracking, no
  randomization; `ORDER BY last_used_at ASC LIMIT 1`. Under bursts of
  concurrent picks the same row won repeatedly until its
  `last_used_at` commit advanced.
- **Multi-binding API keys returned 400.** A key bound to two groups
  couldn't pick between them without an explicit `group_id` in the
  request — no automatic fallback when one group was saturated.

## 3. What we shipped

12 fixes across four rounds, each landing as its own commit with tests
and a doc update. Every fix references a `Pn-M` tag that shows up in
git log, ROADMAP, and CHANGELOG so the trail stays walkable.

### Round P0 — critical, backend already had the pieces

| Tag | Fix | Commit |
|---|---|---|
| P0-1 | Group member priority editable in WebUI | `32aa2a5` |
| P0-2 | `in_flight` leak fix + boot reconciliation | `cf4000a` |
| P0-3 | Group `max_concurrent_jobs` + per-account cap enforcement | `cf4000a` |

### Round P1 — correctness, closes silent no-op fields

| Tag | Fix | Commit |
|---|---|---|
| P1-4 | Group model regex + monthly budget enforcement | `ef33fbc` |
| P1-5 | Per-account HTTP client honouring `bound_proxy_url` | `24c19c5` |

### Round P2 — user-facing honesty

| Tag | Fix | Commit |
|---|---|---|
| P2-6 | Real probe endpoint (`upstream.FetchWallet` + classified errors) | `7b9357a` |
| P2-7 | Mock uptime removed; `generateEmptySlots` + "no data" state | `d2c3fc6` |
| P2-8 | Jittered LRU tiebreaker on every RouteStrategy | `04bec6c` |
| P2-9 | Per-Key monthly quota pre-pick gate | `1c588f7` |

### Round P3 — new capabilities from audit signals

| Tag | Fix | Commit |
|---|---|---|
| P3-10 | Cross-group spillover for multi-binding API keys | `feec5d8` |
| P3-11 | Async job in_flight tracking across full lifecycle | `50142d3` |
| P3-13 | Per-slot time-series probe data (real bars in detail sheet) | `cb87a47` |

## 4. What we deliberately did NOT do

Not everything the audit surfaced needed fixing right now. Explicit
deferrals so the next audit doesn't re-flag them as oversights:

- **`internal/core/resolver/`** — the config-cascade engine
  (Key > Group > Account > Global precedence for concurrency, proxy,
  route, budget, regex, rate limit, markup) is preserved but not
  wired. Rationale: P1-4 (group gates) + P2-9 (key quota) delivered
  the group- and key-level gates directly against the `Group` /
  `APIKey` domain types without needing the resolver's ordered
  cascade. The resolver becomes valuable when we add per-Key rate
  limits and per-Key model regex — features that need a domain
  migration first.
- **`APIKey.RateLimitRPS` / `APIKey.RateLimitBurst` / per-Key model
  regex** — not present in the domain struct yet. Adding them requires
  a migration + admin UI + product decision on rate-limit semantics
  (per-key vs per-account vs per-model). Tracked but not scheduled.
- **Weighted / lease-based load balancing (P3-12)** — deferred until
  we have production QPS data showing the jittered LRU (P2-8)
  isn't good enough. Adding an in-memory lease table with TTL is
  a schema-free but subtle change; premature without hot-spot
  evidence.
- **`account.max_concurrent` per-row override** — column exists,
  isn't enforced, kept because `group.max_concurrent_per_account`
  (P0-3) already covers the operator use case. Wiring the per-row
  field would require picking an override precedence (row vs group);
  no operator has asked.

## 5. Testing footprint

Every fix landed with tests. Rough tally by round:

- **P0**: 3 SQL-level tests (group cap, per-account cap, in_flight
  reset), plus fake-store updates across seven test files for the
  new `ResetAllInFlight` method.
- **P1**: 11-case matrix on `enforceGroupGates`, 6 cases on
  `enforceKeyGates`, 4 on `utls.Pool`, 2 on the upstream `Resolver`
  wiring.
- **P2**: 11 probe-endpoint cases (success + 8 error-kind matrix + 2
  guards), 1 jitter-spread test, 6 per-key quota cases.
- **P3**: 3 pollworker in_flight lifecycle tests, 8 spillover-
  eligibility cases, 1 spillover contract test, 1 slots-bucketing
  test.

Total: **50+ new test cases**. All pre-existing tests still pass;
several stub `AccountStore` / `GroupStore` implementations across the
test suite were updated to satisfy the new port methods
(`ResetAllInFlight`, `ListMembersWithPriority`, `SlotsByJST`).

## 6. Commit hygiene

Each fix is exactly one commit (with one exception: P0-2 and P0-3
share a commit because they touched the same `account_store.go`
functions and separating them would have produced two incoherent
diffs; the commit message calls that out explicitly). Every commit
carries a `Pn-M` tag in the subject line for grep-ability.

Doc changes ride with the code they document — not batched
separately. When P1-5 landed the sticky-proxy fix, the same commit
flipped the `ARCHITECTURE.md` §8 status from ❌ to ✅.

## 7. What operators see now

- Setting `group.max_concurrent_jobs = 5` actually caps that group at
  5 concurrent in-flight jobs. Attempting a sixth returns HTTP 429
  `pool_saturated` with a retryable body.
- Setting `account.bound_proxy_url = socks5://…` routes THAT
  account's requests through THAT proxy. Different accounts on
  different proxies get different egress IPs. Higgsfield sees sticky
  IPs the way registration promised.
- Setting `group.allowed_models_regex = ^seedance-` blocks non-matching
  aliases with 403 `model_not_allowed` before consuming a pool slot.
- Setting `apikey.monthly_quota = 100000` refuses new jobs with 402
  `quota_exhausted` once the key hits its cap, instead of racking up
  Higgsfield charges the caller can't be billed for.
- Clicking "Health" on an account row calls
  `POST /admin/accounts/{id}/probe`, gets a real wallet response
  through the account's own proxy in ~1-2 seconds, and shows latency
  + balance in a toast. Failures classify by kind so an expired
  session, a broken proxy, and a Higgsfield outage all look different.
- Uptime bars on the models page show real per-slot pass/fail counts
  for models the regression ticker has probed. Freshly-added models
  render as muted "No data" instead of a fabricated 97%.
- API keys bound to multiple groups automatically try each in name-
  sorted order when the primary saturates.

## 8. Next audit

The natural next axis is throughput: none of these fixes were
designed for high QPS. When production traffic reveals patterns, the
things to re-examine are (a) the RANDOM() tiebreaker under sustained
load, (b) the per-slot query fan-out in `SlotsByJST` at high probe
frequency, and (c) whether the failover controller's window-based
throttling reacts fast enough. All three have knobs already; the
question is whether the defaults hold up.

---

*Cycle closed 2026-07-19. 12 fixes shipped, 9 silent no-op fields
retired, 2 fake WebUI surfaces replaced with real ones, 50+ tests
added. ROADMAP §1's "stored but never enforced" table is now
entirely green.*
