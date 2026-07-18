import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { OverviewKPIs } from "@/components/dashboard/overview-kpis";
import { PoolComposition } from "@/components/dashboard/pool-composition";
import { OverviewTrend } from "@/components/dashboard/overview-trend";
import { OverviewBreakdown } from "@/components/dashboard/overview-breakdown";
import { TimeWindowPicker } from "@/components/dashboard/time-window-picker";
import { presetWindow, type WindowPreset } from "@/lib/time-window";
import { rootRoute } from "@/routes/root";

// Dashboard is the operator's landing page — an always-pool-wide overview.
// The one global control is a time window (chips + custom range); there's
// no group / key filter on purpose. Detail pages (/groups, /keys) exist
// for scoped drill-down, and mixing a filter here would blur the "state
// of the pool" reading operators want on this page.

function Dashboard() {
  const { t } = useTranslation();
  const [preset, setPreset] = useState<WindowPreset | "custom">("24h");
  const [window, setWindow] = useState(() => presetWindow("24h"));

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">{t("dashboard.title")}</h2>
          <p className="text-sm text-muted-foreground">
            {t("dashboard.description")}
          </p>
        </div>
        <TimeWindowPicker
          value={window}
          onChange={setWindow}
          activePreset={preset}
          onPresetChange={setPreset}
        />
      </div>

      <OverviewKPIs window={window} />
      <PoolComposition />
      <OverviewTrend window={window} />
      <OverviewBreakdown window={window} />
    </div>
  );
}

export const dashboardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  staticData: { titleKey: "nav.dashboard" },
  component: Dashboard,
});
