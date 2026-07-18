import { useMemo } from "react";
import { useQueries } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { IconArrowRight } from "@tabler/icons-react";
import { Link } from "@tanstack/react-router";
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
  loading: boolean;
  rows: Row[];
  moreHref: string;
  metric?: "credits" | "requests";
}

function BreakdownCard({
  title,
  description,
  loading,
  rows,
  moreHref,
  metric = "credits",
}: CardProps) {
  const { t } = useTranslation();
  const max = rows[0]?.value ?? 1;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
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
          <ul className="space-y-3">
            {rows.map((r) => (
              <li key={r.key} className="space-y-1">
                <div className="flex items-baseline justify-between gap-2 text-sm">
                  <div className="min-w-0 truncate font-medium">{r.label}</div>
                  <div className="tabular-nums text-muted-foreground">
                    {metric === "requests"
                      ? r.value.toLocaleString()
                      : r.value.toLocaleString(undefined, {
                          maximumFractionDigits: 2,
                        })}
                  </div>
                </div>
                <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <div
                    className="h-full bg-primary"
                    style={{ width: `${(r.value / max) * 100}%` }}
                  />
                </div>
                {r.sub ? (
                  <div className="text-xs text-muted-foreground">{r.sub}</div>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
