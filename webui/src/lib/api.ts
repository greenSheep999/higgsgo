// Typed fetch wrapper for the higgsgo admin surface.
//
// Every request goes through `request()` so we get one place to attach the
// bearer, one place to normalise error responses, and one place to unwrap
// `{data: […]}` envelopes. Endpoints return the exact JSON shape the Go
// handlers write — see internal/api/admin/*.go for authoritative field
// definitions.

import { getBearer, clearBearer } from "./auth-store";

const BASE = ""; // same-origin in dev (via vite proxy) and prod (embedded).

// ApiError mirrors the {error:{type,message}} envelope the Go handlers
// return. Non-JSON responses (network fails, HTML error pages) surface as
// `type = "network"` so query code doesn't have to sniff.
export class ApiError extends Error {
  status: number;
  type: string;
  constructor(status: number, type: string, message: string) {
    super(message);
    this.status = status;
    this.type = type;
  }
}

async function request<T>(
  path: string,
  init: RequestInit & { unwrap?: boolean } = {},
): Promise<T> {
  const bearer = getBearer();
  const headers = new Headers(init.headers);
  if (bearer) headers.set("Authorization", `Bearer ${bearer}`);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const res = await fetch(BASE + path, { ...init, headers });
  const text = await res.text();
  const json = text ? safeParse(text) : null;
  if (!res.ok) {
    if (res.status === 401) clearBearer();
    const err = json?.error ?? {};
    throw new ApiError(
      res.status,
      err.type ?? "http_error",
      err.message ?? `HTTP ${res.status}`,
    );
  }
  if (init.unwrap && json && typeof json === "object" && "data" in json) {
    return json.data as T;
  }
  return json as T;
}

