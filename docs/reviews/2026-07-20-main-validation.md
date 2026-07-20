# Main Branch Validation Review - 2026-07-20

> Post-merge validation report for the current `main` branch, focused on telemetry, quota accounting, body templates, TLS/uTLS, and account-pool safety.
>
> Status: review
> Owner: engineering
> Last reviewed: 2026-07-20

---

## 1. Context / Motivation

The current `main` branch was reviewed after the merge was completed. The review focused on the core runtime capabilities that carry the highest production risk:

- telemetry and usage accounting
- API key and group quota enforcement
- body-template and catalog loading
- TLS/uTLS and per-account proxy routing
- account-pool pick/lock safety
- automated test coverage around those areas

Repository state at validation time:

- Branch: `main...origin/main`
- HEAD: `e33fbd3`
- Working tree: clean
- Go toolchain observed: `go1.25.6 darwin/amd64`
- pnpm observed: `10.19.0`

No code changes were made during this validation.

## 2. Validation Commands

Backend and plugin checks:

```bash
go test ./...
go test -tags register ./...
go test ./...
# run from plugins/register
go vet ./...
go vet -tags register ./...
go test -race ./internal/adapters/storage/sqlite ./internal/core/proxy ./internal/core/pollworker ./internal/core/metering ./internal/core/upstream ./internal/adapters/httpclient/utls ./internal/adapters/modelregistry/jsonstatic ./internal/e2e
```

Result: all passed.

WebUI checks:

```bash
pnpm --dir webui lint
pnpm --dir webui build
```

Result: both passed. Lint emitted warnings only. The production build emitted a chunk-size warning for the main JavaScript bundle.

Data checks:

- `data/reference/body-templates`: 122 JSON files
- `data/reference/catalogs`: 35 JSON files
- `data/reference/verified-models.json`: 129 models, 6 aliases
- JSON parse failures found: 0
- body-template aliases missing from verified models: 2
- catalog refs that do not match a loaded catalog key exactly: 32

## 3. Executive Assessment

The merged branch is buildable and has meaningful automated coverage across the core backend, register tag, plugin module, WebUI, and selected race-sensitive packages. The implementation is materially beyond scaffolding: the proxy path, quota gates, account-pool lock accounting, pollworker, uTLS client, per-account proxy resolver, and model registry enrichment are all wired and tested.

The main production blocker found in review is idempotency around terminal job handling. Sync requests and the background pollworker can both observe and process the same job terminal transition. The current storage and metering layers do not ensure that only one actor performs terminal side effects. This can duplicate usage events, API key usage, group usage, and webhooks under a race.

The second class of issues is data-contract hardness. Body templates and catalogs load successfully, but annotated `catalogRefs` are not fully understood by the loader, so part of the schema-driven playground experience will miss enum values. The latest dotfile/AppleDouble loader fix is implemented but lacks a dedicated regression test.

Follow-up independent validation confirmed F1-F5 and the metering half of F6. It also corrected F7: the `models.tsx` hook warning is not retained as an actionable finding in this report; the remaining WebUI hook warning is in `model-multiselect.tsx`.

## 4. Core Capability Notes

### 4.1 Telemetry and Quota Accounting

`internal/core/metering/metering.go` records `usage_events`, computes actual credits from pre/post account balance when possible, applies API-key markup, increments Prometheus usage metrics, increments API-key monthly usage, increments group monthly usage, and backfills realized credits on the job row.

API key quota is enforced before account pick in `internal/core/proxy/service.go`. Group model regex and monthly credit budget gates are also enforced before account pick. These are the right placement for capacity safety because rejected requests do not consume an account in-flight slot.

Coverage is good for the arithmetic and basic side effects, including preBalance forwarding from the pollworker. The missing piece is idempotency across multiple terminal observers.

### 4.2 Body Templates and Catalogs

`internal/adapters/modelregistry/jsonstatic/registry.go` loads verified models, model extras, body templates, and catalogs. `proxy.buildBody` starts from `ExampleBodyJSON`, merges user params into `params`, injects media when present, and stamps top-level Higgsgo-owned defaults.

The latest loader behavior skips macOS AppleDouble and dotfiles in both body templates and catalogs. This protects deployments that accidentally package `._foo.json` metadata files.

Real data parses cleanly. The main weakness is that the loader only resolves `catalogRefs` via exact keys like `catalogs/foo.json`, while real template metadata contains annotated refs such as `catalogs/camera_settings.json -> item.camera.id`, `catalogs/reference_elements.json (must be user-created; empty by default)`, and `literal enum: insert_after|replace`.

### 4.3 TLS/uTLS and Per-Account Proxy Routing

`internal/adapters/httpclient/utls` builds uTLS-backed clients, defaults to `chrome_133`, supports multiple browser profiles, and routes HTTPS through HTTP/2. SOCKS5 and HTTP CONNECT proxies are supported.

