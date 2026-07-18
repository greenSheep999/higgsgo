import { useMemo, useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
  IconDotsVertical,
  IconDownload,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlus,
  IconRefresh,
  IconSearch,
  IconTrash,
} from "@tabler/icons-react";
import { toast } from "sonner";

import { admin, ApiError, type Account } from "@/lib/api";
import { formatCredits, formatRelative } from "@/lib/format";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { StatusBadge, accountStatusTone } from "@/components/ui/status-badge";
import {
  ToggleGroup,
  ToggleGroupItem,
} from "@/components/ui/toggle-group";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { AccountCardGrid } from "@/components/accounts/account-card-grid";
import { BulkActionBar } from "@/components/accounts/bulk-action-bar";
import { useAccountGroups } from "@/components/accounts/use-account-groups";
import { useAccountCounters } from "@/components/accounts/use-account-counters";
import { IconLayoutGrid, IconList } from "@tabler/icons-react";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
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
import { rootRoute } from "@/routes/root";
import { ImportAccountsDialog } from "@/components/accounts/import-dialog";
import { AccountDetailSheet } from "@/components/accounts/detail-sheet";

// The accounts page is the anchor slice of the WebUI redesign. It exercises
// list + filter + row actions (pause / resume / soft-delete) + import
// (three formats) + export. Every UI atom is a stock shadcn primitive
// (no custom-styled wrappers), and status colours come from the badge
// variant map rather than one-off Tailwind classes.

// View mode is persisted in localStorage so the operator's last choice
// survives navigation and reloads. The default is "card" — that's the
// scan-friendly view users want on first paint.
type ViewMode = "card" | "list";
const VIEW_KEY = "higgsgo.accountsView";
function readInitialView(): ViewMode {
  try {
    const v = localStorage.getItem(VIEW_KEY);
    return v === "list" ? "list" : "card";
  } catch {
    return "card";
  }
}

