# higgsgo Operations Runbook

> Cheat-sheet for operators and SREs. Every command below is copy-pasteable
> against a live higgsgo deploy. See `API_REFERENCE.md` for exact endpoint
> schemas.
>
> Shell env used throughout:
>
> ```bash
> export BASE=http://localhost:8080          # public /v1
> export ADMIN_BASE=http://localhost:8081    # admin /admin
> export INT_BASE=http://localhost:8082      # internal /internal (CPA-mode only)
> export BEARER=$HIGGSGO_ADMIN_BEARER
> export INT_BEARER=$HIGGSGO_INTERNAL_BEARER
> ```

---

## 1. First-time deploy checklist

1. Copy `configs/higgsgo.example.toml` → `configs/higgsgo.prod.toml`. Set
   `server.admin_bearer` and `server.internal_bearer` to strong secrets
   (or export `HIGGSGO_ADMIN_BEARER` / `HIGGSGO_INTERNAL_BEARER`).
2. Point `storage.sqlite.path` at a persistent volume (default `./data/higgsgo.db`).
3. If serving CPA traffic, set `modes.cpa_plugin = true`; otherwise the
   internal listener never starts.
4. `go run ./cmd/higgsgo -config configs/higgsgo.prod.toml`. Migrations
   are embedded and apply on startup.
5. Verify all three listeners are up:
   ```bash
   curl -s $BASE/health $ADMIN_BASE/health $INT_BASE/health
   ```

---

## 2. Import an account into the pool

Accounts come from the Node `higgsfield-register` project as `higgsfield-*.json`
files. There is **no** `POST /admin/accounts` endpoint — import runs off the
`scripts/import-node-accounts` binary:

```bash
# from an already-produced directory
go run ./scripts/import-node-accounts \
    -config configs/higgsgo.prod.toml \
    -dir /path/to/higgsfield-register/output

# dry-run first: parses + validates without touching the DB
go run ./scripts/import-node-accounts \
    -config configs/higgsgo.prod.toml \
    -dir /path/to/higgsfield-register/output \
    -dry-run
```

The importer reads every `higgsfield-*.json` in `-dir`, upserts an `accounts`
row keyed on `email`, and stamps `imported_at`. Sensitive fields
(`password`, `cookies`, `x_datadome_clientid`, `captured_user_agent`) are
persisted; API responses never leak them (see
`internal/api/admin/accounts.go` `accountView`).

The background balance/entitlement refresher (`internal/core/refresher`)
picks the row up on its next tick — no restart or manual reload is needed.
Cadence is controlled by `[pool] balance_refresh_interval` (default `10m`).

Verify:

```bash
curl -s "$ADMIN_BASE/admin/accounts?status=active" \
  -H "Authorization: Bearer $BEARER" | jq '.data | length'
```

## 3. Issue an API key

```bash
curl -s -X POST $ADMIN_BASE/admin/keys \
  -H "Authorization: Bearer $BEARER" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "team-alpha",
    "created_by": "danlio",
    "monthly_quota": 100000,
    "markup_pct": 1.2
  }' | jq
```

The response includes `plaintext_key` **once**. Store it now; there is no
"reveal" endpoint. `monthly_quota` and `markup_pct` are both in
credits × 100 units, and `monthly_quota=0` means unlimited.

Rotate: `DELETE /admin/keys/{id}` (revokes; `status='revoked'`) then create
a fresh one — there is no update endpoint.

## 4. Create a group, bind accounts + a key

Groups isolate a pool subset for a tenant. Three curls end to end:

```bash
# 4a. Create the group
GID=$(curl -s -X POST $ADMIN_BASE/admin/groups \
  -H "Authorization: Bearer $BEARER" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "tenant-a-prod",
    "max_concurrent_jobs": 20,
    "monthly_credit_budget": 500000,
    "owner_type": "apikey",
    "owner_id": "key_alpha"
  }' | jq -r .id)

# 4b. Add accounts to the group (repeat for each)
curl -s -X POST $ADMIN_BASE/admin/groups/$GID/members \
  -H "Authorization: Bearer $BEARER" \
  -H "Content-Type: application/json" \
  -d '{"account_id":"acc_...", "priority": 10}'

# 4c. Bind the API key to the group
curl -s -X POST $ADMIN_BASE/admin/groups/$GID/bindings \
  -H "Authorization: Bearer $BEARER" \
  -H "Content-Type: application/json" \
  -d '{"api_key_id":"key_alpha"}'
```

When a key has exactly one group binding, `/v1/*` handlers auto-resolve
`group_id` from it. With two or more bindings the caller must send
`group_id` in the request body or gets `400 ambiguous_group`.

