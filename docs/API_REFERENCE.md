# higgsgo API Reference

> Endpoint reference lifted directly from source. Three listeners, three auth models:
>
> | Listener | Default addr     | Auth                                                              |
> |----------|------------------|-------------------------------------------------------------------|
> | Public   | `0.0.0.0:8080`   | Public + optional API key (`Authorization: Bearer sk-hg-*`)       |
> | Admin    | `127.0.0.1:8081` | `admin_bearer` shared secret                                      |
> | Internal | `127.0.0.1:8082` | `internal_bearer` shared secret; only up when `modes.cpa_plugin = true` |
>
> Env vars used in examples: `$BASE` (public), `$ADMIN_BASE`, `$INT_BASE`, `$API_KEY`, `$BEARER`, `$INT_BEARER`.

## Conventions

- Responses are always `application/json`. Collection endpoints wrap results in `{"data":[...], "limit":N, "offset":M}`.
- Error envelope: `{"error":{"type":"<kind>","message":"..."}}`. Common kinds: `invalid_body`, `invalid_query`, `not_found`, `unauthorized`, `rate_limited`, `internal`.
- Timestamps: `/v1` uses **unix seconds** (`created_at`, `finished_at`); `/admin` uses **RFC3339 UTC**.
- Fields ending in `_h` / `_hundredths` are integers of credits × 100 (`1234` = 12.34 credits).
- Job status enum: `pending`, `queued`, `in_progress`, `completed`, `failed`, `refunded`, `timeout` (`internal/domain/job.go`).

---

## Public — `/health`, `/metrics`

### `GET /health`

No auth. Mounted on all three listeners. Returns `{"status":"ok","time":"2026-07-17T04:12:33Z"}`.

### `GET /metrics`

No auth (Prometheus scrape). Content-Type `text/plain; version=0.0.4`. Series (see `internal/observability/metrics.go`):

| Metric                                        | Type      | Labels                       |
|-----------------------------------------------|-----------|------------------------------|
| `higgsgo_http_requests_total`                 | counter   | `method`, `route`, `status`  |
| `higgsgo_http_request_duration_seconds`       | histogram | `method`, `route`            |
| `higgsgo_http_in_flight_requests`             | gauge     | –                            |
| `higgsgo_accounts_active`                     | gauge     | –                            |
| `higgsgo_jobs_in_flight`                      | gauge     | –                            |
| `higgsgo_usage_credits_hundredths_total`      | counter   | `media_type`, `status`       |
| `higgsgo_upstream_request_duration_seconds`   | histogram | `endpoint`, `status`         |

---

## Public — `/v1/*`

### `GET /v1/models`

Auth: API key optional (so integrators can probe capabilities without one).

Query params: `output` (filter by `image`/`video`/`audio`), `include_unstable=1`, `include_deprecated=1`.

```bash
curl -s $BASE/v1/models
```

Response:

```json
{
  "object": "list",
  "data": [
    {
      "id": "seedance-2-0-mini",
      "object": "model",
      "output": "video",
      "jst": "seedance_v2_mini",
      "est_cost": 4.50,
      "required_params": ["prompt", "duration"],
      "unstable": false
    }
  ]
}
```

### `GET /v1/models/{alias}`

Auth: API key optional. Returns the raw `ModelSpec` from the registry
(same fields as `data[]` items above plus params schema).

Errors: `404 model_not_found`.

### `POST /v1/videos/generations`

Auth: API key required. Body:

```json
{
  "model": "seedance-2-0-mini",
  "prompt": "a red apple rolling on marble",
  "image_url": "https://...",             // optional; passed as media.url
  "media_id": "hf_upload_xyz",             // optional; pre-uploaded higgsfield id
  "async": true,                            // optional; default true for video/audio
  "callback_url": "https://you/webhook",   // optional; async only
  "group_id": "grp_...",                   // optional; auto-resolved from key bindings when absent
  "duration": 6                             // any other field forwarded verbatim to upstream params
}
```

Response is a `GenerationResponse`: `{id, object, model, status, created_at, result_url, upstream_job_id, cost, poll_url, data:[{url}]}` (`internal/core/proxy/service.go`).

Errors: `400 invalid_body` (missing `model`, malformed JSON, `ambiguous_group`), `404 model_not_found`, `401 missing_api_key|invalid_api_key`, `402 plan_gate`, `403 api_key_revoked`, `429 rate_limited`, `503 no_account_available`, `502 upstream_5xx`, `504 upstream_timeout`.

