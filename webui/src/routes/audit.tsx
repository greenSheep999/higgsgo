import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import {
  IconClockHour5,
  IconDownload,
  IconRefresh,
} from "@tabler/icons-react";

import { admin, type AuditFilter } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
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
import { StatusBadge } from "@/components/ui/status-badge";

// Audit log page. Every filter maps to a native /admin/audit query
// param — we let the server do the work rather than fanning out into
// a client-side index that would drift on paged results. Export uses
// direct anchor tags so the browser handles the streaming download.

const METHODS = ["POST", "PUT", "PATCH", "DELETE"];
const RESOURCES = ["apikey", "account", "group", "job", "partner"];

function Audit() {
  const { t } = useTranslation();
  const [actor, setActor] = useState("");
  const [method, setMethod] = useState("all");
  const [resource, setResource] = useState("all");
  const [resourceId, setResourceId] = useState("");

  const filter: AuditFilter = {
    actor: actor || undefined,
    method: method === "all" ? undefined : method,
    resource_type: resource === "all" ? undefined : resource,
    resource_id: resourceId || undefined,
    limit: 200,
  };

  const q = useQuery({
    queryKey: ["admin", "audit", filter],
    queryFn: () => admin.listAudit(filter),
    refetchInterval: 30_000,
  });

  const rows = q.data?.data ?? [];

  const exportUrl = (format: "jsonl" | "csv") => {
    const p = new URLSearchParams();
    p.set("format", format);
    if (filter.actor) p.set("actor", filter.actor);
    if (filter.method) p.set("method", filter.method);
    if (filter.resource_type) p.set("resource_type", filter.resource_type);
    if (filter.resource_id) p.set("resource_id", filter.resource_id);
    return `/admin/audit/export?${p.toString()}`;
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconClockHour5 className="size-5" /> {t("audit.title")}
        </CardTitle>
        <CardDescription>{t("audit.description")}</CardDescription>
        <CardAction className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => q.refetch()}
            disabled={q.isFetching}
          >
            <IconRefresh /> {t("common.refresh")}
          </Button>
          <Button variant="outline" size="sm" asChild>
            <a href={exportUrl("jsonl")} target="_blank" rel="noreferrer">
              <IconDownload /> {t("audit.exportJsonl")}
            </a>
          </Button>
          <Button variant="outline" size="sm" asChild>
            <a href={exportUrl("csv")} target="_blank" rel="noreferrer">
              <IconDownload /> {t("audit.exportCsv")}
            </a>
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2">
          <Input
            placeholder={t("audit.filters.actor")}
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            className="w-40"
          />
          <Select value={method} onValueChange={setMethod}>
            <SelectTrigger className="w-32">
              <SelectValue placeholder={t("audit.filters.method")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("audit.filters.allMethods")}</SelectItem>
              {METHODS.map((m) => (
                <SelectItem key={m} value={m}>
                  {m}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select value={resource} onValueChange={setResource}>
            <SelectTrigger className="w-40">
              <SelectValue placeholder={t("audit.filters.resourceType")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                {t("audit.filters.allResources")}
              </SelectItem>
              {RESOURCES.map((r) => (
                <SelectItem key={r} value={r}>
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Input
            placeholder={t("audit.filters.resourceId")}
            value={resourceId}
            onChange={(e) => setResourceId(e.target.value)}
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
                <TableHead>{t("audit.columns.time")}</TableHead>
                <TableHead>{t("audit.columns.actor")}</TableHead>
                <TableHead>{t("audit.columns.method")}</TableHead>
                <TableHead>{t("audit.columns.path")}</TableHead>
                <TableHead>{t("audit.columns.status")}</TableHead>
                <TableHead>{t("audit.columns.resource")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {q.isLoading ? (
                Array.from({ length: 5 }).map((_, i) => (
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
                    className="text-center text-sm text-muted-foreground"
                  >
                    {t("audit.empty")}
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell className="text-xs text-muted-foreground">
                      {formatDateTime(e.ts)}
                    </TableCell>
                    <TableCell className="font-mono text-xs">{e.actor}</TableCell>
                    <TableCell>
                      <StatusBadge tone="muted">{e.method}</StatusBadge>
                    </TableCell>
                    <TableCell className="max-w-64 truncate font-mono text-xs">
                      {e.path}
                    </TableCell>
                    <TableCell>
                      <StatusBadge
                        tone={
                          e.status >= 500
                            ? "danger"
                            : e.status >= 400
                              ? "warning"
                              : "success"
                        }
                      >
                        {e.status}
                      </StatusBadge>
                    </TableCell>
                    <TableCell className="text-xs">
                      {e.resource_type ? (
                        <span className="text-muted-foreground">
                          {e.resource_type}
                          {e.resource_id ? `·${e.resource_id.slice(0, 12)}` : ""}
                        </span>
                      ) : (
                        "—"
                      )}
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

export const auditRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/audit",
  staticData: { titleKey: "nav.audit" },
  component: Audit,
});
