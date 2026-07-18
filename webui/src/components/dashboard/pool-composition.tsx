import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from "recharts";
import { admin, type PoolStats } from "@/lib/api";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

// PoolComposition — two side-by-side cards describing the pool right now:
//
//   • Accounts by plan  → donut chart. Plan is a discrete category with
//     many small values (10 official plan_types), and a donut makes the
//     dominant tier obvious at a glance while still showing the long
//     tail in the legend.
//
//   • Accounts by status → single stacked bar + a big "active %" number.
//     Three-state health signal (active / suspended / banned) reads
//     better as a health bar than a pie — it's the same idiom every
//     ops dashboard uses (grafana, betterstack, statuscake).
//
// Both are time-invariant snapshots so they don't take a TimeWindow.

// Donut palette. Cycles through the tailwind chart tokens shadcn seeds
// into the theme so the chart follows the light/dark switch. Values
// pulled from index.css chart-1..chart-5.
const DONUT_COLORS = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
];

// Status → semantic colour. Green/amber/red maps to the operator's
// mental model of "healthy / degraded / dead" without needing a legend.
const STATUS_COLOR: Record<string, string> = {
  active: "bg-emerald-500",
  suspended: "bg-amber-500",
  banned: "bg-red-500",
};

// Order matters for the stacked bar so the same colour sits in the
// same lane regardless of counts.
const STATUS_ORDER = ["active", "suspended", "banned"];

export function PoolComposition() {
  const pool = useQuery({
    queryKey: ["admin", "stats", "pool"],
    queryFn: admin.poolStats,
    refetchInterval: 30_000,
  });

  return (
    <div className="grid grid-cols-1 gap-4 @xl/main:grid-cols-2">
      <PlanDonutCard data={pool.data} loading={pool.isLoading} />
      <StatusBarCard data={pool.data} loading={pool.isLoading} />
    </div>
  );
}

// PlanDonutCard renders by_plan as a donut chart with a legend beside
// it. Empty / loading states short-circuit to a skeleton so the card
// never jumps size when data arrives.
function PlanDonutCard({
  data,
  loading,
}: {
  data: PoolStats | undefined;
  loading: boolean;
}) {
  const { t } = useTranslation();
  const rows = useMemo(() => {
    if (!data) return [];
    return Object.entries(data.by_plan)
      .filter(([, v]) => v > 0)
      .sort((a, b) => b[1] - a[1])
      .map(([name, value]) => ({ name, value }));
  }, [data]);
  const total = rows.reduce((s, r) => s + r.value, 0);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">
          {t("dashboard.composition.byPlan")}
        </CardTitle>
        <CardDescription>
          {t("dashboard.composition.byPlanDesc")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <Skeleton className="h-48 w-full" />
        ) : rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            {t("dashboard.composition.emptyPlans")}
          </p>
        ) : (
          <div className="flex items-center gap-4">
            <div className="relative size-40 shrink-0">
              <ResponsiveContainer width="100%" height="100%">
                <PieChart>
                  <Pie
                    data={rows}
                    dataKey="value"
                    nameKey="name"
                    innerRadius={38}
                    outerRadius={70}
                    paddingAngle={2}
                    stroke="var(--background)"
                    strokeWidth={2}
                  >
                    {rows.map((_, i) => (
                      <Cell
                        key={i}
                        fill={DONUT_COLORS[i % DONUT_COLORS.length]}
                      />
                    ))}
                  </Pie>
                  <Tooltip
                    contentStyle={{
                      background: "var(--popover)",
                      border: "1px solid var(--border)",
                      borderRadius: "calc(var(--radius) - 4px)",
                      color: "var(--popover-foreground)",
                      fontSize: 12,
                    }}
                    formatter={(v: unknown, n: unknown) => [
                      typeof v === "number" ? v.toLocaleString() : String(v),
                      String(n),
                    ]}
                  />
                </PieChart>
              </ResponsiveContainer>
              {/* Total in the donut hole so the biggest number is
                  the accounts total, not any individual slice. */}
              <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
                <span className="text-2xl font-semibold tabular-nums">
                  {total.toLocaleString()}
                </span>
                <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
                  total
                </span>
              </div>
            </div>
            <ul className="flex min-w-0 flex-1 flex-col gap-1.5 text-sm">
              {rows.map((r, i) => {
                const pct = total > 0 ? (r.value / total) * 100 : 0;
                return (
                  <li
                    key={r.name}
                    className="flex items-center gap-2"
                  >
                    <span
                      aria-hidden="true"
                      className="size-2.5 shrink-0 rounded-sm"
                      style={{
                        background: DONUT_COLORS[i % DONUT_COLORS.length],
                      }}
                    />
                    <span className="min-w-0 flex-1 truncate">{r.name}</span>
                    <span className="shrink-0 tabular-nums text-muted-foreground">
                      {r.value}{" "}
                      <span className="text-xs">
                        ({pct.toFixed(0)}%)
                      </span>
                    </span>
                  </li>
                );
              })}
            </ul>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// StatusBarCard shows by_status as a single stacked bar + big active%
// KPI. Familiar "health bar" idiom, quicker read than a pie for a
// dominant-single-bucket distribution.
function StatusBarCard({
  data,
  loading,
}: {
  data: PoolStats | undefined;
  loading: boolean;
}) {
  const { t } = useTranslation();
  const rows = useMemo(() => {
    if (!data) return [];
    return STATUS_ORDER.map((k) => ({
      name: k,
      value: data.by_status[k] ?? 0,
    })).filter((r) => r.value > 0);
  }, [data]);
  const total = rows.reduce((s, r) => s + r.value, 0);
  const activePct =
    total > 0
      ? (((data?.by_status.active ?? 0) / total) * 100)
      : 0;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">
          {t("dashboard.composition.byStatus")}
        </CardTitle>
        <CardDescription>
          {t("dashboard.composition.byStatusDesc")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <Skeleton className="h-24 w-full" />
        ) : rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            {t("dashboard.composition.emptyStatuses")}
          </p>
        ) : (
          <div className="space-y-4">
            {/* Big active% KPI. Same tone as the bar's active segment
                so the eye pairs them without a legend. */}
            <div className="flex items-baseline gap-2">
              <span className="text-3xl font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">
                {activePct.toFixed(1)}%
              </span>
              <span className="text-xs text-muted-foreground">
                {t("dashboard.composition.activeRate", {
                  active: data?.by_status.active ?? 0,
                  total,
                })}
              </span>
            </div>

            {/* Stacked health bar. Each segment's width = its share of
                total; the segment colour tells you which bucket it is,
                so no legend needed on the bar itself. */}
            <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
              {rows.map((r) => (
                <div
                  key={r.name}
                  className={STATUS_COLOR[r.name] ?? "bg-muted-foreground"}
                  style={{ width: `${(r.value / total) * 100}%` }}
                  title={`${r.name}: ${r.value}`}
                />
              ))}
            </div>

            {/* Legend below the bar as a horizontal list. */}
            <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
              {rows.map((r) => (
                <li key={r.name} className="flex items-center gap-1.5">
                  <span
                    aria-hidden="true"
                    className={`size-2 rounded-full ${STATUS_COLOR[r.name] ?? "bg-muted-foreground"}`}
                  />
                  <span className="text-muted-foreground">
                    {t(`accounts.status.${r.name}`, {
                      defaultValue: r.name,
                    })}
                  </span>
                  <span className="tabular-nums">{r.value}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