```bash
curl -s $BASE/v1/videos/generations \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"seedance-2-0-mini","prompt":"a red apple","async":true}'
```

### `POST /v1/images/generations`

Auth: API key required. OpenAI-shaped body:

```json
{
  "model": "nano-banana-2",
  "prompt": "a cat wearing a hat",
  "n": 1,                        // → params.batch_size
  "size": "1024x1024",           // → params.width / params.height
  "quality": "hd",
  "media_id": "hf_upload_xyz",
  "async": false,
  "callback_url": "https://...",
  "group_id": "grp_..."
}
```

Unknown keys are forwarded to upstream `params`. Response shape and error set identical to `/v1/videos/generations`.

### `GET /v1/jobs`

Auth: API key required. Returns the caller's own jobs, newest first.

Query params: `status`, `since` / `until` (RFC3339 or unix seconds), `limit` (default 100, max 500), `offset`.

```bash
curl -s "$BASE/v1/jobs?status=completed&limit=20" -H "Authorization: Bearer $API_KEY"
```

Row shape: `{id, object, model, status, created_at, finished_at, upstream_job_id, cost, result_url, refunded, latency_ms, data:[{url}]}`. Internal fields (`account_id`, `group_id`, `pre_balance_h`, `actual_credits_h`, `charged_credits_h`) are hidden — use `/admin/jobs`.

Errors: `503 jobs_store_unavailable` when the deploy has no job store.

### `GET /v1/jobs/{id}`

Auth: API key required. Same view as list rows; when the job belongs to a different API key the handler returns `404 job_not_found` (prevents cross-tenant leaks).

---

## Admin — `/admin/*` (Bearer required)

Global auth: `Authorization: Bearer $HIGGSGO_ADMIN_BEARER`.

### `GET /admin/keys`

Every API key row (never plaintext). Row fields: `id`, `name`, `created_by`, `status` (`active`|`revoked`), `monthly_quota`, `monthly_used`, `markup_pct`, `created_at`, `last_used_at`.

### `POST /admin/keys`

Body: `{"name":"team-alpha","created_by":"danlio","monthly_quota":100000,"markup_pct":1.2}`. `monthly_quota` is credits × 100 (`0` = unlimited); `markup_pct` of `1.0` is pass-through.

Response `201`: the standard key view plus `plaintext_key` and `display_hint`. Store the plaintext immediately.

```bash
curl -s $ADMIN_BASE/admin/keys -H "Authorization: Bearer $BEARER" \
  -H "Content-Type: application/json" \
  -d '{"name":"team-alpha","monthly_quota":100000,"markup_pct":1.2}'
```

### `GET /admin/keys/{id}` / `DELETE /admin/keys/{id}`

Get one key / revoke (soft; sets `status='revoked'`, response `{"id":"key_...","status":"revoked"}`). `404 not_found` on missing id.

> No update endpoint — adjust quota by revoke + reissue, or edit the row directly.

### `GET /admin/accounts`

Query params: `plan_type`, `status` (`active`|`suspended`|`banned`|`out_of_credits`), `min_balance` (int, credits × 100).

Sensitive fields (`password_enc`, `cookies_json`, `captured_user_agent`, `x_datadome_clientid`) are stripped from all responses.

### `GET /admin/accounts/{id}`, `POST /admin/accounts/{id}/pause`, `POST /admin/accounts/{id}/resume`, `DELETE /admin/accounts/{id}`

Empty bodies. Pause → `status='suspended'` (picker skips it); resume → `status='active'`; delete → `status='banned'` (rows preserved for audit). `404 not_found` on missing id.

> No `POST /admin/accounts` create endpoint — accounts are imported via `scripts/import-node-accounts` (see OPERATIONS.md §2).

### `GET /admin/stats/pool`

No params. Response: `{total, by_plan{...}, by_status{...}, total_subscription_balance, with_unlim, with_flex_unlim}`. `total_subscription_balance` is a float (already divided by 100).

### `GET /admin/stats/health`

No params. Cheap liveness probe: `{"ok": true, "accounts_active": N, "time": "..."}`.

### `GET /admin/usage`

Query params (all optional): `since`, `until`, `api_key_id`, `cpa_partner_id`, `account_id`, `group_id`, `model_alias`, `status`, `limit` (default 100, max 1000), `offset`. Row keys map 1-to-1 to `usage_events` columns.

