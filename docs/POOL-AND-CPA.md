# higgsgo Pool Management + CPA Dual-Mode + Metering

> Design supplement to ARCHITECTURE.md + PLUGGABLE.md.
>
> **Status legend used throughout:**
> - ✅ **wired** — code exists and runs on the request path
> - ⚠️ **partial** — defined + stored + admin-editable, only partially enforced
> - ❌ **silent no-op** — accepted by admin API / UI, but the runtime ignores it
>
> When a claim in this file diverges from the code, `docs/ROADMAP.md`
> §1 is authoritative and lists the exact `file:line` evidence.
>
> Three requirements this design covers:
> 1. **Per-account usage / request count stats** — clear and queryable ✅
> 2. **Account pool groups** with per-group concurrency and quota — ⚠️ groups exist and route, quota/concurrency fields are stored but not yet enforced (see §7.1, ROADMAP P0/P1)
> 3. **Two consumption modes coexist**: higgsgo self-managed vs CPA-managed — Mode A ✅, Mode B still in flight
>
> Long-term plan: higgsgo + higgs-cpa-plugin published separately, usable
> **standalone** (self-hosted WebUI) or **integrated with CPA**.

---

## 1. Two Consumption Modes at a Glance

```
     ┌──────────────────────────────────────────────────────┐
     │           Upstream higgsfield.ai API                 │
     └──────────────────────────────────────────────────────┘
                            ▲
                            │  higgs upstream call (single entry)
                            │
     ┌──────────────────────┴──────────────────────────────┐
     │                  higgsgo                             │
     │                                                      │
     │  ┌──────────┐   ┌─────────┐   ┌──────────────────┐  │
     │  │ Pool mgmt│──▶│  Proxy  │──▶│ Account executor │  │
     │  │          │   │(reverse)│   │  (JWT/JA3/proxy) │  │
     │  └──────────┘   └─────────┘   └──────────────────┘  │
     │       ▲             ▲                                │
     │       │             │                                │
     │  Mode A: self-mgd   Mode B: CPA-managed              │
     │  ┌────┴─────┐       ┌────┴──────┐                    │
     │  │ User via │       │ User via  │                    │
     │  │ higgsgo  │       │ CPA API   │                    │
     │  │ /v1/*    │       │ (New API  │                    │
     │  │ api_key  │       │ or similar)                    │
     │  └──────────┘       └───────────┘                    │
     └──────────────────────────────────────────────────────┘
             ▲                    ▲
             │                    │
     ┌───────┴─────┐      ┌──────┴────────────────┐
     │ Mode A user │      │ Mode B: higgs-cpa-    │
     │ calls direct│      │ plugin ← CPA platform │
     └─────────────┘      └───────────────────────┘
```

### Mode A: higgsgo self-managed (standalone)

- User holds a higgsgo-issued API key (`sk-hg-xxx`)
- Calls `https://higgsgo.example.com/v1/videos/generations` directly
- higgsgo **ships its own WebUI** for pool management (`/admin/*`)
- Billing, rate limiting, stats are all built into higgsgo

### Mode B: CPA-managed (plugin)

- User holds an API key issued by the CPA platform (the existing CPA platform already has an API key / quota system)
- User calls the CPA platform's endpoint (e.g. `https://cpa.example.com/v1/videos/generations`)
- CPA internally forwards the request to higgsgo
- Pool, API keys, and billing are all managed by CPA; higgsgo only provides **single-account executor capability** ("give me this account, run this job")

### The two modes **coexist** inside the **same higgsgo process**

Toggled from config:
```toml
[modes]
standalone = true          # enable self-managed mode
cpa_plugin = true          # enable CPA plugin mode (exposes internal API for CPA)
```

---

## 2. Account Pool Groups

### 2.1 Data Model

