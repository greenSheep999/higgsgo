// Shared time-window model used by the dashboard.
//
// A window is `{ since, until, granularity }` where granularity is the
// group_by bucket the /admin/usage/aggregate endpoint should be asked to
// use. 24h defaults to hour buckets; anything wider defaults to day
// buckets. Custom ranges auto-pick: <=48h → hour, else day.

export type Granularity = "hour" | "day";

export interface TimeWindow {
  since: string; // RFC3339
  until: string; // RFC3339
  granularity: Granularity;
  // i18n key for the human label (see LABEL_KEY / customWindow). Callers
  // that need the localised string pass it through i18next's t().
  label: string;
}

export type WindowPreset = "24h" | "7d" | "15d" | "30d";

const HOURS = 3_600_000;
const DAYS = 24 * HOURS;

// The label field is an i18n key rather than a rendered string so the
// window can be built outside a React tree (e.g. in useState initialisers)
// and still surface the correct locale at render time.
const LABEL_KEY: Record<WindowPreset, string> = {
  "24h": "dashboard.window.last24h",
  "7d": "dashboard.window.last7d",
  "15d": "dashboard.window.last15d",
  "30d": "dashboard.window.last30d",
};

// Builds a preset window ending at "now" (rounded down to a whole hour so
// aggregate queries hit the hour boundary and are cache-friendly).
export function presetWindow(preset: WindowPreset): TimeWindow {
  const now = new Date();
  now.setMinutes(0, 0, 0);
  const until = now.toISOString();
  const spans: Record<WindowPreset, { ms: number; g: Granularity }> = {
    "24h": { ms: 24 * HOURS, g: "hour" },
    "7d": { ms: 7 * DAYS, g: "day" },
    "15d": { ms: 15 * DAYS, g: "day" },
    "30d": { ms: 30 * DAYS, g: "day" },
  };
  const spec = spans[preset];
  const since = new Date(now.getTime() - spec.ms).toISOString();
  return { since, until, granularity: spec.g, label: LABEL_KEY[preset] };
}

// Builds a custom window from two Dates. The 48h boundary picks hour vs day
// buckets automatically — narrow ranges gain resolution, wide ranges keep
// the payload small.
export function customWindow(from: Date, to: Date): TimeWindow {
  const spanMs = to.getTime() - from.getTime();
  const granularity: Granularity = spanMs <= 48 * HOURS ? "hour" : "day";
  return {
    since: from.toISOString(),
    until: to.toISOString(),
    granularity,
    label: "dashboard.window.customRange",
  };
}

// The group_by column the /aggregate endpoint should use for this window's
// primary time axis.
export function timeBucketKey(g: Granularity): "billing_hour" | "billing_day" {
  return g === "hour" ? "billing_hour" : "billing_day";
}
