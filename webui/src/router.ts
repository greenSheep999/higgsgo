import { createRouter, createHashHistory } from "@tanstack/react-router";
import { rootRoute } from "@/routes/root";
import { dashboardRoute } from "@/routes/dashboard";
import { accountsRoute } from "@/routes/accounts";
import { keysRoute } from "@/routes/keys";
import { groupsRoute } from "@/routes/groups";
import { groupDetailRoute } from "@/routes/group-detail";
import { playgroundRoute } from "@/routes/playground";
import { jobsRoute } from "@/routes/jobs";
import { usageRoute } from "@/routes/usage";
import { modelsRoute } from "@/routes/models";
import { pricingRoute } from "@/routes/pricing";
import { pricingFloorSuggestionsRoute } from "@/routes/pricing-floor-suggestions";
import { pricingPurchaseBatchesRoute } from "@/routes/pricing-purchase-batches";
import { auditRoute } from "@/routes/audit";
import { registrationsRoute } from "@/routes/registrations";
import { cpaPluginRoute } from "@/routes/cpaplugin";

// We use hash history so the same static bundle works whether it's served
// out of the Go binary at /, /webui/, or any subpath — no rewrite rules
// required in front of the admin listener.
const routeTree = rootRoute.addChildren([
  dashboardRoute,
  accountsRoute,
  keysRoute,
  groupsRoute,
  groupDetailRoute,
  playgroundRoute,
  jobsRoute,
  usageRoute,
  modelsRoute,
  pricingRoute,
  pricingFloorSuggestionsRoute,
  pricingPurchaseBatchesRoute,
  auditRoute,
  registrationsRoute,
  cpaPluginRoute,
]);

export const router = createRouter({
  routeTree,
  history: createHashHistory(),
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
