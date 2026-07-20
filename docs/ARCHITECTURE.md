# higgsgo Architecture Design (draft v0.1)

> Goal: rewrite the entire higgsfield-register (Node) capability set (registration / account pool / reverse proxy / auth / mailbox OTP / DataDome / IP proxy) in Go as a long-running production service.
>
> Key constraints (all empirically established in this round):
> 1. Clerk JWT expires in 60s → re-mint every 40s
> 2. `concurrent_jobs_limit = 6` per account
> 3. `credits_balance` briefly drops to 0 (credit freeze) → use `subscription_balance / 100` as the budget
> 4. `has_unlim` ≠ `*_unlimited` endpoint permission
> 5. IP check is per-user → CDN URLs from media uploaded on a shared account are reusable
> 6. Cloudflare + DataDome JA3 fingerprint detection → must use impit / utls / mimic
> 7. Top-level APP_SLUGS field is required reading for the nano_banana_2 family
> 8. OTP has two flavors: Microsoft Graph (refresh_token) + destiny-mmo (disposable inbox)
> 9. Body backend pydantic schema drifts → periodic recheck required

---

## 1. Service Boundaries

higgsgo is a **single Go binary** that exposes 3 classes of port:

```
┌─────────────────────────────────────────────────────────┐
│  higgsgo  (Go binary)                                    │
│                                                          │
│  ┌────────────┐  ┌────────────┐  ┌──────────────────┐   │
│  │ Public API │  │ Admin API  │  │ Internal Ticker  │   │
│  │  /v1/*     │  │  /admin/*  │  │  (goroutine)     │   │
│  │  OpenAI-   │  │  pool mgmt │  │  scheduled tasks │   │
│  │  compat    │  │            │  │                  │   │
│  └────────────┘  └────────────┘  └──────────────────┘   │
│                                                          │
│  ┌──────────────────────────────────────────────────┐   │
│  │  Core components (detailed in later sections)     │   │
│  │  ┌────────┐ ┌────────┐ ┌────────┐ ┌───────────┐  │   │
│  │  │ Pool   │ │ Proxy  │ │Register│ │ Mailbox   │  │   │
│  │  │ (acct) │ │ (rev.) │ │        │ │ / OTP     │  │   │
│  │  └────────┘ └────────┘ └────────┘ └───────────┘  │   │
│  │  ┌────────┐ ┌────────┐ ┌────────┐ ┌───────────┐  │   │
│  │  │ Auth   │ │Browser │ │DataDome│ │  DB (SQLite│  │   │
│  │  │        │ │        │ │        │ │   / PG)   │  │   │
│  │  └────────┘ └────────┘ └────────┘ └───────────┘  │   │
│  └──────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
        │                                          │
        ▼                                          ▼
   IP proxy pool (SOCKS5)                   higgsfield.ai
   Rotating proxies                         fnf.higgsfield.ai
                                            clerk.higgsfield.ai
```

**Why a single binary**:
- Simple deployment: one process per box
- The account pool / reverse proxy / registration all need to share DB and session state; cross-process communication adds complexity
- Go goroutines give natural concurrency: registration and reverse-proxy traffic can run in the same process without blocking each other

**When to split**:
- Registration (with Playwright / Chrome) is resource-intensive and can later be split off into a standalone registrar-worker
- If IP proxy management needs to be refined down to per-account IP binding, the proxy-manager can be split out

---

## 2. Directory Layout

### 2.0 Module Split (monorepo, decided 2026-07-18)

The repository is a **Go workspace** with two modules, so that sensitive
account-registration automation lives outside the public reverse-proxy
binary.

```
higgsgo/
├── go.work                            # binds main + plugins/register
├── go.mod                             # main:  github.com/greensheep999/higgsgo
├── internal/…                         # reverse proxy, admin, WebUI (public)
└── plugins/
    └── register/
        └── go.mod                     # plugin: github.com/greensheep999/higgsgo/plugins/register
```

**Build variants.**
- `go build ./cmd/higgsgo` (default) — compiles only the interface + a
  503-returning stub. Public release binary.
- `go build -tags register ./cmd/higgsgo` — links the bridge in
  `internal/adapters/registrar/higgsfield/higgsfield.go` to the plugin.
  Private / full-featured build.

**Contract point.** `internal/ports/registrar.go` — always compiled.
Both builds satisfy this interface; only the linkage differs.

Migration status (ROADMAP §5.4): `go.work` and module-path realignment
are still pending. Today's `-tags register` bridge panics `TODO`.

### 2.1 Public Module Layout

