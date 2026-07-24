# Pricing Source of Truth

> Status: phase 1 implemented, 2026-07-23. Migration 022, transactional
> upstream snapshot/rule persistence, costsync integration, and authenticated
> `GET /v1/pricing` are in place. Sell policy, quote engine, new-api
> publication and automated official-price watchers remain follow-up work.

> Update 2026-07-23: migration 023 adds the internal comparison layer:
> official Higgs plan rates, provider API price observations, and final sell
> decisions. The admin model detail now renders a granular matrix with
> resolution vertically and Higgs credits, plan-derived cost, official API
> price, and final price horizontally. new-api publication remains deferred.
>
> Update 2026-07-23 (later): the downstream wire contract lives in
> `PRICING-DOWNSTREAM-CONTRACT.md`. That file is the source of truth for
> field names, empty-value semantics, and the resolution-primary tier
> decision. `/api/pricing` MUST match its row shape when it ships.

## 1. Goal

higgsgo owns the pricing truth for every model it exposes. It must preserve
the upstream Higgs credit rules, operator cost assumptions, external official
API reference prices, and the final sell policy as separate layers.

new-api must be able to pull a generated pricing configuration from higgsgo
instead of manually copying model prices into its admin UI.

## 2. Price Layers

| Layer | Meaning | Authoritative source | Used for |
|---|---|---|---|
| `upstream_credits` | Higgs credits charged for a parameter combination | `GET /job-sets/costs` | Account affordability, quote input |
| `plan_unit_cost` | Money paid per usable Higgs credit | Official plan snapshots plus operator adjustments | Internal cost conversion |
| `effective_cost` | Expected money cost for one request | `upstream_credits × plan_unit_cost` | Margin calculation |
| `official_api_reference` | Original provider's public API price | Versioned operator-maintained sources | Market comparison only |
| `sell_price` | Price published to downstream consumers | Operator sell policy | new-api billing |
| `actual_job_cost` | Credits actually consumed by a completed job | `usage_events` / balance delta | Reconciliation and realized margin |

These layers must not overwrite each other. In particular, an unlim/free
request may have a zero marginal `actual_job_cost`, but that must not turn the
public `sell_price` into zero.

## 3. Storage Model

Pricing uses two levels: lossless rules and a flattened current projection.

### 3.1 `pricing_snapshots`

One immutable row per source fetch/import:

```sql
CREATE TABLE pricing_snapshots (
  id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  source_url TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL,
  payload_sha256 TEXT NOT NULL,
  fetched_at TEXT NOT NULL,
  effective_at TEXT NOT NULL DEFAULT ''
);
```

Expected sources include `higgs_job_set_costs`, `higgs_official_plans`,
`provider_official_api`, and `operator_override`.

### 3.2 `model_cost_rules`

One row per independently priced parameter combination:

```sql
CREATE TABLE model_cost_rules (
  id TEXT PRIMARY KEY,
  snapshot_id TEXT NOT NULL REFERENCES pricing_snapshots(id),
  jst TEXT NOT NULL,
  model_alias TEXT NOT NULL DEFAULT '',
  unit TEXT NOT NULL,
  component TEXT NOT NULL DEFAULT '',
  credits_hundredths INTEGER NOT NULL,
  original_credits_hundredths INTEGER NOT NULL DEFAULT 0,
  resolution TEXT NOT NULL DEFAULT '',
  duration_seconds INTEGER NOT NULL DEFAULT 0,
  mode TEXT NOT NULL DEFAULT '',
  audio TEXT NOT NULL DEFAULT '',
  dimensions_json TEXT NOT NULL DEFAULT '{}',
  observed_at TEXT NOT NULL
);
```

`unit` distinguishes `per_request`, `per_second`, and any future billing
basis. `component` preserves whether the value came from `credits`,
`base_credits`, `cost_per_second`, or an audio-state branch. The optional
`original_credits_hundredths` retains the upstream pre-discount/reference
value. `dimensions_json` preserves fields not promoted to first-class columns.

### 3.3 `model_sell_policies`

Operator-owned policy, independent of upstream snapshots:

```sql
CREATE TABLE model_sell_policies (
  model_alias TEXT PRIMARY KEY,
  currency TEXT NOT NULL DEFAULT 'USD',
  markup_pct REAL NOT NULL DEFAULT 1.0,
  minimum_price REAL NOT NULL DEFAULT 0,
  fixed_price REAL,
  rounding_unit REAL NOT NULL DEFAULT 0.001,
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);
```

`fixed_price` wins when present. Otherwise the quote engine applies markup,
minimum price, and rounding to `effective_cost`.

### 3.4 `model_pricing_current`

A view or materialized projection with one row per public model alias. It
contains:

