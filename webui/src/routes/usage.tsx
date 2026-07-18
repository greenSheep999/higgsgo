import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { IconChartBar, IconRefresh } from "@tabler/icons-react";

import { admin, type UsageDim } from "@/lib/api";
import { formatCredits, formatDateTime } from "@/lib/format";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusBadge, type StatusTone } from "@/components/ui/status-badge";
import { presetWindow, type WindowPreset } from "@/lib/time-window";
import { UsageTrend } from "@/components/usage/usage-trend";

// Usage page — two tabs. "Events" is the raw /admin/usage stream so an
// operator can zoom into a specific request. "Aggregate" reuses the
// /admin/usage/aggregate endpoint with a single group-by dim picker,
// giving a lightweight rollup without cloning the dashboard trend.

const STATUS_TONE: Record<string, StatusTone> = {
  completed: "success",
  failed: "danger",
  refunded: "warning",
  timeout: "danger",
};

const GROUP_BY_OPTIONS: { value: UsageDim; label: string }[] = [
  { value: "model_alias", label: "model" },
  { value: "api_key_id", label: "api key" },
  { value: "account_id", label: "account" },
  { value: "group_id", label: "group" },
  { value: "media_type", label: "media" },
  { value: "billing_day", label: "day" },
  { value: "billing_hour", label: "hour" },
];

function Usage() {
  const { t } = useTranslation();
  const [win, setWin] = useState<WindowPreset>("24h");
  const [dim, setDim] = useState<UsageDim>("model_alias");
  const window = presetWindow(win);

  const events = useQuery({
    queryKey: ["admin", "usage", "events", window.since, window.until],
    queryFn: () =>
      admin.listUsage({
        since: window.since,
        until: window.until,
        limit: 200,
      }),
    refetchInterval: 30_000,
  });

  const agg = useQuery({
    queryKey: ["admin", "usage", "agg", window.since, window.until, dim],
    queryFn: () =>
      admin.aggregateUsage({
        since: window.since,
        until: window.until,
        group_by: [dim],
      }),
    refetchInterval: 30_000,
  });

  const evRows = events.data?.data ?? [];
  const aggRows = (agg.data ?? []).slice().sort(
    (a, b) => b.charged_credits_h - a.charged_credits_h,
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconChartBar className="size-5" /> {t("usage.title")}
        </CardTitle>
        <CardDescription>{t("usage.description")}</CardDescription>
        <CardAction className="flex flex-wrap gap-2">
          <Select value={win} onValueChange={(v) => setWin(v as WindowPreset)}>
            <SelectTrigger size="sm" className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="24h">Last 24h</SelectItem>
              <SelectItem value="7d">Last 7d</SelectItem>
              <SelectItem value="15d">Last 15d</SelectItem>
              <SelectItem value="30d">Last 30d</SelectItem>
            </SelectContent>
          </Select>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              events.refetch();
              agg.refetch();
            }}
          >
            <IconRefresh /> {t("common.refresh")}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <UsageTrend window={window} />
        <Tabs defaultValue="events">
          <TabsList>
            <TabsTrigger value="events">{t("usage.tabs.events")}</TabsTrigger>
            <TabsTrigger value="aggregate">
              {t("usage.tabs.aggregate")}
            </TabsTrigger>
          </TabsList>
          <TabsContent value="events" className="pt-4">
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("usage.columns.time")}</TableHead>
                    <TableHead>{t("usage.columns.model")}</TableHead>
                    <TableHead>{t("usage.columns.account")}</TableHead>
                    <TableHead>{t("usage.columns.key")}</TableHead>
                    <TableHead>{t("usage.columns.status")}</TableHead>
                    <TableHead className="text-right">
                      {t("usage.columns.charged")}
                    </TableHead>
                    <TableHead className="text-right">
                      {t("usage.columns.latency")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {events.isLoading ? (
                    Array.from({ length: 6 }).map((_, i) => (
                      <TableRow key={i}>
                        <TableCell colSpan={7}>
                          <Skeleton className="h-6 w-full" />
                        </TableCell>
                      </TableRow>
                    ))
                  ) : evRows.length === 0 ? (
                    <TableRow>
                      <TableCell
                        colSpan={7}
                        className="text-center text-sm text-muted-foreground"
                      >
                        {t("usage.empty")}
                      </TableCell>
                    </TableRow>
                  ) : (
                    evRows.map((e) => (
                      <TableRow key={e.id}>
                        <TableCell className="text-xs text-muted-foreground">
                          {formatDateTime(e.ts)}
                        </TableCell>
                        <TableCell>{e.model_alias}</TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">
                          {e.account_id.slice(0, 12)}…
                        </TableCell>
                        <TableCell className="font-mono text-xs text-muted-foreground">
                          {e.api_key_id ? e.api_key_id.slice(0, 12) + "…" : "—"}
                        </TableCell>
                        <TableCell>
                          <StatusBadge tone={STATUS_TONE[e.status] ?? "muted"}>
                            {e.status}
                          </StatusBadge>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatCredits(e.charged_credits_h)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {e.latency_ms != null ? `${e.latency_ms} ms` : "—"}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>
          </TabsContent>
          <TabsContent value="aggregate" className="space-y-4 pt-4">
            <div className="flex items-center gap-2">
              <span className="text-sm text-muted-foreground">
                {t("usage.aggregate.groupBy")}
              </span>
              <Select value={dim} onValueChange={(v) => setDim(v as UsageDim)}>
                <SelectTrigger size="sm" className="w-40">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {GROUP_BY_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{dim}</TableHead>
                    <TableHead className="text-right">
                      {t("usage.aggregate.requests")}
                    </TableHead>
                    <TableHead className="text-right">
                      {t("usage.aggregate.credits")}
                    </TableHead>
                    <TableHead className="text-right">
                      {t("usage.aggregate.latency")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {agg.isLoading ? (
                    Array.from({ length: 4 }).map((_, i) => (
                      <TableRow key={i}>
                        <TableCell colSpan={4}>
                          <Skeleton className="h-6 w-full" />
                        </TableCell>
                      </TableRow>
                    ))
                  ) : aggRows.length === 0 ? (
                    <TableRow>
                      <TableCell
                        colSpan={4}
                        className="text-center text-sm text-muted-foreground"
                      >
                        {t("usage.empty")}
                      </TableCell>
                    </TableRow>
                  ) : (
                    aggRows.map((r, idx) => (
                      <TableRow key={idx}>
                        <TableCell className="font-mono text-xs">
                          {r.keys[dim] || "—"}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {r.request_count.toLocaleString()}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatCredits(r.charged_credits_h)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {r.avg_latency_ms
                            ? `${Math.round(r.avg_latency_ms)} ms`
                            : "—"}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>
          </TabsContent>
        </Tabs>
      </CardContent>
    </Card>
  );
}

export const usageRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/usage",
  staticData: { titleKey: "nav.usage" },
  component: Usage,
});
