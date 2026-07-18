import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { IconPlug } from "@tabler/icons-react";

import { admin, ApiError, type UsageAggRow } from "@/lib/api";
import { rootRoute } from "@/routes/root";
import {
  Card,
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
import { StatusBadge } from "@/components/ui/status-badge";

// CPA plugin page. Read-only ops view for Mode B — the /internal/*
// surface backed by internal/api/cpaplugin/. There is no
// /admin/modes-style endpoint yet, so the "enabled?" indicator is
// inferred from whether /admin/usage/aggregate returns any rows
// grouped by cpa_partner_id: if yes, CPA traffic has flowed through
// the pool at some point. If zero rows or a 404 comes back, the
// mode is either disabled or nobody has hit it yet.
//
// Deliberate non-goals for this iteration:
//   - No manual JWT refresh trigger. /internal/refresh_jwt lives on
//     the internal listener, which the admin bearer cannot reach.
//     Wiring it here would require a new admin passthrough route.
//   - No config editor. The mode is toggled in higgsgo.toml and
//     needs a restart.

// Known /internal/* routes served by the cpaplugin handler. Static
// list rendered as documentation so operators know the shape without
// grepping the Go source.
const CPA_ROUTES: { method: string; path: string; description: string }[] = [
  {
    method: "POST",
    path: "/internal/register",
    description: "Mint a new API key bound to a partner_id.",
  },
  {
    method: "POST",
    path: "/internal/execute",
    description: "Run /v1 generation on behalf of a partner_id.",
  },
  {
    method: "GET",
    path: "/internal/balance/{partner_id}",
    description: "Usage + quota rollup for every key under a partner_id.",
  },
  {
    method: "POST",
    path: "/internal/refresh_jwt/{partner_id}",
    description:
      "Invalidate cached JWTs for the partner's accounts — forces a fresh mint on next execute.",
  },
  {
    method: "DELETE",
    path: "/internal/{partner_id}",
    description: "Soft-delete every key under a partner_id.",
  },
  {
    method: "GET",
    path: "/internal/registrations/{id}",
    description: "Async signup status (backed by ports.Registrar).",
  },
  {
    method: "GET",
    path: "/internal/status",
    description: "Pool health probe (active accounts + active keys).",
  },
];

function CPAPlugin() {
  const { t } = useTranslation();

  // Best-effort "enabled?" heuristic. We aggregate usage grouped by
  // cpa_partner_id and treat any non-empty result as evidence that
  // Mode B has served real traffic. This misses "enabled but idle"
  // deployments but is the cheapest signal we can lean on without
  // adding a /admin/modes endpoint.
  const q = useQuery<UsageAggRow[]>({
    queryKey: ["admin", "cpa-usage-inference"],
    queryFn: () => admin.aggregateUsage({ group_by: ["cpa_partner_id"] }),
    retry: (failureCount, error) => {
      if (error instanceof ApiError && error.status === 404) return false;
      return failureCount < 2;
    },
  });

  const partnerRows = (q.data ?? []).filter((r) => r.keys.cpa_partner_id);
  const inferredEnabled = partnerRows.length > 0;

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconPlug className="size-5" /> {t("cpaplugin.title")}
          </CardTitle>
          <CardDescription>{t("cpaplugin.description")}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex items-center gap-2 text-sm">
            <StatusBadge tone={inferredEnabled ? "success" : "muted"}>
              {inferredEnabled
                ? t("cpaplugin.enabledLabel")
                : t("cpaplugin.disabledLabel")}
            </StatusBadge>
            {!inferredEnabled && (
              <span className="text-muted-foreground">
                {t("cpaplugin.disabledHint")}
              </span>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("cpaplugin.routesTitle")}</CardTitle>
          <CardDescription>{t("cpaplugin.routesHint")}</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="overflow-hidden rounded-md border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-24">Method</TableHead>
                  <TableHead className="w-72">Path</TableHead>
                  <TableHead>Description</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {CPA_ROUTES.map((r) => (
                  <TableRow key={`${r.method} ${r.path}`}>
                    <TableCell>
                      <StatusBadge tone="muted">{r.method}</StatusBadge>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {r.path}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {r.description}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("cpaplugin.jwtRefreshTitle")}</CardTitle>
          <CardDescription>{t("cpaplugin.jwtRefreshHint")}</CardDescription>
        </CardHeader>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("cpaplugin.providersTitle")}</CardTitle>
          <CardDescription>{t("cpaplugin.providersHint")}</CardDescription>
        </CardHeader>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t("cpaplugin.docsTitle")}</CardTitle>
        </CardHeader>
        <CardContent>
          <ul className="list-disc space-y-1 pl-5 text-sm text-muted-foreground">
            <li className="font-mono">{t("cpaplugin.docs.pluggable")}</li>
            <li className="font-mono">{t("cpaplugin.docs.poolAndCpa")}</li>
          </ul>
        </CardContent>
      </Card>
    </div>
  );
}

export const cpaPluginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/cpaplugin",
  staticData: { titleKey: "nav.cpaplugin" },
  component: CPAPlugin,
});