```
higgsgo/
├── go.mod
├── cmd/
│   ├── higgsgo/                    # main binary
│   │   └── main.go
│   └── higgsgo-cli/                # ops CLI (register accounts, check balance, migrate)
│       └── main.go
│
├── internal/
│   ├── config/                     # env vars + toml config loading
│   │   └── config.go
│   │
│   ├── db/                         # DB layer (SQLite to start, swappable to Postgres)
│   │   ├── db.go
│   │   ├── migrations/
│   │   │   ├── 001_init.sql
│   │   │   ├── 002_accounts.sql
│   │   │   ├── 003_jobs_ledger.sql
│   │   │   └── 004_registrations.sql
│   │   └── queries/                # sqlc-generated
│   │
│   ├── pool/                       # account pool
│   │   ├── pool.go                 # main interface: Pick / Return / MarkFailed
│   │   ├── router.go               # account selection strategy (by gate + credits + concurrent)
│   │   ├── ledger.go               # per-job consumption accounting
│   │   ├── refresher.go            # periodic balance + permission-matrix refresh
│   │   └── models.go               # Account struct
│   │
│   ├── upstream/                   # higgsfield client
│   │   ├── client.go               # HTTP client (with JA3)
│   │   ├── jwt.go                  # Clerk JWT re-mint (auto-refresh within 40s)
│   │   ├── jobs.go                 # POST /jobs/* + polling
│   │   ├── media.go                # media upload (image/video/audio)
│   │   ├── catalog.go              # /styles /outfits /motions and other catalogs
│   │   ├── errors.go               # error classification (body/gate/upstream/rate)
│   │   └── endpoints.go            # all endpoint URL constants
│   │
│   ├── register/                   # account registration
│   │   ├── registrar.go            # main registration orchestration
│   │   ├── flow.go                 # Playwright/chromedp interaction script
│   │   ├── capture.go              # capture cookies / JWT / DataDome clientid
│   │   ├── writer.go               # write to DB
│   │   └── worker.go               # background registration queue goroutine
│   │
│   ├── login/                      # re-login (reuse existing refresh token)
│   │   └── flow.go
│   │
│   ├── browser/                    # Playwright / chromedp wrapper
│   │   ├── launch.go               # launch fingerprinted Chrome
│   │   ├── datadome.go             # pass DataDome challenge
│   │   └── dom.go                  # OTP selectors etc.
│   │
│   ├── mailbox/                    # mailbox OTP
│   │   ├── graph.go                # Microsoft Graph (pull mail via refresh_token)
│   │   ├── destiny.go              # destiny-mmo disposable mailbox
│   │   ├── prompt.go               # interactive stdin OTP entry
│   │   └── provider.go             # unified Fetcher interface
│   │
│   ├── proxy/                      # IP proxy management
│   │   ├── pool.go                 # SOCKS5 pool (rotating / sticky per-account)
│   │   ├── provider.go             # pull proxies from upstream (711proxy etc.)
│   │   └── health.go               # scheduled proxy healthcheck
│   │
│   ├── mapping/                    # model/endpoint mapping (generated from SEALED.json)
│   │   ├── models.go               # VerifiedModels map
│   │   ├── aliases.go              # *_unlimited → proxy through to base model
│   │   ├── slugs.go                # APP_SLUGS constants
│   │   ├── starter_locked.go       # list of models Starter cannot use
│   │   └── data/                   # copies of SEALED.json / body-templates
│   │
│   ├── api/                        # HTTP handlers
│   │   ├── server.go               # startup + route registration
│   │   ├── middleware/             # apikey / logging / recovery
│   │   ├── v1/                     # OpenAI-compatible public surface
│   │   │   ├── images.go           # POST /v1/images/generations
│   │   │   ├── videos.go           # POST /v1/video/generations (+ /videos/generations alias)
│   │   │   ├── models.go           # GET /v1/models
│   │   │   ├── catalogs.go         # GET /v1/catalogs/*
│   │   │   └── jobs.go             # GET /v1/jobs/{id} async poll
│   │   └── admin/                  # admin surface
│   │       ├── keys.go             # api key CRUD
│   │       ├── accounts.go         # account pool CRUD
│   │       ├── register.go         # trigger registration
│   │       └── stats.go            # usage / success rate
│   │
│   ├── ticker/                     # scheduled tasks
│   │   ├── scheduler.go            # cron orchestration
│   │   ├── balance_refresh.go      # refresh every account's balance every 10min
│   │   ├── jwt_refresh.go          # refresh active JWTs every 40s
│   │   ├── a_regression.go         # sample 20 A-class models daily
│   │   ├── x1_recheck.go           # probe the 26 X1 models weekly
│   │   └── body_drift.go           # weekly body-drift check
│   │
│   └── observability/              # logging / metrics / tracing
│       ├── logger.go               # slog + structured logging
│       ├── metrics.go              # Prometheus metrics
│       └── audit.go                # full job-lifecycle audit log
│
├── configs/
│   ├── higgsgo.example.toml
│   └── higgsgo.dev.toml
│
├── data/                           # data migrated in from higgsfield-register
│   ├── sealed.json                 # authoritative classification
│   ├── verified-models.json        # 129 model specs
│   ├── body-templates/             # per-model exampleBody
│   ├── catalogs/                   # styles/motions/outfits/hooks etc.
│   └── reference-media.json        # shared media (3 CDN URLs)
│
├── scripts/
│   ├── migrate-node-data.sh        # import higgsfield-register/output/*.json into higgsgo DB
│   └── build.sh
│
├── docs/
│   ├── DEPLOY.md
│   ├── API.md                      # OpenAI-compatible API docs
│   └── OPERATIONS.md               # ops handbook
│
└── test/
    ├── integration/                # e2e hitting real higgsfield endpoints
    └── mock/                       # unit-test mocks
```

---

## 3. Database Design

**Start with SQLite** (single-machine, embedded, zero dependencies); migrate to Postgres later.

### 3.1 Core Tables

