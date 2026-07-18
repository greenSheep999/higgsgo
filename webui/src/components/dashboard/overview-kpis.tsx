import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
  IconAlertTriangle,
  IconBolt,
  IconClockHour4,
  IconCoin,
  IconInfinity,
  IconKey,
  IconTrendingDown,
  IconTrendingUp,
  IconUsers,
  IconWallet,
} from "@tabler/icons-react";
import { admin, ApiError, type ApiKey, type UsageAggRow } from "@/lib/api";
import {
  Card,
  CardAction,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import type { TimeWindow } from "@/lib/time-window";

// OverviewKPIs renders the top two stat rows.
//
// Row 1 — *Pool* KPIs (time-invariant): accounts / keys / subscription
// balance / unlim. These describe the shape of the pool right now and
// don't change when the operator flips the time window; they're the
// numbers you want to see the moment the page loads.
//
// Row 2 — *Activity* KPIs (time-window driven): requests / charged
// credits / failure rate / avg latency. These follow the currently-
// selected time window and are labelled with the window name so the
// context is unambiguous.
//
// The two rows are visually distinct — a subtle gradient on the pool
// row, plain cards on the activity row — so the eye separates "what
// exists" from "what happened in the last N".

interface Props {
  window: TimeWindow;
}

// Compute the previous equivalent period for trend comparison. If the current
// window spans 7 days (since..until), the previous window is 7 days before
// that (since-7d .. since).
function previousWindow(w: TimeWindow): { since: string; until: string } {
  const sinceMs = new Date(w.since).getTime();
  const untilMs = new Date(w.until).getTime();
  const spanMs = untilMs - sinceMs;
  return {
    since: new Date(sinceMs - spanMs).toISOString(),
    until: w.since,
  };
}

interface Trend {
  value: number; // percentage change
  direction: "up" | "down" | "neutral";
}

function computeTrend(current: number, previous: number): Trend {
  if (previous === 0 && current === 0) return { value: 0, direction: "neutral" };
  if (previous === 0) return { value: 100, direction: "up" };
  const pct = Math.round(((current - previous) / previous) * 1000) / 10;
  if (pct === 0) return { value: 0, direction: "neutral" };
  return { value: Math.abs(pct), direction: pct > 0 ? "up" : "down" };
}

export function OverviewKPIs({ window }: Props) {
  const { t } = useTranslation();
  const windowLabel = t(window.label);
  const prev = previousWindow(window);

  const pool = useQuery({
    queryKey: ["admin", "stats", "pool"],
    queryFn: admin.poolStats,
    refetchInterval: 30_000,
  });

  const keys = useQuery({
    queryKey: ["admin", "keys"],
    queryFn: admin.listKeys,
    refetchInterval: 30_000,
  });

  const usage = useQuery({
    queryKey: [
      "admin",
      "usage",
      "aggregate",
      "kpi",
      window.since,
      window.until,
    ],
    queryFn: () =>
      admin.aggregateUsage({
        since: window.since,
        until: window.until,
      }),
    refetchInterval: 30_000,
  });

  const prevUsage = useQuery({
    queryKey: [
      "admin",
      "usage",
      "aggregate",
      "kpi-prev",
      prev.since,
      prev.until,
    ],
    queryFn: () =>
      admin.aggregateUsage({
        since: prev.since,
        until: prev.until,
      }),
    refetchInterval: 30_000,
  });

  const totals = summariseUsage(usage.data);
  const prevTotals = summariseUsage(prevUsage.data);
  const keyCounts = summariseKeys(keys.data);
  const failureRate =
    totals.requests > 0 ? (totals.failed / totals.requests) * 100 : 0;
  const prevFailureRate =
    prevTotals.requests > 0
      ? (prevTotals.failed / prevTotals.requests) * 100
      : 0;

  const requestsTrend = computeTrend(totals.requests, prevTotals.requests);
  const creditsTrend = computeTrend(totals.chargedCredits, prevTotals.chargedCredits);
  const failureTrend = computeTrend(failureRate, prevFailureRate);
  const latencyTrend = computeTrend(totals.avgLatencyMs, prevTotals.avgLatencyMs);

  const error = pool.error ?? keys.error ?? usage.error;
  if (error) return <ErrorPanel err={error} what={t("dashboard.errors.overviewStats")} />;

  const loading = pool.isLoading || keys.isLoading || usage.isLoading;

  return (
    <div className="flex flex-col gap-4">
      {/* Row 1 — pool KPIs (4 tiles). Wrapped in the same gradient
          treatment the shadcn SectionCards block uses so they read as
          a single "state of the pool" band. */}
      <div className="grid grid-cols-1 gap-4 *:data-[slot=card]:bg-gradient-to-t *:data-[slot=card]:from-primary/5 *:data-[slot=card]:to-card *:data-[slot=card]:shadow-xs @xl/main:grid-cols-2 @5xl/main:grid-cols-4 dark:*:data-[slot=card]:bg-card">
        <Tile
          title={t("dashboard.kpi.accounts")}
          value={pool.data?.total}
          hint={
            pool.data
              ? t("dashboard.kpi.hints.accountsSplit", {
                  active: pool.data.by_status.active ?? 0,
                  suspended: pool.data.by_status.suspended ?? 0,
                })
              : undefined
          }
          icon={<IconUsers className="size-4" />}
          loading={loading}
          large
        />
        <Tile
          title={t("dashboard.kpi.apiKeys")}
          value={keyCounts.active}
          hint={t("dashboard.kpi.hints.keysSplit", {
            active: keyCounts.active,
            paused: keyCounts.paused,
            total: keyCounts.total,
          })}
          icon={<IconKey className="size-4" />}
          loading={loading}
          large
        />
        <Tile
          title={t("dashboard.kpi.subscriptionCredits")}
          value={pool.data?.total_subscription_balance}
          format="credits-flat"
          hint={t("dashboard.kpi.hints.subCreditsHint")}
          icon={<IconWallet className="size-4" />}
          loading={loading}
          large
        />
        <Tile
          title={t("dashboard.kpi.unlim")}
          value={pool.data?.with_unlim}
          hint={
            pool.data
              ? t("dashboard.kpi.hints.unlimHint", {
                  flex: pool.data.with_flex_unlim,
                })
              : undefined
          }
          icon={<IconInfinity className="size-4" />}
          loading={loading}
          large
        />
      </div>

      {/* Row 2 — activity KPIs (4 tiles). No gradient so the eye reads
          them as a separate band; each carries the window label so
          "1,234" cannot be mistaken for a lifetime total. */}
      <div className="grid grid-cols-1 gap-4 @xl/main:grid-cols-2 @5xl/main:grid-cols-4">
        <Tile
          title={t("dashboard.kpi.requests")}
          value={totals.requests}
          hint={t("dashboard.kpi.hints.reqSplit", {
            completed: totals.completed,
            failed: totals.failed,
          })}
          icon={<IconBolt className="size-4" />}
          loading={loading}
          badge={windowLabel}
          trend={requestsTrend}
        />
        <Tile
          title={t("dashboard.kpi.chargedCredits")}
          value={totals.chargedCredits}
          format="credits"
          hint={t("dashboard.kpi.hints.creditsIncludingFree", {
            value: (totals.totalCredits / 100).toLocaleString(undefined, {
              maximumFractionDigits: 2,
            }),
          })}
          icon={<IconCoin className="size-4" />}
          loading={loading}
          badge={windowLabel}
          trend={creditsTrend}
        />
        <Tile
          title={t("dashboard.kpi.failureRate")}
          value={failureRate}
          format="percent"
          hint={
            totals.requests > 0
              ? t("dashboard.kpi.hints.failureOf", {
                  failed: totals.failed,
                  total: totals.requests,
                })
              : t("dashboard.kpi.hints.failureNoRequests")
          }
          icon={<IconAlertTriangle className="size-4" />}
          loading={loading}
          badge={windowLabel}
          trend={failureTrend}
          invertTrend
        />
        <Tile
          title={t("dashboard.kpi.avgLatency")}
          value={totals.avgLatencyMs}
          format="ms"
          hint={t("dashboard.kpi.hints.latencyHint")}
          icon={<IconClockHour4 className="size-4" />}
          loading={loading}
          badge={windowLabel}
          trend={latencyTrend}
          invertTrend
        />
      </div>
    </div>
  );
}

interface Totals {
  requests: number;
  completed: number;
  failed: number;
  chargedCredits: number; // × 100
  totalCredits: number; // × 100
  avgLatencyMs: number;
}

function summariseUsage(rows: UsageAggRow[] | undefined): Totals {
  const zero: Totals = {
    requests: 0,
    completed: 0,
    failed: 0,
    chargedCredits: 0,
    totalCredits: 0,
    avgLatencyMs: 0,
  };
  if (!rows) return zero;
  let latencyWeighted = 0;
  let latencyCount = 0;
  for (const r of rows) {
    zero.requests += r.request_count;
    zero.completed += r.completed_count;
    zero.failed += r.failed_count;
    zero.chargedCredits += r.charged_credits_h;
    zero.totalCredits += r.total_credits_h;
    if (r.avg_latency_ms > 0 && r.request_count > 0) {
      latencyWeighted += r.avg_latency_ms * r.request_count;
      latencyCount += r.request_count;
    }
  }
  zero.avgLatencyMs = latencyCount > 0 ? Math.round(latencyWeighted / latencyCount) : 0;
  return zero;
}

function summariseKeys(rows: ApiKey[] | undefined) {
  const c = { total: 0, active: 0, paused: 0, revoked: 0 };
  if (!rows) return c;
  for (const k of rows) {
    c.total++;
    if (k.status === "active") c.active++;
    else if (k.status === "paused") c.paused++;
    else if (k.status === "revoked") c.revoked++;
  }
  return c;
}

interface TileProps {
  title: string;
  value: number | undefined;
  hint?: string;
  icon: React.ReactNode;
  loading: boolean;
  // "credits" divides by 100 (fractional credits); "credits-flat" also
  // divides by 100 but the pool balance is already a whole-credit float
  // (see stats.go /100 conversion), so we display it as-is with 2dp.
  format?: "plain" | "credits" | "credits-flat" | "percent" | "ms";
  badge?: string;
  large?: boolean;
  trend?: Trend;
  // When true, "down" is good (green) and "up" is bad (red).
  // Use for metrics where lower is better (failure rate, latency).
  invertTrend?: boolean;
}

function Tile({
  title,
  value,
  hint,
  icon,
  loading,
  format = "plain",
  badge,
  large = false,
  trend,
  invertTrend = false,
}: TileProps) {
  const { t } = useTranslation();
  const rendered =
    value === undefined || value === null
      ? "—"
      : format === "credits"
        ? (value / 100).toLocaleString(undefined, { maximumFractionDigits: 2 })
        : format === "credits-flat"
          ? value.toLocaleString(undefined, { maximumFractionDigits: 2 })
          : format === "percent"
            ? `${value.toFixed(1)}%`
            : format === "ms"
              ? `${Math.round(value)} ms`
              : value.toLocaleString();

  // Determine trend color: for inverted metrics (failure rate, latency),
  // "down" is good and "up" is bad.
  let trendElement: React.ReactNode = null;
  if (trend) {
    if (trend.direction === "neutral") {
      trendElement = (
        <span className="inline-flex items-center gap-0.5 text-xs text-muted-foreground">
          —
        </span>
      );
    } else {
      const isGood = invertTrend
        ? trend.direction === "down"
        : trend.direction === "up";
      const colorCls = isGood
        ? "text-green-600 dark:text-green-400"
        : "text-red-600 dark:text-red-400";
      const TrendIcon = trend.direction === "up" ? IconTrendingUp : IconTrendingDown;
      const sign = trend.direction === "up" ? "+" : "-";
      trendElement = (
        <span className={`inline-flex items-center gap-0.5 text-xs font-medium ${colorCls}`}>
          <TrendIcon className="size-3.5" />
          {sign}{trend.value.toFixed(1)}%
        </span>
      );
    }
  }

  return (
    <Card className="@container/card">
      <CardHeader>
        <CardDescription>{title}</CardDescription>
        <CardTitle
          className={
            large
              ? "text-3xl font-semibold tabular-nums @[250px]/card:text-4xl"
              : "text-2xl font-semibold tabular-nums @[250px]/card:text-3xl"
          }
        >
          {loading ? <Skeleton className="h-10 w-28" /> : rendered}
        </CardTitle>
        <CardAction className="flex items-center gap-1">
          {badge ? (
            <Badge variant="secondary" className="text-[10px]">
              {badge}
            </Badge>
          ) : null}
          <Badge variant="outline">{icon}</Badge>
        </CardAction>
      </CardHeader>
      {(hint || trendElement) ? (
        <CardFooter className="flex items-center justify-between text-xs text-muted-foreground">
          {hint ? <span>{hint}</span> : <span />}
          {!loading && trendElement}
        </CardFooter>
      ) : null}
    </Card>
  );
}

function ErrorPanel({ err, what }: { err: unknown; what: string }) {
  const { t } = useTranslation();
  const label =
    err instanceof ApiError
      ? `${err.status} ${err.type}: ${err.message}`
      : err instanceof Error
        ? err.message
        : String(err);
  return (
    <Card className="border-destructive/50 bg-destructive/5">
      <CardHeader>
        <CardTitle className="text-base text-destructive">
          {t("common.couldNotLoad", { what })}
        </CardTitle>
        <CardDescription className="text-destructive/80">{label}</CardDescription>
      </CardHeader>
    </Card>
  );
}
