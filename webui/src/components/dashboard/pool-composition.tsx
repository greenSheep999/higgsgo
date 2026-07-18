import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { admin, type PoolStats } from "@/lib/api";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

// PoolComposition shows how the account pool is distributed across plans
// and statuses. Both are time-invariant (they describe the pool right now,
// not activity), so they don't take a TimeWindow prop. The bars use a
// per-card leader for their max width so a fat "unknown" bucket doesn't
// crush the rest of the plans into 1-pixel slivers.

const STATUS_COLOR: Record<string, string> = {
  active: "bg-primary",
  suspended: "bg-secondary",
  banned: "bg-destructive",
};

export function PoolComposition() {
  const { t } = useTranslation();
  const pool = useQuery({
    queryKey: ["admin", "stats", "pool"],
    queryFn: admin.poolStats,
    refetchInterval: 30_000,
  });

  return (
    <div className="grid grid-cols-1 gap-4 @xl/main:grid-cols-2">
      <DistCard
        title={t("dashboard.composition.byPlan")}
        description={t("dashboard.composition.byPlanDesc")}
        data={pool.data}
        loading={pool.isLoading}
        pick={(s) => s.by_plan}
        colorClass={() => "bg-primary"}
        emptyLabel={t("dashboard.composition.emptyPlans")}
      />
      <DistCard
        title={t("dashboard.composition.byStatus")}
        description={t("dashboard.composition.byStatusDesc")}
        data={pool.data}
        loading={pool.isLoading}
        pick={(s) => s.by_status}
        colorClass={(k) => STATUS_COLOR[k] ?? "bg-primary"}
        emptyLabel={t("dashboard.composition.emptyStatuses")}
      />
    </div>
  );
}

interface CardProps {
  title: string;
  description: string;
  data: PoolStats | undefined;
  loading: boolean;
  pick: (s: PoolStats) => Record<string, number>;
  colorClass: (bucket: string) => string;
  emptyLabel: string;
}

function DistCard({
  title,
  description,
  data,
  loading,
  pick,
  colorClass,
  emptyLabel,
}: CardProps) {
  const entries = data
    ? Object.entries(pick(data))
        .filter(([, v]) => v > 0)
        .sort((a, b) => b[1] - a[1])
    : [];
  const total = entries.reduce((sum, [, v]) => sum + v, 0);
  const max = entries[0]?.[1] ?? 1;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <div className="space-y-2">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-6 w-full" />
            ))}
          </div>
        ) : entries.length === 0 ? (
          <p className="text-sm text-muted-foreground">{emptyLabel}</p>
        ) : (
          <ul className="space-y-3">
            {entries.map(([bucket, count]) => {
              const pct = total > 0 ? (count / total) * 100 : 0;
              return (
                <li key={bucket} className="space-y-1">
                  <div className="flex items-baseline justify-between gap-2 text-sm">
                    <span className="min-w-0 truncate font-medium">
                      {bucket || "(unset)"}
                    </span>
                    <span className="tabular-nums text-muted-foreground">
                      {count.toLocaleString()}{" "}
                      <span className="text-xs">({pct.toFixed(1)}%)</span>
                    </span>
                  </div>
                  <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                    <div
                      className={`h-full ${colorClass(bucket)}`}
                      style={{ width: `${(count / max) * 100}%` }}
                    />
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}