```sql
-- accounts: the account pool
CREATE TABLE accounts (
  id                    TEXT PRIMARY KEY,        -- user_id from clerk
  email                 TEXT UNIQUE NOT NULL,
  password              TEXT NOT NULL,           -- stored encrypted
  plan_type             TEXT NOT NULL,           -- free/starter/pro/plus/ultra/creator
  session_id            TEXT NOT NULL,           -- Clerk session
  cookies_json          TEXT NOT NULL,           -- all cookies
  user_agent            TEXT NOT NULL,           -- UA harvested at login time
  datadome_clientid     TEXT,                    -- x-datadome-clientid
  workspace_id          TEXT,

  -- API-level permissions (harvested from /user)
  has_unlim             BOOLEAN DEFAULT 0,
  has_flex_unlim        BOOLEAN DEFAULT 0,
  is_pro_veo3_available BOOLEAN DEFAULT 0,
  cohort                TEXT,

  -- balances (periodically refreshed)
  subscription_balance  INTEGER DEFAULT 0,       -- higgsfield units (credits × 100)
  credits_balance       INTEGER DEFAULT 0,       -- briefly drops to 0, not trustworthy
  total_plan_credits    INTEGER DEFAULT 0,
  plan_ends_at          DATETIME,

  -- state
  status                TEXT NOT NULL DEFAULT 'active', -- active/suspended/expired/banned
  in_flight_jobs        INTEGER DEFAULT 0,       -- current in_progress + queued count
  last_balance_check_at DATETIME,
  last_used_at          DATETIME,
  last_failed_at        DATETIME,
  fail_streak           INTEGER DEFAULT 0,

  -- IP binding (optional; some models need a sticky IP)
  bound_proxy_url       TEXT,

  registered_at         DATETIME NOT NULL,
  imported_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_accounts_pool ON accounts(status, plan_type, in_flight_jobs, subscription_balance DESC);

-- api_keys: user API keys
CREATE TABLE api_keys (
  id            TEXT PRIMARY KEY,
  key_hash      TEXT UNIQUE NOT NULL,          -- bcrypt hash; never stored in plain
  name          TEXT NOT NULL,
  created_by    TEXT,
  status        TEXT NOT NULL DEFAULT 'active',
  monthly_quota INTEGER DEFAULT 0,             -- 0 = unlimited
  monthly_used  INTEGER DEFAULT 0,
  created_at    DATETIME NOT NULL,
  last_used_at  DATETIME
);

-- jobs: reverse-proxy job records + billing
CREATE TABLE jobs (
  id                 TEXT PRIMARY KEY,          -- our own job id
  api_key_id         TEXT NOT NULL,             -- who called
  account_id         TEXT NOT NULL,             -- which account ran it
  model_alias        TEXT NOT NULL,             -- model the user requested
  jst                TEXT NOT NULL,             -- higgsfield internal job_set_type
  endpoint           TEXT NOT NULL,             -- /jobs/v2/xxx

  -- request
  request_body_json  TEXT NOT NULL,             -- full request body
  request_ts         DATETIME NOT NULL,

  -- higgsfield response
  upstream_job_id    TEXT,
  upstream_status    TEXT,                      -- queued/in_progress/completed/failed
  upstream_cost      INTEGER,                   -- cost field returned by higgsfield
  actual_cost        INTEGER,                   -- true consumption derived from balance delta
  result_url         TEXT,

  -- state machine
  status             TEXT NOT NULL,             -- pending/completed/failed/refunded/timeout
  error_type         TEXT,                      -- body_error/upstream_fail/rate_limit/gate/other
  error_detail       TEXT,
  finished_at        DATETIME,

  -- billing
  charged_credits    INTEGER,                   -- quota charged to api_key
  refunded           BOOLEAN DEFAULT 0
);

CREATE INDEX idx_jobs_account ON jobs(account_id, request_ts);
CREATE INDEX idx_jobs_api_key ON jobs(api_key_id, request_ts);

-- registrations: registration task queue
CREATE TABLE registrations (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  email          TEXT NOT NULL,
  password       TEXT,
  oauth_source   TEXT,                          -- graph/destiny/prompt
  refresh_token  TEXT,                          -- for Graph
  proxy_url      TEXT NOT NULL,
  status         TEXT NOT NULL,                 -- pending/running/completed/failed
  attempts       INTEGER DEFAULT 0,
  last_error     TEXT,
  account_id     TEXT,                          -- populated on success
  created_at     DATETIME NOT NULL,
  finished_at    DATETIME
);

-- model_health: history of scheduled rechecks (daily A sampling, weekly X1)
CREATE TABLE model_health (
  jst           TEXT NOT NULL,
  checked_at    DATETIME NOT NULL,
  verdict       TEXT NOT NULL,                  -- A_completed/B_upstream_fail/D_gated/X_backend/...
  http_status   INTEGER,
  cost          INTEGER,
  poll_time_sec INTEGER,
  PRIMARY KEY (jst, checked_at)
);

CREATE INDEX idx_health_recent ON model_health(jst, checked_at DESC);

-- proxy_pool: IP proxy pool
CREATE TABLE proxy_pool (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  url            TEXT UNIQUE NOT NULL,          -- socks5://user:pass@host:port
  provider       TEXT,                          -- 711proxy/manual/...
  region         TEXT,                          -- US/VN/IN/...
  bound_to       TEXT,                          -- account_id if sticky, null if rotating
  status         TEXT NOT NULL DEFAULT 'active',
  last_health_at DATETIME,
  last_used_at   DATETIME,
  latency_ms     INTEGER
);
```

---

## 4. Account Pool (pool package)

### 4.1 Account Selection Strategy (router.go)

```go
type PickParams struct {
    Model       string   // user-requested model alias
    JST         string   // resolved higgsfield job_set_type
    EstCost     int      // estimated cost (credits × 100)
    RequireUnlim bool    // requires has_unlim=true (uses use_unlim:true param)
    RequireUltra bool    // requires Ultra (uses *_unlimited endpoint)
}

func (p *Pool) Pick(ctx context.Context, params PickParams) (*Account, error)
```