```sql
CREATE TABLE account_groups (
  id            TEXT PRIMARY KEY,
  name          TEXT UNIQUE NOT NULL,      -- "default" / "customer-abc" / "internal-test"
  description   TEXT,

  -- group-level quotas
  max_concurrent_jobs      INTEGER,        -- max concurrent jobs across the whole group (across accounts)
  max_concurrent_per_account INTEGER,      -- per-account cap inside the group (default 5)
  monthly_credit_budget    INTEGER,        -- monthly budget cap for the group (credits × 100)
  monthly_credit_used      INTEGER DEFAULT 0,

  -- allowed models (whitelist / blacklist)
  allowed_models_regex     TEXT,           -- ".*" means all
  blocked_models_regex     TEXT,           -- "seedance-2|veo3.*" blocks expensive models

  -- routing strategy
  route_strategy TEXT DEFAULT 'round_robin',  -- round_robin / least_used / cheapest_first

  -- ownership (which api_key or CPA account can use this group)
  owner_type   TEXT NOT NULL,              -- 'apikey' / 'cpa_partner' / 'internal'
  owner_id     TEXT,                       -- api_keys.id / partner id inside CPA

  status       TEXT NOT NULL DEFAULT 'active',
  created_at   DATETIME NOT NULL
);

-- account group membership (an account can belong to multiple groups)
CREATE TABLE account_group_members (
  account_id  TEXT NOT NULL,
  group_id    TEXT NOT NULL,
  priority    INTEGER DEFAULT 100,          -- priority within the group
  added_at    DATETIME NOT NULL,
  PRIMARY KEY (account_id, group_id)
);

-- API key to group binding (a key can only use groups it is bound to)
CREATE TABLE apikey_group_bindings (
  api_key_id  TEXT NOT NULL,
  group_id    TEXT NOT NULL,
  PRIMARY KEY (api_key_id, group_id)
);
```

### 2.2 Use Cases

**Case 1: Customer A dedicated to a few Plus accounts**
```
group_id = "customer-abc"
- max_concurrent_jobs = 20
- monthly_credit_budget = 500000  (5000 credits/month)
- allowed_models_regex = ".*"
- member accounts: Plus×2, Pro×1
- bound api_key = "sk-hg-abc-xxx"
```

**Case 2: Internal test group (uses cheap starter accounts)**
```
group_id = "internal-test"
- max_concurrent_jobs = 2
- allowed_models_regex = "^(nano-banana|z-image|reve)$"  # cheap images only
- member accounts: Starter×8
```

**Case 3: Video-only group (isolates expensive-model accounts)**
```
group_id = "video-premium"
- allowed_models_regex = "^(seedance-2|veo3.*|sora2.*|kling-3)$"
- member accounts: Plus×1, Pro×1
```

### 2.3 Pool Pick Logic (with groups)

> **Reality vs design.** The current `PickAndLock` at
> `internal/adapters/storage/sqlite/account_store.go:395-503` implements
> the *design intent* below only partially. Enforced clauses are marked
> ✅; silent no-ops are marked ❌ with the corresponding ROADMAP item.

```go
func (s *accountStore) PickAndLock(ctx context.Context, p PickParams) (*Account, error)

// SQL that runs today (paraphrased from account_store.go):
SELECT a.* FROM accounts a
WHERE a.status = 'active'
  AND a.in_flight_jobs < 5                                    // ❌ hardcoded literal (P0-3 replaces with resolver value)
  AND a.subscription_balance >= ? * 1.2                       // ✅ per-account balance headroom
  AND (:GroupID = '' OR a.id IN (SELECT account_id FROM account_group_members WHERE group_id = :GroupID))
                                                              // ✅ group membership filter
  AND ...is jst in the group's allowed_models_regex...        // ✅ enforced pre-pick by proxy.Service.enforceGroupGates (P1-4)
  AND SUM(a.in_flight_jobs) OVER group < g.max_concurrent_jobs
                                                              // ✅ enforced inside the pick tx (P0-3)
ORDER BY
  CASE :RouteStrategy
    WHEN 'least_used'         THEN (a.total_plan_credits - a.subscription_balance) ASC
    WHEN 'cheapest_first'     THEN CASE a.plan_type WHEN 'free' 1 WHEN 'starter' 2 ... END ASC
    WHEN 'most_credits_first' THEN (a.subscription_balance + a.credits_balance) DESC
    WHEN 'priority'           THEN COALESCE(m.priority, a.priority) DESC
    ELSE                          a.last_used_at ASC        -- 'round_robin' is actually LRU
  END,
  a.last_used_at ASC                                          // tie-breaker
LIMIT 1
FOR UPDATE                                                    // BEGIN/COMMIT tx protects SELECT+UPDATE atomicity
```