## 5. Register a CPA partner (CPA mode)

```bash
curl -s -X POST $INT_BASE/internal/register \
  -H "Authorization: Bearer $INT_BEARER" \
  -H "Content-Type: application/json" \
  -d '{
    "partner_id": "cpa_xyz",
    "markup_pct": 1.2,
    "monthly_limit": 100000
  }' | jq
```

The response contains the plaintext key and stamps
`api_keys.cpa_partner_id = cpa_xyz` / `created_by = "cpa-plugin"` — every
subsequent `/internal/*` call filters by that column
(`APIKeyStore.ListByCPAPartner`).

Tear down a partner: `DELETE /internal/{partner_id}` — revokes every one
of the partner's keys and preserves the rows for audit.

## 6. Prometheus scrape

`/metrics` is unauthenticated but bound to the public listener; front it
with an IP allowlist or reverse proxy in production
(see `internal/api/server.go` TODO).

```bash
curl -s $BASE/metrics | head -20
```

Key series (see `internal/observability/metrics.go`):

- `higgsgo_http_requests_total{method,route,status}` — HTTP volume, chi route pattern
- `higgsgo_http_request_duration_seconds{method,route}` — HTTP latency histogram
- `higgsgo_http_in_flight_requests` — concurrent requests gauge
- `higgsgo_accounts_active` — sampled by `PoolCollector` every 15s
- `higgsgo_jobs_in_flight` — non-terminal job count
- `higgsgo_usage_credits_hundredths_total{media_type,status}` — per-terminal-job credit counter (recorded by `core/metering.Recorder`)
- `higgsgo_upstream_request_duration_seconds{endpoint,status}` — upstream call latency

Suggested alerts: `higgsgo_accounts_active < 1` (pool empty),
`rate(higgsgo_http_requests_total{status=~"5.."}[5m]) > threshold`,
`histogram_quantile(0.95, higgsgo_upstream_request_duration_seconds) > 60s`.

---

## 7. Troubleshooting

### Client got `401`

Grab the response body — the `error.type` field says which layer rejected:

| `error.type`             | Layer         | Fix                                       |
|--------------------------|---------------|-------------------------------------------|
| `missing_api_key`        | `/v1` middleware | Client didn't send `Authorization` header |
| `malformed_authorization`| `/v1` middleware | Header doesn't start with `Bearer `       |
| `invalid_api_key`        | `/v1` middleware | Bad prefix, checksum, or unknown hash — verify via `GET /admin/keys` |
| `api_key_revoked`        | `/v1` middleware | `status='revoked'`; issue a fresh key     |
| `upstream_auth`          | upstream      | Higgsfield rejected the account's JWT; force-refresh below |

If it is `upstream_auth` and JWT-related, note commit **5f862e2** already
adds a one-shot remint-and-retry on 401 in the upstream client. You should
only need to intervene when the retry itself fails (revoked account
credentials, DataDome challenge). Force-refresh a partner's cache with:

```bash
curl -s -X POST $INT_BASE/internal/refresh_jwt/$PARTNER_ID \
  -H "Authorization: Bearer $INT_BEARER"
```

### Everything returns `429`

Per-API-key token bucket is empty. Values live in `[server.rate_limit]`:

```toml
[server.rate_limit]
rps   = 5
burst = 10
```

Restart to reload. The bucket is `internal/api/middleware/ratelimit.go` and
keys on the resolved `APIKey.ID`. Idle buckets are evicted after 1 hour so
churn does not leak memory.

### `503 no_account_available`

Pool is empty for the requested constraints. Diagnose:

```bash
curl -s $ADMIN_BASE/admin/stats/pool \
  -H "Authorization: Bearer $BEARER" | jq
curl -s "$ADMIN_BASE/admin/accounts?status=active" \
  -H "Authorization: Bearer $BEARER" | jq '.data | length'
```

If `accounts_active == 0`, import more accounts (§2). If plenty of accounts
exist but the specific model still 503s, the model probably requires
`RequiresPaid` / `RequiresUltra` and no account in the caller's bound
group has that entitlement — check `plan_type` / `has_unlim` in the
account list.

### An async job is stuck

```bash
curl -s "$BASE/v1/jobs/$JOB_ID" -H "Authorization: Bearer $API_KEY" | jq
```

Semantics:

- The pollworker polls upstream every ~8s (`internal/core/pollworker`).
- Timeout is 60 min; after that the job flips to `timeout` and is refunded
  by `metering.Recorder` if credits were held.
- If the job is still `queued`/`in_progress` inside the 60 min budget it
  will resolve on its own; no manual intervention needed.

Operator view (with the account, group, and internal accounting fields):