**Ordering strategy** (SQL query):
```sql
SELECT * FROM accounts
WHERE status = 'active'
  AND in_flight_jobs < 5                            -- keep 1 slot buffer to avoid 429
  AND subscription_balance >= ?estCost * 1.2        -- 20% buffer
  AND (
    -- Starter can only run non-STARTER_LOCKED models
    plan_type IN ('pro','plus','ultra','ultimate','creator') OR
    ?jst NOT IN (SELECT jst FROM starter_locked_models)
  )
  AND (?requireUnlim = 0 OR has_unlim = 1)
  AND (?requireUltra = 0 OR plan_type IN ('ultra','ultimate','creator','scale','team','enterprise'))
ORDER BY
  -- cheap models prefer lower-tier accounts (preserve high-tier quota)
  CASE
    WHEN ?estCost < 500 THEN plan_rank ASC        -- <5 credits use starter
    ELSE plan_rank DESC                            -- >=5 credits use higher tier
  END,
  last_used_at ASC                                 -- round-robin
LIMIT 1
```

### 4.2 Lifecycle

```
Pool.Pick() → row lock (SELECT ... FOR UPDATE) → in_flight_jobs++ → return
Job completes → Pool.Return(account, cost) → in_flight_jobs-- → update balance
Job fails → Pool.MarkFailed(account, err) → fail_streak++ → suspend after 3 consecutive failures
```

### 4.3 Scheduled Tasks

- **Every 10 min** `balance_refresh.go`: run `GET /workspaces/wallet` + `GET /user` to pull each active account's true balance and permission flags
- **Every 40s** `jwt_refresh.go`: for accounts with in_flight jobs, re-mint JWT and write back to memory
- **Daily at 06:00** `a_regression.go`: sample 20 A-class models; regressions get moved to B (alert fires)
- **Every Sunday** `x1_recheck.go`: probe the 26 X1 models; newly-enabled ones need manual review

---

## 5. Reverse Proxy (api/v1)

### 5.1 Request Flow

```
POST /v1/video/generations
Authorization: Bearer <api_key>
Content-Type: application/json

{
  "model": "seedance-2",              # user alias
  "prompt": "a red apple rotating",
  "image_url": "https://...",         # optional
  "duration": 5,
  "generate_audio": false
}

─── higgsgo processing ─────────────────────────────

1. middleware:
   - api key auth (bcrypt verify → get api_keys.id)
   - usage check (monthly_used < monthly_quota)
   - request id injection into logs

2. mapping.Resolve(model):
   - "seedance-2" → { jst: "seedance_2_0", endpoint: "/jobs/v2/seedance_2_0", est_cost: 1800 }
   - "seedance-2-unlimited" → proxy through to "seedance_2_0" (alias)

3. body construction:
   - fetch bestPracticeBody from body-templates/seedance-2.json
   - overlay user-supplied fields (prompt/duration/aspect_ratio/...)
   - fill in SPA-default fields (medias/generate_audio/multi_shots/...)

4. media handling:
   - user supplies image_url → download → upload via the shared account → obtain media_input object
   - user supplies media_id → use directly

5. pool.Pick({ jst, est_cost }) → account

6. upstream.CreateJob(account, endpoint, body):
   - use JA3 client + the account's JWT + cookies
   - POST /jobs/v2/seedance_2_0
   - 200 → obtain job_id
   - 429 → switch account and retry (one switch; still fails → return rate_limited)
   - 422 → return body_error (no account switch)
   - 403 gate → switch to a higher-tier account

7. Response strategy (two modes):
   A. Sync wait (default, short models <30s): poll to completed, return result_url
   B. Async (long models or client explicitly sets async=true):
      return immediately with { job_id, status: "queued", poll_url: "/v1/jobs/{id}" }
      client GETs /v1/jobs/{id} on its own to get status

8. On completion:
   - record in the jobs table
   - pool.Return(account, actual_cost)
   - billing: api_keys.monthly_used += cost
```

### 5.2 Endpoint List

| Endpoint | Description |
|---|---|
| `POST /v1/images/generations` | OpenAI-compatible image generation |
| `POST /v1/video/generations` | OpenAI-compatible video generation (new-api / OneAPI shape) |
| `POST /v1/videos/generations` | Legacy alias — same handler as `/v1/video/generations` |
| `POST /v1/audio/generations` | OpenAI-compatible audio (text2speech) |
| `GET  /v1/models` | list all model aliases (129 total) |
| `GET  /v1/models?detail=1` | includes cost / catalogRefs / example body |
| `GET  /v1/models/{id}` | detail for a single model |
| `GET  /v1/catalogs` | list of 31 catalogs |
| `GET  /v1/catalogs/{key}` | catalog items (styles/hooks/motions...) |
| `GET  /v1/jobs/{id}` | async task status query |
| `POST /v1/media/upload` | user direct media upload (image/video/audio) |
| `GET  /v1/reference-media` | id/url of shared reference media (no upload needed) |

### 5.3 Error Mapping

| higgsfield response | higgsgo response to the user |
|---|---|
| 200 create + completed | 200 + result_url |
| 200 create + failed (refunded) | 502 upstream_fail_refunded (not billed) |
| 200 create + failed (not refunded) | 502 upstream_fail (billed) |
| 200 create + poll timeout | 202 async + poll_url |
| 422 body | 400 bad_request (no account switch) |
| 403 plan gate | switch to higher-tier account internally; if none available, return 402 payment_required |
| 429 rate limit | switch account internally; still 429 after → return 429 |
| 403 unlimited_generation_not_allowed | route via alias to the base model (seedance_2_unlimited → seedance_2_0) |
| 400 IP check | switch account or use shared media internally; if that fails, return 500 |
| 400 Application not found | 400 body_error (config issue) |
| 500 higgsfield | switch account once; still 500 → return 502 upstream_5xx |