`internal/adapters/httpclient/utls/pool.go` caches clients per proxy URL, and `internal/core/upstream/client.go` asks the resolver before each upstream request. This means `account.bound_proxy_url` is honored in the runtime path. Resolver failures are logged and fall back to the default client.

Automated tests cover the pool contract and upstream 401 retry/remint behavior. Live upstream TLS tests are gated behind `HIGGSGO_TEST_UPSTREAM=1`, which is appropriate for CI stability.

### 4.4 Account-Pool Safety

`AccountStore.PickAndLock` runs inside a transaction and enforces:

- group aggregate `MaxGroupInFlight`
- per-account cap from `PickParams.MaxConcurrentPerAccount`
- active or expired-throttled account eligibility
- estimated balance headroom
- paid, ultra, and unlim tier gates
- group membership scoping
- strategy ordering with least-loaded/random tie breaking

`Unlock` decrements with `MAX(0, ...)`, and boot-time `ResetAllInFlight` clears leaked counters from prior process crashes.

The remaining mismatch is that account-level `max_concurrent` is persisted and shown in APIs/WebUI, but the pick SQL does not enforce it. The effective fallback cap is still hardcoded to 5 when the group does not pass a per-account cap.

## 5. Findings

### F1 - High - Terminal job handling is not idempotent between sync path and pollworker

Production startup always launches the pollworker in `cmd/higgsgo/main.go`. The sync generation path persists every successfully-created upstream job as `queued`, then polls upstream directly until terminal. The pollworker scans pending jobs and may also observe the same terminal status during that window.

Relevant code:

- `cmd/higgsgo/main.go`: pollworker startup
- `internal/core/proxy/service.go`: job creation/persistence, sync polling, terminal `UpdateStatus`, metering, webhook
- `internal/core/pollworker/worker.go`: pending scan, terminal `UpdateStatus`, in-flight release, metering, webhook
- `internal/adapters/storage/sqlite/job_store.go`: `UpdateStatus` updates by `WHERE id = ?` without compare-and-set semantics
- `internal/adapters/storage/sqlite/migrations/001_init.sql`: `usage_events` has no uniqueness constraint on `higgsgo_job_id`

Impact:

- duplicate `usage_events` rows
- duplicate API key `monthly_used` increments
- duplicate group `monthly_credit_used` increments
- duplicate webhooks
- possible double in-flight release; the counter will not underflow, but a slot may be freed earlier than intended relative to the second observer
- earlier slot release can admit extra picks while the original sync observer is still finishing its terminal path, making group/account in-flight boundaries more jittery under load

Likelihood:

- Low-QPS deployments with short sync completion windows may rarely hit the race.
- Higher-QPS deployments, long-running sync requests, or tighter pollworker intervals make the overlap much more likely.
- `pollworker.lastPolled` is cadence throttling within the worker, not an ownership lock across the sync path and the worker.

Recommended fix:

1. Add a terminal-transition compare-and-set API, for example `TryMarkTerminal(id, from live statuses, meta) (won bool, err error)`.
2. Only the winner should run metering and webhook side effects.
3. Add an idempotency key for usage events, preferably a unique index on `usage_events(higgsgo_job_id)` for real upstream jobs. If create-failed synthetic `cf_*` events remain, handle them explicitly.
4. Add a regression test that drives a sync terminal path and a pollworker terminal path against the same job and asserts one usage increment, one group increment, one API-key increment, and one webhook.

### F2 - Medium - Annotated `catalogRefs` are not fully enriched into model enums

The registry enriches enums only when `catalogRefs[param]` exactly matches a catalog key. Real template data includes annotations and literal enums. This leaves some playground/schema enum values unavailable even though the backend templates are otherwise valid.

Observed data:

- exact JSON parse failures: 0
- unmatched catalog refs: 32
- sample refs:
  - `catalogs/camera_settings.json U+2192 item.camera.id`
  - `literal enum: insert_after|replace`
  - `catalogs/reference_elements.json (must be user-created; empty by default)`
  - `spa-only-presets.json U+2192 cinema_studio_3_5_video_edit`
- the source data uses a Unicode U+2192 arrow in these annotated refs, not ASCII `->`
- `spa-only-presets.json` is not present in `data/reference/catalogs`

Also observed:

- `cinema-studio-3-5-video-edit` body template alias is not present in verified models
- `nano-banana-animal` body template alias is not present in verified models

Recommended fix:

1. Normalize refs by extracting the leading `catalogs/*.json` path before annotations.
2. Support nested extractor hints such as `item.camera.id` and `item.lens.id`.
3. Support `literal enum: a|b` directly.
4. Add a real-data integrity test with an explicit allowlist for refs that are intentionally SPA-only or user-created.

