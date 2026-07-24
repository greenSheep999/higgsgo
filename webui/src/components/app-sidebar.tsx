import * as React from "react";
import { Link, useMatchRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { FailoverDialog } from "@/components/settings/failover-dialog";
import {
  IconArrowsSort,
  IconBook,
  IconBox,
  IconBrandDiscord,
  IconBrandGithub,
  IconBrandTelegram,
  IconChartBar,
  IconClipboardList,
  IconClockHour5,
  IconCurrencyDollar,
  IconDashboard,
  IconKey,
  IconLogout,
  IconMessageChatbot,
  IconScale,
  IconUserPlus,
  IconUsers,
  IconUsersGroup,
} from "@tabler/icons-react";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import { admin, ApiError, type RoutingSettings } from "@/lib/api";
import { clearBearer } from "@/lib/auth-store";

// External links shown in the sidebar footer. Kept as a module const so
// the URLs live in one place; update here if a channel moves.
const EXTERNAL_LINKS = {
  docs: "https://github.com/greenSheep999/higgsgo",
  github: "https://github.com/greenSheep999/higgsgo",
  telegram: "https://t.me/tokensheep666",
  discord: "https://discord.gg/nH4qMVEpa",
} as const;

interface NavEntry {
  titleKey: string;
  to: string;
  icon: React.ComponentType<{ className?: string }>;
}

const primary: NavEntry[] = [
  { titleKey: "nav.dashboard", to: "/", icon: IconDashboard },
  { titleKey: "nav.accounts", to: "/accounts", icon: IconUsers },
  { titleKey: "nav.keys", to: "/keys", icon: IconKey },
  { titleKey: "nav.groups", to: "/groups", icon: IconUsersGroup },
  { titleKey: "nav.playground", to: "/playground", icon: IconMessageChatbot },
];

const observability: NavEntry[] = [
  { titleKey: "nav.jobs", to: "/jobs", icon: IconClipboardList },
  { titleKey: "nav.usage", to: "/usage", icon: IconChartBar },
  { titleKey: "nav.models", to: "/models", icon: IconBox },
  { titleKey: "nav.pricing", to: "/pricing", icon: IconCurrencyDollar },
  { titleKey: "nav.audit", to: "/audit", icon: IconClockHour5 },
];

// Plugin family. Only Registrations shows by default — the CPA plugin
// entry point exists in the codebase but the /internal/* listener is
// off (modes.cpa_plugin=false) in every standalone deploy, so surfacing
// the UI would just lead operators to a page that can't reach the
// backend. The route file (routes/cpaplugin.tsx) stays wired in the
// router so an operator who explicitly navigates to /cpaplugin still
// gets it — this is a nav-level hide, not a delete.
const plugins: NavEntry[] = [
  { titleKey: "nav.registrations", to: "/registrations", icon: IconUserPlus },
];

export function AppSidebar(props: React.ComponentProps<typeof Sidebar>) {
  const { t } = useTranslation();
  return (
    <Sidebar collapsible="offcanvas" {...props}>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              asChild
              className="data-[slot=sidebar-menu-button]:p-1.5!"
            >
              <Link to="/">
                <img
                  src="/logo.png"
                  alt="higgsgo"
                  className="size-7 shrink-0 rounded"
                />
                <span className="text-base font-semibold">Higgs.go</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <NavGroup label={t("nav.overview")} items={primary} />
        <NavGroup label={t("nav.observability")} items={observability} />
        <NavGroup label={t("nav.plugins")} items={plugins} />
      </SidebarContent>

      <SidebarFooter className="gap-2">
        {/* Group 1: pool controls (load balancing + priority switches +
            pool-health click target). Backend for the two switches is
            still pending (B task) — they persist to localStorage for
            now so the UI works end-to-end. Health click will wire to
            the failover-policy dialog once the C-track lands. */}
        <PoolControlsGroup />

        <SidebarSeparator />

        {/* Group 2: external resources — packed horizontally as icon-
            only tiles to save vertical space. Native <a> instead of
            SidebarMenuButton so the row can be a flexbox rather than
            four full-width buttons. Hidden entirely when the sidebar
            collapses to icons; the vertical rail already exposes the
            primary nav, and duplicating four social icons there would
            just add noise. */}
        {/* Four icon-only secondary buttons, centered as a group with
            wide gap between them. Each tile carries its own secondary
            background — shadcn Button variant="secondary" spec. */}
        <div className="flex items-center justify-center gap-3 px-2 group-data-[collapsible=icon]:hidden">
          <Button
            asChild
            variant="secondary"
            size="icon"
            title={t("sidebar.docs")}
            aria-label={t("sidebar.docs")}
          >
            <a href={EXTERNAL_LINKS.docs} target="_blank" rel="noreferrer">
              <IconBook className="size-4" />
            </a>
          </Button>
          <Button
            asChild
            variant="secondary"
            size="icon"
            title={t("sidebar.github")}
            aria-label={t("sidebar.github")}
          >
            <a href={EXTERNAL_LINKS.github} target="_blank" rel="noreferrer">
              <IconBrandGithub className="size-4" />
            </a>
          </Button>
          <Button
            asChild
            variant="secondary"
            size="icon"
            title={t("sidebar.telegram")}
            aria-label={t("sidebar.telegram")}
          >
            <a href={EXTERNAL_LINKS.telegram} target="_blank" rel="noreferrer">
              <IconBrandTelegram className="size-4" />
            </a>
          </Button>
          <Button
            asChild
            variant="secondary"
            size="icon"
            title={t("sidebar.discord")}
            aria-label={t("sidebar.discord")}
          >
            <a href={EXTERNAL_LINKS.discord} target="_blank" rel="noreferrer">
              <IconBrandDiscord className="size-4" />
            </a>
          </Button>
        </div>

        <SidebarSeparator />

        {/* Group 3: sign out — anchored at the very bottom. Centred
            label so the icon + text sit as a symmetric CTA rather
            than the default left-aligned nav row. */}
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              onClick={() => {
                clearBearer();
              }}
              className="justify-center"
            >
              <IconLogout />
              <span>{t("common.signOut")}</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>

        {/* Copyright — subtle, centred, hidden when the sidebar
            collapses to icons. */}
        <p className="pt-1 text-center text-[10px] text-muted-foreground group-data-[collapsible=icon]:hidden">
          {t("common.copyright", { year: new Date().getFullYear() })}
        </p>
      </SidebarFooter>
    </Sidebar>
  );
}

// SidebarSeparator wraps a horizontal <Separator> with the padding /
// hide-on-collapse behaviour we want inside the SidebarFooter. Kept
// local so the styling stays in one place; nothing else in the app
// separates footer sub-groups.
function SidebarSeparator() {
  return (
    <Separator className="mx-2 my-0.5 w-auto group-data-[collapsible=icon]:hidden" />
  );
}

type RoutingStrategy = "load_balance" | "priority";

// PoolControlsGroup is the top footer group: a mutually-exclusive
// routing-strategy switcher + the pool-health indicator. Load-balance
// and Priority are two ways to pick an account from the pool — a
// single choice, so the UI is a radio. Persistence lives in the
// backend via /admin/settings/routing; new groups pick it up as
// their default route_strategy at creation time.
function PoolControlsGroup() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["admin", "settings", "routing"],
    queryFn: admin.getRoutingSettings,
    staleTime: 60_000,
  });
  const routing: RoutingStrategy = q.data?.strategy ?? "load_balance";

  const mut = useMutation({
    mutationFn: (v: RoutingStrategy) =>
      admin.updateRoutingSettings({ strategy: v }),
    onMutate: async (v) => {
      // Optimistic: flip the radio immediately so the click doesn't
      // feel laggy on flaky networks. On error onError below rolls
      // it back via invalidate.
      await qc.cancelQueries({ queryKey: ["admin", "settings", "routing"] });
      const prev = qc.getQueryData<RoutingSettings>([
        "admin",
        "settings",
        "routing",
      ]);
      qc.setQueryData<RoutingSettings>(
        ["admin", "settings", "routing"],
        { strategy: v, source: "db" },
      );
      return { prev };
    },
    onError: (err, _v, ctx) => {
      if (ctx?.prev) {
        qc.setQueryData(["admin", "settings", "routing"], ctx.prev);
      }
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : String(err),
      );
    },
    onSuccess: () => {
      toast.success(t("sidebar.routingSaved"));
    },
  });

  const changeRouting = (v: string) => {
    // Radio guard: reject empty / unknown values so exactly one
    // option is always selected.
    if (v !== "load_balance" && v !== "priority") return;
    if (v === routing) return;
    mut.mutate(v);
  };

  return (
    <SidebarMenu>
      {/* Routing-strategy switcher (segmented, mutually exclusive).
          No wrapper padding — the label and radio rows use the sidebar's
          own px-2 rhythm so they align with siblings like Pool health. */}
      <SidebarMenuItem
        className="flex-col items-stretch gap-1 group-data-[collapsible=icon]:hidden"
        title={t("sidebar.routingHint")}
      >
        <span className="truncate px-2 text-xs font-medium">
          {t("sidebar.routingLabel")}
        </span>
        <div className="flex flex-col gap-1" role="radiogroup">
          <RoutingOption
            value="load_balance"
            current={routing}
            onSelect={changeRouting}
            icon={<IconScale className="size-3.5" />}
            label={t("sidebar.loadBalance")}
          />
          <RoutingOption
            value="priority"
            current={routing}
            onSelect={changeRouting}
            icon={<IconArrowsSort className="size-3.5" />}
            label={t("sidebar.priority")}
          />
        </div>
      </SidebarMenuItem>

      {/* Icon-only fallback for the collapsed sidebar — shows the
          currently-active strategy's icon so the state stays visible. */}
      <SidebarMenuItem>
        <div
          className="hidden items-center justify-center rounded-md px-2 py-1.5 group-data-[collapsible=icon]:flex"
          title={t(routing === "load_balance" ? "sidebar.loadBalance" : "sidebar.priority")}
        >
          {routing === "load_balance" ? (
            <IconScale className="size-4 text-muted-foreground" />
          ) : (
            <IconArrowsSort className="size-4 text-muted-foreground" />
          )}
        </div>
      </SidebarMenuItem>

      {/* Pool health indicator — click will open the failover policy
          dialog once the C-track lands. For now it toasts a pending
          message so operators can discover the interaction. */}
      <PoolHealthIndicator />
    </SidebarMenu>
  );
}