---

## 6. Registration Subsystem (register package)

### 6.1 Trigger Methods

- **Manual**: `POST /admin/registrations` body: `{email, password, oauth_source, refresh_token, proxy_url}`
- **Batch**: CLI `higgsgo-cli register --file mail-list.txt`
- **Automatic**: `ticker` detects active starter count in the pool below threshold and auto-pulls one from the pending mail list to register

### 6.2 Flow (reuses Node-version logic)

```
1. registrar.Enqueue(email, ...) → DB registrations.status='pending'
2. worker goroutine picks up pending → status='running'
3. browser.Launch(proxy_url, headless=true) → chromedp / rod
   (chromedp is the Go-ecosystem default, but higgsfield's site has canvas
    fingerprinting and DataDome. The Go side may need to keep Playwright-Go,
    or drive an independent chromium daemon via CDP.)
4. flow.Register():
   - goto higgsfield.ai
   - click "Continue with Email"
   - fill email/password
   - wait for OTP challenge
   - mailbox.Fetch(email, source) → OTP code
   - fill OTP
   - wait for successful redirect
5. capture.Harvest() collects:
   - user_id / session_id (Clerk)
   - all cookies (including __session, __client, datadome)
   - captured_user_agent
   - x-datadome-clientid
   - credits_snapshot (via GET /user)
   - plan_type / has_unlim / has_flex_unlim
6. writer.Save() → DB accounts table + status='active'
7. registrations.status='completed', account_id=<uuid>
```

### 6.3 Chrome Fingerprint Problem

**Key decision**: the Go side must pass Cloudflare + DataDome. Alternatives:

| Option | Pros | Cons |
|---|---|---|
| **chromedp (pure Go)** | no external dependencies | default chromium fingerprint gets flagged by DataDome |
| **playwright-go** | has a stealth-plugin ecosystem | requires Node to install Playwright |
| **CloakBrowser daemon** | Node version already proven to pass | must run a Node subprocess |
| **rod (Go)** | similar to chromedp with a nicer API | same as chromedp; fingerprint patching is DIY |

**Recommendation**: **hybrid** — the higgsgo main process is Go; during registration, spawn a standalone CloakBrowser Node subprocess (`node registrar-worker.mjs`) and talk to it over stdin/stdout JSON. This:
- reuses the already-verified Playwright + CloakBrowser flow from higgsfield-register
- keeps the Go main process focused on orchestration and DB writes
- leaves room to migrate later once the Go side has a reliable stealth story

---

## 7. Mailbox / OTP (mailbox package)

### 7.1 Three Providers

```go
type OTPFetcher interface {
    Fetch(ctx context.Context, email string, opts FetchOpts) (string, error)
}

// Microsoft Graph (pull Outlook mail via refresh_token)
type graphProvider struct {
    clientID     string
    refreshToken string
    proxyURL     string
}

// destiny-mmo (disposable-mailbox web UI; must drive a browser)
type destinyProvider struct {
    browser *browser.Context
}

// Prompt (stdin, manual entry)
type promptProvider struct{}
```

### 7.2 Routing Rules

Auto-select by email domain:
- `@outlook.com`, `@hotmail.com` → graph (if a refresh_token is available)
- `@*.headcc.io.vn`, `@*.sorashift.store`, `@vietnamcashewnuts.*`, `@pixelpho.space`, `@daivietartex.bond`, `@whisperwindwalruswhimsy.site`, `@hubcrypto.site` → destiny
- otherwise → prompt

Explicit overrides via `configs/mailbox-routes.toml`.

---

## 8. IP Proxy Management (proxy package)

### 8.1 Requirements (design goal)

- Each account uses one IP at registration time and should **stay on the same IP** thereafter (IP changes trigger DataDome challenges + `ip_check_finished` blocking)
- Reverse-proxy traffic also uses the account's bound IP
- Failed proxies must swap out automatically (proxy_pool probes)

### 8.2 Strategy (target)

```
Type A: sticky per-account
  accounts.bound_proxy_url = "socks5://xxx"
  at registration, pick a healthy proxy and bind it
  always use this proxy for reverse-proxy traffic

Type B: rotating (fallback)
  accounts with bound_proxy_url IS NULL draw from the pool at random
  used for ad-hoc verification and registration probes
```

### 8.3 Runtime wiring (2026-07-19) — ✅ **Sticky per-account proxy is live**

`account.bound_proxy_url` now drives outbound traffic per request.
Implementation:

- `internal/adapters/httpclient/utls/pool.go` — `utls.Pool` caches one
  `*utls.Client` per unique proxy URL. `Resolve(ctx, account)` returns
  the account's client, or nil (fallback) when `BoundProxyURL == ""`
  or the URL previously failed to build.
- `internal/core/upstream/client.go` — `Client.Resolver` field
  (`AccountClientResolver` interface). `doWithRetry` consults the
  resolver on every call; nil-return / error → falls back to the
  shared default client so a misconfigured proxy degrades gracefully
  instead of failing the request.
- `cmd/higgsgo/main.go` — builds one default `utls.Client` from
  `HIGGSGO_UPSTREAM_PROXY_URL`, wraps it in a Pool, assigns the pool
  to `upstreamClient.Resolver`. JWT mint (Clerk) still uses the
  default client — Higgsfield's JWT endpoint is not account-egress
  bound.