- default quote and the exact default dimensions;
- minimum and maximum known upstream credits;
- current sell price or generated billing expression;
- source snapshot IDs and timestamps;
- completeness state: `complete`, `partial`, `unpriced`, or `stale`.

The existing `ModelSpec.EstCostHundredths` remains an affordability floor. It
must not be presented as a complete sales price.

### 3.5 Internal comparison sources

Migration 023 keeps comparison inputs and decisions separate:

- `higgs_plan_rates`: official monthly plan amount, credits, and USD/credit;
- `official_price_observations`: provider API prices at exact resolution,
  duration, mode, and audio combinations;
- `model_price_decisions`: operator-approved final prices at the same
  granularity.

Money is stored as integer USD micros. Missing final decisions remain empty;
the UI never substitutes a cost or minimum observed price as the sell price.

## 4. Collection Rules

`costsync` must preserve the complete `/job-sets/costs` payload before
reducing it. The current minimum-positive reduction may continue feeding
`EstCostHundredths`, but only as a derived projection.

Phase 1 performs the following sequence on every successful sync:

1. Insert an immutable `pricing_snapshots` row.
2. Normalize every cost variant into `model_cost_rules`.
3. Commit the snapshot and all rules atomically.
4. Swap the in-memory affordability overlay only after persistence succeeds.
5. Emit model/rule counts to logs.

`model_pricing_current`, quote derivation, and pricing metrics are phase 2.

An empty or malformed fetch never replaces the previous active snapshot.

## 5. Quote Contract

The quote engine accepts the public model alias and normalized request
parameters:

```json
{
  "model": "kling-3-turbo",
  "seconds": 5,
  "size": "1280x720",
  "mode": "quality",
  "audio": "off"
}
```

It returns both the upstream cost and the proposed sell price with provenance:

```json
{
  "model": "kling-3-turbo",
  "upstream": {"credits": 7.5, "unit": "per_request"},
  "effective_cost": {"amount": 0.075, "currency": "USD"},
  "sell_price": {"amount": 0.12, "currency": "USD"},
  "dimensions": {"seconds": 5, "resolution": "720p", "audio": "off"},
  "source": {"snapshot_id": "price_...", "observed_at": "2026-07-23T00:00:00Z"}
}
```

Unknown dimensions produce a validation error or a `partial` quote. The quote
engine must never silently fall back to the cheapest known rule.

## 6. Public Endpoints

### `GET /v1/pricing`

Implemented in phase 1 and protected by `sk-hg-*` API-key authentication.
Returns the latest upstream-credit snapshot and every normalized dimension.
The current optional filter is `model` (public alias or JST). The response is
labelled `pricing_scope=upstream_credits_only`; official reference prices,
sell policy, completeness, and USD amounts are not fabricated.

### `POST /v1/pricing/quote`

Returns a parameter-aware quote using the contract above. This is the source
for the future WebUI estimator and operator margin preview.

### `GET /api/pricing`

new-api ratio-sync compatibility response:

```json
{
  "success": true,
  "data": [
    {
      "model_name": "kling-3-turbo",
      "quota_type": 1,
      "model_price": 0.12,
      "billing_mode": "tiered_expr",
      "billing_expr": "v1:..."
    }
  ]
}
```

Rules for publication:

- Truly fixed-price models use `quota_type=1` and `model_price`.
- Parameterized models publish `billing_mode=tiered_expr` and a tested
  `billing_expr` derived from current cost rules and sell policy.
- `partial`, `unpriced`, or `stale` models are omitted by default rather than
  published at an unsafe minimum price.
- The endpoint is read-only and cacheable; it contains sell configuration,
  never account credentials or raw internal plan assignments.

new-api's Sora adaptor currently applies only `seconds` and one size multiplier.
That shortcut cannot represent Higgs mode/audio/resolution matrices, so the
compatibility endpoint must use generated billing expressions for those models.

## 7. Delivery Order

1. ✅ Add snapshot/rule/policy migration and snapshot/rule store.
2. ✅ Change costsync to preserve and normalize raw rules transactionally.
3. ✅ Expose authenticated, upstream-credit-only `GET /v1/pricing`.
4. Add official-plan/API-reference imports, sell-policy store, quote service,
   and completeness validation.
5. Expose `/v1/pricing/quote` and safe `/api/pricing` output.
6. Add operator UI only after API golden tests and new-api sync verification.

## 8. Acceptance Criteria

- Restarting higgsgo does not lose the latest upstream cost snapshot.
- A 5s/10s or 720p/1080p request can resolve to different costs when upstream
  prices differ.
- Every published new-api price identifies its source snapshot and policy.
- No model with incomplete dimensions is exported at the minimum observed cost.
- A golden `/api/pricing` fixture imports successfully through new-api's
  `controller/ratio_sync.go` type-2 parser.