**Group budget enforcement.** ✅ Live since P1-4.
`proxy.Service.enforceGroupGates` compares
`MonthlyCreditUsed + EstCost` against `MonthlyCreditBudget` before
the pick; over-budget returns `ErrGroupQuotaExhausted` → HTTP 402
`group_budget_exhausted`. `metering.Recorder.OnJobTerminal` calls
`GroupStore.IncrementUsed` at every non-zero terminal so the counter
climbs against real-world spend. Zero budget disables the check.

### 2.4 Group Default Policy

- Every imported account is placed in the `default` group by default
- API keys with no group binding default to `default`
- The `default` group has no group-level quota limits unless an admin sets one

---

## 3. Usage & Metering

### 3.1 Recording Dimensions (multi-dimensional aggregation)

On each job completion, write **one usage detail row** + **multiple aggregation buckets**:

```sql
CREATE TABLE usage_events (
  id               TEXT PRIMARY KEY,
  ts               DATETIME NOT NULL,        -- second precision
  
  -- Who
  api_key_id       TEXT,                     -- Mode A: issued by higgsgo
  cpa_partner_id   TEXT,                     -- Mode B: partner on the CPA side
  cpa_user_id      TEXT,                     -- Mode B: end user on the CPA side
  group_id         TEXT NOT NULL,            -- which group was used
  account_id       TEXT NOT NULL,            -- which specific account

  -- What
  model_alias      TEXT NOT NULL,            -- "seedance-2"
  jst              TEXT NOT NULL,            -- "seedance_2_0"
  media_type       TEXT NOT NULL,            -- "video"/"image"/"audio"

  -- Cost
  upstream_cost    INTEGER,                  -- cost field returned by higgsfield
  actual_credits   INTEGER,                  -- real consumption computed from balance delta (credits × 100)
  charged_credits  INTEGER,                  -- quota charged to the user
  markup_pct       REAL,                     -- markup multiplier
  
  -- Outcome
  status           TEXT NOT NULL,            -- completed/failed/refunded
  latency_ms       INTEGER,                  -- from request to result_url
  poll_count       INTEGER,
  error_type       TEXT,                     -- body/upstream/rate/gate/timeout

  -- Job reference
  higgsgo_job_id   TEXT NOT NULL,
  upstream_job_id  TEXT,
  result_url       TEXT,

  -- Denormalized for query speed
  billing_month    TEXT NOT NULL,            -- "2026-07"
  billing_day      TEXT NOT NULL             -- "2026-07-17"
);

CREATE INDEX idx_usage_api_key_month ON usage_events(api_key_id, billing_month);
CREATE INDEX idx_usage_cpa_month ON usage_events(cpa_partner_id, billing_month);
CREATE INDEX idx_usage_group_day ON usage_events(group_id, billing_day);
CREATE INDEX idx_usage_account_day ON usage_events(account_id, billing_day);
CREATE INDEX idx_usage_model_day ON usage_events(model_alias, billing_day);

-- Pre-aggregated table (updated every 5min by a scheduled task, avoids group-by on a huge table)
CREATE TABLE usage_daily_agg (
  day              TEXT NOT NULL,            -- "2026-07-17"
  api_key_id       TEXT,
  cpa_partner_id   TEXT,
  group_id         TEXT,
  account_id       TEXT,
  model_alias      TEXT,
  
  request_count    INTEGER NOT NULL DEFAULT 0,
  completed_count  INTEGER NOT NULL DEFAULT 0,
  failed_count     INTEGER NOT NULL DEFAULT 0,
  refunded_count   INTEGER NOT NULL DEFAULT 0,
  total_credits    INTEGER NOT NULL DEFAULT 0,    -- sum of actual_credits
  charged_credits  INTEGER NOT NULL DEFAULT 0,    -- sum of charged_credits
  total_latency_ms INTEGER NOT NULL DEFAULT 0,
  
  updated_at       DATETIME NOT NULL,
  PRIMARY KEY (day, api_key_id, cpa_partner_id, group_id, account_id, model_alias)
);
```

