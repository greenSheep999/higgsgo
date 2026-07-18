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
  IconPencil,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlus,
  IconRefresh,
  IconTrash,
  IconUsersGroup,
} from "@tabler/icons-react";

import { admin, ApiError, type Group } from "@/lib/api";
import { formatCredits, formatDateTime } from "@/lib/format";
import { rootRoute } from "@/routes/root";
import { Button } from "@/components/ui/button";
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { StatusBadge, groupStatusTone } from "@/components/ui/status-badge";
import { CreateGroupDialog } from "@/components/groups/create-dialog";
import { EditGroupDialog } from "@/components/groups/edit-dialog";
import { parseAnchoredList } from "@/components/groups/model-multiselect";
import { useGroupCounts, useGroupStats } from "@/components/groups/use-group-stats";

// Groups page. Groups are the user-facing bucket that ties accounts and
// API keys together (see docs/POOL-AND-CPA.md). Same list-management
// shape as the Keys page: inline icon buttons for high-frequency
// actions (edit / pause-resume) with the low-frequency ones (reset
// usage, delete) in a "more" dropdown. Detail sheet handles member /
// binding management.

function Groups() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Group | null>(null);
  const [editing, setEditing] = useState<Group | null>(null);

  const list = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
    refetchInterval: 30_000,
  });

  const rows = list.data ?? [];
  const groupIds = rows.map((g) => g.id);
  const { map: counts } = useGroupCounts(groupIds);
  const { map: stats } = useGroupStats();

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["admin", "groups"] });

  const del = useMutation({
    mutationFn: (id: string) => admin.deleteGroup(id),
    onSuccess: (_r, id) => {
      const name = rows.find((g) => g.id === id)?.name ?? id;
      toast.success(t("groups.toasts.deleted", { name }));
      setPendingDelete(null);
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  // Pause / resume ride on PUT /admin/groups/{id} with just a status
  // change; the endpoint's update is field-merge so unrelated columns
  // are left untouched.
  const pause = useMutation({
    mutationFn: (id: string) => admin.updateGroup(id, { status: "paused" }),
    onSuccess: (_r, id) => {
      const name = rows.find((g) => g.id === id)?.name ?? id;
      toast.success(t("groups.toasts.paused", { name }));
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });
  const resume = useMutation({
    mutationFn: (id: string) => admin.updateGroup(id, { status: "active" }),
    onSuccess: (_r, id) => {
      const name = rows.find((g) => g.id === id)?.name ?? id;
      toast.success(t("groups.toasts.resumed", { name }));
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  // Note: there's no /admin/groups/{id}/reset_usage endpoint yet
  // (see internal/api/admin/groups.go). monthly_credit_used is only
  // written via IncrementUsed in the metering pipeline. Add the
  // action here once the backend gains a reset route so we don't
  // ship a button that looks functional but silently no-ops.

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
                  {/* Widths mirror the Keys table's rhythm — long
                      text (Name) left, all numeric / badge cells
                      centred so the eye can sweep a row without
                      chasing the numbers. Adds to ~100%. */}
                  <TableHead className="w-[3%] text-center text-muted-foreground">
                    #
                  </TableHead>
                  <TableHead className="w-[12%]">
                    {t("groups.columns.name")}
                  </TableHead>
                  <TableHead className="w-[7%] text-center">
                    {t("groups.columns.status")}
                  </TableHead>
                  <TableHead className="w-[8%] text-center">
                    {t("groups.columns.routing")}
                  </TableHead>
                  <TableHead className="w-[5%] text-center">
                    {t("groups.columns.members")}
                  </TableHead>
                  <TableHead className="w-[5%] text-center">
                    {t("groups.columns.bindings")}
                  </TableHead>
                  <TableHead className="w-[14%] text-center">
                    {t("groups.columns.models")}
                  </TableHead>
                  <TableHead className="w-[7%] text-center">
                    {t("groups.columns.concurrency")}
                  </TableHead>
                  <TableHead className="w-[15%] text-center">
                    {t("groups.columns.budget")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("groups.columns.charged")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("groups.columns.free")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("groups.columns.created")}
                  </TableHead>
                  <TableHead className="w-[6%] text-right">
                    {t("groups.columns.actions")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.isLoading ? (
                  <SkeletonRows />
                ) : rows.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={13}
                      className="text-center text-sm text-muted-foreground"
                    >
                      {t("common.nothing")}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((g, i) => (
                    <GroupRow
                      key={g.id}
                      g={g}
                      index={i + 1}
                      counts={counts.get(g.id)}
                      stats={stats.get(g.id)}
                      onEdit={setEditing}
                      onDelete={setPendingDelete}
                      onPause={(id) => pause.mutate(id)}
                      onResume={(id) => resume.mutate(id)}
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

      <EditGroupDialog
        group={editing}
        onOpenChange={(open) => !open && setEditing(null)}
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
  index,
  counts,
  stats,
  onEdit,
  onDelete,
  onPause,
  onResume,
}: {
  g: Group;
  index: number;
  counts: { accounts: number; keys: number } | undefined;
  stats:
    | {
        chargedCreditsH: number;
        freeCreditsH: number;
      }
    | undefined;
  onEdit: (g: Group) => void;
  onDelete: (g: Group) => void;
  onPause: (id: string) => void;
  onResume: (id: string) => void;
}) {
  const { t } = useTranslation();
  const hasBudget = g.monthly_credit_budget > 0;
  // Same "remaining" mental model as the Keys quota bar: bar fill =
  // headroom left, colour drains green → amber → red.
  const remaining = Math.max(
    0,
    g.monthly_credit_budget - g.monthly_credit_used,
  );
  const remainingPct = hasBudget
    ? Math.min(100, Math.max(0, (remaining / g.monthly_credit_budget) * 100))
    : 0;
  const budgetBarColour =
    remainingPct >= 60
      ? "bg-emerald-500"
      : remainingPct >= 25
        ? "bg-amber-500"
        : "bg-red-500";
  const status = g.status || "active";
  const isPaused = status === "paused";

  return (
    <TableRow>
      <TableCell className="text-center text-xs tabular-nums text-muted-foreground">
        #{index}
      </TableCell>
      <TableCell>
        <Link
          to="/groups/$id"
          params={{ id: g.id }}
          className="truncate font-medium hover:underline"
        >
          {g.name}
        </Link>
        {g.description ? (
          <div className="truncate text-[10px] text-muted-foreground">
            {g.description}
          </div>
        ) : null}
      </TableCell>
      <TableCell className="text-center">
        <StatusBadge tone={groupStatusTone(status)}>
          {t(`groups.status.${status}`, { defaultValue: status })}
        </StatusBadge>
      </TableCell>
      <TableCell className="text-center">
        <StatusBadge tone="muted">
          {g.route_strategy || "round_robin"}
        </StatusBadge>
      </TableCell>
      <TableCell className="text-center tabular-nums text-muted-foreground">
        {counts?.accounts ?? 0}
      </TableCell>
      <TableCell className="text-center tabular-nums text-muted-foreground">
        {counts?.keys ?? 0}
      </TableCell>
      <TableCell className="text-center">
        <ModelTagsCell
          allowed={g.allowed_models_regex}
          blocked={g.blocked_models_regex}
        />
      </TableCell>
      <TableCell className="text-center">
        {g.max_concurrent_jobs > 0 ? (
          <span className="tabular-nums">{g.max_concurrent_jobs}</span>
        ) : (
          <StatusBadge tone="brand">{t("common.unlimited")}</StatusBadge>
        )}
      </TableCell>
      {/* Budget — bar left / "remaining / total (pct%)" right,
          same shape as the Keys quota cell. */}
      <TableCell className="text-center">
        {hasBudget ? (
          <div className="flex items-center gap-2">
            <div className="h-1.5 min-w-[3rem] flex-1 overflow-hidden rounded-full bg-muted">
              <div
                className={`h-full transition-all ${budgetBarColour}`}
                style={{ width: `${remainingPct}%` }}
              />
            </div>
            <span className="whitespace-nowrap text-[11px] tabular-nums text-muted-foreground">
              {formatCredits(remaining)} / {formatCredits(g.monthly_credit_budget)} ({remainingPct.toFixed(0)}%)
            </span>
          </div>
        ) : (
          <StatusBadge tone="brand">{t("common.unlimited")}</StatusBadge>
        )}
      </TableCell>
      <TableCell className="text-center tabular-nums">
        {formatCredits(stats?.chargedCreditsH ?? 0)}
      </TableCell>
      <TableCell className="text-center tabular-nums text-muted-foreground">
        {formatCredits(stats?.freeCreditsH ?? 0)}
      </TableCell>
      <TableCell className="text-center text-xs text-muted-foreground">
        {formatDateTime(g.created_at)}
      </TableCell>
      {/* Inline high-frequency actions as icon buttons; low-frequency
          (reset usage, delete) stay in the "more" dropdown. Matches
          the Keys row so the eye lands on the same slot for both
          tables. */}
      <TableCell className="text-right">
        <div className="flex justify-end gap-0.5">
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="size-8"
                onClick={() => onEdit(g)}
              >
                <IconPencil className="size-4" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>{t("groups.actions.edit")}</TooltipContent>
          </Tooltip>
          {isPaused ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  onClick={() => onResume(g.id)}
                >
                  <IconPlayerPlay className="size-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("groups.actions.resume")}</TooltipContent>
            </Tooltip>
          ) : (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  onClick={() => onPause(g.id)}
                >
                  <IconPlayerPause className="size-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("groups.actions.pause")}</TooltipContent>
            </Tooltip>
          )}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" className="size-8">
                <IconDotsVertical className="size-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem
                className="text-destructive focus:text-destructive"
                onClick={() => onDelete(g)}
              >
                <IconTrash /> {t("groups.actions.delete")}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </TableCell>
    </TableRow>
  );
}

function SkeletonRows() {
  return (
    <>
      {Array.from({ length: 4 }).map((_, i) => (
        <TableRow key={i}>
          <TableCell colSpan={13}>
            <Skeleton className="h-6 w-full" />
          </TableCell>
        </TableRow>
      ))}
    </>
  );
}

// ModelTagsCell renders the group's model filter as two colour-coded
// rows — allowed on top (emerald), blocked below (rose). Each row
// shows up to MAX_INLINE tags; the rest collapse into a "+N" chip
// whose tooltip lists them all. Regexes that don't parse as an
// alias list (a hand-crafted `veo.*`) fall back to a single "custom"
// chip so operators know the picker isn't authoritative for that row.
function ModelTagsCell({
  allowed,
  blocked,
}: {
  allowed: string;
  blocked: string;
}) {
  const { t } = useTranslation();
  const bothEmpty = !allowed && !blocked;
  if (bothEmpty) {
    return (
      <span className="text-[10px] text-muted-foreground">
        {t("groups.models.any")}
      </span>
    );
  }
  return (
    <div className="flex flex-col items-center gap-1">
      <TagRow regex={allowed} tone="allowed" />
      <TagRow regex={blocked} tone="blocked" />
    </div>
  );
}

const MAX_INLINE = 3;

function TagRow({
  regex,
  tone,
}: {
  regex: string;
  tone: "allowed" | "blocked";
}) {
  const { t } = useTranslation();
  if (!regex) return null;
  const parsed = parseAnchoredList(regex);
  // Custom regex — surface a single warning chip so an operator can
  // tell picker output apart from hand-authored filters.
  if (parsed === null) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            className={`inline-flex cursor-help items-center rounded border px-1.5 py-0.5 font-mono text-[10px] ${
              tone === "allowed" ? RE_ALLOWED : RE_BLOCKED
            }`}
          >
            {t("groups.models.customRegex")}
          </span>
        </TooltipTrigger>
        <TooltipContent>
          <code className="text-xs">{regex}</code>
        </TooltipContent>
      </Tooltip>
    );
  }
  const aliases = parsed.aliases;
  if (aliases.length === 0) return null;
  const shown = aliases.slice(0, MAX_INLINE);
  const overflow = aliases.slice(MAX_INLINE);
  const chipClass =
    tone === "allowed" ? CHIP_ALLOWED : CHIP_BLOCKED;
  return (
    <div className="flex flex-wrap justify-center gap-0.5">
      {shown.map((a) => (
        <span
          key={a}
          className={`inline-flex items-center rounded border px-1.5 py-0.5 font-mono text-[10px] ${chipClass}`}
        >
          {a}
        </span>
      ))}
      {overflow.length > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              className={`inline-flex cursor-help items-center rounded border px-1.5 py-0.5 text-[10px] font-medium ${chipClass}`}
            >
              +{overflow.length}
            </span>
          </TooltipTrigger>
          <TooltipContent>
            <div className="flex max-w-56 flex-col gap-0.5">
              {overflow.map((a) => (
                <code key={a} className="text-xs">
                  {a}
                </code>
              ))}
            </div>
          </TooltipContent>
        </Tooltip>
      ) : null}
    </div>
  );
}

// Tag palettes. Solid-ish enough to read as "allow / deny" at a
// glance without competing with the row's status badge. Emerald +
// rose because the app already uses them for quota bars and destructive
// buttons respectively — so operators recognise the semantics.
const CHIP_ALLOWED =
  "border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300";
const CHIP_BLOCKED =
  "border-rose-500/40 bg-rose-500/10 text-rose-700 dark:text-rose-300";
// Slightly softer variants for the "custom regex" fallback chip so it
// reads as an advisory, not a data tag.
const RE_ALLOWED =
  "border-emerald-500/30 bg-emerald-500/5 text-emerald-700/80 dark:text-emerald-300/80";
const RE_BLOCKED =
  "border-rose-500/30 bg-rose-500/5 text-rose-700/80 dark:text-rose-300/80";

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
