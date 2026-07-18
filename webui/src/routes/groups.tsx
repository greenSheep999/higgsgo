import { useState } from "react";
import { createRoute, Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconDotsVertical,
  IconPlus,
  IconRefresh,
  IconTrash,
  IconUsersGroup,
} from "@tabler/icons-react";

import { admin, ApiError, type Group } from "@/lib/api";
import { formatCredits, formatDateTime } from "@/lib/format";
import { rootRoute } from "@/routes/root";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { StatusBadge } from "@/components/ui/status-badge";
import { CreateGroupDialog } from "@/components/groups/create-dialog";

// Groups page. Groups are the user-facing bucket that ties accounts and
// API keys together (see docs/POOL-AND-CPA.md). This page focuses on
// list + create + delete + budget visualisation; member / binding
// management lives on the detail sheet (deferred to the next slice so
// the initial CRUD lands quickly).

function Groups() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Group | null>(null);

  const list = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
    refetchInterval: 30_000,
  });

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["admin", "groups"] });

  const del = useMutation({
    mutationFn: (id: string) => admin.deleteGroup(id),
    onSuccess: (_r, id) => {
      const name =
        (list.data ?? []).find((g) => g.id === id)?.name ?? id;
      toast.success(t("groups.toasts.deleted", { name }));
      setPendingDelete(null);
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const rows = list.data ?? [];

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconUsersGroup className="size-5" /> {t("groups.title")}
          </CardTitle>
          <CardDescription>{t("groups.description")}</CardDescription>
          <CardAction className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => list.refetch()}
              disabled={list.isFetching}
            >
              <IconRefresh /> {t("common.refresh")}
            </Button>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              <IconPlus /> {t("groups.create")}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent>
          <div className="overflow-hidden rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("groups.columns.name")}</TableHead>
                  <TableHead>{t("groups.columns.description")}</TableHead>
                  <TableHead>{t("groups.columns.routing")}</TableHead>
                  <TableHead className="min-w-56">
                    {t("groups.columns.budget")}
                  </TableHead>
                  <TableHead>{t("groups.columns.created")}</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.isLoading ? (
                  <SkeletonRows />
                ) : rows.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={6}
                      className="text-center text-sm text-muted-foreground"
                    >
                      {t("common.nothing")}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((g) => (
                    <GroupRow
                      key={g.id}
                      g={g}
                      onDelete={setPendingDelete}
                    />
                  ))
                )}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

      <CreateGroupDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={invalidate}
      />

      <AlertDialog
        open={!!pendingDelete}
        onOpenChange={(open) => !open && setPendingDelete(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("groups.actions.deleteTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("groups.actions.deleteDescription")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => pendingDelete && del.mutate(pendingDelete.id)}
            >
              {t("groups.actions.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function GroupRow({
  g,
  onDelete,
}: {
  g: Group;
  onDelete: (g: Group) => void;
}) {
  const { t } = useTranslation();
  const hasBudget = g.monthly_credit_budget > 0;
  const pct = hasBudget
    ? Math.min(100, Math.max(0, (g.monthly_credit_used / g.monthly_credit_budget) * 100))
    : 0;
  return (
    <TableRow>
      <TableCell>
        <Link
          to="/groups/$id"
          params={{ id: g.id }}
          className="font-medium hover:underline"
        >
          {g.name}
        </Link>
        <div className="text-xs text-muted-foreground">{g.id.slice(0, 12)}…</div>
      </TableCell>
      <TableCell className="max-w-64 truncate text-muted-foreground">
        {g.description || "—"}
      </TableCell>
      <TableCell>
        <div className="flex flex-wrap gap-1">
          <StatusBadge tone="muted">
            {g.route_strategy || "round_robin"}
          </StatusBadge>
          {g.max_concurrent_jobs > 0 ? (
            <StatusBadge tone="info">
              cap {g.max_concurrent_jobs}
            </StatusBadge>
          ) : null}
        </div>
      </TableCell>
      <TableCell>
        {hasBudget ? (
          <div className="space-y-1">
            <div className="flex items-baseline justify-between text-xs text-muted-foreground">
              <span>
                {t("groups.budgetProgress", {
                  used: formatCredits(g.monthly_credit_used),
                  budget: formatCredits(g.monthly_credit_budget),
                })}
              </span>
              <span className="tabular-nums">{pct.toFixed(0)}%</span>
            </div>
            <Progress value={pct} className="h-1.5" />
          </div>
        ) : (
          <StatusBadge tone="brand">{t("groups.noBudget")}</StatusBadge>
        )}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        {formatDateTime(g.created_at)}
      </TableCell>
      <TableCell>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8">
              <IconDotsVertical className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem disabled>
              {t("groups.actions.edit")}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="text-destructive focus:text-destructive"
              onClick={() => onDelete(g)}
            >
              <IconTrash /> {t("groups.actions.delete")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </TableCell>
    </TableRow>
  );
}

function SkeletonRows() {
  return (
    <>
      {Array.from({ length: 4 }).map((_, i) => (
        <TableRow key={i}>
          <TableCell colSpan={6}>
            <Skeleton className="h-6 w-full" />
          </TableCell>
        </TableRow>
      ))}
    </>
  );
}

function errMsg(err: unknown): string {
  if (err instanceof ApiError)
    return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}

export const groupsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/groups",
  staticData: { titleKey: "nav.groups" },
  component: Groups,
});
