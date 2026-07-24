# Pricing Downstream Contract

> Status: draft, revised 2026-07-23 after new-api review. Field spec
> that new-api (and any other downstream consumer) MUST follow when
> reading pricing from higgsgo. Complements `PRICING-SOURCE-OF-TRUTH.md`,
> which describes the internal storage and layer semantics.

## 1. Scope

This document is the wire contract for pricing data flowing **higgsgo →
downstream**. It fixes:

- the set of dimension fields that identify a priceable variant,
- which field is the primary billing tier,
- how empty dimensions are interpreted,
- the JSON shape published to downstream (compact) and the richer shape
  the operator UI consumes (admin-only),
- the `billing_expr` DSL new-api uses to route requests to a tier,
- the split between the billing feed (§6.2) and the official-provider
  price feed (§6.4),
- the retail-anchor pricing rule higgsgo enforces server-side (§10).

Downstream consumers key off the fields defined here. If higgsgo needs a
new dimension (e.g. `style`, `voice_id`), it will be added to this
document first; downstream code MUST ignore unknown dimension fields
rather than fail.

## 2. Dimension Fields

Every priceable variant is identified by exactly **five string/int
fields**. There is no hierarchical object — the tuple is flat so it
survives every serializer (JSON, URL query, DB row) without ambiguity.