- Registration flow (`internal/api/admin/registrations.go`,
  `ports/registrar.go`, `ports/captcha.go`, `ports/browser.go`) is
  the other consumer of proxy config, wired independently on the
  registration side.

Failure modes:

- Empty `BoundProxyURL` → default client, expected path for freshly
  imported accounts.
- Malformed URL → resolver returns error, cached so repeat calls
  don't hammer the build path. Upstream client logs a WARN and uses
  the default. Operators fix the URL then call
  `pool.Invalidate(url)` (or restart) to force a rebuild.
- Live proxy failure (auth timeout, SOCKS handshake reject) → surfaces
  as a normal network_error at the upstream layer, which the failover
  controller counts as attributable and can eventually throttle /
  disable the account.

Previously (pre-2026-07-19): `bound_proxy_url` was stored but ignored.
All accounts shared a single process-level client from
`HIGGSGO_UPSTREAM_PROXY_URL`, defeating the sticky-IP promise made at
registration time. See ROADMAP P1-5 for the audit that surfaced this.

### 8.4 Proxy Sources (used only by registration flow today)

- 711proxy (existing): US / VN regions
- manual: uploaded via admin API
- future: Bright Data / Oxylabs / SmartProxy

---

## 9. Auth and JWT Management (upstream/jwt.go)

### 9.1 Clerk JWT Lifecycle

```
Refresh every 40s (before the 60s expiry).
Only actively refresh accounts with in_flight_jobs > 0 (idle accounts refresh lazily).

Refresh:
  POST clerk.higgsfield.ai/v1/client/sessions/{session_id}/tokens
    Cookie: __session=... __client=...
  → { jwt: "eyJhb...", expires_at: ... }

In-memory cache:
  map[account_id]struct{ jwt string; expiresAt time.Time; mu sync.Mutex }
  before each use, if expiresAt - time.Now() < 20s, refresh first
```

### 9.2 DataDome Cookie

DataDome rotates periodically (server-side Set-Cookie). Sniff every response and update it in the DB.

---

## 10. Scheduled Tasks (ticker package)

```go
type Scheduler struct {
    tasks []Task
    logger *slog.Logger
}

type Task struct {
    Name     string
    Cron     string          // "*/40 * * * * *" 40s
    Fn       func(ctx context.Context) error
    Timeout  time.Duration
}

Tasks:
  {"jwt_refresh",      "*/40 * * * * *",  jwtRefresh,      15*time.Second}
  {"balance_refresh",  "0 */10 * * * *",  balanceRefresh,  5*time.Minute}
  {"a_regression",     "0 0 6 * * *",     aRegression,     30*time.Minute}
  {"x1_recheck",       "0 0 6 * * 0",     x1Recheck,       10*time.Minute}
  {"body_drift",       "0 0 3 * * 1",     bodyDrift,       30*time.Minute}
  {"proxy_health",     "0 */5 * * * *",   proxyHealth,     2*time.Minute}
  {"account_expire",   "0 0 4 * * *",     accountExpire,   1*time.Minute}
  {"registrar_topup",  "0 */30 * * * *",  registrarTopup,  30*time.Minute}  # auto-topup when pool is short
```

---

## 11. Observability (observability package)

### 11.1 Logging

- `slog` + JSON handler
- Structured fields: `request_id`, `api_key_id`, `account_id`, `jst`, `elapsed_ms`
- Log levels: info (normal request) / warn (degraded / account switch) / error (unexpected)
- Output to stdout + optional log file

### 11.2 Metrics (Prometheus)

```
higgsgo_requests_total{model, status}
higgsgo_job_duration_seconds{model, verdict}
higgsgo_pool_accounts{plan_type, status}
higgsgo_pool_balance_credits{plan_type}   # gauge
higgsgo_pool_in_flight{account_id}        # gauge
higgsgo_upstream_errors_total{type}       # body/gate/rate/upstream/timeout
higgsgo_jwt_refresh_total{account_id, status}
higgsgo_datadome_challenges_total
higgsgo_registrations_total{status}
```

### 11.3 Audit Log

Every job writes a full audit record (request + response + used account + cost), retained for 90 days. Used for:
- Tracing specific user outputs (customer support)
- Regression testing (build a golden set)
- Cost accounting

---

## 11a. Post-v0.1 Modules (added after the original design freeze)

These packages were introduced after the base doc was drafted. Documented
here rather than back-fitted into §§1-11 so history stays readable.

### 11a.1 Failover Controller (`internal/core/failover`)

**Purpose.** Turn account errors into pool-health signals and automated
state transitions. Two mechanisms, both landing on `accounts.status` +
`status_reason`:

- **Consecutive-failure circuit breaker** (default on). Attributable
  failures accumulate; at `fail_limit` the account is marked
  `disabled`.
- **Sliding-window throttling** (default off). 429 / anti-abuse signals
  within `judge_window` mark the account `throttled` for `cooldown`;
  repeated blacklist hits within `evict_window` promote to `disabled`.

**Pool-level guard.** `outage_guard` (`controller.go:362-375`) refuses
to disable when the number of accounts already disabled in the current
window exceeds the threshold — protects against a global upstream
outage silently draining the pool.

**Recovery.** `Recoverer` runs on its own goroutine (started in
`main.go:144-153`), ticks every 30 s, calls
`Accounts.RecoverThrottled` to move `throttled → active` when
`throttled_until` passes. `disabled → active` requires manual
`POST /admin/accounts/{id}/recover`.

