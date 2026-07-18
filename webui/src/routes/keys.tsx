import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconCopy,
  IconDotsVertical,
  IconKey,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlus,
  IconRefresh,
  IconRotate,
  IconTrash,
} from "@tabler/icons-react";

import { admin, ApiError, type ApiKey } from "@/lib/api";
import { formatCredits, formatRelative } from "@/lib/format";
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
import { StatusBadge, keyStatusTone } from "@/components/ui/status-badge";
import { CreateKeyDialog } from "@/components/keys/create-dialog";
import { EditKeyDialog } from "@/components/keys/edit-dialog";
import { PlaintextRevealDialog } from "@/components/keys/plaintext-dialog";
import { useKeyGroups } from "@/components/keys/use-key-groups";
import { useKeyStats } from "@/components/keys/use-key-stats";
import { IconPencil } from "@tabler/icons-react";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

// Keys page wraps /admin/keys. Every mutating action funnels through a
// TanStack Query mutation so React Query owns the "in flight" state and
// re-fetches the list on success. Plaintext (create + rotate) is shown
// via PlaintextRevealDialog, which is the only surface that ever sees
// the secret — the caller never persists it anywhere.

function Keys() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [plaintext, setPlaintext] = useState<{
    key: string;
    label: string;
  } | null>(null);
  const [pendingRevoke, setPendingRevoke] = useState<ApiKey | null>(null);
  const [editing, setEditing] = useState<ApiKey | null>(null);

  const list = useQuery({
    queryKey: ["admin", "keys"],
    queryFn: admin.listKeys,
    refetchInterval: 30_000,
  });
  const { index: keyGroupIndex } = useKeyGroups();
  const { map: keyStatsMap } = useKeyStats();

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["admin", "keys"] });

  const rotate = useMutation({
    mutationFn: (id: string) => admin.rotateKey(id),
    onSuccess: (res, id) => {
      const row = (list.data ?? []).find((k) => k.id === id);
      setPlaintext({ key: res.key, label: row?.name ?? id });
      toast.success(t("keys.toasts.rotated", { name: row?.name ?? id }));
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => admin.revokeKey(id),
    onSuccess: (_r, id) => {
      const name = pendingRevoke?.name ?? id;
      toast.success(t("keys.toasts.revoked", { name }));
      setPendingRevoke(null);
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const pause = useMutation({
    mutationFn: (id: string) => admin.pauseKey(id),
    onSuccess: (_r, id) => {
      const name = (list.data ?? []).find((k) => k.id === id)?.name ?? id;
      toast.success(t("keys.toasts.paused", { name }));
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const resume = useMutation({
    mutationFn: (id: string) => admin.resumeKey(id),
    onSuccess: (_r, id) => {
      const name = (list.data ?? []).find((k) => k.id === id)?.name ?? id;
      toast.success(t("keys.toasts.resumed", { name }));
      invalidate();
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const resetUsage = useMutation({
    mutationFn: (id: string) => admin.resetKeyUsage(id),
    onSuccess: (_r, id) => {
      const name = (list.data ?? []).find((k) => k.id === id)?.name ?? id;
      toast.success(t("keys.toasts.usageReset", { name }));
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
            <IconKey className="size-5" /> {t("keys.title")}
          </CardTitle>
          <CardDescription>{t("keys.description")}</CardDescription>
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
              <IconPlus /> {t("keys.create")}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent>
          <div className="overflow-hidden rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  {/* Balance columns: name/key take the lion's share
                      of horizontal space; numeric + status columns are
                      pinned tight so the eye scans across the row
                      without playing hide-and-seek with the numbers. */}
                  {/* Widths add to 100%. Name+Key take 38% (long text
                      needs room); the rest are compact center-aligned
                      cells so the eye scans across without playing
                      hide-and-seek. Long-text columns (Name/Key) stay
                      left-aligned because centred prose reads badly. */}
                  {/* Widths add to ~100%. Name compresses, Key
                      expands because it's a long stable string. Type
                      goes right after Name so the operator can scan
                      "which of these are my own keys vs partner keys"
                      down the first two columns. */}
                  <TableHead className="w-[10%]">
                    {t("keys.columns.name")}
                  </TableHead>
                  <TableHead className="w-[7%] text-center">
                    {t("keys.columns.kind")}
                  </TableHead>
                  <TableHead className="w-[22%]">
                    {t("keys.columns.key")}
                  </TableHead>
                  <TableHead className="w-[9%] text-center">
                    {t("keys.columns.status")}
                  </TableHead>
                  <TableHead className="w-[9%] text-center">
                    {t("keys.groupsColumn")}
                  </TableHead>
                  <TableHead className="w-[11%] text-center">
                    {t("keys.columns.quota")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("keys.columns.requests")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("keys.columns.charged")}
                  </TableHead>
                  <TableHead className="w-[6%] text-center">
                    {t("keys.columns.free")}
                  </TableHead>
                  <TableHead className="w-[7%] text-center">
                    {t("keys.columns.lastUsed")}
                  </TableHead>
                  <TableHead className="w-[6%] text-right">
                    {t("keys.columns.actions")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.isLoading ? (
                  <SkeletonRows />
                ) : rows.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={11}
                      className="text-center text-sm text-muted-foreground"
                    >
                      {t("common.nothing")}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((k) => <KeyRow key={k.id} k={k} />)
                )}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

      <CreateKeyDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(res) => {
          setPlaintext({ key: res.plaintext_key, label: res.name });
          invalidate();
        }}
      />

      <PlaintextRevealDialog
        open={!!plaintext}
        label={plaintext?.label ?? ""}
        secret={plaintext?.key ?? ""}
        onOpenChange={(open) => !open && setPlaintext(null)}
      />

      <EditKeyDialog
        keyRow={editing}
        onOpenChange={(open) => !open && setEditing(null)}
      />

      <AlertDialog
        open={!!pendingRevoke}
        onOpenChange={(open) => !open && setPendingRevoke(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("keys.actions.revokeTitle")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t("keys.actions.revokeDescription")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                pendingRevoke && revoke.mutate(pendingRevoke.id)
              }
            >
              {t("keys.actions.revoke")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );

  // ------------------------------------------------------------------
  // Row renderer stays inside the function so the mutations above are in
  // scope — avoids threading five callbacks through props. React re-uses
  // the closure across renders anyway, so the extra allocation is not
  // meaningful at pool sizes we care about (<< 1000 keys).
  function KeyRow({ k }: { k: ApiKey }) {
    // Quota reads as "remaining" — bar fill = credits still available.
    // Matches the accounts card's mental model: green when the key
    // has plenty of headroom, red when it's almost exhausted.
    const remaining = Math.max(0, k.monthly_quota - k.monthly_used);
    const remainingPct =
      k.monthly_quota > 0
        ? Math.min(100, Math.max(0, (remaining / k.monthly_quota) * 100))
        : 0;
    const quotaBarColour =
      remainingPct >= 60
        ? "bg-emerald-500"
        : remainingPct >= 25
          ? "bg-amber-500"
          : "bg-red-500";
    const stats = keyStatsMap.get(k.id);
    const requests = stats?.requests ?? 0;
    const charged = stats?.chargedCreditsH ?? 0;
    const freeH = stats?.freeCreditsH ?? 0;
    // Tooltip breakdown used on the request-count column so the operator
    // can hover to see the completed / failed / refunded split without
    // adding three separate columns.
    const requestsBreakdown = stats
      ? `${stats.completed} completed · ${stats.failed} failed · ${stats.refunded} refunded`
      : "";
    return (
      <TableRow>
        <TableCell>
          <div className="truncate font-medium">{k.name || "—"}</div>
          {k.created_by ? (
            <div className="text-[10px] text-muted-foreground">
              by {k.created_by}
            </div>
          ) : null}
        </TableCell>
        <TableCell className="text-center">
          {/* Kind badge — brand tone for default (operator's own),
              muted for project (downstream partner keys). Tooltip
              explains the difference so an operator new to the
              console knows which one they're looking at. */}
          <Tooltip>
            <TooltipTrigger asChild>
              <span>
                <StatusBadge
                  tone={k.kind === "default" ? "brand" : "muted"}
                  className="cursor-help"
                >
                  {t(`keys.kind.${k.kind || "project"}`, {
                    defaultValue: k.kind || "project",
                  })}
                </StatusBadge>
              </span>
            </TooltipTrigger>
            <TooltipContent>
              {t(`keys.kind.${k.kind || "project"}Hint`)}
            </TooltipContent>
          </Tooltip>
        </TableCell>
        {/* Key column — shows the internal id, masked in the middle
            so a long id doesn't stretch the layout and shoulder-
            surfing can't read the full string at a glance. Copy
            grabs the untruncated id for pasting into other admin
            endpoints. We deliberately do NOT dress this up as an
            sk-hg-… stand-in: the plaintext isn't recoverable and
            faking it in the row would only mislead. */}
        {/* Key column — displays the prefix that matches this key's
            kind (sk-adm-… for default, sk-hg-… for project) with the
            body masked. The full plaintext is unrecoverable after
            create, so we never render it or hint at it. Copy grabs
            the internal id for referencing this key from other
            admin endpoints. */}
        <TableCell>
          <div className="flex items-center gap-1">
            <Tooltip>
              <TooltipTrigger asChild>
                <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs">
                  {maskedKey(k)}
                </code>
              </TooltipTrigger>
              <TooltipContent>
                {t(
                  k.kind === "default"
                    ? "keys.columns.keyHintAdmin"
                    : "keys.columns.keyHintProject",
                )}
              </TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 shrink-0"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(k.id);
                      toast.success(t("keys.columns.copiedId"));
                    } catch (err) {
                      toast.error(
                        err instanceof Error ? err.message : String(err),
                      );
                    }
                  }}
                >
                  <IconCopy className="size-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>
                {t("keys.columns.copyIdTooltip")}
              </TooltipContent>
            </Tooltip>
          </div>
        </TableCell>
        <TableCell className="text-center">
          <StatusBadge tone={keyStatusTone(k.status)}>
            {t(`keys.status.${k.status}`, { defaultValue: k.status })}
          </StatusBadge>
        </TableCell>
        <TableCell className="text-center">
          {(() => {
            // Show the first group badge inline; anything beyond
            // that condenses into a single "+N" badge whose tooltip
            // spells out the overflow set. Keeps the row narrow
            // for keys bound to many groups.
            const gs = keyGroupIndex.get(k.id) ?? [];
            if (gs.length === 0) {
              return <span className="text-xs text-muted-foreground">—</span>;
            }
            const shown = gs.slice(0, 1);
            const overflow = gs.slice(1);
            return (
              <div className="flex flex-wrap justify-center gap-1">
                {shown.map((g) => (
                  <StatusBadge key={g.id} tone="muted">
                    {g.name}
                  </StatusBadge>
                ))}
                {overflow.length > 0 ? (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className="inline-flex cursor-help items-center rounded-md border bg-muted/60 px-1.5 py-0.5 text-xs font-medium text-muted-foreground">
                        +{overflow.length}
                      </span>
                    </TooltipTrigger>
                    <TooltipContent>
                      <div className="max-w-56 space-y-0.5">
                        {overflow.map((g) => (
                          <div key={g.id} className="text-xs">
                            {g.name}
                          </div>
                        ))}
                      </div>
                    </TooltipContent>
                  </Tooltip>
                ) : null}
              </div>
            );
          })()}
        </TableCell>
        <TableCell className="text-center">
          {k.monthly_quota > 0 ? (
            // Bar left · "remaining / total (pct%)" number right. The
            // bar drains as the key spends, and its colour maps to
            // remaining headroom (green > amber > red).
            <div className="flex items-center gap-2">
              <div className="h-1.5 min-w-[3rem] flex-1 overflow-hidden rounded-full bg-muted">
                <div
                  className={`h-full transition-all ${quotaBarColour}`}
                  style={{ width: `${remainingPct}%` }}
                />
              </div>
              <span className="whitespace-nowrap text-[11px] tabular-nums text-muted-foreground">
                {formatCredits(remaining)} / {formatCredits(k.monthly_quota)} ({remainingPct.toFixed(0)}%)
              </span>
            </div>
          ) : (
            <div className="flex items-center justify-center gap-2 text-xs text-muted-foreground">
              <StatusBadge tone="brand">
                {t("keys.unlimitedShort")}
              </StatusBadge>
              <span>{formatCredits(k.monthly_used)} used</span>
            </div>
          )}
        </TableCell>
        {/* Requests — hover for completed/failed/refunded split. */}
        <TableCell className="text-center">
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="cursor-help tabular-nums">
                {requests.toLocaleString()}
              </span>
            </TooltipTrigger>
            {requests > 0 ? (
              <TooltipContent>{requestsBreakdown}</TooltipContent>
            ) : null}
          </Tooltip>
        </TableCell>
        {/* Charged — credits actually billed to the caller (drives
            revenue; comes off the subscription/extra pools). */}
        <TableCell className="text-center tabular-nums">
          {formatCredits(charged)}
        </TableCell>
        {/* Free — credits absorbed by the account's unlim / flex-unlim
            entitlement; the caller paid 0 for these. */}
        <TableCell className="text-center tabular-nums text-muted-foreground">
          {formatCredits(freeH)}
        </TableCell>
        <TableCell className="text-center text-xs text-muted-foreground">
          {formatRelative(k.last_used_at)}
        </TableCell>
        {/* Inline high-frequency actions as icon buttons; low-frequency
            (reset usage, revoke) stay in the "more" dropdown. */}
        <TableCell className="text-right">
          <div className="flex justify-end gap-0.5">
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  onClick={() => setEditing(k)}
                >
                  <IconPencil className="size-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("keys.actions.edit")}</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  onClick={() => rotate.mutate(k.id)}
                  disabled={k.status === "revoked"}
                >
                  <IconRotate className="size-4" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("keys.actions.rotate")}</TooltipContent>
            </Tooltip>
            {k.status === "paused" ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-8"
                    onClick={() => resume.mutate(k.id)}
                  >
                    <IconPlayerPlay className="size-4" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("keys.actions.resume")}</TooltipContent>
              </Tooltip>
            ) : (
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-8"
                    onClick={() => pause.mutate(k.id)}
                    disabled={k.status === "revoked"}
                  >
                    <IconPlayerPause className="size-4" />
                  </Button>
                </TooltipTrigger>
                <TooltipContent>{t("keys.actions.pause")}</TooltipContent>
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
                  onClick={() => resetUsage.mutate(k.id)}
                  disabled={k.monthly_used === 0}
                >
                  {t("keys.actions.resetUsage")}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  className="text-destructive focus:text-destructive"
                  onClick={() => setPendingRevoke(k)}
                  disabled={k.status === "revoked"}
                >
                  <IconTrash /> {t("keys.actions.revoke")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        </TableCell>
      </TableRow>
    );
  }
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
  if (err instanceof ApiError)
    return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}

// maskedKey renders `<prefix>••••<real last 4>`. The prefix comes from
// kind (sk-adm- for default, sk-hg- for project). The trailing 4 chars
// are the real tail of the plaintext (stored in key_last4 at create/
// rotate) so each row is visually distinguishable while still hiding
// enough of the key to be safe for shoulder-surf viewing. Rows created
// before migration 012 (empty key_last4) fall back to fully-masked.
function maskedKey(k: { kind: string; key_last4: string }): string {
  const prefix = k.kind === "default" ? "sk-adm-" : "sk-hg-";
  const dots = "•".repeat(12);
  return k.key_last4 ? `${prefix}${dots}${k.key_last4}` : `${prefix}${dots}`;
}

export const keysRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/keys",
  staticData: { titleKey: "nav.keys" },
  component: Keys,
});