### `GET /admin/usage/aggregate`

Same filters plus `group_by` (CSV). Whitelisted dimensions: `api_key_id`, `cpa_partner_id`, `account_id`, `group_id`, `model_alias`, `billing_day`, `billing_month`, `media_type`, `status`. Unknown values dropped silently.

```bash
curl -s "$ADMIN_BASE/admin/usage/aggregate?group_by=api_key_id,billing_day" \
  -H "Authorization: Bearer $BEARER"
```

Row: `{keys:{...}, request_count, completed_count, failed_count, refunded_count, total_credits_h, charged_credits_h, avg_latency_ms}`.

### `GET /admin/groups`, `POST /admin/groups`, `GET /admin/groups/{id}`, `DELETE /admin/groups/{id}`

List / create / get / delete. Delete cascades on members + bindings. Create body:

```json
{
  "name": "prod-tenant-a",
  "description": "shared pool for tenant A",
  "max_concurrent_jobs": 20,
  "max_concurrent_per_account": 5,
  "monthly_credit_budget": 500000,
  "allowed_models_regex": "^(seedance|kling).*",
  "blocked_models_regex": "",
  "route_strategy": "round_robin",
  "owner_type": "apikey",           // apikey | cpa_partner | internal
  "owner_id": "key_..."
}
```

Only `name` is required. Defaults: `owner_type=internal`, `route_strategy=round_robin`.

### `PUT /admin/groups/{id}`

Update a group's mutable fields. Body accepts same fields as create;
only supplied keys are modified.

### `GET /admin/groups/{id}/members`, `POST /admin/groups/{id}/members`, `DELETE /admin/groups/{id}/members/{accountId}`

List returns `{"group_id":..., "members":["acc_...", ...]}`. Add body: `{"account_id":"acc_...","priority":10}` (idempotent). Remove has no body.

### `GET /admin/groups/{id}/bindings`, `POST /admin/groups/{id}/bindings`, `DELETE /admin/groups/{id}/bindings/{apiKeyId}`

List bindings for a group. Bind an API key to the group. Body: `{"api_key_id":"key_..."}`. When a key has exactly one binding, `/v1/*` auto-resolves `group_id` from it; multiple bindings use the spillover resolution (sorted by binding name ascending).

### `GET /admin/jobs`, `GET /admin/jobs/{id}`

Query params (list): `status`, `account_id`, `api_key_id`, `group_id`, `model_alias`, `since` / `until`, `limit` (default 100, max 500), `offset`. Response echoes effective `limit`/`offset`.

Unlike `/v1/jobs`, this view exposes internal accounting: `account_id`, `group_id`, `pre_balance_h`, `actual_credits_h`, `charged_credits_h`. Timestamps are RFC3339.

### `POST /admin/jobs/purge`

Body: `{"older_than":"2026-07-01T00:00:00Z"}`. Deletes job rows older
than the given timestamp. Usage events are preserved. Response:
`{"purged": N}`.

### `GET /admin/model-health`, `GET /admin/model-health/{jst}`

Wired. Backed by `internal/api/admin/model_health.go`; data comes from
the regression ticker.

- List: `GET /admin/model-health?verdict=&stale_before=` — newest first.
- Detail: `GET /admin/model-health/{jst}` — latest probe for one model.
- Time-series: `GET /admin/model-health/{jst}/slots?count=N&slot_sec=S` —
  bucketed success/fail slots for the uptime bar. Count capped at 168.

WebUI consumer: `webui/src/routes/models.tsx`. Note: the UI currently
backfills a mock uptime value when no health row exists for a JST — see
`docs/ROADMAP.md` P2-7.

### `POST /admin/models/reload`

Hot-reload the model registry from disk plus the runtime overrides
table. Returns the new model count. Used by the WebUI "reload" button
and by CI post-deploy hooks.

### Account operations (bulk / lifecycle / import-export)

- `PATCH /admin/accounts/{id}` — mutable columns:
  `bound_proxy_url` (enforced — P1-5 landed; `utls.Pool` routes per-account),
  `priority` (int, [-1000, 1000]), `max_concurrent`,
  `note`, `source`.
- `POST /admin/accounts` (bulk import) — accepts a JSON array; upserts
  by `id`, skipping duplicates.
- `GET /admin/accounts/export?format=json|csv` — streaming export of the
  full pool for backup / migration.
