import { useMemo } from "react";
import { useQueries } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
  IconArrowRight,
  IconBox,
  IconKey,
} from "@tabler/icons-react";
import { Link } from "@tanstack/react-router";
import {
  Bar,
  BarChart,
  Cell,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { admin, type UsageAggRow, type ApiKey } from "@/lib/api";
import type { TimeWindow } from "@/lib/time-window";

// OverviewBreakdown renders two side-by-side "Top 5" cards summarising
// which API keys and models drove the most usage in the selected window.
// Groups used to be a third card, but the operator uses groups as a
// filter (see dashboard header) rather than a target of top-N, so it is
// no longer a card — it's a knob that changes what all three sections
// see. The bars are relative to the leader of each card so the eyeball
// can compare within a card without needing an absolute axis.

interface Props {
  window: TimeWindow;
}

export function OverviewBreakdown({ window }: Props) {
  const { t } = useTranslation();
  const queries = useQueries({
    queries: [
      {
        queryKey: [
          "admin",
          "usage",
          "breakdown",
          "keys",
          window.since,
          window.until,
        ],
        queryFn: () =>
          admin.aggregateUsage({
            since: window.since,
            until: window.until,
            group_by: ["api_key_id"],
          }),
      },
      {
        queryKey: [
          "admin",
          "usage",
          "breakdown",
          "models",
          window.since,
          window.until,
        ],
        queryFn: () =>
          admin.aggregateUsage({
            since: window.since,
            until: window.until,
            group_by: ["model_alias"],
          }),
      },
      { queryKey: ["admin", "keys"], queryFn: admin.listKeys },
    ],
  });
  const [byKey, byModel, keys] = queries;

  const keyLookup = useMemo(() => {
    const map: Record<string, ApiKey> = {};
    (keys.data ?? []).forEach((k: ApiKey) => (map[k.id] = k));
    return map;
  }, [keys.data]);

  return (
    <div className="grid grid-cols-1 gap-4 @xl/main:grid-cols-2">
      <BreakdownCard
        title={t("dashboard.breakdown.topKeys")}
        description={t("dashboard.breakdown.creditsInWindow")}
        icon={<IconKey className="size-4" />}
        loading={byKey.isLoading}
        rows={topRows(byKey.data, "api_key_id", (id) => ({
          label: keyLookup[id]?.name || id.slice(0, 12),
          sub:
            keyLookup[id] && keyLookup[id]!.monthly_quota > 0
              ? t("dashboard.breakdown.monthlyQuota", {
                  used: ((keyLookup[id]!.monthly_used ?? 0) / 100).toFixed(0),
                  quota: (keyLookup[id]!.monthly_quota / 100).toFixed(0),
                })
              : keyLookup[id]?.playground_scope
                ? t("dashboard.breakdown.playgroundScope", {
                    scope: keyLookup[id]!.playground_scope,
                  })
                : undefined,
        }))}
        moreHref="/keys"
      />
      <BreakdownCard
        title={t("dashboard.breakdown.topModels")}
        description={t("dashboard.breakdown.requestsInWindow")}
        icon={<IconBox className="size-4" />}
        loading={byModel.isLoading}
        metric="requests"
        rows={topRows(
          byModel.data,
          "model_alias",
          (id) => ({ label: id || "(unset)" }),
          "requests",
        )}
        moreHref="/usage"
      />
    </div>
  );
}

interface Row {
  key: string;
  label: string;
  sub?: string;
  value: number;
}

function topRows(
  agg: UsageAggRow[] | undefined,
  dim: "api_key_id" | "model_alias",
  fmt: (id: string) => { label: string; sub?: string },
  metric: "credits" | "requests" = "credits",
): Row[] {
  if (!agg) return [];
  const value = (r: UsageAggRow) =>
    metric === "credits" ? r.charged_credits_h / 100 : r.request_count;
  return agg
    .map<Row>((r) => {
      const key = r.keys[dim] ?? "(none)";
      const meta = fmt(key);
      return { key: key || "(none)", label: meta.label, sub: meta.sub, value: value(r) };
    })
    .filter((r) => r.value > 0)
    .sort((a, b) => b.value - a.value)
    .slice(0, 5);
}

interface CardProps {
  title: string;
  description: string;
  icon?: React.ReactNode;
  loading: boolean;
  rows: Row[];
  moreHref: string;
  metric?: "credits" | "requests";
}

// Palette cycles through the 5 chart tokens so the bars pick up the
// theme accent (light + dark) without hard-coding hex values.
const BAR_COLORS = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
];

function BreakdownCard({
  title,
  description,
  icon,
  loading,
  rows,
  moreHref,
  metric = "credits",
}: CardProps) {
  const { t } = useTranslation();

  // Recharts wants a stable-shaped array; give each row a display label
  // (used for the y-axis tick + tooltip) and its raw value. Height per
  // row keeps the chart size deterministic regardless of row count so
  // the card doesn't jump when data changes.
  const rowHeight = 28;
  const chartHeight = Math.max(rowHeight * rows.length + 20, 60);

  const formatValue = (v: number) =>
    metric === "requests"
      ? v.toLocaleString()
      : v.toLocaleString(undefined, { maximumFractionDigits: 2 });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          {icon ? (
            <span className="text-muted-foreground">{icon}</span>
          ) : null}
          {title}
        </CardTitle>
        <CardDescription>{description}</CardDescription>
        <CardAction>
          <Button variant="ghost" size="sm" asChild>
            <Link to={moreHref}>
              {t("common.seeAll")}
              <IconArrowRight />
            </Link>
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        {loading ? (
          <div className="space-y-2">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-6 w-full" />
            ))}
          </div>
        ) : rows.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("common.nothing")}</p>
        ) : (
          <div style={{ height: chartHeight }} className="w-full">
            <ResponsiveContainer width="100%" height="100%">
              <BarChart
                data={rows}
                layout="vertical"
                margin={{ top: 4, right: 12, bottom: 4, left: 8 }}
                barCategoryGap={6}
              >
                <XAxis type="number" hide domain={[0, "dataMax"]} />
                <YAxis
                  type="category"
                  dataKey="label"
                  width={110}
                  tickLine={false}
                  axisLine={false}
                  tick={{
                    fill: "var(--muted-foreground)",
                    fontSize: 12,
                  }}
                />
                <Tooltip
                  cursor={{ fill: "var(--muted)" }}
                  contentStyle={{
                    background: "var(--popover)",
                    border: "1px solid var(--border)",
                    borderRadius: "calc(var(--radius) - 4px)",
                    color: "var(--popover-foreground)",
                    fontSize: 12,
                  }}
                  formatter={(v: unknown) => [
                    typeof v === "number" ? formatValue(v) : String(v),
                    metric === "requests"
                      ? t("dashboard.trend.metric.requests")
                      : t("dashboard.trend.metric.charged"),
                  ]}
                  labelStyle={{ color: "var(--foreground)" }}
                />
                <Bar dataKey="value" radius={[4, 4, 4, 4]}>
                  {rows.map((_, i) => (
                    <Cell key={i} fill={BAR_COLORS[i % BAR_COLORS.length]} />
                  ))}
                </Bar>
              </BarChart>
            </ResponsiveContainer>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
