import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { IconUserPlus, IconRefresh } from "@tabler/icons-react";

import { admin, ApiError, type Registration } from "@/lib/api";
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

// Registrations page. Skeleton-only surface: lists rows returned by
// /admin/registrations if the deploy actually has a real Registrar,
// otherwise renders a clear "build with -tags register" hint driven
// off the ApiError.type === "registrar_disabled" envelope the Go
// handler emits.
//
// Enqueue button is intentionally disabled — real registration input
// (proxy pool selection, mailbox provider, captcha budget) is
// coupled to the higgsfield adapter's Deps struct and will land
// alongside the port implementation.

function statusTone(status: string): StatusTone {
  switch (status) {
    case "success":
      return "success";
    case "running":
      return "info";
    case "failed":
      return "danger";
    case "pending":
      return "warning";
    default:
      return "muted";
  }
}

function Registrations() {
  const { t } = useTranslation();

  const q = useQuery<{ data: Registration[]; limit: number; offset: number }>({
    queryKey: ["admin", "registrations"],
    queryFn: () => admin.listRegistrations({ limit: 200 }),
    // ApiError.type === "registrar_disabled" is expected in slim
    // builds; suppress retries so the UI settles quickly and we can
    // read the envelope off the query.error to render the opt-in
    // hint. Any other error still gets React Query's default retry.
    retry: (failureCount, error) => {
      if (error instanceof ApiError && error.type === "registrar_disabled") {
        return false;
      }
      return failureCount < 3;
    },
  });

  const disabled =
    q.error instanceof ApiError && q.error.type === "registrar_disabled";

  const rows = q.data?.data ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconUserPlus className="size-5" /> {t("registrations.title")}
        </CardTitle>
        <CardDescription>{t("registrations.description")}</CardDescription>
        <CardAction className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => q.refetch()}
            disabled={q.isFetching}
          >
            <IconRefresh /> {t("common.refresh")}
          </Button>
          {/* Enqueue is intentionally disabled — real signup input
              lands with the register build tag. Keep the button in
              the layout so operators see the eventual surface. */}
          <Button
            variant="default"
            size="sm"
            disabled
            title={t("registrations.enqueueHint")}
          >
            <IconUserPlus /> {t("registrations.enqueue")}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="space-y-4">
        {disabled ? (
          <div className="rounded-md border border-amber-500/30 bg-amber-500/5 p-4 text-sm">
            <div className="mb-1 font-medium text-amber-700 dark:text-amber-300">
              {t("registrations.disabledTitle")}
            </div>
            <div className="text-muted-foreground">
              {t("registrations.disabledHint")}
            </div>
          </div>
        ) : (
          <div className="overflow-hidden rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("registrations.columns.id")}</TableHead>
                  <TableHead>{t("registrations.columns.email")}</TableHead>
                  <TableHead>{t("registrations.columns.status")}</TableHead>
                  <TableHead>{t("registrations.columns.attempts")}</TableHead>
                  <TableHead>{t("registrations.columns.account")}</TableHead>
                  <TableHead>{t("registrations.columns.created")}</TableHead>
                  <TableHead>{t("registrations.columns.finished")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {q.isLoading ? (
                  Array.from({ length: 3 }).map((_, i) => (
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
                      {t("registrations.empty")}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((r) => (
                    <TableRow key={r.id}>
                      <TableCell className="font-mono text-xs">{r.id}</TableCell>
                      <TableCell>{r.email}</TableCell>
                      <TableCell>
                        <StatusBadge tone={statusTone(r.status)}>
                          {t(`registrations.status.${r.status}`, r.status)}
                        </StatusBadge>
                      </TableCell>
                      <TableCell>{r.attempts}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {r.account_id || "—"}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {r.created_at ? formatDateTime(r.created_at) : "—"}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {r.finished_at ? formatDateTime(r.finished_at) : "—"}
                      </TableCell>
                    </TableRow>
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export const registrationsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/registrations",
  staticData: { titleKey: "nav.registrations" },
  component: Registrations,
});