### 3.2 Query API (admin)

```
GET /admin/usage
  ?since=2026-07-01
  &until=2026-07-17
  &group_by=api_key,day        # aggregate by api_key + day
  &filter.group_id=customer-abc
  &filter.model=seedance-2

Response:
[
  { "api_key_id":"sk-hg-abc-1", "day":"2026-07-01",
    "request_count":42, "completed":40, "failed":2,
    "total_credits":836, "charged_credits":1250,
    "avg_latency_ms":8200 },
  ...
]
```

### 3.3 Per-Account View (how much each account spent)

```
GET /admin/accounts/{id}/usage?since=...

Response:
{
  "account_id": "user_3GPd...",
  "email": "owmr1rhwz1@vietnamcashewnuts.space",
  "plan_type": "pro",
  "period": "2026-07-01 → 2026-07-17",
  "summary": {
    "requests": 128,
    "completed": 115,
    "failed": 13,
    "credits_consumed": 178.5,
    "credits_remaining": 425.5,
    "top_models": [
      { "model": "seedance-2", "count": 25, "credits": 45 },
      { "model": "veo3-1", "count": 8, "credits": 58 }
    ]
  }
}
```

### 3.4 Three-Layer Accounting

**Layer 1: Upstream cost** (`actual_credits`)
- **True** consumption computed from the delta of `subscription_balance` before and after
- One usage_event per job

**Layer 2: Group budget** (`group.monthly_credit_used`)
- Atomic += actual_credits on each job completion
- Once `monthly_credit_budget` is reached, all requests in the group return 402

**Layer 3: User / API key budget** (`api_keys.monthly_used`)
- Atomic += charged_credits on each job completion (may include markup)
- Mode B (CPA) bills on the CPA side; higgsgo just reports events

### 3.5 Billing Reconciliation (drift protection)

A scheduled task reconciles every hour:
- Pull each account's wallet from the higgsfield API
- Compare against the cumulative `usage_events`
- Alert if the delta exceeds 5% (possible unrecorded jobs)

---

## 4. Mode B: CPA Plugin Integration

### 4.1 What is higgs-cpa-plugin

An **independent Node/TS package** (its own git repository) installed into the CPA platform. CPA's existing pool capability then manages higgsgo accounts, except each "account" on the CPA side is really **a higgsgo API-call credential**.

```
higgs-cpa-plugin/
├── package.json
├── src/
│   ├── register.ts           # trigger higgsgo's registration API
│   ├── executor.ts           # call higgsgo /v1/* reverse proxy
│   ├── balance.ts            # query per-account balance
│   ├── metrics.ts            # report usage
│   └── health.ts             # healthcheck
└── cpa-descriptor.json       # CPA plugin metadata
```

**CPA existing capabilities (reused)**:
- Pool management (CRUD)
- API key issuance and binding
- User quota control
- Web UI

**Capabilities added to CPA (via plugin)**:
- Ability to call higgsgo's reverse proxy
- Ability to use higgsgo's registration endpoint to top up the pool
- Reporting higgsgo-side usage

### 4.2 Internal API higgsgo Exposes to CPA