```bash
curl -s $ADMIN_BASE/admin/jobs/$JOB_ID -H "Authorization: Bearer $BEARER" | jq
```

### An account keeps failing

Check its `fail_streak` and `last_failed_at`:

```bash
curl -s $ADMIN_BASE/admin/accounts/$ACC_ID -H "Authorization: Bearer $BEARER" | jq
```

Pool auto-suspends accounts once `fail_streak` exceeds
`[pool] fail_streak_threshold`. To pull one out manually while you
investigate:

```bash
curl -s -X POST $ADMIN_BASE/admin/accounts/$ACC_ID/pause \
  -H "Authorization: Bearer $BEARER"
```

Pool picker skips suspended accounts on `PickAndLock`; resume once the
underlying issue is fixed:

```bash
curl -s -X POST $ADMIN_BASE/admin/accounts/$ACC_ID/resume \
  -H "Authorization: Bearer $BEARER"
```

### Usage / billing reconciliation

```bash
# Per-key monthly totals
curl -s "$ADMIN_BASE/admin/usage/aggregate?group_by=api_key_id,billing_month" \
  -H "Authorization: Bearer $BEARER" | jq

# Every failed job in a window
curl -s "$ADMIN_BASE/admin/jobs?status=failed&since=2026-07-01T00:00:00Z" \
  -H "Authorization: Bearer $BEARER" | jq
```

`actual_credits_h` is what the account was actually billed by upstream;
`charged_credits_h` is what the caller was billed (after `markup_pct`).
Deltas between them are the platform margin.

---

## 8. Disaster recovery

### Backups

SQLite lives at `[storage.sqlite] path` (default `./data/higgsgo.db`). Take
consistent snapshots with `sqlite3 higgsgo.db ".backup /backups/$(date +%F).db"`
on a schedule; the file is safe to `rsync` when the server is stopped.

Restore: stop higgsgo, replace the file, start. Embedded migrations are
idempotent so version drift between binary and DB is auto-repaired.

### Pause the whole pool without stopping the service

Iterate over accounts and pause each one:

```bash
curl -s "$ADMIN_BASE/admin/accounts?status=active" \
  -H "Authorization: Bearer $BEARER" \
  | jq -r '.data[].id' \
  | xargs -I{} curl -s -X POST $ADMIN_BASE/admin/accounts/{}/pause \
      -H "Authorization: Bearer $BEARER"
```

Every subsequent `/v1/*` request will 503 `no_account_available` until you
resume at least one.

### Disable one API key

```bash
curl -s -X DELETE $ADMIN_BASE/admin/keys/$KEY_ID \
  -H "Authorization: Bearer $BEARER"
```

Sets `status='revoked'`. The `/v1` auth middleware short-circuits the very
next request from that key with `403 api_key_revoked`.

### Rotate the admin bearer

Update `configs/higgsgo.prod.toml` (or the env var) and restart. There is
no zero-downtime rotation today; do it during a maintenance window.

---

## 9. Config knobs quick-reference

| Section                        | Field                          | Typical value | Purpose                                              |
|--------------------------------|--------------------------------|---------------|------------------------------------------------------|
| `[server]`                     | `listen`                       | `0.0.0.0:8080`| Public `/v1` bind                                    |
| `[server]`                     | `admin_listen`                 | `127.0.0.1:8081` | Admin bind (private)                              |
| `[server]`                     | `internal_listen`              | `127.0.0.1:8082` | CPA `/internal` bind (private, gated by `modes.cpa_plugin`) |
| `[server.rate_limit]`          | `rps`, `burst`                 | `5`, `10`     | Per-API-key token bucket                             |
| `[pool]`                       | `max_in_flight_per_account`    | `5`           | Concurrency cap; upstream refuses > 6                |
| `[pool]`                       | `fail_streak_threshold`        | `3`           | Auto-suspend threshold                               |
| `[pool]`                       | `balance_refresh_interval`     | `10m`         | Refresher cadence                                    |
| `[pool]`                       | `jwt_refresh_interval`         | `40s`         | Clerk JWT expires in 60s; refresh at 40s             |
| `[modes]`                      | `cpa_plugin`                   | `false`       | Start `/internal` listener and wire cpaplugin handler |
| `[tickers.a_regression]`       | `enabled`, `interval`, `sample_size` | `false`, `24h`, `10` | Regression probes (write to `model_health`)  |
| `HIGGSGO_UPSTREAM_PROXY_URL`   | env only                       | –             | SOCKS5/HTTP proxy for utls client                    |
| `HIGGSGO_WEBHOOK_SIGNING_KEY`  | env only                       | –             | HMAC for `callback_url` deliveries (empty = unsigned)|
