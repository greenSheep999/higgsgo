# higgsgo Pluggable Architecture

> Two orthogonal pluggability layers:
>
> 1. **Provider ports inside the main module** — proxy, mailbox, captcha
>    solver, browser, storage, notifier, model registry are all
>    interface-driven. Swap vendors by editing config.
> 2. **Monorepo module split** for sensitive code — the account
>    registration flow lives in a separately-versioned Go sub-module
>    under `plugins/register/`. The public reverse-proxy build compiles
>    only a 503-returning stub, so private automation logic never ships
>    in a public binary.
>
> §0 covers the module split (the newer decision). §§1-8 cover the
> in-module Provider abstraction. §9 is out of date and superseded by
> `docs/ROADMAP.md`; §10 is the current wiring status.

---

## 0. Monorepo Module Split (registration plugin)

### 0.1 Why split

Reverse proxy = commodity infrastructure, safe to open-source. Account
registration = sensitive automation (captcha solves, browser
fingerprinting, mailbox cookies) that we don't want in public builds.

Two build variants from one repo:

| Variant | Command | Contents | Registration behaviour |
|---|---|---|---|
| Public (slim) | `go build ./cmd/higgsgo` | main module only | `POST /admin/registrations` → `503 registrar_disabled` |
| Private (full) | `go build -tags register ./cmd/higgsgo` | main + `plugins/register` linked in | Real automation runs |

### 0.2 Layout

```
higgsgo/
├── go.work                              # binds both modules for local dev
├── go.mod                               # main:    github.com/greensheep999/higgsgo
├── internal/
│   ├── ports/registrar.go               # interface — always compiled
│   └── adapters/registrar/higgsfield/
│       ├── higgsfield_disabled.go       # default: return cpaplugin.StubRegistrar{}
│       └── higgsfield.go                # -tags register: bridge to plugin
├── internal/api/admin/registrations.go  # HTTP handlers — always compiled
└── plugins/
    └── register/
        ├── go.mod                       # plugin:  github.com/greensheep999/higgsgo/plugins/register
        ├── flow.go / worker.go / ports.go / types.go
        ├── api/                         # if the plugin needs to expose HTTP
        └── adapters/
            ├── camoufox/                # browser adapter
            └── cloak/                   # alt browser adapter
```

### 0.3 Contract

`internal/ports/registrar.go` is the *only* handshake:

```go
type Registrar interface {
    Enqueue(ctx context.Context, req RegistrationRequest) (string, error)
    GetStatus(ctx context.Context, id string) (*Registration, error)
    List(ctx context.Context, f RegistrationFilter) ([]RegistrationRow, error)
    Retry(ctx context.Context, id string) error
}
```

- **Default build** (`higgsfield_disabled.go`) returns
  `cpaplugin.StubRegistrar{}` — every method returns
  `domain.ErrRegistrarDisabled` which maps to HTTP 503.
- **`-tags register` build** returns a bridge (`higgsfield.go`) that
  wraps the plugin's `Registrar` implementation. Today this file is
  `panic("TODO")` — see ROADMAP §5.

### 0.4 Migration status (ROADMAP §5.3)

- ✅ Interface, admin handlers, stub, table schema (migration `001`).
- ❌ `go.work`.
- ❌ Module-path alignment: `plugins/register/go.mod` says
  `github.com/higgsgo/higgsgo/plugins/register`; needs to match
  `github.com/greensheep999/higgsgo/plugins/register`.
- ❌ Bridge implementation under `-tags register`.
- ❌ SQLite `registration_store` adapter (schema exists, no CRUD).
- ❌ Real browser adapters (camoufox / cloak are placeholders).

---

## 1. Provider Abstraction Overview

