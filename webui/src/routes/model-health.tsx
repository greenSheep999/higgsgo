import { useMemo, useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { IconBolt, IconHeartRateMonitor, IconRefresh } from "@tabler/icons-react";

import { admin, ApiError } from "@/lib/api";
import { formatRelative } from "@/lib/format";
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

// Model health page — flat table of the latest verdict per JST from the
// regression ticker. No paging (the endpoint returns everything;
// regression targets are always a bounded set).

const VERDICT_TONE: Record<string, StatusTone> = {
  ok: "success",
  completed: "success",
  failed: "danger",
  timeout: "danger",
  pending: "warning",
};

function ModelHealth() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [verdict, setVerdict] = useState<string>("all");

  const q = useQuery({
    queryKey: ["admin", "model-health", verdict],
    queryFn: () =>
      admin.listModelHealth({
        verdict: verdict === "all" ? undefined : verdict,
      }),
    refetchInterval: 30_000,
  });

  // Manually kick the regression ticker so operators can populate an
  // empty table without waiting for the next scheduled tick.
  const probe = useMutation({
    mutationFn: admin.triggerRegression,
    onSuccess: () => {
      toast.success(t("modelHealth.probe"));
      qc.invalidateQueries({ queryKey: ["admin", "model-health"] });
    },
    onError: (err) => {
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "probe failed",
      );
    },
  });

  const rows = useMemo(() => q.data ?? [], [q.data]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconHeartRateMonitor className="size-5" /> {t("modelHealth.title")}
        </CardTitle>
        <CardDescription>{t("modelHealth.description")}</CardDescription>
        <CardAction className="flex gap-2">
          <Select value={verdict} onValueChange={setVerdict}>
            <SelectTrigger size="sm" className="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("modelHealth.filters.allVerdicts")}</SelectItem>
              <SelectItem value="ok">{t("modelHealth.verdict.ok")}</SelectItem>
              <SelectItem value="completed">{t("modelHealth.verdict.completed")}</SelectItem>
              <SelectItem value="failed">{t("modelHealth.verdict.failed")}</SelectItem>
              <SelectItem value="timeout">{t("modelHealth.verdict.timeout")}</SelectItem>
              <SelectItem value="pending">{t("modelHealth.verdict.pending")}</SelectItem>
            </SelectContent>
          </Select>
          <Button
            variant="outline"
            size="sm"
            onClick={() => q.refetch()}
            disabled={q.isFetching}
          >
            <IconRefresh /> {t("common.refresh")}
          </Button>
          <Button
            size="sm"
            onClick={() => probe.mutate()}
            disabled={probe.isPending}
            title={t("modelHealth.probeHint")}
          >
            <IconBolt />
            {probe.isPending ? t("common.loading") : t("modelHealth.probe")}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <div className="overflow-hidden rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("modelHealth.columns.jst")}</TableHead>
                <TableHead>{t("modelHealth.columns.verdict")}</TableHead>
                <TableHead className="text-right">
                  {t("modelHealth.columns.httpStatus")}
                </TableHead>
                <TableHead className="text-right">
                  {t("modelHealth.columns.cost")}
                </TableHead>
                <TableHead className="text-right">
                  {t("modelHealth.columns.pollTime")}
                </TableHead>
                <TableHead>{t("modelHealth.columns.checkedAt")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {q.isLoading ? (
                Array.from({ length: 4 }).map((_, i) => (
                  <TableRow key={i}>
                    <TableCell colSpan={6}>
                      <Skeleton className="h-6 w-full" />
                    </TableCell>
                  </TableRow>
                ))
              ) : rows.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={6}
                    className="py-8 text-center text-sm text-muted-foreground"
                  >
                    <div className="space-y-2">
                      <div>{t("modelHealth.emptyCta")}</div>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => probe.mutate()}
                        disabled={probe.isPending}
                      >
                        <IconBolt />
                        {probe.isPending
                          ? t("common.loading")
                          : t("modelHealth.probe")}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((r) => (
                  <TableRow key={r.jst}>
                    <TableCell className="font-mono text-xs">{r.jst}</TableCell>
                    <TableCell>
                      <StatusBadge tone={VERDICT_TONE[r.verdict] ?? "muted"}>
                        {t(`modelHealth.verdict.${r.verdict}`, {
                          defaultValue: r.verdict,
                        })}
                      </StatusBadge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.http_status ?? "—"}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.cost != null ? r.cost.toLocaleString(undefined, { maximumFractionDigits: 2 }) : "—"}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.poll_time_sec != null
                        ? `${r.poll_time_sec.toFixed(1)}s`
                        : "—"}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(r.checked_at)}
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  );
}

export const modelHealthRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/model-health",
  staticData: { titleKey: "nav.modelHealth" },
  component: ModelHealth,
});