In addition to the public `/v1/*` reverse proxy, an extra `/internal/*` surface exists for the CPA plugin:

```
POST /internal/register
  X-Internal-Token: <shared secret from config>
  Body: { email, password?, mailbox_provider?, refresh_token?, proxy_url? }
  → triggers higgsgo registration; returns { registration_id, status:"pending" }

GET  /internal/registrations/{id}
  → { status, account_id?, error? }

POST /internal/accounts/{id}/execute
  Body: {
    model_alias, params, image_url?, video_url?, async?
  }
  → runs one generation on the specified account_id (bypasses pool selection)
  → returns { job_id, status, result_url? }

GET  /internal/accounts/{id}/balance
  → { subscription_balance, credits_balance, plan_type, has_unlim, ... }

GET  /internal/accounts/{id}/health
  → { status, in_flight_jobs, last_used_at, ... }

POST /internal/accounts/{id}/refresh_jwt
  → force refresh JWT

DELETE /internal/accounts/{id}
  → remove from higgsgo pool (notify when CPA-side account stops working)

WebSocket /internal/events
  → push usage / status_change events to CPA's real-time dashboard
```

### 4.3 CPA-Side Account = higgsgo-Side Account (1:1 mapping)

Each "account" in the CPA pool records:
```json
{
  "id": "cpa-account-uuid",
  "provider": "higgsgo",
  "external_id": "user_3GPd9jsbehZYBJsnJiA9Zvf2PFp",   // higgsgo account_id
  "higgsgo_endpoint": "https://higgsgo.internal.example.com",
  "plan_type": "pro",
  "credits_balance": 425.5,
  "last_synced_at": "..."
}
```

**Sync strategy**:
- CPA plugin pulls `/internal/accounts/{id}/balance` every 5min
- higgsgo pushes usage events in real time over WebSocket

### 4.4 Usage Isolation Across the Two Modes

The same higgsgo account can:
- Belong to higgsgo internal group A (Mode A users hit this group)
- Be referenced by the CPA plugin (Mode B users route through CPA)

`usage_events` distinguishes ownership via `api_key_id` vs `cpa_partner_id`:
- Mode A job: `api_key_id != null`, `cpa_partner_id = null`
- Mode B job: `api_key_id = null`, `cpa_partner_id = "cpa-partner-xxx"`

**Avoiding contention**: if an account is used by both Mode A and Mode B, `concurrent_jobs_limit=6` is a hard cap — first come, first served. Accounts can be given a "dedicated mode" via config:

```toml
[[accounts.dedication]]
account_id = "user_3GPd..."
mode = "cpa_only"        # CPA-only

[[accounts.dedication]]
account_id = "user_3GPi..."
mode = "standalone_only" # self-managed only

# Default (unset): both modes share
```

### 4.5 CPA's New API (dispatch / forwarding)

Your mention of "using CPA's pool-management capability to dispatch to the New API":

**CPA current flow** (assumed):
- The platform holds a bunch of "upstream resource pools" (e.g. OpenAI accounts, Claude accounts, Midjourney accounts)
- Each pool has its own API key generation + usage billing
- Users apply for access to a pool and receive a CPA-issued key

**After adding higgs-cpa-plugin**:
- A new upstream type `higgsgo` is added
- CPA users apply for higgsgo access and receive a CPA-issued key
- User calls the CPA endpoint (may be branded New API); CPA forwards internally through the plugin to higgsgo
- After higgsgo returns result_url, CPA persists billing

Benefits:
- One CPA platform manages all upstream resources
- Users don't have to swap API keys — the same key routes to multiple upstreams
- Usage, rate limiting, and billing are unified in CPA

---

## 5. WebUI (Mode A only)

Under Mode A, higgsgo needs its own Web UI, equivalent to CPA's pool-management pages:

