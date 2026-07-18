import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueries } from "@tanstack/react-query";
import {
  Area,
  AreaChart,
  CartesianGrid,
  XAxis,
  YAxis,
} from "recharts";
import { format } from "date-fns";
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { admin, type UsageAggRow, type Group, type ApiKey } from "@/lib/api";
import { timeBucketKey, type TimeWindow } from "@/lib/time-window";

// UsageTrend renders a time-series area chart at the top of the Usage page.
// It reuses the same aggregate endpoint and charting patterns as the
// dashboard OverviewTrend but lives in its own component so the usage page
// can pass its own time window.

type Metric = "charged" | "requests" | "failed";
type Split = "total" | "model_alias" | "api_key_id" | "group_id";

const METRIC_OPTIONS: { value: Metric; labelKey: string }[] = [
  { value: "charged", labelKey: "usage.trend.metric.charged" },
  { value: "requests", labelKey: "usage.trend.metric.requests" },
  { value: "failed", labelKey: "usage.trend.metric.failed" },
];

const SPLIT_OPTIONS: { value: Split; labelKey: string }[] = [
  { value: "total", labelKey: "usage.trend.split.total" },
  { value: "model_alias", labelKey: "usage.trend.split.model" },
  { value: "api_key_id", labelKey: "usage.trend.split.key" },
  { value: "group_id", labelKey: "usage.trend.split.group" },
];

interface Props {
  window: TimeWindow;
}

export function UsageTrend({ window }: Props) {
  const { t } = useTranslation();
  const [metric, setMetric] = useState<Metric>("charged");
  const [split, setSplit] = useState<Split>("model_alias");

  const bucket = timeBucketKey(window.granularity);
  const groupBy = split === "total" ? [bucket] : [bucket, split];

  const queries = useQueries({
    queries: [
      {
        queryKey: [
          "admin",
          "usage",
          "usage-trend",
          window.since,
          window.until,
          groupBy.join(","),
        ],
        queryFn: () =>
          admin.aggregateUsage({
            since: window.since,
            until: window.until,
            group_by: groupBy,
          }),
        refetchInterval: 30_000,
      },
      {
        queryKey: ["admin", "keys"],
        queryFn: admin.listKeys,
        enabled: split === "api_key_id",
      },
      {
        queryKey: ["admin", "groups"],
        queryFn: admin.listGroups,
        enabled: split === "group_id",
      },
    ],
  });
  const [usageQ, keysQ, groupsQ] = queries;

  const nameLookup = useMemo(() => {
    const map: Record<string, string> = {};
    if (split === "api_key_id" && keysQ.data) {
      (keysQ.data as ApiKey[]).forEach((k) => {
        map[k.id] = k.name || k.id.slice(0, 8);
      });
    }
    if (split === "group_id" && groupsQ.data) {
      (groupsQ.data as Group[]).forEach((g) => {
        map[g.id] = g.name || g.id.slice(0, 8);
      });
    }
    return map;
  }, [split, keysQ.data, groupsQ.data]);

  const { rows, series } = useMemo(
    () => buildSeries(usageQ.data ?? [], bucket, split, metric, nameLookup),
    [usageQ.data, bucket, split, metric, nameLookup],
  );

  const chartConfig = useMemo<ChartConfig>(() => {
    const cfg: ChartConfig = {};
    series.forEach((s, i) => {
      cfg[s.key] = {
        label: s.label,
        color: `var(--chart-${(i % 5) + 1})`,
      };
    });
    return cfg;
  }, [series]);

  return (
    <div className="mb-6">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-muted-foreground">
          {t("usage.trend.title")}
        </span>
        <Select value={metric} onValueChange={(v) => setMetric(v as Metric)}>
          <SelectTrigger size="sm" className="w-40">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {METRIC_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {t(o.labelKey)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={split} onValueChange={(v) => setSplit(v as Split)}>
          <SelectTrigger size="sm" className="w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SPLIT_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {t(o.labelKey)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {usageQ.isLoading ? (
        <Skeleton className="h-64 w-full rounded-md" />
      ) : usageQ.isError ? (
        <p className="text-sm text-destructive">
          {usageQ.error instanceof Error
            ? usageQ.error.message
            : "Failed to load"}
        </p>
      ) : rows.length === 0 ? (
        <div className="flex h-64 items-center justify-center text-sm text-muted-foreground">
          {t("common.noEvents")}
        </div>
      ) : (
        <ChartContainer config={chartConfig} className="h-72 w-full">
          <AreaChart data={rows}>
            <defs>
              {series.map((s, i) => (
                <linearGradient
                  key={s.key}
                  id={`usage-fill-${s.key}`}
                  x1="0"
                  y1="0"
                  x2="0"
                  y2="1"
                >
                  <stop
                    offset="5%"
                    stopColor={`var(--chart-${(i % 5) + 1})`}
                    stopOpacity={0.6}
                  />
                  <stop
                    offset="95%"
                    stopColor={`var(--chart-${(i % 5) + 1})`}
                    stopOpacity={0.05}
                  />
                </linearGradient>
              ))}
            </defs>
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey="bucket"
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={32}
              tickFormatter={(v: string) =>
                format(
                  new Date(v),
                  window.granularity === "hour" ? "MM/dd HH:mm" : "MM/dd",
                )
              }
            />
            <YAxis
              tickLine={false}
              axisLine={false}
              width={44}
              tickFormatter={(v: number) => formatMetric(v, metric)}
            />
            <ChartTooltip
              cursor={false}
              content={
                <ChartTooltipContent
                  labelFormatter={(v) =>
                    format(
                      new Date(v as string),
                      window.granularity === "hour"
                        ? "yyyy-MM-dd HH:mm"
                        : "yyyy-MM-dd",
                    )
                  }
                  formatter={(value) =>
                    formatMetric(Number(value), metric)
                  }
                />
              }
            />
            <ChartLegend content={<ChartLegendContent />} />
            {series.map((s, i) => (
              <Area
                key={s.key}
                dataKey={s.key}
                type="monotone"
                fill={`url(#usage-fill-${s.key})`}
                stroke={`var(--chart-${(i % 5) + 1})`}
                strokeWidth={2}
                stackId={series.length > 1 ? "stack" : undefined}
              />
            ))}
          </AreaChart>
        </ChartContainer>
      )}
    </div>
  );
}
// ---------- helpers ----------

