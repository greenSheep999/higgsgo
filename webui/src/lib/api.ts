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
    request<{ group_id: string; members: string[] }>(
      `/admin/groups/${id}/members`,
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
  playgroundEstimate: (body: { model: string; params?: Record<string, unknown> }) =>
    request<PlaygroundEstimate>("/v1/playground/estimate", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  playgroundExecute: (body: Record<string, unknown>) =>
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
};

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