```
┌─ higgsgo core (depends on no concrete vendor) ────────────┐
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │  domain/  pure business models (Account/Job/Model) │  │
│  │  ports/   Provider interface definitions           │  │
│  └────────────────────────────────────────────────────┘  │
│                        ▲                                 │
│                        │ dependency inversion            │
│                        │                                 │
│  ┌────────────────────────────────────────────────────┐  │
│  │  adapters/  one implementation per Provider        │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐            │  │
│  │  │ Proxy    │ │ Mailbox  │ │ Captcha  │            │  │
│  │  │          │ │          │ │          │            │  │
│  │  ├──────────┤ ├──────────┤ ├──────────┤            │  │
│  │  │ 711proxy │ │ graph    │ │ datadome-│            │  │
│  │  │ bright   │ │ destiny  │ │  cookie  │            │  │
│  │  │ oxylabs  │ │ imap     │ │ 2captcha │            │  │
│  │  │ static   │ │ prompt   │ │ capsolver│            │  │
│  │  │ ...      │ │ ...      │ │ manual   │            │  │
│  │  └──────────┘ └──────────┘ └──────────┘            │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐            │  │
│  │  │ Browser  │ │ Storage  │ │ Notifier │            │  │
│  │  │          │ │          │ │          │            │  │
│  │  ├──────────┤ ├──────────┤ ├──────────┤            │  │
│  │  │ cloak-js │ │ sqlite   │ │ slack    │            │  │
│  │  │ chromedp │ │ postgres │ │ telegram │            │  │
│  │  │ rod      │ │ mysql    │ │ email    │            │  │
│  │  │ playwrgt │ │ ...      │ │ webhook  │            │  │
│  │  │ patchr   │ │          │ │ ...      │            │  │
│  │  └──────────┘ └──────────┘ └──────────┘            │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

---

## 2. Directory Layout Restructured

```
higgsgo/
├── go.mod
├── cmd/
│   ├── higgsgo/main.go
│   └── higgsgo-cli/main.go
│
├── internal/
│   ├── domain/                     # pure business domain models (no external deps)
│   │   ├── account.go
│   │   ├── job.go
│   │   ├── model_spec.go          # model spec (jst/endpoint/cost/body_template)
│   │   ├── credits.go
│   │   └── errors.go              # business error classification
│   │
│   ├── ports/                     # Provider interface definitions (heart of dependency inversion)
│   │   ├── proxy.go               # ProxyProvider
│   │   ├── mailbox.go             # MailboxProvider  
│   │   ├── captcha.go             # CaptchaSolver
│   │   ├── browser.go             # BrowserAutomator
│   │   ├── storage.go             # AccountStore / JobStore / ModelStore
│   │   ├── notifier.go            # Notifier
│   │   ├── httpclient.go          # UpstreamClient (JA3-capable)
│   │   ├── modelregistry.go       # ModelRegistry (hot-reloadable)
│   │   └── clock.go               # Clock (for tests)
│   │
│   ├── core/                      # core business logic (uses ports.*, does not import adapters)
│   │   ├── pool/                  # account pool orchestration (uses storage + httpclient)
│   │   ├── proxy/                 # reverse-proxy business (uses pool + modelregistry + httpclient)
│   │   ├── register/              # registration orchestration (uses mailbox + browser + captcha + proxy)
│   │   ├── jwt/                   # JWT lifecycle
│   │   └── ticker/                # scheduled-task orchestration
│   │
│   ├── adapters/                  # Provider concrete implementations
│   │   ├── proxy/                 # proxy adapters
│   │   │   ├── static.go          # read proxy list from config file
│   │   │   ├── proxy711.go        # 711proxy API
│   │   │   ├── brightdata.go      # BrightData
│   │   │   ├── oxylabs.go
│   │   │   └── noop.go            # direct connection (for tests)
│   │   │
│   │   ├── mailbox/               # mailbox adapters
│   │   │   ├── graph.go           # Microsoft Graph
│   │   │   ├── destiny.go         # destiny-mmo web
│   │   │   ├── imap.go            # generic IMAP
│   │   │   ├── prompt.go          # stdin
│   │   │   ├── kmail.go           # KMail / other disposable mail
│   │   │   └── webhook.go         # user pushes OTP to us
│   │   │
│   │   ├── captcha/               # captcha adapters (DataDome/hCaptcha/Turnstile)
│   │   │   ├── datadome_cookie.go # via sadcaptcha / CapSolver DataDome API
│   │   │   ├── capsolver.go       # CapSolver generic
│   │   │   ├── twocaptcha.go      # 2Captcha
│   │   │   ├── nopecha.go         # NopeCHA
│   │   │   ├── anticaptcha.go
│   │   │   └── manual.go          # pop up terminal, wait for human
│   │   │
│   │   ├── browser/               # browser adapters
│   │   │   ├── cloak_nodejs.go    # spawn Node CloakBrowser (keep existing)
│   │   │   ├── chromedp.go        # pure Go chromedp
│   │   │   ├── rod.go             # rod
│   │   │   ├── playwright.go      # playwright-go
│   │   │   └── patchright.go      # patchright (stealth chromium)
│   │   │
│   │   ├── storage/               # DB adapters
│   │   │   ├── sqlite/            # sqlite implementation
│   │   │   ├── postgres/          # postgres implementation
│   │   │   └── memory/            # in-memory for unit tests
│   │   │
│   │   ├── notifier/              # notifier adapters
│   │   │   ├── slack.go
│   │   │   ├── telegram.go
│   │   │   ├── webhook.go
│   │   │   ├── email.go
│   │   │   └── stdout.go
│   │   │
│   │   ├── httpclient/            # HTTP client (JA3 fingerprint)
│   │   │   ├── utls.go            # refraction-networking/utls
│   │   │   ├── mimic.go           # mimic Chrome
│   │   │   ├── cycletls.go        # cycletls (fork)
│   │   │   ├── impit_bridge.go    # bridge to a Node impit subprocess
│   │   │   └── stdhttp.go         # stdlib (for tests)
│   │   │
│   │   └── modelregistry/         # model registry
│   │       ├── json_static.go     # load from sealed.json
│   │       ├── db_dynamic.go      # load from DB (hot-updatable)
│   │       └── remote_api.go      # pull from remote config service
│   │
│   ├── api/                       # HTTP handlers (thin shell over core)
│   │   ├── v1/
│   │   ├── admin/
│   │   └── middleware/
│   │
│   ├── config/                    # config loading + Provider wiring
│   │   ├── config.go              # structs
│   │   ├── loader.go              # toml/yaml → struct
│   │   └── wire.go                # wire up Providers from config (no DI lib, hand-written)
│   │
│   └── observability/
│       ├── logger.go
│       ├── metrics.go
│       └── audit.go
│
├── configs/
│   ├── higgsgo.example.toml       # full example config
│   ├── higgsgo.dev.toml
│   ├── higgsgo.prod.toml
│   ├── models/                    # model config shards (dynamically reloadable)
│   │   ├── image.toml
│   │   ├── video.toml
│   │   ├── audio.toml
│   │   └── aliases.toml           # *_unlimited alias mapping
│   └── providers/                 # per-Provider standalone config
│       ├── proxy-711.toml
│       ├── mailbox-graph.toml
│       └── captcha-capsolver.toml
│
├── data/                          # static data (migrated from higgsfield-register)
│   ├── sealed.json
│   ├── verified-models.json
│   ├── body-templates/
│   ├── catalogs/
│   └── starter-locked.json
│
├── plugins/                       # future Go plugin or wasm plugins (optional)
│
└── docs/
```

---

## 3. Provider Interface Definitions (`ports/`)

### 3.1 ProxyProvider

```go
package ports