interface Series {
  key: string;
  label: string;
}

function buildSeries(
  agg: UsageAggRow[],
  bucketKey: "billing_hour" | "billing_day",
  split: Split,
  metric: Metric,
  nameLookup: Record<string, string>,
): { rows: Record<string, unknown>[]; series: Series[] } {
  if (agg.length === 0) return { rows: [], series: [] };

  const valueOf = (r: UsageAggRow) => {
    switch (metric) {
      case "charged":
        return r.charged_credits_h / 100;
      case "requests":
        return r.request_count;
      case "failed":
        return r.failed_count;
    }
  };

  const buckets = new Set<string>();
  const seriesTotals: Record<string, number> = {};
  const seriesLabel: Record<string, string> = {};
  const seriesKey = (r: UsageAggRow) => {
    if (split === "total") return "total";
    const raw = r.keys[split] ?? "(none)";
    return raw || "(none)";
  };

  const cell: Record<string, Record<string, number>> = {};
  agg.forEach((r) => {
    const b = r.keys[bucketKey];
    if (!b) return;
    buckets.add(b);
    const s = seriesKey(r);
    if (!cell[b]) cell[b] = {};
    cell[b][s] = (cell[b][s] ?? 0) + valueOf(r);
    seriesTotals[s] = (seriesTotals[s] ?? 0) + valueOf(r);
    if (!seriesLabel[s]) {
      seriesLabel[s] =
        s === "total"
          ? "Total"
          : (nameLookup[s] ?? shortenId(s));
    }
  });

  const orderedBuckets = Array.from(buckets).sort();
  const orderedSeries = Object.keys(seriesTotals).sort(
    (a, b) => seriesTotals[b]! - seriesTotals[a]!,
  );
  const primary = orderedSeries.slice(0, 5);
  const overflow = orderedSeries.slice(5);
  const series: Series[] = primary.map((k) => ({
    key: k,
    label: seriesLabel[k]!,
  }));
  if (overflow.length > 0) {
    series.push({ key: "__other", label: `Other (${overflow.length})` });
  }

  const rows = orderedBuckets.map((b) => {
    const row: Record<string, unknown> = { bucket: b };
    primary.forEach((k) => {
      row[k] = cell[b]?.[k] ?? 0;
    });
    if (overflow.length > 0) {
      row["__other"] = overflow.reduce(
        (sum, k) => sum + (cell[b]?.[k] ?? 0),
        0,
      );
    }
    return row;
  });

  return { rows, series };
}

function shortenId(id: string) {
  return id.length > 10 ? `${id.slice(0, 8)}…` : id;
}

function formatMetric(v: number, metric: Metric): string {
  if (metric === "requests" || metric === "failed")
    return v.toLocaleString();
  return v.toLocaleString(undefined, { maximumFractionDigits: 2 });
}