### F3 - Medium - Dotfile/AppleDouble loader fix lacks a direct regression test

The loader now skips `._foo.json` and `.hidden.json` for body templates and catalogs. That implementation is correct, but the latest behavior is only indirectly covered by loading the real clean data set.

Recommended fix:

Add unit tests around `loadBodyTemplates` and `loadCatalogs` using temporary directories containing:

- one valid JSON file
- one invalid AppleDouble-style `._bad.json`
- one hidden `.hidden.json`

The test should assert that no parse error is returned and the valid file still loads.

### F4 - Medium/Low - Account-level `max_concurrent` is persisted but not enforced

The account schema, admin API, and WebUI expose `account.max_concurrent`, but `PickAndLock` does not use the account row's value in its eligibility predicate. The actual per-account cap comes from `PickParams.MaxConcurrentPerAccount`; when unset, the store falls back to 5.

Impact:

Operators can configure a per-account value that appears meaningful in UI/API but has no scheduling effect.

Recommended fix:

Define precedence and test it. A conservative rule would be:

```text
effective_cap = min(non_zero(account.max_concurrent), non_zero(group.max_concurrent_per_account), non_zero(config.pool.max_in_flight_per_account), default_5)
```

If account-level caps are intentionally deferred, hide or clearly label the field in API/WebUI until enforcement lands.

### F5 - Medium/Low - Public `/metrics` is unauthenticated

The public router exposes `/metrics` without authentication when metrics are enabled. The code comment already calls this out and expects private-interface deployment.

Impact:

If the public listener is reachable from untrusted networks, metrics can leak operational information such as request volume, error rates, usage counters, active accounts, and pending jobs.

Recommended fix:

Protect `/metrics` before production exposure using one of:

- private bind address
- reverse-proxy ACL
- bearer auth
- separate admin listener

### F6 - Low - Some comments and docs are stale

Examples:

- `internal/core/metering/metering.go` still says the pollworker cannot snapshot preBalance and falls back to upstream cost. Current code passes `j.PreBalanceH`.
- Some top-level docs still contain historical statements about scaffolding or older pool behavior.

Recommended fix:

Update the metering package comment first because it sits next to critical accounting paths. Then refresh README/OPERATIONS/ARCHITECTURE/PLUGGABLE/POOL-AND-CPA sections that still contradict source behavior.

### F7 - Low - WebUI warnings and bundle size should be tracked

`pnpm --dir webui lint` passes with warnings. Most are Fast Refresh `only-export-components` warnings. One hook dependency warning is worth triage:

- `src/components/groups/model-multiselect.tsx`: the dependency array serializes `parsed?.aliases?.join("|")`, which is intentional for identity stability but still triggers the hook dependency rule

`pnpm --dir webui build` passes but emits a Vite warning for the main JavaScript bundle:

- main JS: about 1,538 kB minified
- gzip: about 436 kB

Recommended fix:

Either refactor the `model-multiselect.tsx` memoization to satisfy the hook rule or add a narrow suppression with a comment explaining the identity-stability intent. Treat chunk splitting as a follow-up performance task.

## 6. Test Coverage Assessment

Strong coverage observed:

- API key quota gate
- group model/budget gates
- spillover eligibility
- usage accounting math and side effects
- pollworker preBalance forwarding
- async in-flight release and timeout behavior
- group-scoped picking and concurrency caps
- per-account cap from group policy
- boot-time in-flight reset
- uTLS pool resolver contract
- upstream 401 remint retry
- model registry loading from real data
- WebUI lint/build

Important missing coverage:

- terminal-transition idempotency across sync path and pollworker
- uniqueness/idempotency of `usage_events` by Higgsgo job id
- real-data `catalogRefs` integrity
- dotfile/AppleDouble fixture tests for body templates and catalogs
- account-level `max_concurrent` enforcement, if that field is meant to be operational

## 7. Recommended Next Actions

Priority order:

1. Fix terminal idempotency for sync path versus pollworker. This is the only high-severity production risk found in this pass.
2. Add a regression test that proves one terminal job creates exactly one billing event and one webhook.
3. Harden `catalogRefs` parsing and add a real-data integrity test.
4. Add direct tests for AppleDouble/dotfile skipping.
5. Decide and implement the semantics for `account.max_concurrent`.
6. Protect `/metrics` for any non-private deployment.
7. Refresh stale comments and docs.
8. Triage WebUI hook warnings and plan bundle splitting.

## 8. Bottom Line

The current `main` branch is in a generally healthy merged state: tests pass, core services are wired, and most high-value runtime paths have focused coverage. The main gap is not basic correctness or build stability; it is hard idempotency at the terminal job boundary. Fixing that boundary should be treated as the next production-readiness task before relying on usage totals and quota counters under concurrent load.