type ProxyProvider interface {
    // Acquire an available proxy (may be drawn from the pool or newly requested)
    Acquire(ctx context.Context, opts ProxyOpts) (Proxy, error)

    // Release (no need to actually release; mark last_used)
    Release(ctx context.Context, p Proxy) error

    // Probe whether the proxy is usable
    Healthcheck(ctx context.Context, p Proxy) error

    // Name (for logs and metrics)
    Name() string
}

type Proxy struct {
    URL          string            // socks5://user:pass@host:port
    Region       string            // US / VN / IN / ...
    Provider     string            // 711proxy / brightdata / ...
    Sticky       bool              // whether long-term bound
    Metadata     map[string]string
}

type ProxyOpts struct {
    Region       string            // desired region, "" = any
    Sticky       bool              // whether a long-term-bound IP is required (for registration)
    ForAccountID string            // which account to bind to under sticky
    MinLatencyMs int
}
```

### 3.2 MailboxProvider

```go
type MailboxProvider interface {
    // Fetch OTP mail (blocks until received or timeout)
    FetchOTP(ctx context.Context, req FetchOTPReq) (string, error)

    // Mailbox send/receive capability (may be used for password reset in the future)
    ListInbox(ctx context.Context, email string, since time.Time) ([]Email, error)

    // Whether this provider supports the given email domain
    Supports(email string) bool

    Name() string
}