```
higgsgo-webui/                     # standalone sub-project (frontend)
├── package.json (React/Vue/Svelte — pick one)
└── src/
    ├── pages/
    │   ├── Dashboard.tsx         # pool overview / usage charts
    │   ├── Accounts.tsx          # account list / detail / manual ops
    │   ├── Groups.tsx            # group management
    │   ├── ApiKeys.tsx           # API key issuance
    │   ├── Registrations.tsx     # registration task queue
    │   ├── Usage.tsx             # usage reports (by key/account/model/day)
    │   ├── Models.tsx            # model list / manual reload
    │   ├── HealthChecks.tsx      # A regression / X1 recheck results
    │   └── Settings.tsx          # config editing (reload signal)
    └── ...
```

Frontend and backend communicate over higgsgo's `/admin/*` API (same process).

---

## 6. Full Directory (final v0.3)

```
higgsgo/                            # Go monorepo
├── go.mod
├── cmd/
│   ├── higgsgo/main.go
│   └── higgsgo-cli/main.go
│
├── internal/
│   ├── domain/                     # pure business
│   │   ├── account.go
│   │   ├── group.go               # ← new
│   │   ├── apikey.go
│   │   ├── job.go
│   │   ├── usage_event.go         # ← new
│   │   ├── model_spec.go
│   │   └── errors.go
│   │
│   ├── ports/                     # Provider interfaces
│   │   ├── proxy.go
│   │   ├── mailbox.go
│   │   ├── captcha.go
│   │   ├── browser.go
│   │   ├── storage.go
│   │   ├── notifier.go
│   │   ├── httpclient.go
│   │   ├── modelregistry.go
│   │   ├── metering.go            # ← new: usage reporting
│   │   └── clock.go
│   │
│   ├── core/
│   │   ├── pool/                  # account pool (group-aware)
│   │   ├── proxy/                 # reverse proxy
│   │   ├── register/
│   │   ├── jwt/
│   │   ├── metering/              # ← new: usage accounting + pre-aggregation
│   │   ├── groups/                # ← new: group management logic
│   │   └── ticker/
│   │
│   ├── adapters/                  # Provider implementations (omitted, see v0.2)
│   │
│   ├── api/
│   │   ├── v1/                    # Mode A: public OpenAI-compatible
│   │   ├── admin/                 # Mode A: admin surface
│   │   ├── internal/              # ← new: Mode B (CPA plugin) surface
│   │   │   ├── register.go
│   │   │   ├── execute.go
│   │   │   ├── accounts.go
│   │   │   └── events_ws.go
│   │   └── middleware/
│   │
│   ├── config/
│   └── observability/
│
├── configs/
├── data/
│
└── docs/

# Standalone git repos (future)
higgs-cpa-plugin/                   # TypeScript/Node, installed into CPA
higgsgo-webui/                      # Web frontend (used with Mode A)
```

---

## 7. Key Design Decisions (Follow-up Questions)

### 7.1 How to Compute Group-Level Concurrency

**Design.** `max_concurrent_jobs` is **aggregated across accounts** — the
sum of `in_flight_jobs` across every account in the group; enforced by a
`SUM()` subquery in `PickAndLock` WHERE.

**Reality (2026-07-18).** ❌ Not enforced. `PickAndLock` at
`account_store.go:418` still uses a hardcoded per-account
`in_flight_jobs < 5`. `GroupStore.CurrentInFlight`
(`group_store.go:339`) exists and computes the aggregate, but has no
production caller. See ROADMAP P0-3.

### 7.2 Group Routing Strategies

`route_strategy` accepts five values (`internal/domain/group.go:8-29`).
The `internal/adapters/storage/sqlite/account_store.go:445-479`
`ORDER BY` implementations are listed as they actually run today:

- `round_robin` — **misnamed; actually LRU.**
  `ORDER BY last_used_at ASC LIMIT 1`. No round-tracking, no
  randomization. Under concurrent bursts the same row wins repeatedly
  until the surrounding tx commits (hot-spot risk).