**Wire points.**
- `internal/core/proxy/service.go:224,309,335` — CreateJob / sync-poll
  failure + success paths.
- `internal/core/pollworker/worker.go:223,282` — async job failure +
  success paths.

**Admin API.** `internal/api/admin/failover.go:55-62` — six endpoints
under `/admin/failover/*` and `/admin/accounts/{id}/failover`. WebUI
consumer: `webui/src/components/settings/failover-dialog.tsx`, opened
from `PoolHealthIndicator` in the sidebar. **This is the pool-health
console.**

### 11a.2 Admin Bearer Runtime Rotation (`internal/core/bearer`)

**Purpose.** Rotate the admin bearer token without a process restart.
Falls back to `config.Server.AdminBearer` when no override is set.

`bearer.Manager` (`manager.go:81`) uses a lock-free `atomic.Pointer` with
a 30 s grace window during rotation. Consulted from
`internal/api/server.go:427-431` `adminBearerAccepter()`; when
`s.Bearer != nil` middleware auth reads from it, otherwise it uses
`StaticBearer(cfg.Server.AdminBearer)`.

**Persistence.** `internal/adapters/storage/sqlite/settings_store.go`
(K/V table `system_settings` from migration `014`), shared with routing
defaults.

**Admin API.** `internal/api/admin/settings.go` — `GET/POST
/admin/settings/bearer` and `POST /admin/settings/bearer/rotate`
(hash-only read, plaintext write). WebUI has a settings tab.

### 11a.3 Runtime Settings Store

Migration `014` adds `system_settings(key, value_json)` as a generic
runtime K/V. Consumed by:

- Bearer manager (see 11a.2).
- `internal/api/admin/routing_settings.go` — persists a default
  `RouteStrategy` used when a new group is created via
  `admin/groups.go:114 ResolveDefaultRouteStrategy`.
- Not consulted at pick time — it only seeds new-group defaults.

### 11a.4 Model Overrides

**Purpose.** Runtime overrides on the static `verified-models.json`
registry without a rebuild. Migration `015` adds `model_overrides`
table; `internal/domain/model_override.go` uses pointer semantics
(nil = inherit spec, set = override).

**Wire.** `main.go:110` builds the store; `main.go:177-183` calls
`registry.SetOverrideProvider()` then triggers `Registry.Reload()`.
`internal/api/admin/model_overrides.go` writes trigger another Reload.
WebUI: `webui/src/lib/api.ts:631`.

Two separate mechanisms coexist:
- **Overrides** (this module) — runtime-editable via admin API.
- **Extras** (`data/reference/model-specs-extra.json`, consumed by
  `registry.go:202-203`) — static supplement for
  `max_resolution` / `max_duration_sec`.

### 11a.5 Version Endpoint

`internal/version` + `internal/api/admin/version.go`. Exposes
`GET /admin/version` and `GET /admin/version/check` (1 h in-memory cache
of GitHub `releases/latest`). Always mounted regardless of
`Updates.CheckEnabled`. WebUI consumers:
`webui/src/lib/api.ts:766,773`.

### 11a.6 Config Resolver (`internal/core/resolver`) — **not wired**

**Purpose.** Cascade layered configuration (Key > Group > Account >
Global) into a single `ResolvedConfig`. Correctly resolves the
precedence for concurrency, proxy URL, route strategy, monthly budget,
model regex, rate limits, and markup.

**Status.** `grep -rn "core/resolver"` returns **zero** external
importers in production. `PickAndLock` never sees `ResolvedConfig`;
`proxy/service.go` never calls `Resolve()`. This is the primary reason
the group / account / apikey fields listed in ROADMAP §1's "silent
no-op" table have no runtime effect — the engine that reads them exists
but is not plugged in.

ROADMAP P1-4 wires this into `proxy.Service.Generate` between
`resolveGroup` and `PickAndLock`.

### 11a.7 Playground

