import { useLayoutEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  IconActivityHeartbeat,
  IconClipboardCopy,
  IconCrown,
  IconDotsVertical,
  IconEdit,
  IconEye,
  IconPlayerPause,
  IconPlayerPlay,
  IconRefresh,
  IconTrash,
} from "@tabler/icons-react";
import logoBrand from "@/assets/logo-brand.svg";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { StatusBadge, accountStatusTone } from "@/components/ui/status-badge";
import { formatCredits, formatDateTime } from "@/lib/format";
import type { Account } from "@/lib/api";
import type { AccountGroupIndex } from "./use-account-groups";

// AccountCardGrid — card view of the account pool.
//
// Header row (single line):
//   [checkbox]  email + id  ...  [Switch active/paused]
// The Switch on the top-right maps directly to /admin/accounts/{id}/
// pause + resume; banned rows show it disabled because the ban is a
// one-way soft-delete.
//
// Row 2 (still inside the header): the pill line — status, plan, tier,
// unlim flags, group memberships, and an explicit "disabled" pill on
// banned accounts. Every pill uses StatusBadge so the tone stays
// consistent with the dashboard.
//
// CardContent surfaces three sections:
//   (A) Subscription progress bar — the ONE bar on the card. It counts
//       DOWN: the fill represents the remaining subscription balance
//       against total_plan_credits. Bar colour shifts green → amber →
//       red as the account drains; the operator wanted a single glance
//       to tell healthy accounts from ones about to run out.
//   (B) The three "credit resources" — subscription, extra, free — laid
//       out as one triplet card so the operator reads them as parts of
//       one story instead of scattered numbers.
//   (C) Runtime status grid — concurrency, fail streak, proxy, plan end.
//       Plain numbers, no bars.
//
// CardFooter houses the actions. Detail / Refresh sit as icon buttons
// left-aligned, "More" opens a dropdown for copy, ban, and future
// dangerous operations. The 3-dot on the top of previous versions is
// gone per operator feedback.

const CONCURRENCY_CAP = 6; // upstream limit — matches Account.AvailableSlots.

// Card grid columns by container width (max 4):
//   default:  1  — mobile / narrow
//   @xl:      2  — small desktop
//   @5xl:     3  — medium desktop
//   @7xl:     4  — wide desktop (cap)
const CARD_GRID_CLASS =
  "grid grid-cols-1 gap-4 @xl/main:grid-cols-2 @5xl/main:grid-cols-3 @7xl/main:grid-cols-4";

interface Props {
  rows: Account[];
  loading: boolean;
  selected: Set<string>;
  groupIndex: AccountGroupIndex;
  counters: Map<string, { successes: number; failures: number }>;
  onToggleSelect: (id: string, next: boolean) => void;
  onOpen: (id: string) => void;
  onEdit: (account: Account) => void;
  onRefresh: (id: string) => void;
  onCopyId: (id: string) => void;
  onPause: (id: string) => void;
  onResume: (id: string) => void;
  onBan: (account: Account) => void;
  onProbe: (id: string) => void;
}

export function AccountCardGrid(props: Props) {
  const { t } = useTranslation();

  if (props.loading) {
    return (
      <div className={CARD_GRID_CLASS}>
        {Array.from({ length: 6 }).map((_, i) => (
          <Card key={i}>
            <CardHeader>
              <Skeleton className="h-5 w-40" />
              <Skeleton className="mt-1 h-3 w-24" />
            </CardHeader>
            <CardContent>
              <Skeleton className="h-2 w-full" />
              <Skeleton className="mt-3 h-2 w-3/4" />
            </CardContent>
          </Card>
        ))}
      </div>
    );
  }

  return (
    <div className={CARD_GRID_CLASS}>
      {props.rows.map((a) => (
        <AccountCard
          key={a.id}
          account={a}
          selected={props.selected.has(a.id)}
          groups={props.groupIndex.get(a.id) ?? []}
          counters={props.counters.get(a.id)}
          onToggleSelect={props.onToggleSelect}
          onOpen={props.onOpen}
          onEdit={props.onEdit}
          onRefresh={props.onRefresh}
          onCopyId={props.onCopyId}
          onPause={props.onPause}
          onResume={props.onResume}
          onBan={props.onBan}
          onProbe={props.onProbe}
          t={t}
        />
      ))}
    </div>
  );
}

