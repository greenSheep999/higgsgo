# Stack Decisions

> Locked-in technology choices for higgsgo. Every entry here is either
> user-directed or resolved during scaffolding. Later changes require an
> explicit decision record appended at the bottom.
>
> Last updated: 2026-07-17

---

## Backend runtime

| Concern | Choice | Notes |
|---|---|---|
| Language | Go 1.22+ | Single binary. |
| HTTP router | `go-chi/chi` v5 | Stdlib-friendly, middleware ecosystem. |
| TLS / JA3 fingerprint | `refraction-networking/utls` | Pure Go, no CGO, works in Alpine containers. |
| SQLite driver | `modernc.org/sqlite` | Pure Go, no CGO. |
| Postgres driver (future) | `jackc/pgx/v5` | Direct wire protocol, best performance. |
| Migrations | Embedded via `//go:embed`, applied on startup | No external migration tool required. |
| Config format | TOML via `pelletier/go-toml/v2` | Human-friendly for ops. |
| Logging | `log/slog` (stdlib) + JSON handler | Structured logs, no third-party. |
| Metrics | `prometheus/client_golang` | Standard. |
| WebSocket (CPA events) | `nhooyr.io/websocket` | Modern, context-aware. |
| Testing | Stdlib `testing` + `testify/require` for readability | |
| Bcrypt (API keys) | `golang.org/x/crypto/bcrypt` | |

## Storage

**SQLite by default.** Sufficient for:
- <500 accounts
- <100k jobs/month
- Single-node deployment

Migrate to Postgres when any of:
- Multi-node higgsgo (need shared account pool locking)
- >1M usage_events rows
- User demands hosted DB (RDS, Cloud SQL)

**Container-friendly**: mount `/data/` as a Docker volume. SQLite WAL mode
enabled, no fsync tuning at start.

## Browser automation

Two subprocess bridges kept side-by-side behind `ports.BrowserAutomator`:

1. **Node + CloakBrowser** (`adapters/browser/cloak_nodejs/`) — Chromium
   fingerprint. Proven against higgsfield's DataDome + Cloudflare via the
   existing higgsfield-register project. Worker script:
   `cloak-worker.mjs`.

2. **Python + Camoufox** (`adapters/browser/camoufox_python/`) — Firefox
   fingerprint. Only usable via the Python SDK; Camoufox has no Go
   bindings. Reuses the operator's existing `camoufox_driver.py`
   shipped inside this adapter directory.

Both bridges share the same protocol: **stdin/stdout JSON-RPC**. Go picks
one at config time (`browser.type = "cloak_nodejs" | "camoufox_python"`).
Config lets us swap without touching core logic.

Rationale for the subprocess approach in both cases:
- Rewriting stealth patches in pure Go (chromedp / rod / patchright) is
  a full-time job; upstream fingerprint detection moves faster than we do.
- Registration is not hot-path; latency of subprocess spawn is acceptable.
- Multi-runtime deployment (Node + Python + Go) is a container packaging
  concern, not an architecture concern.

## Frontend

**In-tree** at `higgsgo/webui/`, embedded into the Go binary via `//go:embed`.

| Concern | Choice |
|---|---|
| Framework | React 19 |
| Build tool | Vite 8 |
| Component library | shadcn/ui — official primitives + `dashboard-01` block, no custom UI atoms |
| Styling | Tailwind CSS v4 (no config file), shadcn `neutral` base + CSS variables |
| Data fetching | TanStack Query |
| Router | TanStack Router (hash history so the bundle is subpath-agnostic) |
| Forms | React Hook Form + Zod |
| Charts | Recharts (shipped by `dashboard-01`) |

Deploy: `pnpm --dir webui build` writes `webui/dist/`, then `go build` picks it up
via `//go:embed all:dist`. The admin listener serves the SPA on any path not
already matched by `/admin/*`, `/v1/playground/*`, `/health`, `/metrics`.
Dev: `pnpm --dir webui dev` runs Vite on `:5173` and proxies `/admin` and
`/v1/playground` to `http://127.0.0.1:18081`. Auth is the admin bearer token
(same per-deploy secret as the `/admin/*` XHR calls).

Sibling-repo mode was considered and rejected — a single binary + zero-nginx
deploy story is worth more than the isolation, and shadcn's registry-based
workflow means the UI can still be updated without touching Go code.

## CPA plugin (Mode B)

Separate repo (`higgs-cpa-plugin/`), TypeScript, embedded in CPA platform.

Protocol: **REST + WebSocket** to higgsgo `/internal/*`. Shared
`X-Internal-Token` secret between CPA and higgsgo.

REST is enough for register / execute / balance / health. WebSocket carries
push events (usage / status_change) to the CPA dashboard in real time.

## Deployment

- Docker image, single stage from `golang:1.22-alpine` builder, `gcr.io/distroless/base-debian12` runtime.
- Node subprocess: ship a separate `higgsgo-registrar` image containing
  `node:20-alpine` + Playwright + CloakBrowser. Communicate over local
  Unix socket or TCP.
- Docker Compose for dev: higgsgo + higgsgo-registrar + optional Postgres.
  The webui is embedded into the higgsgo binary — no extra service.
- Kubernetes manifests: TBD when we outgrow single-node.

## Observability

- Logs → stdout (JSON), structured with `slog`.
- Metrics → `/metrics` on the admin port, Prometheus scrape.
- Traces → OpenTelemetry SDK, exporter configured via env var
  (`OTEL_EXPORTER_OTLP_ENDPOINT`). Off by default.

## What I chose without asking

- SQLite by default, Postgres path documented — small deploys don't need
  the operational overhead of a real DB.
- Node CloakBrowser subprocess over pure-Go browser — the existing solution
  works, and stealth fingerprinting is a full-time job.
- `utls` for HTTP client — impit was Node-only; `utls` is its closest Go
  equivalent and needs no external service.
- REST + WebSocket for CPA plugin — gRPC is heavier and CPA is Node/TS.

## Decision log

- **2026-07-17** — Initial stack locked (this document).
