# higgsgo

OpenAI-compatible reverse proxy for higgsfield.ai, with a pluggable account pool, registration pipeline, and dual delivery modes (standalone WebUI and CPA plugin).

> **Status**: scaffolding
> **Owner**: daniellee2015
> **Language**: Go 1.22+

---

## What this is

- **Reverse proxy** for `higgsfield.ai` behind an OpenAI-compatible `/v1/*` API.
- **Account pool** management with group-level quotas, concurrency limits, and health checks.
- **Registration pipeline** that automates new account provisioning (Playwright / CloakBrowser + Microsoft Graph / disposable mailbox + CAPTCHA solvers).
- **Pluggable adapters**: proxy providers, mailbox providers, CAPTCHA solvers, browsers, storage backends, and notifiers are all interfaces — swap by editing config.
- **Two consumption modes**:
  - **Mode A (standalone)**: users hit higgsgo directly, higgsgo issues API keys, ships its own WebUI.
  - **Mode B (CPA plugin)**: an external CPA platform manages accounts, higgsgo exposes an `/internal/*` API for the CPA plugin (`higgs-cpa-plugin`, separate repo) to consume.

## Empirical grounding

Every service-side quirk in the code is backed by an empirical test recorded in `data/reference/`:

- `sealed.json` — the authoritative 216-model classification (A/B/C/D/X).
- `unlimited-semantics.json` — permission matrix showing that `has_unlim: true` on Pro/Plus is **not** sufficient to reach `/jobs/v2/*_unlimited` endpoints (Ultra required).
- `final-classification.json` — per-JST classification with subclass breakdown.
- `reference-media.json` — shared media UUIDs (starter-owned CDN URLs bypass per-user IP checks).

See `docs/ARCHITECTURE.md` §Context for the full list of empirical constraints.

## Directory layout

```
higgsgo/
├── cmd/
│   ├── higgsgo/            main binary
│   └── higgsgo-cli/        ops CLI (register accounts, check balance, migrate)
├── internal/
│   ├── domain/             pure business types (Account, Job, ModelSpec, ...)
│   ├── ports/              provider interfaces (dependency inversion boundary)
│   ├── core/               business logic (pool, proxy, register, jwt, metering, groups, ticker)
│   ├── adapters/           provider implementations (711proxy / graph / capsolver / chromedp / ...)
│   ├── api/                HTTP handlers (v1 / admin / internal / middleware)
│   ├── config/             config loading + wire-up
│   └── observability/      logging / metrics / audit
├── configs/                sample configs (higgsgo.example.toml + models/ + providers/)
├── data/reference/         empirical data brought over from higgsfield-register
├── scripts/                migration and build scripts
├── test/                   integration + mock
└── docs/                   ARCHITECTURE.md / CONVENTIONS.md / PLUGGABLE.md / POOL-AND-CPA.md
```

## Rules

**All source code, documentation, comments, log messages, config keys, and error messages are in English.** See `docs/CONVENTIONS.md`.

## Related repos (planned)

- `higgs-cpa-plugin` — TypeScript plugin embedded in the CPA platform; talks to higgsgo `/internal/*`.
- `higgsgo-webui` — React/Vue/Svelte frontend for standalone mode; talks to higgsgo `/admin/*`.

## Getting started

Not yet runnable. This is scaffolding. See `docs/ARCHITECTURE.md` for the migration plan (Phase 0 → Phase 4).

## License

TBD.