function Accounts() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState<string>("all");
  const [plan, setPlan] = useState<string>("all");
  const [detailId, setDetailId] = useState<string | null>(null);
  const [importOpen, setImportOpen] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Account | null>(null);
  const [view, setView] = useState<ViewMode>(readInitialView);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [bulkBusy, setBulkBusy] = useState(false);
  const [bulkPendingBan, setBulkPendingBan] = useState(false);
  const { index: groupIndex } = useAccountGroups();
  const { map: counters } = useAccountCounters(30);

  const list = useQuery({
    queryKey: ["admin", "accounts", { status, plan }],
    queryFn: () =>
      admin.listAccounts({
        status: status === "all" ? undefined : status,
        plan_type: plan === "all" ? undefined : plan,
      }),
    refetchInterval: 20_000,
  });

  const pause = useMutation({
    mutationFn: (id: string) => admin.pauseAccount(id),
    onSuccess: (_r, id) => {
      toast.success(t("accounts.toasts.paused", { id }));
      qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const resume = useMutation({
    mutationFn: (id: string) => admin.resumeAccount(id),
    onSuccess: (_r, id) => {
      toast.success(t("accounts.toasts.resumed", { id }));
      qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const softDelete = useMutation({
    mutationFn: (id: string) => admin.deleteAccount(id),
    onSuccess: (_r, id) => {
      toast.success(t("accounts.toasts.banned", { id }));
      qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
      setPendingDelete(null);
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  // Refresher tick: syncs /user + wallet for every account. Previously
  // lived on the standalone /tickers page but conceptually this is an
  // accounts-pool operation, so it belongs here.
  const refreshBalances = useMutation({
    mutationFn: admin.triggerRefresher,
    onSuccess: () => {
      toast.success(t("accounts.refreshBalances"));
      qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  // runBulk fans a mutation out over every selected account id and
  // aggregates the outcome into a single toast. The batch is bounded
  // (typically <100 rows) and each call is a distinct HTTP request —
  // if that ever becomes a hotspot we can add a POST /admin/accounts/
  // bulk_status endpoint on the server side. For now the simple fan-out
  // keeps the server surface flat.
  async function runBulk(kind: "pause" | "resume" | "ban") {
    const ids = Array.from(selected);
    if (ids.length === 0) return;
    setBulkBusy(true);
    const action =
      kind === "pause"
        ? admin.pauseAccount
        : kind === "resume"
          ? admin.resumeAccount
          : admin.deleteAccount;
    let ok = 0;
    let fail = 0;
    for (const id of ids) {
      try {
        await action(id);
        ok++;
      } catch {
        fail++;
      }
    }
    setBulkBusy(false);
    qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
    setSelected(new Set());
    setBulkPendingBan(false);
    toast.success(t("accounts.bulk.done", { ok, fail }));
  }

  const rows = useMemo(() => {
    const raw = list.data ?? [];
    const needle = search.trim().toLowerCase();
    if (!needle) return raw;
    return raw.filter(
      (a) =>
        a.email.toLowerCase().includes(needle) ||
        a.id.toLowerCase().includes(needle) ||
        a.workspace_id.toLowerCase().includes(needle),
    );
  }, [list.data, search]);

  const planOptions = useMemo(() => {
    const set = new Set<string>();
    (list.data ?? []).forEach((a) => a.plan_type && set.add(a.plan_type));
    return Array.from(set).sort();
  }, [list.data]);

  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle>{t("accounts.title")}</CardTitle>
          <CardDescription>{t("accounts.description")}</CardDescription>
          <CardAction className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => list.refetch()}
              disabled={list.isFetching}
            >
              <IconRefresh />
              {t("common.refresh")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => refreshBalances.mutate()}
              disabled={refreshBalances.isPending}
              title={t("accounts.refreshBalancesHint")}
            >
              <IconRefresh />
              {refreshBalances.isPending
                ? t("common.loading")
                : t("accounts.refreshBalances")}
            </Button>
            <Button variant="outline" size="sm" asChild>
              <a
                href="/admin/accounts/export?format=jsonl"
                target="_blank"
                rel="noreferrer"
              >
                <IconDownload />
                {t("common.export")}
              </a>
            </Button>
            <Button size="sm" onClick={() => setImportOpen(true)}>
              <IconPlus />
              {t("common.import")}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative">
              <IconSearch className="pointer-events-none absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
              <Input
                placeholder={t("accounts.filters.search")}
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                className="w-72 pl-8"
              />
            </div>
            <Select value={status} onValueChange={setStatus}>
              <SelectTrigger className="w-40">
                <SelectValue placeholder={t("accounts.filters.status")} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">
                  {t("accounts.filters.allStatuses")}
                </SelectItem>
                <SelectItem value="active">Active</SelectItem>
                <SelectItem value="suspended">Suspended</SelectItem>
                <SelectItem value="banned">Banned</SelectItem>
              </SelectContent>
            </Select>
            <Select value={plan} onValueChange={setPlan}>
              <SelectTrigger className="w-40">
                <SelectValue placeholder={t("accounts.filters.plan")} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">
                  {t("accounts.filters.allPlans")}
                </SelectItem>
                {planOptions.map((p) => (
                  <SelectItem key={p} value={p}>
                    {p}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <div className="ml-auto flex items-center gap-3 text-sm text-muted-foreground">
              <span>
                {list.isLoading
                  ? t("common.loading")
                  : t("accounts.filters.countOfTotal", {
                      count: rows.length,
                      total: (list.data ?? []).length,
                    })}
              </span>
              <ToggleGroup
                type="single"
                size="sm"
                variant="outline"
                value={view}
                onValueChange={(v) => {
                  if (!v) return;
                  const next = v as ViewMode;
                  setView(next);
                  try {
                    localStorage.setItem(VIEW_KEY, next);
                  } catch {
                    /* ignore */
                  }
                }}
              >
                <ToggleGroupItem value="card" aria-label={t("accounts.view.card")}>
                  <IconLayoutGrid className="size-4" />
                </ToggleGroupItem>
                <ToggleGroupItem value="list" aria-label={t("accounts.view.list")}>
                  <IconList className="size-4" />
                </ToggleGroupItem>
              </ToggleGroup>
            </div>
          </div>

          {list.isError ? (
            <p className="text-sm text-destructive">{errMsg(list.error)}</p>
          ) : !list.isLoading && rows.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("common.noneMatch")}
            </p>
          ) : view === "card" ? (
            <>
              <BulkActionBar
                count={selected.size}
                total={rows.length}
                busy={bulkBusy}
                onSelectAll={() =>
                  setSelected(new Set(rows.map((r) => r.id)))
                }
                onClear={() => setSelected(new Set())}
                onPause={() => runBulk("pause")}
                onResume={() => runBulk("resume")}
                onBan={() => setBulkPendingBan(true)}
              />
              <AccountCardGrid
                rows={rows}
                loading={list.isLoading}
                selected={selected}
                groupIndex={groupIndex}
                counters={counters}
                onToggleSelect={(id, next) => {
                  setSelected((prev) => {
                    const s = new Set(prev);
                    if (next) s.add(id);
                    else s.delete(id);
                    return s;
                  });
                }}
                onOpen={setDetailId}
                onPause={(id) => pause.mutate(id)}
                onResume={(id) => resume.mutate(id)}
                onBan={setPendingDelete}
                onRefresh={async (id) => {
                // Refetch the account row via /admin/accounts/{id} — this
                // updates the /accounts list cache in place for the row so
                // the operator gets the row's latest state without paying
                // the full list refetch cost.
                try {
                  const fresh = await admin.getAccount(id);
                  qc.setQueryData<Account[]>(
                    ["admin", "accounts", { status, plan }],
                    (prev) => {
                      if (!prev) return prev;
                      return prev.map((a) => (a.id === id ? fresh : a));
                    },
                  );
                  toast.success(t("common.refresh"));
                } catch (err) {
                  toast.error(errMsg(err));
                }
              }}
                onCopyId={async (id) => {
                  try {
                    await navigator.clipboard.writeText(id);
                    toast.success(t("accounts.card.copiedId"));
                  } catch (err) {
                    toast.error(
                      err instanceof Error ? err.message : String(err),
                    );
                  }
                }}
              />
            </>
          ) : (
            <div className="overflow-hidden rounded-md border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("accounts.table.email")}</TableHead>
                    <TableHead>{t("accounts.table.plan")}</TableHead>
                    <TableHead>{t("accounts.table.status")}</TableHead>
                    <TableHead className="text-right">
                      {t("accounts.table.subCredits")}
                    </TableHead>
                    <TableHead className="text-right">
                      {t("accounts.table.inFlight")}
                    </TableHead>
                    <TableHead>{t("accounts.table.lastUsed")}</TableHead>
                    <TableHead className="w-10" />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {list.isLoading ? (
                    <SkeletonRows />
                  ) : (
                    rows.map((a) => (
                      <TableRow
                        key={a.id}
                        className="cursor-pointer"
                        onClick={() => setDetailId(a.id)}
                      >
                        <TableCell>
                          <div className="font-medium">{a.email}</div>
                          <div className="text-xs text-muted-foreground">
                            {a.id.slice(0, 12)}…
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap gap-1">
                            <StatusBadge tone="muted">
                              {a.plan_type || t("accounts.tags.noPlan")}
                            </StatusBadge>
                            {a.has_unlim ? (
                              <StatusBadge tone="brand">
                                {t("accounts.tags.unlim")}
                              </StatusBadge>
                            ) : null}
                            {a.has_flex_unlim ? (
                              <StatusBadge tone="info">
                                {t("accounts.tags.flexUnlim")}
                              </StatusBadge>
                            ) : null}
                          </div>
                        </TableCell>
                        <TableCell>
                          <StatusBadge tone={accountStatusTone(a.status)}>
                            {t(`accounts.status.${a.status}`, {
                              defaultValue: a.status,
                            })}
                          </StatusBadge>
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatCredits(a.subscription_balance)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {a.in_flight_jobs}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {formatRelative(a.last_used_at)}
                        </TableCell>
                        <TableCell onClick={(e) => e.stopPropagation()}>
                          <DropdownMenu>
                            <DropdownMenuTrigger asChild>
                              <Button
                                variant="ghost"
                                size="icon"
                                className="size-8"
                              >
                                <IconDotsVertical className="size-4" />
                              </Button>
                            </DropdownMenuTrigger>
                            <DropdownMenuContent align="end">
                              {a.status === "suspended" ? (
                                <DropdownMenuItem
                                  onClick={() => resume.mutate(a.id)}
                                >
                                  <IconPlayerPlay />{" "}
                                  {t("accounts.actions.resume")}
                                </DropdownMenuItem>
                              ) : (
                                <DropdownMenuItem
                                  onClick={() => pause.mutate(a.id)}
                                  disabled={a.status === "banned"}
                                >
                                  <IconPlayerPause />{" "}
                                  {t("accounts.actions.pause")}
                                </DropdownMenuItem>
                              )}
                              <DropdownMenuSeparator />
                              <DropdownMenuItem
                                className="text-destructive focus:text-destructive"
                                disabled={a.status === "banned"}
                                onClick={() => setPendingDelete(a)}
                              >
                                <IconTrash /> {t("accounts.actions.ban")}
                              </DropdownMenuItem>
                            </DropdownMenuContent>
                          </DropdownMenu>
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

      <ImportAccountsDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        onImported={() =>
          qc.invalidateQueries({ queryKey: ["admin", "accounts"] })
        }
      />

      <AccountDetailSheet
        id={detailId}
        onOpenChange={(open) => !open && setDetailId(null)}
      />

      <AlertDialog
        open={!!pendingDelete}
        onOpenChange={(open) => !open && setPendingDelete(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("accounts.actions.banConfirmTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("accounts.actions.banConfirmDescription", {
                email: pendingDelete?.email ?? "",
              })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                pendingDelete && softDelete.mutate(pendingDelete.id)
              }
            >
              {t("accounts.actions.ban")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        open={bulkPendingBan}
        onOpenChange={(open) => !open && setBulkPendingBan(false)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("accounts.bulk.confirmBanTitle", { count: selected.size })}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("accounts.bulk.confirmBanDescription")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => runBulk("ban")}>
              {t("accounts.bulk.ban")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function SkeletonRows() {
  return (
    <>
      {Array.from({ length: 5 }).map((_, i) => (
        <TableRow key={i}>
          <TableCell colSpan={7}>
            <Skeleton className="h-6 w-full" />
          </TableCell>
        </TableRow>
      ))}
    </>
  );
}

function errMsg(err: unknown): string {
  if (err instanceof ApiError) return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}

export const accountsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/accounts",
  staticData: { titleKey: "nav.accounts" },
  component: Accounts,
});