- `least_used` — `ORDER BY (total_plan_credits - subscription_balance)
  ASC, last_used_at ASC`. **Sorts by lifetime consumed credits**, not
  month-to-date. If you need "spread this month's usage evenly", we still
  need to wire it up against `monthly_credit_used`.
- `cheapest_first` — `ORDER BY CASE plan_type ... END ASC, last_used_at
  ASC`. Hardcoded 11-tier ladder (`free`=1 … `enterprise`=11). Burns
  low-tier plans first, preserves high-tier.
- `most_credits_first` — `ORDER BY (subscription_balance +
  credits_balance) DESC, last_used_at ASC`.
- `priority` — `ORDER BY m.priority DESC` when a group is set (member
  priority from `account_group_members.priority`, backend ✅ / WebUI ❌ —
  ROADMAP P0-1), else `accounts.priority DESC`.

Every strategy uses `last_used_at` as a tie-breaker and returns exactly
one row (`LIMIT 1`). There is **no** weighted round-robin,
least-connections, fair-share reservation, or in-memory counter. If you
need real fair-share, ROADMAP P2-8 sketches the cheapest upgrade
(jittered in-memory pick counter).

### 7.3 API Key → Group Mapping

**Design.** A key can be bound to multiple groups; tried in some order;
falls back on exhaustion; every-group-exhausted returns 429/402.

**Reality (2026-07-18).** ⚠️ Partial. `resolveGroup`
(`internal/api/v1/handler.go:318-352`) picks **exactly one** group via
this precedence: (1) explicit `group_id` in the request body, (2)
`api_keys.group_id` direct column, (3) if a key has exactly one entry in
`apikey_group_bindings` use it, if it has zero fall back to the global
pool (empty group filter), if it has multiple return
`400 ambiguous_group`. There is **no inter-group spillover** — an
exhausted group returns without trying the caller's other bindings. See
ROADMAP P3-10 if we decide spillover is needed.

### 7.4 CPA Plugin: REST or gRPC

**REST** (initial): the CPA side already has an HTTP client stack; Node/TS integration is simple

**gRPC** (future): if CPA scales up + emits many event pushes, gRPC bi-di is better

REST + WebSocket events to start.

### 7.5 Write Amplification of Usage Events

Under the current design of one usage_event per job, at a daily 10k-100k request volume, SQLite handles a single-table row count of a few million just fine; Postgres is comfortable. At scale, add:
- Monthly partitioning (`usage_events_2026_07`)
- Archive cold data to object storage

---

## 8. Open Questions (Need Your Call)

1. **Group membership rules**: an account can belong to multiple groups (allow many-to-many), or is it restricted to a single group? I lean many-to-many, but that raises the question of "which group's credits get charged"
2. **Group quota window**: monthly / daily / hourly all supported, or monthly only?
3. **CPA plugin auth**: is an X-Internal-Token shared secret enough, or do we need mTLS?
4. **Billing markup location**: Mode A does it in higgsgo (api_keys carries markup_pct), Mode B does it in CPA (higgsgo only reports actual_credits) — is that right?
5. **Whether the CPA plugin is also written in Go**: or TypeScript? If the CPA platform is Node, TS integration is simpler
6. **Minimum viable WebUI scope**: Accounts + Usage + ApiKeys three pages to start?
7. **Registration external API**: Mode A's `POST /admin/registrations` and Mode B's `POST /internal/register` have identical logic — should they share a handler with different middleware?

---

**This is the v0.3 pool + CPA supplement. Together the three docs make up the complete design**:

1. `HIGGSGO-ARCHITECTURE.md` v0.1 — overall skeleton, directory, DB, deployment
2. `HIGGSGO-PLUGGABLE.md` v0.2 — Provider abstraction, pluggable layer
3. `HIGGSGO-POOL-AND-CPA.md` v0.3 — pool groups, usage metering, dual mode + CPA plugin

**Once you've reviewed all three, I'll go stand up the higgsgo/ directory**.