// RoutingOption is a radio-style row for the routing-strategy picker.
// Rolled by hand instead of pulling in shadcn's RadioGroup so the row
// can carry an icon + label together with a strong, unmistakable
// selected state (primary ring + filled dot). Behaves as a role=radio
// button for a11y.
function RoutingOption({
  value,
  current,
  onSelect,
  icon,
  label,
}: {
  value: RoutingStrategy;
  current: RoutingStrategy;
  onSelect: (v: string) => void;
  icon: React.ReactNode;
  label: string;
}) {
  const selected = current === value;
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={() => onSelect(value)}
      className={
        "flex items-center gap-2 rounded-md border px-2 py-1.5 text-left text-xs transition-colors " +
        (selected
          ? "border-primary bg-primary/10 text-foreground"
          : "border-transparent text-muted-foreground hover:bg-muted/50")
      }
    >
      <span
        className={
          "flex size-3.5 shrink-0 items-center justify-center rounded-full border " +
          (selected ? "border-primary" : "border-muted-foreground/40")
        }
      >
        {selected ? <span className="size-1.5 rounded-full bg-primary" /> : null}
      </span>
      <span className="shrink-0">{icon}</span>
      <span className="truncate">{label}</span>
    </button>
  );
}

// PoolHealthIndicator renders a compact "N / total active" line with
// a colour dot. Click opens the failover / risk-control dialog so an
// operator can adjust tunables + recover isolated accounts on the
// spot without navigating away.
function PoolHealthIndicator() {
  const { t } = useTranslation();
  const [dialogOpen, setDialogOpen] = React.useState(false);
  const q = useQuery({
    queryKey: ["admin", "stats", "pool"],
    queryFn: admin.poolStats,
    refetchInterval: 30_000,
    staleTime: 15_000,
  });

  const total = q.data?.total ?? 0;
  const active = q.data?.by_status?.active ?? 0;
  const bad = Math.max(0, total - active);

  let dot = "bg-muted-foreground/40";
  let detail = t("sidebar.health.loading");
  if (q.data) {
    if (total === 0) {
      dot = "bg-muted-foreground/40";
      detail = t("sidebar.health.empty");
    } else if (bad === 0) {
      dot = "bg-emerald-500";
      detail = t("sidebar.health.allActive");
    } else if (active * 2 >= total) {
      dot = "bg-amber-500";
      detail = t("sidebar.health.degraded", { bad });
    } else {
      dot = "bg-red-500";
      detail = t("sidebar.health.degraded", { bad });
    }
  }

  const countLabel = q.data
    ? `${active}/${total}`
    : q.isLoading
      ? "…"
      : "—";

  return (
    <>
      {/* Outlined card-style trigger — the default SidebarMenuButton
          looks like a plain nav row so operators wouldn't guess it
          opens a dialog. Bordered white background + tooltip make
          the affordance obvious without shouting. */}
      <SidebarMenuItem>
        <button
          type="button"
          onClick={() => setDialogOpen(true)}
          title={detail}
          className="flex w-full items-center justify-between rounded-md border bg-background px-2 py-1.5 text-left transition-colors hover:bg-muted/50 group-data-[collapsible=icon]:justify-center"
        >
          <span className="flex min-w-0 items-center gap-2">
            <span className="flex size-4 shrink-0 items-center justify-center">
              <span className={`size-2 rounded-full ${dot}`} />
            </span>
            <span className="truncate text-xs font-medium group-data-[collapsible=icon]:hidden">
              {t("sidebar.health.label")}
            </span>
          </span>
          <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground group-data-[collapsible=icon]:hidden">
            {countLabel}
          </span>
        </button>
      </SidebarMenuItem>
      <FailoverDialog open={dialogOpen} onOpenChange={setDialogOpen} />
    </>
  );
}

function NavGroup({ label, items }: { label: string; items: NavEntry[] }) {
  const { t } = useTranslation();
  const matchRoute = useMatchRoute();
  return (
    <SidebarGroup>
      <SidebarGroupLabel>{label}</SidebarGroupLabel>
      <SidebarGroupContent>
        <SidebarMenu>
          {items.map((item) => {
            const active =
              item.to === "/"
                ? !!matchRoute({ to: "/", fuzzy: false })
                : !!matchRoute({ to: item.to, fuzzy: true });
            const title = t(item.titleKey);
            return (
              <SidebarMenuItem key={item.to}>
                <SidebarMenuButton asChild isActive={active} tooltip={title}>
                  <Link to={item.to}>
                    <item.icon />
                    <span>{title}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            );
          })}
        </SidebarMenu>
      </SidebarGroupContent>
    </SidebarGroup>
  );
}