type FetchOTPReq struct {
    Email        string
    Timeout      time.Duration
    Subject      string            // optional, used for filter
    From         string            // optional
    Credentials  map[string]string // provider-specific: refresh_token / password / ...
}
```

### 3.3 CaptchaSolver

```go
type CaptchaSolver interface {
    // DataDome captcha solve
    SolveDataDome(ctx context.Context, req DataDomeReq) (DataDomeResult, error)

    // Generic CAPTCHA (Turnstile / hCaptcha / reCAPTCHA v2/v3)
    Solve(ctx context.Context, req CaptchaReq) (CaptchaResult, error)

    // Balance query (drives low-balance alerts)
    Balance(ctx context.Context) (float64, error)

    Name() string
}

type DataDomeReq struct {
    SiteURL      string
    CaptchaURL   string
    UserAgent    string
    ProxyURL     string             // solver must use the same IP
}

type DataDomeResult struct {
    Cookie       string             // resulting datadome cookie
    ClientID     string
}
```

### 3.4 BrowserAutomator

```go
type BrowserAutomator interface {
    // Launch a controlled browser (headless / headed)
    Launch(ctx context.Context, opts LaunchOpts) (BrowserSession, error)

    Name() string
}

type BrowserSession interface {
    // Navigate to a URL
    Goto(ctx context.Context, url string) error

    // Form fill, click etc. (exposes a generic DOM operation set)
    Fill(ctx context.Context, selector, value string) error
    Click(ctx context.Context, selector string) error
    WaitFor(ctx context.Context, selector string, timeout time.Duration) error

    // Harvest cookies / localStorage / user-agent
    Cookies(ctx context.Context) ([]Cookie, error)
    LocalStorage(ctx context.Context) (map[string]string, error)
    UserAgent(ctx context.Context) (string, error)

    // Run arbitrary JS
    EvalJS(ctx context.Context, script string) (any, error)

    // Intercept network events (used to capture JWT)
    OnRequest(cb func(NetRequest))
    OnResponse(cb func(NetResponse))

    Close() error
}

type LaunchOpts struct {
    ProxyURL    string
    UserAgent   string             // optional override
    Headless    bool
    Profile     string             // isolated profile directory
    ExtraFlags  []string
}
```

### 3.5 UpstreamClient (JA3 HTTP)

```go
type UpstreamClient interface {
    Do(ctx context.Context, req *http.Request) (*http.Response, error)

    // JA3 / fingerprint metadata (convenient for logs)
    Fingerprint() string
    Name() string
}
```

### 3.6 Storage (split fine-grained)

```go
type AccountStore interface {
    Get(ctx context.Context, id string) (*domain.Account, error)
    List(ctx context.Context, filter AccountFilter) ([]domain.Account, error)
    Upsert(ctx context.Context, a *domain.Account) error
    UpdateBalance(ctx context.Context, id string, sub, credits, pkg int64) error
    UpdateInFlight(ctx context.Context, id string, delta int) error
    MarkStatus(ctx context.Context, id string, status string, reason string) error

    // Atomic lock (prevents multiple goroutines from picking the same account)
    PickAndLock(ctx context.Context, params PickParams) (*domain.Account, string, error)
    Unlock(ctx context.Context, id string, lockToken string) error
}

type JobStore interface {
    Create(ctx context.Context, j *domain.Job) error
    UpdateStatus(ctx context.Context, id string, status string, meta JobMeta) error
    Get(ctx context.Context, id string) (*domain.Job, error)
    ListPending(ctx context.Context) ([]domain.Job, error)
}

type APIKeyStore interface { ... }
type RegistrationStore interface { ... }
type ProxyStore interface { ... }
type ModelHealthStore interface { ... }
```

### 3.7 ModelRegistry

```go
type ModelRegistry interface {
    // Resolve the user-requested model alias into an internal spec
    Resolve(alias string) (*domain.ModelSpec, error)

    // List all models (with category filter)
    List(filter ModelFilter) []*domain.ModelSpec

    // Hot reload (add new models without restart)
    Reload(ctx context.Context) error

    // Alias (*_unlimited → base-model proxy through)
    ResolveAlias(alias string) (string, bool)

    // Whether Starter is gated
    StarterLocked(jst string) bool
}
```

### 3.8 Notifier

```go
type Notifier interface {
    Send(ctx context.Context, msg Notification) error
    Name() string
}