- `GET /admin/accounts/{id}/eligible-models` — resolves which models the
  account's plan / cohort is entitled to. Consumed by the WebUI
  account detail sheet.
- `POST /admin/accounts/{id}/probe` — actively pings the account via
  the upstream client (JWT mint + per-account proxy + TLS fingerprint).
  Response: `{ok, latency_ms, balance}` on success, or
  `{ok:false, error:{kind, message}}` on failure. 15 s deadline.
  Returns 503 `probe_disabled` when no upstream client is wired.

### Key operations (write verbs beyond CRUD)

- `PATCH /admin/keys/{id}` — updates `markup_pct`, `monthly_limit`,
  `note`, `playground_scope`, active flag.
- `POST /admin/keys/{id}/rotate` — mints a new secret, invalidates the
  old one, returns the plaintext once.
- `POST /admin/keys/{id}/pause`, `POST /admin/keys/{id}/resume` —
  temporary disable / re-enable without deleting.
- `POST /admin/keys/{id}/reset_usage` — zeros `monthly_used` for a key
  outside the natural month boundary.
- `GET /admin/keys/{id}/stats` — call counts, credits used, last-seen.
- `GET /admin/keys/{id}/groups` — groups this key is bound to.
- `POST /admin/keys/{id}/playground_scope` — toggle playground access
  for a key (body: `{"enabled": true|false}`).

### Failover console

Wired at `/admin/failover/*` and `/admin/accounts/{id}/failover`
(`internal/api/admin/failover.go`).

- `GET /admin/failover/config`, `PUT /admin/failover/config` — read /
  update global controller policy (`fail_limit`, `judge_window`,
  `evict_window`, `cooldown`, `outage_guard`).
- `GET /admin/failover/isolated` — accounts currently `disabled` or
  `throttled`. Backs the sidebar `PoolHealthIndicator` red-dot list.
- `GET /admin/accounts/{id}/failover`,
  `PUT /admin/accounts/{id}/failover` — per-account override for
  `fail_limit` and window sizes.
- `POST /admin/accounts/{id}/recover` — manually flip
  `disabled → active` (`throttled` recovers automatically via the
  Recoverer goroutine).

### Model overrides

- `GET /admin/models/overrides` — list runtime overrides on top of the
  static registry.
- `GET /admin/models/{alias}/override` — get the override for one model.
- `PUT /admin/models/{alias}/override` — set / clear per-field overrides
  (pointer semantics: `null` clears, value overrides). Triggers
  `Registry.Reload()`.
- `DELETE /admin/models/{alias}/override` — remove all overrides for
  one alias.

### Routing default

- `GET /admin/settings/routing`, `PUT /admin/settings/routing` — the
  default `RouteStrategy` used when a new group is created.
  ⚠️ **Does not** retroactively change existing groups' strategies.

### Admin bearer runtime rotation

- `GET /admin/settings/bearer` — hash-only read (returns SHA-256 of
  current token for confirmation).
- `POST /admin/settings/bearer/rotate` — mint a fresh token, activate
  after a 30 s grace window.

### Registrations (registrar plugin)

- `GET /admin/registrations` — list rows (paged).
- `POST /admin/registrations` — enqueue a new registration request.
  Body: `{"email":"...", "password":"...", "mailbox_client_id":"...",
  "mailbox_refresh_token":"...", "proxy_url":"...", "oauth_source":"..."}`.
  Password + mailbox fields required for the password flow (no
  `oauth_source`); OAuth flows skip them.
- `POST /admin/registrations/bulk` — bulk import from a mailbox list.
  Body: `{"lines":"email----password----client_id----refresh_token\n...",
  "proxy_url":"socks5://..."}`. Response:
  `{enqueued, ids, skipped:[{line, reason}]}`.
- `GET /admin/registrations/{id}` — status detail.
- `POST /admin/registrations/{id}/retry` — retry a failed registration.

**⚠️ Default builds return `503 registrar_disabled` for every write /
retry endpoint.** Only the `-tags register` build compiles the bridge to
`plugins/register/`. See `docs/PLUGGABLE.md` §0.

### Audit trail

- `GET /admin/audit` — paginated audit events. Filters: `action`,
  `actor`, `since`, `until`.
- `GET /admin/audit/export?format=csv|jsonl` — streaming export for
  cold storage.

### Tickers (manual runs)

- `POST /admin/tickers/refresher` — force-run the JWT + balance refresher
  tick synchronously. Returns 200 on success, 503 when not wired.