| Field              | Type    | Meaning                                                                                                                                                            | Empty (`""` / `0`) means                                                                            |
|--------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------|
| `resolution`       | string  | **Primary tier.** Output size class. Values are provider-native strings.                                                                                           | Model is not resolution-tiered (e.g. audio TTS, some plain image models).                           |
| `duration_seconds` | int     | Fixed duration in seconds when the model bills per fixed clip length.                                                                                              | Model bills per-second (`unit=per_second`) or the request length is not the price axis.             |
| `mode`             | string  | **Provider-native quality sub-tier** — see §2.2. Provider-generic; covers Kling `pro`/`std`, gpt-image-2 `low`/`medium`/`high`, FLUX `klein`/`pro`/`flex`/`max`, Imagen `fast`/`standard`/`ultra`. | Model has no quality sub-tier, OR the sub-tier has already been folded into another axis (see §4.1). |
| `audio`            | string  | Audio state for video models. Provider-native values (`off`, `on`, `voice_control`, …).                                                                            | Model has no audio dimension (image, audio-only, pure video).                                       |
| `component`        | string  | **Charge component** for multi-segment billing (e.g. FLUX's `first_mp` starter price + `additional_mp` marginal price + `ref_image` per-input-image add-on). One row per component; final charge is the sum of matching rows. Known values: `first_mp`, `additional_mp`, `ref_image`, `""`.                                                                                                                                                | Model has flat single-component billing (i.e. exactly one row per (resolution, audio, mode) tuple).  |
| `unit`             | string  | Billing basis. One of `per_request`, `per_second`, `per_token`, `per_megapixel`. New units MUST be added here before shipping.                                     | Never empty on a well-formed row. Absence signals a data bug and MUST NOT be published.             |

### 2.1 Resolution values

`resolution` is a string, not an enum, because providers ship different
conventions. Known values that MUST be recognised as-is:

- **Video**: `480p`, `720p`, `1080p`, `2k`, `4k`
- **Image**: `1k`, `1.5k`, `2k`
- **Audio / TTS**: `""` (empty)

Downstream MUST NOT normalize `1080p` → `1920x1080` or `720p` → `HD`;
it forwards the string verbatim. Any translation to a customer-facing
label is downstream UI concern, not billing key concern.

### 2.2 Mode values (provider-native quality sub-tier)

`mode` is provider-generic. It represents the "quality tier" axis when
that axis is orthogonal to `resolution`. Known values that MUST be
recognised as-is:

- **Kling (video)**: `pro`, `std`
- **gpt-image-2 (image)**: `low`, `medium`, `high`
- **FLUX (image)**: `klein`, `pro`, `flex`, `max`
- **Imagen 4 (image)**: `fast`, `standard`, `ultra`
- **Audio / TTS**: `""` (empty)

Downstream MUST NOT normalize these — the strings are provider-native
and preserved verbatim. If a provider adds a new tier, this list is
appended and the DSL keeps working.

### 2.3 Why `resolution` is the primary tier

- All resolution-tiered providers publish their prices as a
  `resolution × …` matrix. Aligning our primary key with theirs keeps
  sync/diff cheap.
- `mode` is a per-request parameter, not a membership tier; it must not
  partition billing rows on its own. On models where `mode` deterministically
  implies a resolution (e.g. Kling `pro` → `1080p`, `std` → `720p`), we
  fold `mode` into `resolution` at fan-out time (§4.1) and publish an
  empty `mode`, so downstream sees exactly one row per resolution.
- The internal `pricing_matrix` endpoint fans out resolution-agnostic
  Higgs cost rules onto every observed resolution row server-side
  (see §4.1), so downstream never needs to know about the fan-out.

### 2.4.a Multi-component billing (FLUX-style stripe pricing)

Some providers publish a **piecewise / stripe** price schedule instead of
a single number per variant. FLUX.2 is the canonical example: for a given
mode (e.g. `[max]`), the customer pays `first_mp` for the first
megapixel, `additional_mp` for each MP beyond that, and `ref_image` per
MP of every input reference image. The final charge is a sum, not a
lookup.

higgsgo represents this by emitting **one downstream row per component**.
Each row carries an identical `(mode, resolution, audio)` tuple and
differs only on the `component` field:

```
mode=max  component=first_mp        unit=per_megapixel  amount_micros=70000
mode=max  component=additional_mp   unit=per_megapixel  amount_micros=30000
mode=max  component=ref_image       unit=per_megapixel  amount_micros=30000
```

Downstream lookup (§5) MUST NOT reject a multi-row match when the extra
axis is `component`; instead it aggregates: given an incoming request
`(output_mp=2, input_mp=1)`, the total charge is
`first_mp × 1 + additional_mp × (2-1) + ref_image × 1`.

UI rendering (this is a downstream concern, not a wire concern): the
primary tab key stays `mode` (§2.4). Each mode tab holds one variant
per component, displayed as three side-by-side pricing lines
("first MP $0.07 · additional MP $0.03 · ref image $0.03/MP"), NOT as
three separate tabs — the customer picks a mode; components are not
independently selectable.

### 2.4 Fallback primary tier for resolution-less models (image / audio)

Some providers do not partition billing by resolution at all. FLUX.2 is
the canonical example: `[max]`, `[pro]`, `[klein]`, `[flex]` are
independent priced variants that share the same underlying compute
axis (megapixels, billed continuously), so `resolution` is meaningless
as a primary tier key. Other examples: gpt-image-2's `low`/`medium`/
`high`, Imagen 4's `fast`/`standard`/`ultra` — when the provider does
not itself expose a discrete resolution axis on top.

When higgsgo publishes rows for such a model, `resolution` is
`""` on every row, `mode` carries the provider-native quality tier
verbatim (§2.2), and **downstream MUST treat `mode` as the primary
tier key**, keyed exactly as it would key `resolution` for a video
model. Specifically:

1. The UI tab label MUST be `mode` verbatim (`max`, `klein`, `pro`, …),
   not translated or grouped.
2. `groupTiersByPrimaryDim`-equivalent logic MUST fall back through the
   priority list `[resolution, mode]` in that order; whichever is
   populated first on a given row is that row's primary key. Video and
   fully-tiered image models keep `resolution` primary; FLUX-style
   models fall through to `mode`.
3. **Both `resolution` and `mode` are never populated on the same
   downstream row.** If they were, §5 lookup would ambiguate the
   primary axis. Producers that would emit both MUST fold one into the
   other server-side (§4.1 for Kling video; a parallel rule applies to
   any future model whose `mode` deterministically implies a
   resolution).
4. If a row has `resolution=""` AND `mode=""` AND `model=""`, it is a
   no-dimension model (§4.2). Downstream renders it as a single flat
   tier labelled `default` — no tab UI.

This is a downstream UI concern; it does NOT change the wire format.
The wire is still the five-tuple in §2. Only the interpretation of
"which field is the tab key" is extended to accept `mode` as fallback
when `resolution` is empty.

## 3. Wire Shapes

There are **two** shapes with clearly separated audiences.

### 3.1 Downstream feed (`/api/pricing`, public / API-key auth)

new-api's existing `controller/ratio_sync.go` type-2 parser
(`PricingItem`) is the target. higgsgo publishes rows using only the
fields that parser understands, plus a `lifecycle` field new-api can
ignore today but adopt later:

```json
{
  "success": true,
  "data": [
    {
      "model_name": "kling-3",
      "quota_type": 2,
      "billing_mode": "tiered_expr",
      "billing_expr": "has(param(\"resolution\"),\"1080p\") ? tier(\"1080p · audio=on\", 168000, \"per_second · duration_seconds=5\") : has(param(\"resolution\"),\"720p\") ? tier(\"720p · audio=on\", 126000, \"per_second · duration_seconds=5\") : tier(\"unpriced\", 0, \"no matching variant\")",
      "lifecycle": { "status": "active" }
    }
  ]
}
```

Rules:

- **`model_name`** is the higgsgo public alias (matches `/v1/models`).
- **`quota_type`** is always `2` when a model has any pricing dimensions.
  quota_type=1 (`model_price`) is reserved for models with no dimensions
  AND a single tier; even then, higgsgo emits quota_type=2 so downstream
  never has to switch parsers, avoiding the "per 500K tokens" unit
  ambiguity of `model_price`.
- **`billing_expr`** is a single string in the DSL specified in §6. It
  is version-implicit (parser must be v1-compatible); a future v2 is
  gated on a coordinated deploy.
- **`lifecycle`** is optional; missing → treat as `{"status":"active"}`
  (see §7).
- Aliases with zero priced rows are **omitted** from `data`. higgsgo
  does not synthesize a sell price from cost.
- **`provenance` is never present in this shape.** Rationale strings,
  cost inputs, margin notes, decision IDs — none of it. They are
  admin-only per §3.3.

### 3.2 Wire shape of one priced row (canonical, for reference)

The DSL in §6 is generated from rows of this shape. This is the
canonical struct used inside higgsgo and in the admin endpoint (§6.3).
Downstream does not consume this JSON directly — it consumes the DSL —
but the field names below are the source of truth for what the DSL
`param(...)` accessors look up.

```json
{
  "model_alias": "kling-3",
  "jst": "kling3_0",
  "dimensions": {
    "resolution": "720p",
    "duration_seconds": 5,
    "mode": "",
    "audio": "on",
    "unit": "per_second"
  },
  "sell_price": {
    "currency": "USD",
    "amount_micros": 630000
  }
}
```

- **`amount_micros`** is authoritative (integer USD × 1e6). No floats
  cross the wire in this canonical form. `$0.63 = 630000`.
- **`dimensions`** always has all five fields present, using the empty
  sentinels from §2 for absent axes.

### 3.3 Admin shape (`/admin/models/{alias}/pricing-matrix`, bearer auth)

Same row structure as §3.2 but with a `provenance` block appended for
each priced row:

```json
{
  "provenance": {
    "decision_id": "prc_01J...",
    "decided_at": "2026-07-23T09:12:11Z",
    "rationale": "40% margin over Kling 720p std/on"
  }
}
```

`provenance.rationale` is operator free-text. It may reference cost,
margin, upstream prices, or competitor pricing. **It MUST NOT leak
into any endpoint that ordinary users can reach.** Downstream services
that receive both the admin matrix and the public feed MUST NOT copy
`provenance` into user-visible surfaces (support widgets, invoices,
usage dashboards). The safe rule: only the operator UI (bearer-scoped
`/admin/*`) renders `rationale`.

## 4. Special Cases

### 4.1 Resolution-agnostic upstream + `mode` folding (kling3_0)

Kling 3.0's `/job-sets/costs` payload publishes credits by `mode × audio`
with `resolution` unset, while the official Kling API prices are per
`resolution × audio`. On this model:

1. higgsgo maps `mode=pro → resolution=1080p` and `mode=std → resolution=720p`
   using the Kling upstream documentation (this mapping is stored server-side
   and versioned alongside the model).
2. It then fans the Higgs cost out onto each `(resolution, audio)` cell of
   the official-API grid.
3. **On every downstream row for `kling3_0`, `mode` MUST be `""`.**
   The `mode` axis has been consumed by `resolution`; leaving both
   populated would create ambiguity in §5's matcher.

Concrete example — a `kling-3` payload downstream receives:

```
resolution=720p  audio=off  unit=per_second   mode=""  → row A
resolution=720p  audio=on   unit=per_second   mode=""  → row B
resolution=1080p audio=off  unit=per_second   mode=""  → row C
resolution=1080p audio=on   unit=per_second   mode=""  → row D
resolution=4k    audio=off  unit=per_second   mode=""  → row E
resolution=4k    audio=on   unit=per_second   mode=""  → row F
```

Other models where `mode` is orthogonal to `resolution` (gpt-image-2's
`low/medium/high`, FLUX's `klein/pro/flex/max`) keep `mode` populated
because it does not deterministically imply a resolution.

### 4.2 No-dimension models (qwen-audio-tts)

For models with no resolution / no audio / no mode / no duration, all
four dimension axes are empty and `unit` describes the billing basis
(`per_token` for TTS). Exactly one row is published per alias, and the
generated DSL degenerates to `tier("default", <micros>, "per_token")`
without any `has(param(...))` guard.

### 4.3 Mixed audio coverage / partial priced grid

When Higgs does not publish credits for every audio value the official
API supports (e.g. Kling `voice_control`), the variant still exists in
the internal matrix but has no operator decision → the row is **omitted
from the downstream feed**. Additionally, higgsgo emits a
`partial_coverage` warning log tagged with the model alias and missing
tuple so ops can follow up. `docs/OPERATIONS.md` will point at this log
for the pricing runbook.

### 4.4 `per_second` billing → fixed-duration tiers

new-api's tier DSL uses `tier(label, fixedPrice, note)` where `fixedPrice`
is the **total** charge for that tier, not a per-second rate. higgsgo
therefore expands each `unit=per_second` row into one tier per observed
duration:

- For each priced `(resolution, audio, mode)` variant with
  `unit=per_second`, higgsgo emits N tiers where N is the set of
  supported durations for that model (e.g. `{5, 10}` for kling-3 as of
  2026-07-23).
- Each emitted tier's `fixedPrice` = `amount_micros × duration_seconds`,
  still expressed in USD micros integer.
- `duration_seconds` is carried in the tier's `note` string so
  new-api's front-end (`parseTierExpr` / `groupTiersByPrimaryDim`) can
  present it under the resolution primary tab.

This is the pragmatic compromise called out in the review: no new-api
backend changes required. When new-api's billing engine grows a native
`per_second` branch, we can revisit and stop pre-expanding.

## 5. Downstream Lookup Algorithm

Given a request `(model, resolution?, audio?, mode?, seconds?)`,
downstream resolves the row like this:

1. Filter feed rows where `model_name == request.model`.
2. Match on the flat tuple `(resolution, audio, mode, unit)` using
   **exact string equality on both sides**. Empty string on the request
   side matches ONLY rows with empty string on that axis; it is NOT a
   wildcard. This keeps the semantics single-valued: a client that
   omits `mode` on gpt-image-2 will not accidentally match `mode=high`.
   `duration_seconds` matches when `unit=per_request`, otherwise it
   selects which pre-expanded tier fires (see §4.4).
3. If exactly one row matches → use its `amount_micros` (or, in DSL
   form, the tier's `fixedPrice`).
4. If multiple rows match → the request was underspecified. Downstream
   returns a 400 to its caller. This is by construction impossible when
   the feed is well-formed per §4.1's `mode`-folding rule.
5. If zero rows match → either a new variant not yet priced or a spec
   mismatch. Downstream returns 402 (or its equivalent) AND logs the
   unmatched tuple so ops can add the decision.

The "no silent fallback" rule mirrors §5 of PRICING-SOURCE-OF-TRUTH.md
and is non-negotiable.

## 6. Endpoints

### 6.1 `GET /v1/pricing` (raw upstream credits, informational)

Current shape (unchanged). `pricing_scope=upstream_credits_only`.
Downstream MUST NOT use this to derive a sell price — it is the raw
input layer, not the published-price layer.

### 6.2 `GET /api/pricing` (downstream feed, new-api compatible)

Returns the shape from §3.1. Compatible with new-api's existing
`controller/ratio_sync.go` `PricingItem` decoder — no schema changes on
the new-api side required.

**Semantics: retail anchor for downstream's list-price field.** The
`fixed_micros` higgsgo emits here is the **retail price at the default
1× ratio** — this is the value new-api (or any equivalent downstream)
plugs directly into its `model_pricing` / list-price field. It carries
our internally-decided margin over Higgs cost (see §10) and MUST NOT
be interpreted as either a promotional price or a wholesale cost.

Group / tier ratios on the downstream side are **out of higgsgo's
scope**. Whatever the downstream operator configures — 0.3× loss-leader
group, 1.5× premium group, anything in between — higgsgo does not see,
validate, constrain, or care. The actual customer charge is
`fixed_micros × group_ratio`, computed entirely downstream.

Discount % is a **derived value**, not a wire field. If downstream
wants to render "N% off" anywhere in its UI, it computes it locally
from the data it already has (`(fixed_micros × group_ratio) ÷
official_api_price_micros`, or any other basis it chooses). higgsgo
never emits a `discount`, `savings`, `discount_percent`, or comparable
derived field, in this endpoint or in `/api/pricing/official-api`.
Adding one is explicitly out of scope (§9).

DSL grammar (v1, implicit — no `v1:` prefix, because
`PricingItem.BillingExpr` is a plain string and prefixing would break
the current parser):

```
expr    := ternary | tier_call
ternary := guard "?" tier_call ":" expr
guard   := "has(" param_ref "," string ")"
        |  "has(" param_ref "," string ")" " && " guard
        |  guard " || " guard
param_ref := "param(" string ")"          // string is a dimension field name
tier_call := "tier(" string "," integer "," string ")"
              //     label   fixed_micros    note
```

- `param("resolution")`, `param("audio")`, `param("mode")` are the only
  request-side accessors used in v1. new-api's parser exposes these
  from the incoming customer request.
- `fixed_micros` is USD × 1e6, integer. new-api converts to its own
  quota unit at charge time; it is NOT `model_price` and MUST NOT be
  interpreted as "USD per 500K tokens".
- `label` is a short human-readable identifier used by new-api's
  front-end as the tier tab name (`groupTiersByPrimaryDim` picks the
  first `resolution=…` fragment).
- `note` carries the remaining dimension context as a `k=v · k=v`
  string. Free-form for display; downstream MUST NOT parse it back into
  billing logic (use the DSL guards for that).

Fallback tier: every generated `billing_expr` ends with
`tier("unpriced", 0, "no matching variant")` so the DSL evaluator never
falls off the end. §5 still requires downstream to treat the resulting
`fixedPrice=0` as "unmatched", not "free" — the `label="unpriced"` and
`note="no matching variant"` are the machine-readable signal.

### 6.3 `GET /admin/models/{alias}/pricing-matrix` (operator only)

The four-layer comparison view (Higgs credits, Higgs plan cost, official
API, final price) plus `provenance` (§3.3). Bearer-authenticated. Not
part of the downstream contract; it is an operator tool. Field names
overlap with §3.2 but the semantics differ (it exposes cost inputs and
operator rationale, not sell prices).

### 6.4 `GET /api/pricing/official-api` (official provider prices, informational)

Separate endpoint that publishes third-party provider list prices —
the raw official / public API price each model would cost if the
customer went to the provider directly. Purely a data pipe: higgsgo
records what it sees on providers' public pricing pages and forwards
it unmodified.

What downstream does with it is downstream's business. Concretely,
new-api uses it as the source for its model-price-adapter (the
"official reference" column of its model catalog); any UI, discount,
or comparison rendering is derived downstream from this raw value and
higgsgo takes no position on the framing.

Distinct from §6.2:

- §6.2 (`/api/pricing`) is the **billing feed**. Downstream writes it
  into its `model_pricing` list-price field. High frequency.
- §6.4 (`/api/pricing/official-api`) is the **official-price feed**.
  Raw provider prices, no derived fields. Low frequency.

Wired independently, the two syncs mirror new-api's existing
`ratio_sync` vs. `model_list_sync` split: billing MUST stay simple and
fast; the official-price feed is cacheable, independently failable, and
never on the request path. If `/api/pricing/official-api` is stale or
5xx, billing continues undisturbed — the downstream consumer just
doesn't have the reference data for that window.

Shape:

```json
{
  "success": true,
  "generated_at": "2026-07-23T10:00:00Z",
  "data": [
    {
      "model_name": "kling-3",
      "jst": "kling3_0",
      "references": [
        {
          "provider": "Kling Official",
          "resolution": "1080p",
          "audio": "on",
          "mode": "",
          "unit": "per_second",
          "duration_seconds": 0,
          "amount_micros": 168000,
          "currency": "USD",
          "source_url": "https://docs.kling.com/pricing",
          "observed_at": "2026-07-20T00:00:00Z"
        },
        {
          "provider": "Kling Official",
          "resolution": "720p",
          "audio": "on",
          "mode": "",
          "unit": "per_second",
          "duration_seconds": 0,
          "amount_micros": 126000,
          "currency": "USD",
          "source_url": "https://docs.kling.com/pricing",
          "observed_at": "2026-07-20T00:00:00Z"
        }
      ]
    }
  ]
}
```

Rules:

- Each entry is keyed by the same `(resolution, audio, mode, unit,
  duration_seconds)` tuple as §3.2 so downstream can join it against
  `/api/pricing` tiers with a straight tuple compare.
- `amount_micros` is USD × 1e6, integer — same convention as §3.
- Multiple `provider` entries per model are allowed (Kling official +
  Kuaishou reseller + our internal Higgs estimate, if we ever want to
  publish it). Downstream is expected to pick which to display.
- `observed_at` is the timestamp we captured the provider's public
  price. Downstream SHOULD show a "last verified {n} days ago" note
  and treat entries older than 90 days as stale.
- Models with zero references are omitted from `data`.
- Cache-friendly: this endpoint changes only when the operator imports
  a new `official_price_observations` batch. Downstream may cache
  aggressively (recommend 6-24 h TTL).

## 7. Lifecycle

Every downstream row MAY carry a `lifecycle` object at the model level
(not per row):

```json
"lifecycle": {
  "status": "active" | "deprecated" | "sunset",
  "sunset_at": "2026-12-31T00:00:00Z"
}
```

- `active` — default. Missing `lifecycle` is treated as `active`.
- `deprecated` — model still bills normally, but downstream SHOULD
  surface a warning ("this model is deprecated; migrate to …") and
  MUST NOT let new users create API keys scoped only to it.
- `sunset` — model will stop responding on `sunset_at`. Downstream MUST
  refuse new requests after that timestamp; existing keys degrade to
  402 the same way an unpriced variant does.

`sunset_at` is RFC 3339. Absent when `status ∈ {active, deprecated}`.

## 8. Compatibility & Versioning

- **Additive changes** (backwards compatible):
  - New `dimensions` keys — downstream MUST tolerate unknown keys.
  - New `mode` / `resolution` / `audio` string values — appended to
    §2's known-values lists; downstream forwards verbatim.
  - New `lifecycle.status` values (e.g. `preview`, `retired`) —
    downstream MUST treat unknown statuses as `deprecated` (fail
    conservatively).
  - New optional top-level fields on §3.1 rows — downstream MUST ignore
    unknown fields.
  - New optional fields on §6.4 `references` entries (e.g. `notes`,
    `region`) — downstream MUST ignore unknown fields.
- **Breaking changes** (coordinated deploy required):
  - Removing or repurposing a `dimensions` field.
  - Changing the empty-string semantics of `resolution` / `mode` / `audio`.
  - Changing `amount_micros` scaling or currency.
  - Changing the DSL grammar in §6.2 (`ternary` shape, `tier` arity,
    accessor names).
  A breaking change bumps the DSL to v2 and adds an explicit `v2:`
  prefix on `billing_expr`. Old readers reject; new readers accept both.
- **Not versioned**: adding a new `unit` value (e.g. `per_minute`).
  Downstream MUST reject rows with an unrecognised unit rather than
  guess — but this is enforced at row level, not via version bump.

## 9. Non-Goals

- **higgsgo never publishes promotional / discount prices.** The value
  on `/api/pricing` is the retail anchor at the default 1× ratio (§10).
  Promotions, per-group ratios, per-user coupons, seasonal campaigns —
  all downstream's responsibility. This is intentional: the wire value
  is a stable cost-anchored number, not a time-varying signal, so it
  can be pulled at any cadence without coordination between higgsgo
  and its consumers.
- **higgsgo does not emit derived comparison fields.** No `discount`,
  `savings`, `discount_percent`, `list_price`, `strikethrough_price`,
  or any other field that positions the wire value against an
  alternative. If a downstream consumer wants such a field, it derives
  it locally from the data it already has (`fixed_micros`, the
  `amount_micros` from `/api/pricing/official-api`, its own group
  ratio). The wire contract stays narrow: it says what the current
  price is, nothing about how that price should be framed.
- **higgsgo does not model or constrain downstream group ratios.**
  A downstream operator may run a loss-leader group whose effective
  price falls below Higgs cost (e.g. a "特价" / free-tier group used
  for user acquisition), or a "premium / official-routed" group whose
  effective price sits above retail. Both are legitimate business
  decisions on the downstream side; the retail anchor higgsgo
  publishes is what those ratios are applied to, not a floor on the
  final customer charge.
- This contract does not describe how higgsgo *computes* the retail
  price (that is the operator's job via the Pricing WebUI /
  `POST /admin/models/{alias}/pricing-decisions`; the pricing rule
  lives in §10 and `docs/OPERATIONS.md`).
- It does not describe internal storage columns (see
  PRICING-SOURCE-OF-TRUTH.md §3).
- It does not include real-time balance, unlim/free semantics, or
  actual-cost reconciliation. **`/api/pricing` always returns a
  `sell_price` regardless of the caller's plan or unlim status.**
  Whether a specific request is billed is a runtime account-state
  decision made downstream, not a pricing-config decision made by
  higgsgo.

## 10. Retail Pricing Rule (higgs-internal)

This section is normative for the operator UI and for anyone importing
`model_price_decisions` via script. It is informational for downstream:
downstream reads the resulting `fixed_micros` — it does not need to
re-check the floor.

**Rule**: for every priced variant,

```
retail_price ≥ higgs_cost × 1.8         (soft floor, cost markup ≥ 80%)
retail_price ≈ higgs_cost × 1.9-2.0     (recommended, cost markup 90-100%)
retail_price <  higgs_cost × 1.8        (accepted with warning; ops alert)
```

The floor is **advisory, not enforced**: `POST
/admin/models/{alias}/pricing-decisions` returns `201 Created` with the
row written, plus a `warnings[]` entry (`code: "retail_below_floor"`)
carrying the computed floor, the retail value, and the reference unit
cost that produced them. Operators see the warning inline in the
Pricing WebUI and can either revise the price or accept the risk. The
row is durable either way — the warning is a signal, not a gate.

Enforcement is deliberately soft because higgs_cost is not a fixed
number. Three cost bases exist in parallel:

| Layer | Source | Stability |
|---|---|---|
| Official list | `higgs_plan_rates` (migration 023 seed) | Static, most expensive |
| Official promo | 8% common discount + short-window 48-61% off promos | Semi-static; not persisted |
| Channel purchase | Operator batch buys, cheaper than list | Dynamic, batch-dependent |

Concrete channel-purchase medians as of 2026-07 (operator-supplied):

| Bucket | credits | USD | unit_cost (micros/cr) |
|---|---|---|---|
| starter | 200 | $5.5 | 27_500 |
| plus | 1_000 | $13.5 | 13_500 |
| ultra-3000 | 3_000 | $50 | 16_667 |
| ultra-9000 | 9_000 | $95 | 10_556 |
| ultra-18000 | 18_000 | $170 | 9_444 |
| ultra-59000 | 59_000 | $510 | 8_644 |
| — | — | median | **12_028** |

`higgs_cost` in phase 1 is `variant_credits × reference_unit_cost`
where `reference_unit_cost` is a config value
(`[pricing] floor_reference_unit_cost_micros`, default **27_500** =
starter channel, most conservative). Operators tune it to the
channel-buy mix that dominates their pool. Phase 2 will replace the
config with a `higgs_plan_purchase_batches` log so the effective unit
cost moves as batch prices drift; the warning contract stays the same.

Worked example (higgs_cost = $0.05):

- $0.10 retail → markup 100%
- $0.095 retail → markup 90%, recommended
- $0.090 retail → markup 80%, soft floor (no warning)
- $0.080 retail → **accepted with warning**; POST returns 201 +
  `warnings: [{ code: "retail_below_floor", ... }]` and trips an ops
  alert

Note the "markup vs margin" gotcha: the rule is written in **cost
markup** terms (`(retail − cost) / cost`). The equivalent gross-margin
figure is smaller (`(retail − cost) / retail` — 80% markup ≈ 44%
margin). We standardise on markup here because it maps directly to the
multiplier operators actually type in.

The check runs server-side at `POST
/admin/models/{alias}/pricing-decisions`. The row is always written;
warnings are returned in the response body and mirrored in the Pricing
WebUI's EditModal. `docs/OPERATIONS.md` carries the ops-side runbook
for `retail_below_floor` review cadence.

**Why this floor exists.** The floor protects the *anchor itself*, not
the customer's final charge. Downstream group ratios can (and will) go
in both directions — some groups will bill above retail, some below.
Those are downstream business decisions and out of scope here (§9). By
locking retail ≥ 1.8× Higgs cost, higgsgo guarantees only one thing:
the wire value it publishes is provably above its own cost basis. That
is what makes the number safe to use as an anchor at all. What each
downstream group does with the anchor after multiplying its own ratio
is not something higgsgo needs to gate or reason about.