type Notification struct {
    Level    string  // info / warn / error / critical
    Title    string
    Body     string
    Tags     map[string]string
}
```

---

## 4. Model Management (Key Addition)

**Requirement**: models are **parameterized + hot-reloadable** first-class citizens; they cannot be hard-coded in Go.

### 4.1 Model Definition Shard Files

`configs/models/video.toml`:

```toml
[[model]]
alias = "seedance-2"
jst = "seedance_2_0"
endpoint = "/jobs/v2/seedance_2_0"
version = "v2"
output = "video"
cost_per_second = 4.5             # credits/sec at default resolution
default_resolution = "720p"
default_duration = 4
starter_locked = true             # Starter cannot enable
requires_paid = true              # any paid tier
requires_ultra = false            # does not require Ultra
supports_unlim_param = false      # use_unlim: true param has no effect
required_params = ["prompt", "medias", "width", "height", "duration", "resolution", "aspect_ratio", "model", "batch_size"]
enum.model = ["seedance_2_0", "seedance_2_0_fast"]
enum.resolution = ["480p", "720p", "1080p", "4k"]
media_role = "start_image"        # role when the user provides an image
example_body_file = "body-templates/seedance-2.json"
verified_at = "2026-07-17"
verified_by = ["pro", "plus"]
notes = "SPA-aligned. T2V medias=[] works."

[[model.alias_of]]                # alias list
name = "seedance-2-unlimited"
strategy = "fallback_to_base"     # fall back to base when Ultra endpoint 403s

[[model]]
alias = "kling-3"
jst = "kling3_0"
...
```

### 4.2 Alias Strategy

`configs/models/aliases.toml`:

```toml
# *_unlimited proxies through to the base model (higgsgo has no Ultra account)
[[alias]]
from = "seedance-2-unlimited"
to = "seedance-2"
strategy = "transparent"           # invisible to the user

[[alias]]
from = "kling-3-unlimited"
to = "kling-3"
strategy = "transparent"

[[alias]]
from = "gpt-image-2-unlimited"
to = "gpt-image-2"