`internal/api/v1/playground.go` + `internal/api/middleware/playground.go`
+ migration `012`. Adds `api_keys.playground_scope` so a key can be
restricted to `/v1/playground/{models,estimate,execute}` (used by the
WebUI's playground page). Full endpoint list in `API_REFERENCE.md`.

### 11a.8 Registration Plugin (`-tags register`)

Full end-to-end signup pipeline: WebUI Enqueue → SQLite queue →
Go worker → Node subprocess → higgsfield.ai → harvested cookies →
new `domain.Account` in the pool. Only linked under `-tags register`
so public / reverse-proxy binaries ship none of the automation
code. See `docs/PLUGGABLE.md` §0 for the monorepo split and
`docs/ROADMAP.md` §5 for the delivery history.

**Layers:**

- `internal/ports/registrar.go` — `Registrar` interface (Enqueue,
  GetStatus, List, Retry). Always compiled.
- `internal/adapters/registrar/higgsfield/higgsfield_disabled.go` —
  default build: `NewRegistrar` returns a stub that answers 503
  `registrar_disabled` on every method.
- `internal/adapters/registrar/higgsfield/higgsfield.go`
  (`//go:build register`) — the bridge. Owns:
    - `storeAdapter` — translates between the main module's int64-id
      4-state schema and the plugin's string-id 6-state model.
      `MarkCompleted` runs a two-step transition (Account upsert
      then registrations flip) so a `success` row never dangles.
    - `registrar` — the `ports.Registrar` facade. Enqueue → store,
      GetStatus → store, List → store (via
      `ports.RegistrationStore.List` which supports status /
      since / limit / offset), Retry → `ResetToPending` + worker
      trigger.
    - Optional `Start(ctx)` starts a `plugins/register.Worker`
      goroutine when a Driver is wired.
- `plugins/register/` — separately-versioned Go sub-module.
  Defines its own `RegistrationStore` + `Driver` interfaces plus
  the queue state machine. Under `-tags register`
  `internal/adapters/registrar/higgsfield/` implements the
  storeAdapter that satisfies the plugin's `RegistrationStore` by
  delegating to `ports.RegistrationStore`.
- `plugins/register/adapters/camoufox/driver_node.go` — `NodeDriver`
  spawns and talks to the Node subprocess. Klinggo pattern: process
  group via `Setpgid` so `Close()` reaps the whole
  node→chromium/firefox tree.
- `plugins/register/adapters/mock/driver.go` — in-process fake
  Driver used by unit tests to prove the flow-driver plumbing
  without a subprocess.
- `plugins/register/driver-node/index.mjs` — Node HTTP driver.
  Lazily imports `../../../higgsfield-register/src/register/flow.mjs`
  (or `HIGGSFIELD_REGISTER_ROOT`) so no Playwright / camoufox /
  DataDome / Graph OTP logic is duplicated into higgsgo. `POST
  /register` runs one signup end-to-end; `GET /ready` reports the
  driver's health; `POST /shutdown` exits cleanly.
- `internal/adapters/storage/sqlite/registration_store.go` — full
  CRUD over the `registrations` table (built by migration 001).

**Data flow (`-tags register` build):**

```
WebUI Register form
    ↓  POST /admin/registrations {email, oauth_source?, proxy_url?}
Admin handler (internal/api/admin/registrations.go)
    ↓  Registrar.Enqueue
higgsfield.registrar.Enqueue (register-tag bridge)
    ↓  storeAdapter.Enqueue → RegistrationStore.Enqueue
sqlite.RegistrationStore
    ↓  INSERT INTO registrations (…, status='pending')
    ← worker.Trigger fires an out-of-cycle poll

register.Worker.poll (plugins/register/worker.go)
    ↓  RegistrationStore.NextPending → sqlite row
    ↓  flow.Execute(reg)
register.Flow (driver path)
    ↓  RegistrationStore.MarkRunning
    ↓  Driver.Register({email, password, proxy, mailbox_config})
camoufox.NodeDriver (plugins/register/adapters/camoufox/driver_node.go)
    ↓  HTTP POST http://127.0.0.1:8801/register
Node driver (plugins/register/driver-node/index.mjs)
    ↓  registerAccount(opts) from higgsfield-register
Playwright + Camoufox
    ↓  signup + OTP + DataDome
    ↑  { account_id, session_id, cookies, ua, ... }
Node driver responds
    ↑  { ok: true, result }
NodeDriver.Register → CompletedResult
    ↑
register.Flow
    ↓  RegistrationStore.MarkCompleted(id, result)
storeAdapter.MarkCompleted
    ↓  AccountStore.Upsert (cookies → JSON, plan map,
                             credits → hundredths)
    ↓  RegistrationStore.MarkCompleted (status='success')
```

Bootstrap and env-var reference are in `docs/ROADMAP.md` §5.5.

---

## 12. Deployment

**Single-machine systemd** (starting point):
```
[Unit]
Description=higgsgo
After=network.target

[Service]
Type=simple
User=higgsgo
WorkingDirectory=/opt/higgsgo
ExecStart=/opt/higgsgo/higgsgo -config /etc/higgsgo/config.toml
Restart=always
```

Dependencies:
- Chromium (for registration, or a Node CloakBrowser daemon)
- SOCKS5 proxy pool
- SQLite (built-in) → migrate to Postgres at scale

---

## 13. Migration Path

### Phase 0: Skeleton
1. mkdir higgsgo + go.mod
2. config / db / migrations
3. `higgsgo-cli import-accounts /path/to/higgsfield-register/output` imports the existing 22 accounts

### Phase 1: Reverse proxy only (reuse existing accounts)
4. upstream client (JA3 + JWT + jobs)
5. api/v1 (4 endpoints: images/videos/models/catalogs)
6. basic pool (no scheduled refresh; Pick just queries the DB)
7. deploy, dual-run alongside the existing node server for comparison

### Phase 2: Add pool management
8. ticker jwt_refresh + balance_refresh
9. admin/accounts CRUD
10. shut down the node server

### Phase 3: Add registration
11. register subsystem (initially spawns a Node subprocess as the registrar)
12. mailbox providers
13. proxy pool

### Phase 4: Self-healing
14. a_regression / x1_recheck / body_drift scheduled tasks
15. auto-topup (register when active starter count < N)
16. metrics + alerting

---

## 14. Open Questions (need your call)

1. **Web framework**: chi / gin / fiber / echo (leaning chi — stdlib + good middleware ecosystem)
2. **Starting DB**: is SQLite OK, or go straight to Postgres?
3. **Chrome strategy**: rewrite the CloakBrowser logic on the Go side, or keep the Node subprocess for registration?
4. **Config format**: TOML / YAML / .env — pick one?
5. **API key issuance**: who signs them? Manually via admin, or hook into the existing muxpay?
6. **Billing model**: charge users by credits × multiplier, or by request count?
7. **Async job storage**: completed result_urls sit on higgsfield's CDN (30-day hf retention); do we mirror to our own S3?
8. **Multi-machine deploy**: start single-machine, but at scale do we consider a distributed pool lock (Redis)?

---

**This doc is for discussion**. Once we've walked through each item and you're happy, I'll go stand up the higgsgo directory + go.mod.