function safeParse(text: string): any {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

// ---------- Accounts ------------------------------------------------------

export interface Account {
  id: string;
  email: string;
  workspace_id: string;
  plan_type: string;
  has_unlim: boolean;
  has_flex_unlim: boolean;
  is_pro_veo3: boolean;
  cohort: string;
  subscription_balance: number; // credits × 100
  credits_balance: number;
  total_plan_credits: number;
  status: "active" | "suspended" | "banned" | string;
  in_flight_jobs: number;
  fail_streak: number;
  bound_proxy_url: string;
  priority: number;
  max_concurrent: number;
  note: string;
  source: string;
  plan_ends_at?: string;
  last_balance_at?: string;
  last_used_at?: string;
  last_failed_at?: string;
  registered_at?: string;
  imported_at?: string;
}

export interface PatchAccountRequest {
  priority?: number;
  bound_proxy_url?: string;
  max_concurrent?: number;
  note?: string;
  source?: string;
}

// ProbeAccountResponse mirrors POST /admin/accounts/{id}/probe. The
// backend returns 200 for both successful and failed probes — the ok
// bool discriminates. This keeps error-path UI code identical to
// success-path UI code (no throw / catch, no HTTP-status switching).
// See docs/ROADMAP.md P2-6.
export interface ProbeAccountResponse {
  account_id: string;
  ok: boolean;
  latency_ms: number;
  balance?: {
    workspace_id: string;
    subscription_hundredths: number;
    credits_hundredths: number;
  };
  error?: {
    // Coarse category — the WebUI branches on this for icon / color
    // choice. Message carries the human-readable detail.
    kind:
      | "unauthorized"
      | "forbidden"
      | "rate_limit"
      | "upstream_5xx"
      | "timeout"
      | "network"
      | "internal";
    message: string;
  };
  ts: string;
}

export interface AccountFilter {
  plan_type?: string;
  status?: string;
  min_balance?: number;
}

// ---------- API keys ------------------------------------------------------

export interface ApiKey {
  id: string;
  name: string;
  created_by: string;
  status: string;
  monthly_quota: number;
  monthly_used: number;
  markup_pct: number;
  created_at: string;
  playground_scope: "none" | "cheap" | "full" | string;
  kind: "default" | "project" | string;
  key_last4: string; // last 4 chars of plaintext for masked display
  last_used_at?: string;
}

export interface CreateKeyRequest {
  name: string;
  created_by?: string;
  monthly_quota?: number;
  markup_pct?: number;
  playground_scope?: "none" | "cheap" | "full";
  kind?: "default" | "project";
}

// ---------- Pool stats ----------------------------------------------------

export interface PoolStats {
  total: number;
  by_plan: Record<string, number>;
  by_status: Record<string, number>;
  total_subscription_balance: number;
  with_unlim: number;
  with_flex_unlim: number;
}

// ---------- Jobs ----------------------------------------------------------

export interface Job {
  id: string;
  api_key_id?: string;
  cpa_partner_id?: string;
  group_id?: string;
  account_id: string;
  model_alias: string;
  jst: string;
  endpoint: string;
  status: string;
  upstream_job_id?: string;
  upstream_cost?: number;
  result_url?: string;
  latency_ms?: number;
  poll_count?: number;
  pre_balance_h?: number;
  actual_credits_h?: number;
  charged_credits_h?: number;
  refunded?: boolean;
  request_ts: string;
  finished_at?: string;
  error?: { type: string; message: string };
  callback_url?: string;
}

export interface JobFilter {
  status?: string;
  account_id?: string;
  api_key_id?: string;
  group_id?: string;
  model_alias?: string;
  since?: string;
  until?: string;
  limit?: number;
  offset?: number;
}

// ---------- Usage list ---------------------------------------------------

export interface UsageEvent {
  id: string;
  ts: string;
  api_key_id?: string;
  cpa_partner_id?: string;
  group_id?: string;
  account_id: string;
  model_alias: string;
  jst: string;
  media_type: string;
  upstream_cost?: number;
  actual_credits_h: number;
  charged_credits_h: number;
  markup_pct: number;
  status: string;
  latency_ms?: number;
  poll_count?: number;
  error_type?: string;
  higgsgo_job_id: string;
  upstream_job_id?: string;
  result_url?: string;
  billing_month: string;
  billing_day: string;
}

// ---------- Model health -------------------------------------------------

export interface ModelHealthRow {
  jst: string;
  checked_at: string;
  verdict: string;
  http_status?: number;
  cost?: number;
  poll_time_sec?: number;
  uptime_pct?: number | null;
}

// ModelHealthSlotsResponse is the shape of GET
// /admin/model-health/{jst}/slots. See docs/ROADMAP.md P3-13.
//
// slots is oldest-first — the frontend iterates left-to-right without
// reversing. Buckets with total=0 mean "no probe hit this window";
// the UptimeBar renders those in muted gray with a "No data" tooltip.
export interface ModelHealthSlot {
  time: string; // RFC3339 UTC
  total: number;
  passed: number;
}

export interface ModelHealthSlotsResponse {
  jst: string;
  count: number;
  slot_sec: number;
  slots: ModelHealthSlot[];
}

// ---------- Audit --------------------------------------------------------

export interface AuditEvent {
  id: string;
  ts: string;
  actor: string;
  method: string;
  path: string;
  route: string;
  status: number;
  resource_type: string;
  resource_id: string;
  body_hash: string;
  error_detail?: string;
}

export interface AuditFilter {
  since?: string;
  until?: string;
  actor?: string;
  resource_type?: string;
  resource_id?: string;
  method?: string;
  limit?: number;
  offset?: number;
}

// ---------- Playground ---------------------------------------------------

export interface PlaygroundModel {
  id: string;
  output: "image" | "video" | string;
  jst: string;
  est_cost: number;
  required_params: string[];
  // Compact JSON of a working request body captured from a completed
  // upstream job. When present, ParamForm can seed defaults for every
  // param the model expects (seed, style_id, aspect_ratio, ...) instead
  // of leaving them blank. Empty string when the model has no template.
  example_body_json?: string;
  // Optional enum-valued params (param name -> allowed values). Sourced
  // from the on-disk catalogs referenced by the body template's
  // catalogRefs. Missing entries mean the SPA falls back to free-text
  // input for that param.
  enums?: Record<string, string[]>;
  unstable: boolean;
  requires_paid: boolean;
  requires_unlim: boolean;
  requires_ultra: boolean;
  starter_locked: boolean;
  allowed: boolean;
  blocked_reason?: string;
}

export interface PlaygroundModelsResponse {
  object: string;
  data: PlaygroundModel[];
  scope: "cheap" | "full" | string;
  total: number;
}

// Response shape of GET /v1/models — the public catalog. Fewer
// fields than PlaygroundModel because there's no per-caller
// eligibility to expose here.
export interface PublicModel {
  id: string;
  output: string;
  jst: string;
  // Upstream request routing hints — surfaced so a downstream
  // aggregator (new-api) or an operator inspecting the detail sheet
  // can see the exact HTTP path higgsgo will hit + which endpoint
  // family (v1 hyphenated / v2 snake_case /v2) it belongs to.
  endpoint: string;
  version: string;
  // Present only on models that take a user-supplied image / video.
  media_role: string;
  // Populated only for the nano_banana_2 family.
  app_slug: string;
  // Serialized JSON of a working request body captured from a
  // completed job. Empty when the dump script didn't record one.
  example_body_json: string;
  // Optional enum-valued params (param name -> allowed values).
  // Sourced from body-templates + catalogs; empty when the model has no
  // catalog-backed params.
  enums?: Record<string, string[]>;
  est_cost: number;
  unstable: boolean;
  requires_paid: boolean;
  requires_unlim: boolean;
  requires_ultra: boolean;
  starter_locked: boolean;
  required_params: string[];
  tier_source: string;
  min_credits_hundredths: number;
  // Model-override extension surface (migration 015). extra_aliases
  // is always an array (never null); a downstream aggregator reads
  // it to register the model under additional public names without
  // higgsgo running its own reverse proxy. `note` is a free-form
  // operator memo attached to the override.
  extra_aliases: string[];
  note: string;
  // Derived by the registry loader from the four requires_* /
  // starter_locked flags — the *minimum plan tier* that satisfies
  // the model's gate, expressed as a higgsfield plan name (`""` /
  // `free` / `starter` / `basic` / `pro` / `plus` / `ultimate` /
  // `team` / `ultra` / `scale` / `creator` / `enterprise`).
  min_plan: string;
  // Operational classification labels rendered separately from
  // min_plan. Stable slugs the UI maps to tone + label — see
  // `models.tags.*` in webui/src/locales/en.ts.
  tags: string[];
  // Highest resolution the model can emit (short form: "1080p",
  // "4k", "1024x1024", ...). Empty when unknown.
  max_resolution: string;
  // Longest duration the model can produce, in seconds. 0 when
  // unknown or when duration is not applicable (image models).
  max_duration_sec: number;
}

// ModelOverride mirrors one row of the model_overrides table. Pointer-
// style nullability: null = "inherit from spec"; explicit value =
// override the spec default. The wire uses `null` in place of Go's
// pointer-nil so branching on presence vs value is unambiguous.
export interface ModelOverride {
  alias: string;
  starter_locked: boolean | null;
  requires_paid: boolean | null;
  requires_ultra: boolean | null;
  requires_unlim: boolean | null;
  min_credits_hundredths: number | null;
  extra_aliases: string[];
  note: string;
  updated_at?: string;
}

// ModelOverridePatch is the PUT body — every field is optional; omit
// to leave unchanged, pass explicit null to clear.
export interface ModelOverridePatch {
  starter_locked?: boolean | null;
  requires_paid?: boolean | null;
  requires_ultra?: boolean | null;
  requires_unlim?: boolean | null;
  min_credits_hundredths?: number | null;
  extra_aliases?: string[];
  note?: string;
}
export interface PublicModelsResponse {
  object: string;
  data: PublicModel[];
  limit: number;
  offset: number;
  total_before_pagination: number;
}

export interface PlaygroundEstimate {
  model_alias: string;
  output: string;
  cost_credits_h: number;
  cost_credits: number;
  needs_paid: boolean;
  needs_ultra: boolean;
  will_charge: boolean;
}

// ---------- Key stats & eligibility --------------------------------------

export interface KeyStats {
  since: string;
  window: string;
  request_count: number;
  completed_count: number;
  failed_count: number;
  refunded_count: number;
  total_credits_h: number;
  charged_credits_h: number;
  by_model: {
    model_alias: string;
    request_count: number;
    charged_credits_h: number;
  }[];
}

export interface AccountEligibleModels {
  account_id: string;
  total: number;
  eligible: number;
  by_output: { image: number; video: number; audio: number };
  data: {
    alias: string;
    jst: string;
    output: string;
    est_cost: number;
    unstable: boolean;
  }[];
}

// ---------- Endpoint bindings --------------------------------------------

export interface CreateGroupRequest {
  name: string;
  description?: string;
  max_concurrent_jobs?: number;
  max_concurrent_per_account?: number;
  monthly_credit_budget?: number;
  allowed_models_regex?: string;
  blocked_models_regex?: string;
  route_strategy?: string;
  owner_type?: string;
  owner_id?: string;
}

export type UpdateGroupRequest = Partial<CreateGroupRequest> & {
  status?: string;
};

export const admin = {
  poolStats: () => request<PoolStats>("/admin/stats/pool"),

  listAccounts: (filter: AccountFilter = {}) => {
    const q = new URLSearchParams();
    if (filter.plan_type) q.set("plan_type", filter.plan_type);
    if (filter.status) q.set("status", filter.status);
    if (filter.min_balance != null)
      q.set("min_balance", String(filter.min_balance));
    const suffix = q.toString();
    return request<Account[]>(
      `/admin/accounts${suffix ? "?" + suffix : ""}`,
      { unwrap: true },
    );
  },
  getAccount: (id: string) => request<Account>(`/admin/accounts/${id}`),
  pauseAccount: (id: string) =>
    request<{ id: string; status: string }>(`/admin/accounts/${id}/pause`, {
      method: "POST",
    }),
  resumeAccount: (id: string) =>
    request<{ id: string; status: string }>(`/admin/accounts/${id}/resume`, {
      method: "POST",
    }),
  deleteAccount: (id: string) =>
    request<{ id: string; status: string }>(`/admin/accounts/${id}`, {
      method: "DELETE",
    }),
  patchAccount: (id: string, patch: PatchAccountRequest) =>
    request<Account>(`/admin/accounts/${id}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),
  // probeAccount actively pings the account through the upstream
  // client (JWT mint + per-account proxy + TLS fingerprint). Returns
  // 200 with ok=false + a classified error on upstream failure — so
  // the UI can render success and failure with the same code path
  // instead of a try/catch. See docs/ROADMAP.md P2-6.
  //
  // Failure kinds: "unauthorized" | "forbidden" | "rate_limit" |
  //                "upstream_5xx" | "timeout" | "network" | "internal"
  probeAccount: (id: string) =>
    request<ProbeAccountResponse>(`/admin/accounts/${id}/probe`, {
      method: "POST",
    }),
  importAccounts: (body: unknown) =>
    request<{ imported: number; skipped?: number }>("/admin/accounts", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  listKeys: () => request<ApiKey[]>("/admin/keys", { unwrap: true }),
  createKey: (req: CreateKeyRequest) =>
    request<ApiKey & { plaintext_key: string; display_hint: string }>(
      "/admin/keys",
      { method: "POST", body: JSON.stringify(req) },
    ),
  rotateKey: (id: string) =>
    request<{ id: string; key: string; display_hint: string }>(
      `/admin/keys/${id}/rotate`,
      { method: "POST" },
    ),
  revokeKey: (id: string) =>
    request<{ id: string }>(`/admin/keys/${id}`, { method: "DELETE" }),
  pauseKey: (id: string) =>
    request<{ id: string; status: string }>(`/admin/keys/${id}/pause`, {
      method: "POST",
    }),
  resumeKey: (id: string) =>
    request<{ id: string; status: string }>(`/admin/keys/${id}/resume`, {
      method: "POST",
    }),
  resetKeyUsage: (id: string) =>
    request<{ id: string }>(`/admin/keys/${id}/reset_usage`, {
      method: "POST",
    }),
  updatePlaygroundScope: (id: string, scope: "none" | "cheap" | "full") =>
    request<{ id: string; playground_scope: string }>(
      `/admin/keys/${id}/playground_scope`,
      { method: "POST", body: JSON.stringify({ scope }) },
    ),
  patchKey: (
    id: string,
    patch: { name?: string; monthly_quota?: number; markup_pct?: number },
  ) =>
    request<ApiKey>(`/admin/keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),
  keyStats: (id: string, since?: string) => {
    const q = since ? `?since=${encodeURIComponent(since)}` : "";
    return request<KeyStats>(`/admin/keys/${id}/stats${q}`);
  },
  keyGroups: (id: string) =>
    request<Group[]>(`/admin/keys/${id}/groups`, { unwrap: true }),
  bindKeyToGroup: (groupId: string, apiKeyId: string) =>
    request<{ group_id: string; api_key_id: string }>(
      `/admin/groups/${groupId}/bindings`,
      { method: "POST", body: JSON.stringify({ api_key_id: apiKeyId }) },
    ),
  unbindKeyFromGroup: (groupId: string, apiKeyId: string) =>
    request<{ group_id: string; api_key_id: string }>(
      `/admin/groups/${groupId}/bindings/${apiKeyId}`,
      { method: "DELETE" },
    ),
  accountEligibleModels: (id: string) =>
    request<AccountEligibleModels>(
      `/admin/accounts/${id}/eligible-models`,
    ),

  listGroups: () => request<Group[]>("/admin/groups", { unwrap: true }),
  createGroup: (req: CreateGroupRequest) =>
    request<Group>("/admin/groups", {
      method: "POST",
      body: JSON.stringify(req),
    }),
  updateGroup: (id: string, req: UpdateGroupRequest) =>
    request<Group>(`/admin/groups/${id}`, {
      method: "PUT",
      body: JSON.stringify(req),
    }),
  deleteGroup: (id: string) =>
    request<{ id: string }>(`/admin/groups/${id}`, { method: "DELETE" }),
  listGroupMembers: (id: string) =>
    request<{
      group_id: string;
      // members preserves the legacy string[] shape so existing callers
      // (multi-select filter, plain lists) keep working unchanged.
      members: string[];
      // members_detail carries the per-member priority column that
      // PickAndLock sorts on when route_strategy = "priority". Higher
      // wins. Default 100.
      members_detail: { account_id: string; priority: number }[];
    }>(`/admin/groups/${id}/members`),
  // addGroupMember is an upsert: repeating the call with the same
  // (groupId, accountId) updates the priority via ON CONFLICT DO UPDATE
  // in the sqlite store. Priority controls sort order within a group
  // when route_strategy = "priority" (higher wins). Defaults to 100
  // if omitted, matching the DB default.
  addGroupMember: (groupId: string, accountId: string, priority?: number) =>
    request<{ group_id: string; account_id: string; priority: number }>(
      `/admin/groups/${groupId}/members`,
      {
        method: "POST",
        body: JSON.stringify({
          account_id: accountId,
          ...(priority !== undefined ? { priority } : {}),
        }),
      },
    ),
  removeGroupMember: (groupId: string, accountId: string) =>
    request<{ group_id: string; account_id: string }>(
      `/admin/groups/${groupId}/members/${accountId}`,
      { method: "DELETE" },
    ),

  listJobs: (filter: JobFilter = {}) => {
    const q = new URLSearchParams();
    Object.entries(filter).forEach(([k, v]) => {
      if (v === undefined || v === null || v === "") return;
      q.set(k, String(v));
    });
    return request<{ data: Job[]; limit: number; offset: number }>(
      `/admin/jobs${q.toString() ? "?" + q.toString() : ""}`,
    );
  },
  getJob: (id: string) => request<Job>(`/admin/jobs/${id}`),

  listUsage: (filter: UsageAggregateParams & { limit?: number; offset?: number } = {}) => {
    const q = new URLSearchParams();
    Object.entries(filter).forEach(([k, v]) => {
      if (v === undefined || v === null || v === "" || (Array.isArray(v) && v.length === 0)) return;
      q.set(k, Array.isArray(v) ? v.join(",") : String(v));
    });
    return request<{ data: UsageEvent[]; limit: number; offset: number }>(
      `/admin/usage${q.toString() ? "?" + q.toString() : ""}`,
    );
  },

  listModelHealth: (filter: { verdict?: string; stale_before?: string } = {}) => {
    const q = new URLSearchParams();
    if (filter.verdict) q.set("verdict", filter.verdict);
    if (filter.stale_before) q.set("stale_before", filter.stale_before);
    return request<ModelHealthRow[]>(
      `/admin/model-health${q.toString() ? "?" + q.toString() : ""}`,
      { unwrap: true },
    );
  },

  // getModelHealthSlots returns the per-slot pass/fail time series
  // for one model. Backs the WebUI's UptimeBar with real regression
  // ticker output instead of the empty placeholder that used to
  // paper over "we haven't probed this yet". See docs/ROADMAP.md
  // P3-13.
  //
  // count defaults to 12 (mini table view); pass 48 for the detail
  // sheet view. slot_sec picks the bucket width — 3600 for hourly,
  // 86400 for daily. The backend caps count at 168 to bound the
  // fan-out.
  getModelHealthSlots: (
    jst: string,
    opts: { count?: number; slotSec?: number } = {},
  ) => {
    const q = new URLSearchParams();
    if (opts.count) q.set("count", String(opts.count));
    if (opts.slotSec) q.set("slot_sec", String(opts.slotSec));
    return request<ModelHealthSlotsResponse>(
      `/admin/model-health/${encodeURIComponent(jst)}/slots${
        q.toString() ? "?" + q.toString() : ""
      }`,
    );
  },

  listAudit: (filter: AuditFilter = {}) => {
    const q = new URLSearchParams();
    Object.entries(filter).forEach(([k, v]) => {
      if (v === undefined || v === null || v === "") return;
      q.set(k, String(v));
    });
    return request<{ data: AuditEvent[]; limit: number; offset: number }>(
      `/admin/audit${q.toString() ? "?" + q.toString() : ""}`,
    );
  },

  triggerRefresher: () =>
    request<{ ok: boolean; triggered: string }>(
      "/admin/tickers/refresher",
      { method: "POST" },
    ),
  triggerRegression: () =>
    request<{ ok: boolean; triggered: string }>(
      "/admin/tickers/regression",
      { method: "POST" },
    ),

  // Playground is mounted at /v1/playground/* on the admin listener
  // (see internal/api/server.go). Admin bearer is accepted as full
  // scope by the PlaygroundAuth middleware.
  playgroundModels: () =>
    request<PlaygroundModelsResponse>("/v1/playground/models"),

  // /v1/models is the public model catalog, unfiltered by playground
  // scope. Used by the group / key admin editors to render an
  // alias picker without leaking playground-scope constraints into
  // the admin surface.
  listPublicModels: () =>
    request<PublicModelsResponse>(
      "/v1/models?limit=500&include_unstable=1",
    ),
  // Force the model registry to reload from disk (verified-models.json).
  // Used by the Models management page's "reload" button so an operator
  // can pick up a fresh catalog without restarting the process.
  reloadModels: () =>
    request<{
      ok: boolean;
      previous_count: number;
      current_count: number;
      reloaded_at: string;
    }>("/admin/models/reload", { method: "POST" }),

  // Model-override admin surface (migration 015).
  listModelOverrides: () =>
    request<{ total: number; data: ModelOverride[] }>(
      "/admin/models/overrides",
    ),
  getModelOverride: (alias: string) =>
    request<ModelOverride>(
      `/admin/models/${encodeURIComponent(alias)}/override`,
    ),
  updateModelOverride: (alias: string, body: ModelOverridePatch) =>
    request<ModelOverride>(
      `/admin/models/${encodeURIComponent(alias)}/override`,
      { method: "PUT", body: JSON.stringify(body) },
    ),
  deleteModelOverride: (alias: string) =>
    request<void>(
      `/admin/models/${encodeURIComponent(alias)}/override`,
      { method: "DELETE" },
    ),

  // as_api_key_id (optional) lets an admin-bearer caller run under a
  // specific API key's identity: the backend gates by that key's
  // playground_scope instead of the admin's full scope.
  playgroundEstimate: (body: {
    model: string;
    params?: Record<string, unknown>;
    as_api_key_id?: string;
  }) =>
    request<PlaygroundEstimate>("/v1/playground/estimate", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  // as_api_key_id (when present) tells the backend to execute as that
  // key so usage, markup, group routing and quota all accrue against it.
  playgroundExecute: (body: Record<string, unknown> & { as_api_key_id?: string }) =>
    request<unknown>("/v1/playground/execute", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  aggregateUsage: (params: UsageAggregateParams) => {
    const q = new URLSearchParams();
    if (params.since) q.set("since", params.since);
    if (params.until) q.set("until", params.until);
    if (params.group_by?.length) q.set("group_by", params.group_by.join(","));
    if (params.status) q.set("status", params.status);
    if (params.api_key_id) q.set("api_key_id", params.api_key_id);
    if (params.group_id) q.set("group_id", params.group_id);
    if (params.model_alias) q.set("model_alias", params.model_alias);
    return request<UsageAggRow[]>(`/admin/usage/aggregate?${q.toString()}`, {
      unwrap: true,
    });
  },

  // Whoami-style probe used by AuthGate to verify a pasted bearer before
  // storing it. The pool stats endpoint is cheap and requires the bearer,
  // so a 200 confirms both connectivity and auth.
  ping: () => request<PoolStats>("/admin/stats/pool"),

  // ---- Registrations (plugin family) ------------------------------------

  listRegistrations: (filter: RegistrationFilter = {}) => {
    const q = new URLSearchParams();
    if (filter.status) q.set("status", filter.status);
    if (filter.limit != null) q.set("limit", String(filter.limit));
    if (filter.offset != null) q.set("offset", String(filter.offset));
    return request<{ data: Registration[]; limit: number; offset: number }>(
      `/admin/registrations${q.toString() ? "?" + q.toString() : ""}`,
    );
  },
  getRegistration: (id: string) =>
    request<Registration>(`/admin/registrations/${id}`),
  enqueueRegistration: (body: EnqueueRegistrationRequest) =>
    request<{ id: string; status: string }>("/admin/registrations", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  // bulkEnqueueRegistrations parses a mailbox list line-by-line
  // server-side. The response's `skipped` array lets the UI show
  // per-line errors without aborting the whole batch. Handled at
  // /admin/registrations/bulk (see ROADMAP §5.4 P4-3d).
  bulkEnqueueRegistrations: (body: BulkEnqueueRegistrationRequest) =>
    request<BulkEnqueueRegistrationResponse>(
      "/admin/registrations/bulk",
      {
        method: "POST",
        body: JSON.stringify(body),
      },
    ),
  retryRegistration: (id: string) =>
    request<{ id: string; status: string }>(
      `/admin/registrations/${id}/retry`,
      { method: "POST" },
    ),

  // ---- Settings ---------------------------------------------------------

  // Reads runtime metadata about the currently-active admin bearer.
  // Never returns the plaintext — only source (toml|db), last_4,
  // and (for DB-sourced overrides) the last update timestamp.
  getBearerSettings: () =>
    request<BearerSettings>("/admin/settings/bearer"),

  // Rotates the admin bearer. When new_bearer is omitted the server
  // generates a fresh 32-byte hex string. current_bearer is required
  // and must match the currently active bearer.
  rotateBearer: (body: RotateBearerRequest) =>
    request<RotateBearerResponse>("/admin/settings/bearer/rotate", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  // ---- Routing preference (global default) ------------------------------

  // Get returns { strategy: "load_balance" | "priority", source:
  // "db" | "default" }. Never 404s — a missing row surfaces as
  // default:load_balance so the sidebar always has something to
  // render.
  getRoutingSettings: () =>
    request<RoutingSettings>("/admin/settings/routing"),
  updateRoutingSettings: (body: { strategy: "load_balance" | "priority" }) =>
    request<RoutingSettings>("/admin/settings/routing", {
      method: "PUT",
      body: JSON.stringify(body),
    }),

  // ---- Load-balance advanced settings ---------------------------------
  //
  // Six operator-editable knobs that steer the load_balance route
  // strategy's internal ordering. Reads never 404 — missing rows fall
  // back to the hardcoded defaults with source="default".
  getLoadBalanceSettings: () =>
    request<LoadBalanceSettings>("/admin/settings/load_balance"),
  updateLoadBalanceSettings: (body: LoadBalanceSettingsInput) =>
    request<LoadBalanceSettings>("/admin/settings/load_balance", {
      method: "PUT",
      body: JSON.stringify(body),
    }),

  // ---- Failover --------------------------------------------------------

  getFailoverConfig: () =>
    request<FailoverConfig>("/admin/failover/config"),
  updateFailoverConfig: (patch: FailoverConfigPatch) =>
    request<FailoverConfig>("/admin/failover/config", {
      method: "PUT",
      body: JSON.stringify(patch),
    }),
  listIsolatedAccounts: () =>
    request<{ total: number; data: IsolatedAccount[] }>(
      "/admin/failover/isolated",
    ),
  recoverAccount: (id: string) =>
    request<{ id: string; status: string }>(
      `/admin/accounts/${encodeURIComponent(id)}/recover`,
      { method: "POST" },
    ),

  // ---- Version ----------------------------------------------------------

  // Returns the currently running binary's version metadata. Never
  // errors on network — the handler always returns a JSON body even
  // for dev builds ({version: "dev", dev: true}).
  getVersion: () => request<VersionInfo>("/admin/version"),

  // Compares the running version against the latest GitHub release.
  // Cached server-side for 1 hour to stay inside GitHub's anonymous
  // rate limit. On GitHub outage the server returns {error:
  // "upstream_unavailable"} + 200; never surfaces as a hard error to
  // the UI.
  checkVersion: () => request<VersionCheckResult>("/admin/version/check"),
};

// ---------- Version -------------------------------------------------------

export interface VersionInfo {
  version: string;
  commit: string;
  build_time: string;
  go_version: string;
  os_arch: string;
}

// VersionCheckResult carries both a normal "you're up to date / new
// release available" result and a degraded "GitHub was unavailable,
// try later" payload. Callers should render on `error` before
// interpreting `update_available`.
export interface VersionCheckResult {
  current: string;
  latest?: string;
  update_available?: boolean;
  release_url?: string;
  published_at?: string;
  dev?: boolean;
  error?: string;
}

// ---------- Settings ------------------------------------------------------

export interface BearerSettings {
  source: "toml" | "db" | string;
  last_4: string;
  updated_at: string;
}
export interface RotateBearerRequest {
  current_bearer: string;
  new_bearer?: string;
}
export interface RotateBearerResponse {
  new_bearer: string;
  source: string;
  last_4: string;
  display_hint: string;
}

// ---------- Groups --------------------------------------------------------

export interface Group {
  id: string;
  name: string;
  description: string;
  max_concurrent_jobs: number;
  max_concurrent_per_account: number;
  monthly_credit_budget: number; // credits × 100
  monthly_credit_used: number; // credits × 100
  allowed_models_regex: string;
  blocked_models_regex: string;
  route_strategy: string;
  owner_type: string;
  owner_id: string;
  status: string;
  created_at?: string;
}

// ---------- Usage aggregate ----------------------------------------------

// Dimensions the /admin/usage/aggregate endpoint accepts under group_by.
// `billing_hour` is a synthetic strftime bucket (see internal/adapters/
// storage/sqlite/usage_store.go) — added to serve the intraday trend.
export type UsageDim =
  | "api_key_id"
  | "cpa_partner_id"
  | "account_id"
  | "group_id"
  | "model_alias"
  | "billing_hour"
  | "billing_day"
  | "billing_month"
  | "media_type"
  | "status";

export interface UsageAggregateParams {
  since?: string; // RFC3339
  until?: string; // RFC3339
  group_by?: UsageDim[];
  status?: string;
  api_key_id?: string;
  group_id?: string;
  model_alias?: string;
}

export interface UsageAggRow {
  keys: Partial<Record<UsageDim, string>>;
  request_count: number;
  completed_count: number;
  failed_count: number;
  refunded_count: number;
  total_credits_h: number; // credits × 100
  charged_credits_h: number; // credits × 100
  avg_latency_ms: number;
}

// ---------- Routing preference -------------------------------------------

export interface RoutingSettings {
  strategy: "load_balance" | "priority";
  source: "db" | "default";
}

// ---------- Load-balance advanced settings --------------------------------
//
// Mirrors internal/core/loadbalance.Settings. Every field is required
// on PUT; GET always returns the full struct with defaults filled in.
export interface LoadBalanceSettingsInput {
  tier_aware: boolean;
  prefer_unlim: boolean;
  prefer_free_quota: boolean;
  prefer_richer: boolean;
  balance_headroom_pct: number; // 100..500
  jitter: boolean;
}

export interface LoadBalanceSettings extends LoadBalanceSettingsInput {
  // "db" when at least one key has been written; "default" when every
  // key is falling back to the hardcoded default. The UI renders a
  // "using default" hint until the first save.
  source: "db" | "default";
}

// ---------- Failover ------------------------------------------------------

// FailoverConfig mirrors the runtime failover subsystem knobs. Every
// nested block is optional on the patch surface — the PUT handler
// merges partial payloads onto the live in-memory config.
export interface FailoverConfig {
  enabled: boolean;
  consecutive: {
    enabled: boolean;
    fail_limit: number;
  };
  throttle: {
    enabled: boolean;
    judge_window_sec: number;
    judge_count: number;
    cooldown_sec: number;
    evict_window_sec: number;
    evict_count: number;
    risk_markers: string[];
  };
  outage_guard: {
    window_sec: number;
    disable_count_limit: number;
  };
}

// Patch body — same shape but every field is optional so callers can
// send only the section they touched.
export type FailoverConfigPatch = {
  enabled?: boolean;
  consecutive?: Partial<FailoverConfig["consecutive"]>;
  throttle?: Partial<FailoverConfig["throttle"]>;
  outage_guard?: Partial<FailoverConfig["outage_guard"]>;
};

// IsolatedAccount is one row of GET /admin/failover/isolated —
// accounts currently sitting in throttled or disabled status.
export interface IsolatedAccount {
  id: string;
  email: string;
  status: string;
  status_reason: string;
  throttled_until: string;
  fail_streak: number;
  last_failed_at: string;
  recent_events: number;
}

// ---------- Registrations (plugin family) --------------------------------

// Registration mirrors one row of the registrations table + Registrar
// runtime state. Slice/string fields are always non-null on the wire.
export interface Registration {
  id: string;
  email: string;
  oauth_source: string;
  proxy_url: string;
  status: "pending" | "running" | "success" | "failed" | string;
  attempts: number;
  last_error: string;
  account_id: string;
  created_at?: string;
  finished_at?: string;
}

export interface RegistrationFilter {
  status?: string;
  limit?: number;
  offset?: number;
}

export interface EnqueueRegistrationRequest {
  email: string;
  password?: string;
  oauth_source?: string;
  proxy_url?: string;
  mailbox_client_id?: string;
  mailbox_refresh_token?: string;
}

// BulkEnqueueRegistrationRequest is the shape of POST
// /admin/registrations/bulk. `lines` is the raw text from the
// mailbox list file — one row per line, format:
//   email----password----client_id----refresh_token
// Blank lines and lines beginning with `#` are ignored.
export interface BulkEnqueueRegistrationRequest {
  lines: string;
  proxy_url?: string;
}

// BulkEnqueueRegistrationResponse is the summary the server returns
// so the UI can render partial-success rather than all-or-nothing:
// even a batch with 3 bad lines out of 100 still enqueues the other
// 97.
export interface BulkEnqueueRegistrationResponse {
  enqueued: number;
  ids: string[];
  skipped: Array<{ line: number; reason: string }>;
}