interface CardProps {
  account: Account;
  selected: boolean;
  groups: { id: string; name: string }[];
  counters: { successes: number; failures: number } | undefined;
  onToggleSelect: (id: string, next: boolean) => void;
  onOpen: (id: string) => void;
  onEdit: (a: Account) => void;
  onRefresh: (id: string) => void;
  onCopyId: (id: string) => void;
  onPause: (id: string) => void;
  onResume: (id: string) => void;
  onBan: (a: Account) => void;
  onProbe: (id: string) => void;
  t: ReturnType<typeof useTranslation>["t"];
}

function AccountCard({
  account: a,
  selected,
  groups,
  counters,
  onToggleSelect,
  onOpen,
  onEdit,
  onRefresh,
  onCopyId,
  onPause,
  onResume,
  onBan,
  onProbe,
  t,
}: CardProps) {
  // Subscription progress counts DOWN — bar fill = remaining balance
  // against the plan's initial allotment. Colour segments read:
  //   ≥ 60%: comfortable (emerald)
  //   ≥ 25%: watch    (amber)
  //   <  25%: critical (red)
  // Accounts on uncapped plans (total_plan_credits = 0) skip the bar
  // and print the raw balance instead so we don't fake a percentage.
  const hasCap = a.total_plan_credits > 0;
  const remainingPct = hasCap
    ? Math.min(
        100,
        Math.max(0, (a.subscription_balance / a.total_plan_credits) * 100),
      )
    : 100;
  const barColour =
    remainingPct >= 60
      ? "bg-emerald-500"
      : remainingPct >= 25
        ? "bg-amber-500"
        : "bg-red-500";

  const availableSlots = Math.max(0, CONCURRENCY_CAP - a.in_flight_jobs);
  const switchOn = a.status === "active";
  const switchDisabled = a.status === "banned";

  // Free / uncapped tier: fold has_unlim + has_flex_unlim into one
  // human-facing label. Neither being set means the account pays for
  // every credit; flex-unlim beats unlim in expressive power so we
  // pick the strongest label that applies.
  const freeLabel = a.has_flex_unlim
    ? t("accounts.tags.flexUnlim")
    : a.has_unlim
      ? t("accounts.tags.unlim")
      : t("accounts.card.freeNone");

  return (
    <Card
      data-selected={selected}
      className="group flex flex-col cursor-pointer shadow-none transition-all hover:shadow-xl hover:border-primary/40 data-[selected=true]:border-primary data-[selected=true]:ring-1 data-[selected=true]:ring-primary/40"
      onClick={() => onOpen(a.id)}
    >
      <CardHeader>
        {/* 三段 flex 布局：左固定 | 中 flex-1 min-w-0 | 右固定 */}
        <div className="flex w-full min-w-0 items-start gap-2">
          {/* 左：checkbox + logo 固定宽度 */}
          <div className="flex shrink-0 items-start gap-2">
            <div
              onClick={(e) => e.stopPropagation()}
              className="flex h-9 items-center"
            >
              <Checkbox
                checked={selected}
                onCheckedChange={(v) => onToggleSelect(a.id, v === true)}
                aria-label={t("accounts.card.selectAccount")}
              />
            </div>
            <div className="flex size-9 items-center justify-center rounded-md bg-[#D1FE16]/30 p-1.5">
              <img src={logoBrand} alt="" className="size-full" />
            </div>
          </div>
          {/* 中：email/id/tags — flex-1 min-w-0 让 truncate 生效 */}
          <div className="min-w-0 flex-1">
            <div className="truncate text-base font-semibold">{a.email}</div>
            <div className="truncate font-mono text-xs text-muted-foreground">
              {a.id}
            </div>
            <TagsRow account={a} groups={groups} t={t} />
          </div>
          {/* 右:Switch 固定宽度 */}
          <div
            onClick={(e) => e.stopPropagation()}
            className="flex h-9 shrink-0 items-center"
          >
            <Switch
              checked={switchOn}
              disabled={switchDisabled}
              className="data-[state=checked]:bg-[#D1FE16]"
              onCheckedChange={(v) => (v ? onResume(a.id) : onPause(a.id))}
            />
          </div>
        </div>
      </CardHeader>

      <CardContent className="-mt-6 flex-1 space-y-2.5">
        {/* Credits block — progress bar + subscription/extra/free
            triplet all live inside one rounded card so the operator
            reads them as parts of one "how much can this account
            spend" story. Bar colour drains green → amber → red as the
            subscription balance falls, so a wall of cards reveals the
            near-empty ones at a glance. */}
        <div className="rounded-lg border bg-muted/20 p-3 space-y-3">
          <div>
            <div className="flex items-baseline justify-between text-xs">
              <span className="font-medium">
                {t("accounts.card.subCreditsLabel")}
              </span>
              <span className="tabular-nums text-muted-foreground">
                {hasCap ? (
                  <>
                    <span className="font-medium text-foreground">
                      {formatCredits(a.subscription_balance)}
                    </span>
                    <span> / {formatCredits(a.total_plan_credits)}</span>
                    <span className="ml-2">
                      ({remainingPct.toFixed(0)}%)
                    </span>
                  </>
                ) : (
                  <span className="font-medium text-foreground">
                    {formatCredits(a.subscription_balance)}
                  </span>
                )}
              </span>
            </div>
            <div className="mt-1 h-2 overflow-hidden rounded-full bg-muted">
              <div
                className={`h-full transition-all ${barColour}`}
                style={{ width: `${hasCap ? remainingPct : 100}%` }}
              />
            </div>
          </div>

          {/* The three credit resources under the bar. Their sum is
              the account's true spendable capacity — keeping them
              inside the same card makes that relationship obvious. */}
          <div className="grid grid-cols-3 gap-2 border-t pt-2 text-center">
            <ResourceCell
              label={t("accounts.card.subCreditsLabel")}
              value={formatCredits(a.subscription_balance)}
            />
            <ResourceCell
              label={t("accounts.card.creditsLabel")}
              value={formatCredits(a.credits_balance)}
            />
            <ResourceCell
              label={t("accounts.card.freeLabel")}
              value={freeLabel}
            />
          </div>
        </div>

        {/* Runtime metrics — 2×2 grid so each cell has room for a
            longer value (e.g. "3 / 6 (3 free)") without truncating.
            Order top-left→bottom-right: priority, failures on the
            first row; concurrency, success on the second. */}
        <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 px-3 text-xs">
          <Metric
            label={t("accounts.card.priorityLabel")}
            value={String(a.priority ?? 0)}
            tone={
              (a.priority ?? 0) > 0
                ? "info"
                : (a.priority ?? 0) < 0
                  ? "warning"
                  : undefined
            }
          />
          <Metric
            label={t("accounts.card.failuresLabel")}
            value={String(counters?.failures ?? 0)}
            tone={(counters?.failures ?? 0) > 0 ? "warning" : undefined}
            hint={
              a.fail_streak > 0
                ? t("accounts.card.streakHint", { count: a.fail_streak })
                : undefined
            }
          />
          <Metric
            label={t("accounts.card.concurrencyLabel")}
            value={t("accounts.card.slotsValue", {
              inflight: a.in_flight_jobs,
              cap: CONCURRENCY_CAP,
            })}
            tone={
              a.in_flight_jobs >= CONCURRENCY_CAP
                ? "warning"
                : a.in_flight_jobs > 0
                  ? "info"
                  : undefined
            }
            hint={`${availableSlots} free`}
          />
          <Metric
            label={t("accounts.card.successLabel")}
            value={String(counters?.successes ?? 0)}
          />
        </div>

        {/* Meta — static / rarely-changing context. Vertical list so
            the values line up on the right edge for a fast scan; each
            row is `label ....... value`. */}
        <div className="space-y-1 border-t px-3 pt-3 text-[11px]">
          <MetaRow
            label={t("accounts.card.lastUsedShort")}
            value={a.last_used_at ? formatDateTime(a.last_used_at) : "—"}
          />
          <MetaRow
            label={t("accounts.card.proxyShort")}
            value={a.bound_proxy_url || "—"}
          />
          <MetaRow
            label={t("accounts.card.planEndsShort")}
            value={a.plan_ends_at ? formatDateTime(a.plan_ends_at) : "—"}
          />
        </div>
      </CardContent>

      {/* Footer: primary actions inline, "more" tucked into a dropdown */}
      <CardFooter
        onClick={(e) => e.stopPropagation()}
        className="mt-auto flex flex-wrap items-center justify-between gap-1 border-t pt-3"
      >
        <div className="flex flex-wrap gap-1">
          <Button
            variant="ghost"
            size="icon"
            className="size-8"
            onClick={() => onOpen(a.id)}
            title={t("accounts.card.openDetail")}
          >
            <IconEye className="size-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="size-8"
            onClick={() => onRefresh(a.id)}
            title={t("accounts.card.refresh")}
          >
            <IconRefresh className="size-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="size-8"
            onClick={() => onProbe(a.id)}
            title={t("accounts.card.probe")}
          >
            <IconActivityHeartbeat className="size-4" />
          </Button>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon" className="size-8">
              <IconDotsVertical className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={() => onEdit(a)}>
              <IconEdit /> {t("accounts.actions.edit")}
            </DropdownMenuItem>
            <DropdownMenuItem onClick={() => onCopyId(a.id)}>
              <IconClipboardCopy /> {t("accounts.card.copyId")}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            {a.status === "suspended" ? (
              <DropdownMenuItem onClick={() => onResume(a.id)}>
                <IconPlayerPlay /> {t("accounts.actions.resume")}
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem
                onClick={() => onPause(a.id)}
                disabled={a.status === "banned"}
              >
                <IconPlayerPause /> {t("accounts.actions.pause")}
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem
              className="text-destructive focus:text-destructive"
              disabled={a.status === "banned"}
              onClick={() => onBan(a)}
            >
              <IconTrash /> {t("accounts.actions.ban")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </CardFooter>
    </Card>
  );
}

// TagsRow: 限制 pills 最多 2 行，溢出显示 +N。
function TagsRow({
  account: a,
  groups,
  t,
}: {
  account: Account;
  groups: { id: string; name: string }[];
  t: ReturnType<typeof useTranslation>["t"];
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [visibleCount, setVisibleCount] = useState<number>(-1);

  type Pill =
    | { kind: "plan" }
    | { kind: "status" }
    | { kind: "cohort" }
    | { kind: "unlim" }
    | { kind: "flexUnlim" }
    | { kind: "banned" }
    | { kind: "group"; id: string; name: string };

  const pills: Pill[] = [
    { kind: "plan" },
    { kind: "status" },
    ...(a.cohort ? [{ kind: "cohort" as const }] : []),
    ...(a.has_unlim ? [{ kind: "unlim" as const }] : []),
    ...(a.has_flex_unlim ? [{ kind: "flexUnlim" as const }] : []),
    ...(a.status === "banned" ? [{ kind: "banned" as const }] : []),
    ...groups.map((g) => ({ kind: "group" as const, id: g.id, name: g.name })),
  ];

  const renderPill = (p: Pill, key: string | number) => {
    switch (p.kind) {
      case "plan":
        return (
          <span
            key={key}
            data-pill
            className="inline-flex items-center gap-1 rounded-md border border-[#D1FE16] bg-[#D1FE16] px-1.5 py-0.5 text-xs font-semibold text-black"
          >
            <IconCrown className="size-3" />
            {a.plan_type || t("accounts.tags.noPlan")}
          </span>
        );
      case "status":
        return (
          <StatusBadge key={key} data-pill tone={accountStatusTone(a.status)}>
            {t(`accounts.status.${a.status}`, { defaultValue: a.status })}
          </StatusBadge>
        );
      case "cohort":
        return (
          <StatusBadge key={key} data-pill tone="info">
            {t("accounts.card.tierPrefix")}·{a.cohort}
          </StatusBadge>
        );
      case "unlim":
        return (
          <StatusBadge key={key} data-pill tone="brand">
            {t("accounts.tags.unlim")}
          </StatusBadge>
        );
      case "flexUnlim":
        return (
          <StatusBadge key={key} data-pill tone="info">
            {t("accounts.tags.flexUnlim")}
          </StatusBadge>
        );
      case "banned":
        return (
          <StatusBadge key={key} data-pill tone="danger">
            {t("accounts.card.disabled")}
          </StatusBadge>
        );
      case "group":
        return (
          <StatusBadge key={key} data-pill tone="muted">
            {p.name}
          </StatusBadge>
        );
    }
  };

  const measureRef = useRef<HTMLDivElement | null>(null);

  // 测量：在隐藏容器渲染全部 pill，找到第 3 行开始的位置，砍到那之前。
  // 这样 ResizeObserver 触发时测的永远是完整列表，不会反复抖动。
  useLayoutEffect(() => {
    const el = measureRef.current;
    if (!el) return;
    const measure = () => {
      const children = Array.from(
        el.querySelectorAll<HTMLElement>("[data-pill]"),
      );
      if (children.length === 0) {
        setVisibleCount(0);
        return;
      }
      const tops = children.map((c) => c.offsetTop);
      const firstRowTop = tops[0];
      let secondRowTop: number | null = null;
      let overflowIndex = -1;
      for (let i = 0; i < tops.length; i++) {
        if (tops[i] !== firstRowTop) {
          if (secondRowTop === null) secondRowTop = tops[i];
          else if (tops[i] !== secondRowTop) {
            overflowIndex = i;
            break;
          }
        }
      }
      if (overflowIndex === -1) {
        setVisibleCount(children.length);
      } else {
        setVisibleCount(overflowIndex);
      }
    };
    measure();
    const ro = new ResizeObserver(measure);
    if (containerRef.current) ro.observe(containerRef.current);
    return () => ro.disconnect();
  }, [pills.length]);

  const measured = visibleCount !== -1;
  const shown = measured ? pills.slice(0, visibleCount) : [];
  const hidden = measured ? pills.length - visibleCount : 0;

  return (
    <div
      ref={containerRef}
      className="relative mt-1.5"
      style={{ height: "3rem" }}
    >
      {/* 隐藏测量层：渲染全部 pills，用来计算行数 */}
      <div
        ref={measureRef}
        aria-hidden
        className="pointer-events-none absolute inset-0 flex flex-wrap items-start gap-1"
        style={{ visibility: "hidden" }}
      >
        {pills.map((p, i) =>
          renderPill(p, p.kind === "group" ? `g-${p.id}` : `${p.kind}-${i}`),
        )}
      </div>
      {/* 可见层：只渲染前 N 个 + N */}
      <div
        className="flex flex-wrap items-start gap-1 overflow-hidden"
        style={{
          maxHeight: "3rem",
          visibility: measured ? "visible" : "hidden",
        }}
      >
        {shown.map((p, i) =>
          renderPill(p, p.kind === "group" ? `g-${p.id}` : `${p.kind}-${i}`),
        )}
        {hidden > 0 ? (
          <StatusBadge tone="muted">+{hidden}</StatusBadge>
        ) : null}
      </div>
    </div>
  );
}

function MetaRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2 overflow-hidden">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className="truncate text-foreground/80">{value}</span>
    </div>
  );
}

function ResourceCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <div className="truncate text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div className="truncate text-sm font-semibold tabular-nums">{value}</div>
    </div>
  );
}

interface MetricProps {
  label: string;
  value: string;
  hint?: string;
  tone?: "warning" | "danger" | "info";
  className?: string;
}

function Metric({ label, value, hint, tone, className }: MetricProps) {
  const toneClass =
    tone === "danger"
      ? "text-red-600 dark:text-red-400"
      : tone === "warning"
        ? "text-amber-700 dark:text-amber-400"
        : tone === "info"
          ? "text-sky-700 dark:text-sky-400"
          : "text-foreground";
  return (
    <div
      className={`flex items-baseline justify-between gap-2 ${className ?? ""}`}
    >
      <span className="text-muted-foreground">{label}</span>
      <span className={`font-medium tabular-nums ${toneClass}`}>
        {value}
        {hint ? (
          <span className="ml-1 text-muted-foreground text-[10px]">
            ({hint})
          </span>
        ) : null}
      </span>
    </div>
  );
}