- `POST /admin/tickers/regression` — force-run the model regression check
  tick synchronously. Returns 200 on success, 503 when not wired.

### Version endpoint

- `GET /admin/version` — build info from ldflags + `runtime/debug`.
- `GET /admin/version/check` — compares against latest GitHub release
  (1 h in-memory cache).

### Webhooks

- `GET /admin/webhooks/stats` — aggregated delivery statistics from the
  webhook dispatcher. Full CRUD for webhook subscriptions is not yet
  wired.

---

## Playground — `/v1/playground/*` (WebUI scope keys only)

Requires a key whose `playground_scope = true` (migration `012`). The
same `X-API-Key` middleware but a separate rate bucket.

### `POST /v1/playground/estimate`

Body: `{ "model": "video_bad", "params": {...} }`. Returns the expected
credit cost so the WebUI can show a warning before submitting.

### `POST /v1/playground/execute`

Enqueues a job in the playground pool. Identical semantics to
`POST /v1/videos/generations` but constrained to models with
`playground = true`.

### `GET /v1/playground/models`

Filtered list of models exposed to playground callers.

---

## Internal — `/internal/*` (Bearer required, CPA-mode only)

Global auth: `Authorization: Bearer $HIGGSGO_INTERNAL_BEARER`. The listener
only starts when `[modes] cpa_plugin = true`.

### `POST /internal/register`

Mint an API key scoped to a CPA partner. Body:

```json
{
  "partner_id": "cpa_xyz",
  "email": "ops@example.com",       // optional
  "name": "cpa/xyz",                 // optional; defaults to "cpa/<partner_id>"
  "markup_pct": 1.2,                 // optional; 1.0 = pass-through
  "monthly_limit": 100000             // optional; credits × 100; 0 = unlimited
}
```

Response `201`: `{api_key_id, key, cpa_partner_id, markup_pct, monthly_limit, display_hint}`. The key is stamped `created_by='cpa-plugin'` and `cpa_partner_id=<partner_id>`. Errors: `400 invalid_body`, `500 gen_key` / `insert`.

### `POST /internal/execute`

Proxy a CPA-side user request through the pool. Body:

```json
{
  "api_key_id": "key_...",           // preferred; verified active
  "cpa_partner_id": "cpa_xyz",       // fallback: first active key matching partner
  "model": "seedance-2-0-mini",
  "prompt": "a red apple",
  "async": true,
  "callback_url": "https://...",
  "group_id": "grp_...",
  "image_url": "https://...",         // → media.url, type=image
  "video_url": "https://...",         // → media.url, type=video
  "media_id": "hf_upload_xyz",        // pre-uploaded higgsfield id
  "params": {"duration": 6}            // free-form; forwarded to upstream
}
```

Response: `GenerationResponse` (same shape as `/v1/*`). Errors: `400 invalid_body`, `404 api_key_not_found`/`partner_not_registered`/`model_not_found`, `403 api_key_revoked`, `502 upstream_error`.

### `GET /internal/balance/{partner_id}`

Aggregate usage + quota for a partner:

```json
{
  "partner_id": "cpa_xyz",
  "total_used_h": 1234,
  "total_limit_h": 100000,             // 0 => at least one key is unlimited
  "key_count": 3,
  "keys": [{"id":"key_...","name":"cpa/xyz","status":"active","monthly_used_h":1234,"monthly_limit_h":100000,"markup_pct":1.2}]
}
```

### `POST /internal/refresh_jwt/{partner_id}`

Empty body. Flushes the upstream-JWT cache pool-wide (partner → account membership isn't modelled yet). Response: `{partner_id, invalidated, scope:"pool-wide"}`. Errors: `404 partner_not_registered`, `503 not_ready`.

### `DELETE /internal/{partner_id}`

Soft-delete every active key belonging to the partner. Response: `{partner_id, disabled, total_keys}`. Errors: `404 partner_not_registered`.

### `GET /internal/registrations/{id}`

Async-registration status stub. Returns **`501 not_implemented`** with a stable body until a background registration worker lands: `{"registration_id":"...","error":{"type":"not_implemented","message":"async registration TODO"}}`.

### `GET /internal/status`

Pool-sizing probe (partner-agnostic): `{mode:"cpa_plugin", accounts_active, accounts_total, keys_active, keys_total}`. Use to distinguish "up but pool empty" (all `/internal/execute` calls would 503) from a real outage.
