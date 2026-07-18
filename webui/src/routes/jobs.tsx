import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { IconClipboardList, IconRefresh } from "@tabler/icons-react";

import { admin, type Job, type JobFilter } from "@/lib/api";
import { formatCredits, formatDateTime, formatRelative } from "@/lib/format";
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
import { Input } from "@/components/ui/input";
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
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { StatusBadge, type StatusTone } from "@/components/ui/status-badge";

// Jobs page — filterable list of every /v1 generation flowing through
// higgsgo. Clicking a row opens a right-side detail sheet with the full
// jobView. Filters map 1:1 to /admin/jobs query params; the backend
// caps limit at 500, so we fetch 200 at a time.

const STATUSES = ["completed", "failed", "refunded", "timeout", "pending", "in_progress"];
const STATUS_TONE: Record<string, StatusTone> = {
  completed: "success",
  in_progress: "info",
  pending: "info",
  failed: "danger",
  timeout: "danger",
  refunded: "warning",
};

function Jobs() {
  const { t } = useTranslation();
  const [status, setStatus] = useState("all");
  const [model, setModel] = useState("");
  const [accountId, setAccountId] = useState("");
  const [keyId, setKeyId] = useState("");
  const [detail, setDetail] = useState<Job | null>(null);

  const filter: JobFilter = {
    status: status === "all" ? undefined : status,
    model_alias: model || undefined,
    account_id: accountId || undefined,
    api_key_id: keyId || undefined,
    limit: 200,
  };

  const q = useQuery({
    queryKey: ["admin", "jobs", filter],
    queryFn: () => admin.listJobs(filter),
    refetchInterval: 20_000,
  });

  const rows = q.data?.data ?? [];

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconClipboardList className="size-5" /> {t("jobs.title")}
          </CardTitle>
          <CardDescription>{t("jobs.description")}</CardDescription>
          <CardAction>
            <Button
              variant="outline"
              size="sm"
              onClick={() => q.refetch()}
              disabled={q.isFetching}
            >
              <IconRefresh /> {t("common.refresh")}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            <Select value={status} onValueChange={setStatus}>
              <SelectTrigger className="w-40">
                <SelectValue placeholder={t("jobs.filters.status")} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">
                  {t("jobs.filters.allStatuses")}
                </SelectItem>
                {STATUSES.map((s) => (
                  <SelectItem key={s} value={s}>
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Input
              placeholder={t("jobs.filters.model")}
              value={model}
              onChange={(e) => setModel(e.target.value)}
              className="w-48"
            />
            <Input
              placeholder={t("jobs.filters.account")}
              value={accountId}
              onChange={(e) => setAccountId(e.target.value)}
              className="w-56"
            />
            <Input
              placeholder={t("jobs.filters.apiKey")}
              value={keyId}
              onChange={(e) => setKeyId(e.target.value)}
              className="w-56"
            />
            <div className="ml-auto text-sm text-muted-foreground">
              {q.isLoading ? t("common.loading") : `${rows.length} rows`}
            </div>
          </div>

          <div className="overflow-hidden rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("jobs.columns.id")}</TableHead>
                  <TableHead>{t("jobs.columns.model")}</TableHead>
                  <TableHead>{t("jobs.columns.account")}</TableHead>
                  <TableHead>{t("jobs.columns.status")}</TableHead>
                  <TableHead className="text-right">
                    {t("jobs.columns.credits")}
                  </TableHead>
                  <TableHead className="text-right">
                    {t("jobs.columns.latency")}
                  </TableHead>
                  <TableHead>{t("jobs.columns.time")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {q.isLoading ? (
                  Array.from({ length: 6 }).map((_, i) => (
                    <TableRow key={i}>
                      <TableCell colSpan={7}>
                        <Skeleton className="h-6 w-full" />
                      </TableCell>
                    </TableRow>
                  ))
                ) : rows.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={7}
                      className="text-center text-sm text-muted-foreground"
                    >
                      {t("jobs.empty")}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((job) => (
                    <TableRow
                      key={job.id}
                      className="cursor-pointer"
                      onClick={() => setDetail(job)}
                    >
                      <TableCell className="font-mono text-xs">
                        {job.id.slice(0, 12)}…
                      </TableCell>
                      <TableCell>{job.model_alias}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {job.account_id.slice(0, 12)}…
                      </TableCell>
                      <TableCell>
                        <StatusBadge tone={STATUS_TONE[job.status] ?? "muted"}>
                          {job.status}
                        </StatusBadge>
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {job.charged_credits_h != null
                          ? formatCredits(job.charged_credits_h)
                          : "—"}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {job.latency_ms != null ? `${job.latency_ms} ms` : "—"}
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatRelative(job.request_ts)}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

      <Sheet open={!!detail} onOpenChange={(open) => !open && setDetail(null)}>
        <SheetContent className="w-full sm:max-w-lg overflow-y-auto">
          <SheetHeader>
            <SheetTitle>
              {t("jobs.detail.title", { id: detail?.id.slice(0, 12) ?? "" })}
            </SheetTitle>
            <SheetDescription>{detail?.model_alias}</SheetDescription>
          </SheetHeader>
          {detail ? <JobDetail job={detail} /> : null}
        </SheetContent>
      </Sheet>
    </>
  );
}

function JobDetail({ job }: { job: Job }) {
  const { t } = useTranslation();
  return (
    <div className="grid grid-cols-2 gap-x-4 gap-y-3 px-4 pb-4 text-sm">
      <Field label={t("jobs.detail.endpoint")} value={job.endpoint || "—"} />
      <Field label={t("jobs.detail.upstream")} value={job.upstream_job_id || "—"} mono />
      <Field
        label={t("jobs.detail.preBalance")}
        value={job.pre_balance_h != null ? formatCredits(job.pre_balance_h) : "—"}
      />
      <Field
        label={t("jobs.detail.actualCredits")}
        value={job.actual_credits_h != null ? formatCredits(job.actual_credits_h) : "—"}
      />
      <Field
        label={t("jobs.detail.chargedCredits")}
        value={job.charged_credits_h != null ? formatCredits(job.charged_credits_h) : "—"}
      />
      <Field label={t("jobs.detail.polls")} value={String(job.poll_count ?? 0)} />
      <Field label={t("jobs.detail.refunded")} value={job.refunded ? "yes" : "no"} />
      <Field label={t("jobs.detail.requestTs")} value={formatDateTime(job.request_ts)} />
      <Field
        label={t("jobs.detail.finishedAt")}
        value={job.finished_at ? formatDateTime(job.finished_at) : "—"}
      />
      {job.error ? (
        <>
          <Field
            label={t("jobs.detail.errorType")}
            value={job.error.type}
            className="col-span-2"
          />
          <Field
            label={t("jobs.detail.errorMessage")}
            value={job.error.message}
            className="col-span-2"
          />
        </>
      ) : null}
      {job.result_url ? (
        <div className="col-span-2 space-y-1">
          <div className="text-xs text-muted-foreground">
            {t("jobs.detail.resultUrl")}
          </div>
          <a
            href={job.result_url}
            target="_blank"
            rel="noreferrer"
            className="break-all font-mono text-xs text-primary hover:underline"
          >
            {job.result_url}
          </a>
        </div>
      ) : null}
    </div>
  );
}

function Field({
  label,
  value,
  mono,
  className,
}: {
  label: string;
  value: string;
  mono?: boolean;
  className?: string;
}) {
  return (
    <div className={`space-y-0.5 ${className ?? ""}`}>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={mono ? "break-all font-mono text-xs" : "text-sm"}>
        {value}
      </div>
    </div>
  );
}

export const jobsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/jobs",
  staticData: { titleKey: "nav.jobs" },
  component: Jobs,
});