# In the future, if higgsgo gains an Ultra account, change strategy to "try_native_fallback":
# try the *_unlimited native endpoint first; only fall back on 403
```

### 4.3 Hot Reload

```
POST /admin/models/reload
→ ModelRegistry.Reload(ctx)
→ read configs/models/*.toml + data/verified-models.json
→ atomically replace the in-memory registry
→ takes effect immediately, no restart
```

**Uses**:
- Manually adding a model without restarting
- The `body_drift` scheduled task detects body drift, auto-updates required_params, and reloads
- `x1_recheck` discovers a newly-enabled model, writes it to the DB, and reloads

### 4.4 Versioning & Audit

Model definitions support git version control (the configs directory is committed). Each reload records:
- Who triggered it
- The diff
- Time it took effect

---

## 5. Provider Wiring (`config/wire.go`)

**Core idea**: main.go only assembles; it writes no business logic.

```go
// pseudo-code
func BuildApp(cfg Config) (*App, error) {
    // 1. Storage
    var store Storage
    switch cfg.Storage.Driver {
    case "sqlite":   store = sqlite.New(cfg.Storage.SQLite)
    case "postgres": store = postgres.New(cfg.Storage.Postgres)
    }

    // 2. UpstreamClient
    var client UpstreamClient
    switch cfg.HTTPClient.Type {
    case "utls":         client = utls.New(cfg.HTTPClient.UTLS)
    case "impit_bridge": client = impitbridge.New(cfg.HTTPClient.ImpitBridge)
    }

    // 3. Proxy
    var proxy ProxyProvider
    switch cfg.Proxy.Provider {
    case "static":    proxy = static.New(cfg.Proxy.Static)
    case "711proxy":  proxy = proxy711.New(cfg.Proxy.Proxy711)
    case "brightdata": proxy = brightdata.New(cfg.Proxy.BrightData)
    }

    // 4. Mailbox (may combine multiple providers into a chain)
    mailChain := []MailboxProvider{}
    for _, mb := range cfg.Mailbox {
        switch mb.Type {
        case "graph":   mailChain = append(mailChain, graph.New(mb.Graph))
        case "destiny": mailChain = append(mailChain, destiny.New(mb.Destiny))
        case "imap":    mailChain = append(mailChain, imap.New(mb.IMAP))
        }
    }
    mail := mailbox.Chain(mailChain)  // auto-route by email domain

    // 5. Captcha
    var captcha CaptchaSolver
    switch cfg.Captcha.Provider {
    case "capsolver": captcha = capsolver.New(cfg.Captcha.CapSolver)
    case "2captcha":  captcha = twocaptcha.New(cfg.Captcha.TwoCaptcha)
    case "manual":    captcha = manual.New()
    }

    // 6. Browser
    var browser BrowserAutomator
    switch cfg.Browser.Type {
    case "cloak_nodejs": browser = cloaknode.New(cfg.Browser.CloakNodeJS)
    case "chromedp":     browser = chromedpimpl.New(cfg.Browser.ChromeDP)
    case "patchright":   browser = patchright.New(cfg.Browser.Patchright)
    }

    // 7. ModelRegistry
    registry := modelregistry.NewJSONStatic(cfg.Models.Path)
    if err := registry.Reload(context.Background()); err != nil {
        return nil, err
    }

    // 8. Notifier chain
    notifiers := []Notifier{}
    for _, n := range cfg.Notifiers {
        // ...
    }

    // 9. Wire up Core
    pool := pool.New(store, client, cfg.Pool)
    proxy := coreproxy.New(pool, registry, client, cfg.Proxy)
    registrar := register.New(browser, mail, captcha, proxy, store, cfg.Register)
    scheduler := ticker.New(cfg.Tickers, pool, registrar, ...)

    // 10. HTTP server
    server := api.New(pool, proxy, registrar, store, cfg.API)

    return &App{server, scheduler, pool, ...}, nil
}
```

---

## 6. Config Example (`configs/higgsgo.example.toml`)

```toml
[server]
listen = "0.0.0.0:8080"
admin_listen = "127.0.0.1:8081"
public_url = "https://api.example.com"

[storage]
driver = "sqlite"
[storage.sqlite]
path = "./data/higgsgo.db"

[http_client]
type = "utls"                # utls / impit_bridge / stdhttp
[http_client.utls]
profile = "chrome_133"       # Chrome version fingerprint

[proxy]
provider = "static"          # static / 711proxy / brightdata / ...
[proxy.static]
file = "./configs/proxies.txt"

[[mailbox]]
type = "graph"
[mailbox.graph]
list_file = "/path/to/graph-refresh-tokens.txt"

[[mailbox]]
type = "destiny"
[mailbox.destiny]
web_url = "https://www.mail.destiny-mmo.com/domain"
supported_domains = ["headcc.io.vn", "sorashift.store", "hubcrypto.site", "daivietartex.bond", "vietnamcashewnuts.space", "vietnamcashewnuts.store", "pixelpho.space", "whisperwindwalruswhimsy.site"]

[[mailbox]]
type = "prompt"              # fallback: interactive input

[captcha]
provider = "capsolver"
[captcha.capsolver]
api_key = "${CAPSOLVER_KEY}"
enable_datadome = true

[browser]
type = "cloak_nodejs"        # keep existing Node implementation
[browser.cloak_nodejs]
node_bin = "/usr/local/bin/node"
worker_script = "./adapters/browser/cloak-worker.mjs"
pool_size = 3                # at most 3 concurrent browsers

[models]
path = "./configs/models"
data_path = "./data"
reload_on_signal = "SIGHUP"

[pool]
max_in_flight_per_account = 5
fail_streak_threshold = 3
balance_refresh_interval = "10m"
jwt_refresh_interval = "40s"

[register]
auto_topup = true
min_starter_accounts = 5
mail_list_file = "/path/to/mail-list.txt"
max_concurrent_registrations = 2

[tickers]
[tickers.a_regression]
cron = "0 0 6 * * *"
sample_size = 20

[tickers.x1_recheck]
cron = "0 0 6 * * 0"

[tickers.body_drift]
cron = "0 0 3 * * 1"

[[notifiers]]
type = "slack"
[notifiers.slack]
webhook = "${SLACK_WEBHOOK}"
min_level = "warn"

[[notifiers]]
type = "stdout"
[notifiers.stdout]
min_level = "info"
```

---

## 7. Vendor Switch Playbook (Real Cases)

### Case 1: Swap proxy provider 711proxy → BrightData

1. Add `adapters/proxy/brightdata.go` implementing `ProxyProvider`
2. Update `configs/higgsgo.prod.toml`:
   ```toml
   [proxy]
   provider = "brightdata"
   [proxy.brightdata]
   customer = "..."
   password = "..."
   zone = "residential"
   ```
3. Restart the service — no code changes

### Case 2: Add captcha-platform redundancy

Only CapSolver today; add 2Captcha as fallback:

1. Add `adapters/captcha/twocaptcha.go`
2. Wrap with a chain adapter:
   ```go
   captcha := captcha.Chain(
       capsolver.New(...),   // primary
       twocaptcha.New(...),  // fallback
       manual.New(),         // final fallback (human)
   )
   ```
3. Chain logic: primary fails → try fallback automatically

### Case 3: Add a mailbox source

Bought a batch of new-domain disposable mailboxes:

1. Use the existing `destiny` provider? → just add to `supported_domains`, 0 code changes
2. New web UI? → add `adapters/mailbox/newmail.go`

### Case 4: Swap browser (CloakBrowser → patchright)

1. Add `adapters/browser/patchright.go` implementing `BrowserAutomator`
2. Change type in config, restart
3. Watch registration success rate; roll back any time if unsatisfied

---

## 8. Layering Discipline

**Rule per layer**:
- **domain**: zero dependencies; pure structs and business rules (e.g. "credits_balance < subscription_balance is normal")
- **ports**: only imports domain
- **adapters**: imports domain + ports + third-party SDKs
- **core**: imports domain + ports (**does not** import adapters directly)
- **api**: imports core (api is a thin shell)
- **config**: imports all adapters to wire them up

**Compile-time guarantee**: if core imports adapters, CI fails (via `go vet` or an arch-lint tool).

---

## 9. Historical Open Questions (superseded)

The list below reflects questions from the initial design draft. Most
have been settled in code; the remainder are tracked in
`docs/ROADMAP.md`.

- Model definitions → shipped as `data/reference/verified-models.json`
  with runtime override table (see ARCHITECTURE §11a.4).
- DataDome cookie solving → CapSolver / CloakBrowser both accessible via
  the Captcha and Browser ports; choice is per-deployment config.
- Hot reload scope → `POST /admin/models/reload` reloads the model
  registry (with runtime overrides); providers still require restart.
- Provider versioning → deferred; no vendor has forced this yet.
- Adapter testing → each adapter gets in-package unit tests; integration
  tests are hermetic per-feature (`internal/e2e/`).
- Plugin mechanism → **decided: monorepo Go sub-module + build tag**,
  see §0 above. Not `go plugin`, not wasm, not RPC subprocess.

---

## 10. Wiring Status (2026-07-18)

**Provider ports** (`internal/ports/`) — all defined, wired via
`config/wire.go`, adapters selected at startup from
`configs/higgsgo.example.toml`:

| Port | Adapter(s) | Status |
|---|---|---|
| `ProxyProvider` | env / static | ✅ built once at startup (see ARCHITECTURE §8.3 for the sticky-proxy gap) |
| `MailboxProvider` | used by registration only | ⚠️ referenced by `plugins/register`, not on runtime path |
| `CaptchaSolver` | used by registration only | ⚠️ same |
| `BrowserAutomator` | camoufox / cloak | ⚠️ placeholders (ROADMAP §5.4) |
| `UpstreamClient` | utls / stdhttp | ✅ chosen via config |
| `Storage` | sqlite | ✅ single adapter today |
| `ModelRegistry` | jsonstatic + overrides | ✅ hot-reloadable |
| `Notifier` | webhook / slack | ✅ webhook adapter used by metering |

**Registrar port** — see §0.4 above.

**Config resolver** (`internal/core/resolver/`) — implemented but not
imported anywhere. Meant to sit *between* the raw config layer and the
pick path; wiring is ROADMAP P1-4.

---

**Cross-reference.** `ARCHITECTURE.md` §11a documents the modules
added after the initial design freeze. `ROADMAP.md` is the source of
truth for what actually runs today and what P0/P1/P2 work remains.
