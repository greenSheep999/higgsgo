import * as React from "react";
import { Link, useMatchRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import {
  IconChartBar,
  IconClipboardList,
  IconClockHour5,
  IconDashboard,
  IconHeartRateMonitor,
  IconInnerShadowTop,
  IconKey,
  IconLogout,
  IconMessageChatbot,
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
import { clearBearer } from "@/lib/auth-store";

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
  { titleKey: "nav.modelHealth", to: "/model-health", icon: IconHeartRateMonitor },
  { titleKey: "nav.audit", to: "/audit", icon: IconClockHour5 },
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
                <IconInnerShadowTop className="size-5!" />
                <span className="text-base font-semibold">higgsgo</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <NavGroup label={t("nav.overview")} items={primary} />
        <NavGroup label={t("nav.observability")} items={observability} />
      </SidebarContent>

      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              onClick={() => {
                clearBearer();
              }}
            >
              <IconLogout />
              <span>{t("common.signOut")}</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
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
