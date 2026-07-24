import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { createRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { IconAlertTriangle, IconCheck, IconReload } from "@tabler/icons-react";

import { admin, type PricingFloorSuggestion } from "@/lib/api";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatUSD } from "@/lib/format";

// PricingFloorSuggestionsPage renders the per-(alias × variant) planning
// table served by GET /admin/pricing/floor-suggestions. See the Go
// handler for full contract detail; UX here is intentionally minimal:
// one dense table sorted by alias with search + a status column that
// tells the operator whether their current price is at/above/below the
// floor and the market median. No inline editing — a suggestion doesn't
// commit anything; the operator uses this to inform decisions on the
// main /pricing page.
function PricingFloorSuggestionsPage() {
  const { t } = useTranslation();
  const [search, setSearch] = useState("");
  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ["admin", "pricing-floor-suggestions"],
    queryFn: () => admin.getPricingFloorSuggestions(),
    // Reasonably fresh — floor and official prices change on daily
    // scale; poll on focus so operators returning to the tab see
    // freshly-imported data without a manual reload.
    staleTime: 60_000,
    refetchOnWindowFocus: true,
  });

  const rows = data?.rows ?? [];
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(
      (r) =>
        r.model_alias.toLowerCase().includes(q) ||
        r.jst.toLowerCase().includes(q) ||
        r.resolution.toLowerCase().includes(q),
    );
  }, [rows, search]);

  return (
    <div className="mx-auto max-w-[1400px] space-y-4 p-4">
      <Card>
        <CardHeader>
          <CardTitle>
            {t("pricing.floor.title", {
              defaultValue: "Floor & market suggestions",
            })}
          </CardTitle>
          <CardDescription>
            {t("pricing.floor.description", {
              defaultValue:
                "Recommended retail per model × variant based on the §10 cost floor and the current official-provider price range.",
            })}
          </CardDescription>
          <CardAction>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => refetch()}
              disabled={isFetching}
            >
              <IconReload
                className={isFetching ? "animate-spin" : undefined}
              />
              {t("common.refresh", { defaultValue: "Refresh" })}
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="flex items-center gap-3">
            <Input
              placeholder={t("pricing.floor.filterPlaceholder", {
                defaultValue: "Filter alias / JST / resolution",
              })}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="max-w-sm"
            />
            {data ? (
              <div className="text-xs text-muted-foreground">
                {t("pricing.floor.configLine", {
                  defaultValue:
                    "Unit cost {{unit}} µ/cr · markup ×{{markup}} · {{status}}",
                  unit: data.config.reference_unit_cost_micros,
                  markup: data.config.markup_multiplier.toFixed(2),
                  status: data.config.enabled
                    ? t("pricing.floor.enabled", { defaultValue: "enabled" })
                    : t("pricing.floor.disabled", { defaultValue: "disabled" }),
                })}
              </div>
            ) : null}
            <div className="ml-auto text-xs text-muted-foreground">
              {rows.length > 0
                ? t("pricing.floor.rowCount", {
                    defaultValue: "{{visible}} of {{total}} rows",
                    visible: filtered.length,
                    total: rows.length,
                  })
                : null}
            </div>
          </div>

          {isLoading ? (
            <Skeleton className="h-96 w-full" />
          ) : (
            <FloorSuggestionsTable rows={filtered} />
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// FloorSuggestionsTable is split out so a later "compact" view or an
// export-to-CSV button can share the projection logic without pulling
// the whole page.
function FloorSuggestionsTable({ rows }: { rows: PricingFloorSuggestion[] }) {
  const { t } = useTranslation();
  if (rows.length === 0) {
    return (
      <div className="rounded-md border p-6 text-center text-sm text-muted-foreground">
        {t("pricing.floor.empty", {
          defaultValue: "No variants match — try broadening the filter or run costsync/import to seed data.",
        })}
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-md border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("pricing.floor.col.model", { defaultValue: "Model" })}</TableHead>
            <TableHead>{t("pricing.floor.col.variant", { defaultValue: "Variant" })}</TableHead>
            <TableHead className="text-right">
              {t("pricing.floor.col.credits", { defaultValue: "Credits" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.floor.col.floor", { defaultValue: "Floor" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.floor.col.officialRange", { defaultValue: "Official low / mid / high" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.floor.col.recommended", { defaultValue: "Recommended" })}
            </TableHead>
            <TableHead className="text-right">
              {t("pricing.floor.col.current", { defaultValue: "Current" })}
            </TableHead>
            <TableHead>{t("pricing.floor.col.status", { defaultValue: "Status" })}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r, i) => (
            <TableRow key={`${r.model_alias}-${r.resolution}-${r.mode}-${r.audio}-${r.duration_seconds}-${i}`}>
              <TableCell>
                <div className="font-mono text-xs">{r.model_alias}</div>
                <div className="text-[10px] text-muted-foreground">{r.jst || "—"}</div>
              </TableCell>
              <TableCell className="text-xs">
                <VariantChips row={r} />
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {r.credits.toFixed(2)}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                <MicrosCell v={r.floor_micros} reason={r.floor_reason} />
              </TableCell>
              <TableCell className="text-right tabular-nums text-xs">
                <OfficialRange
                  low={r.official_low_micros}
                  mid={r.official_mid_micros}
                  high={r.official_high_micros}
                  providers={r.providers}
                />
              </TableCell>
              <TableCell className="text-right tabular-nums font-medium">
                <MicrosCell v={r.recommended_micros} />
              </TableCell>
              <TableCell className="text-right tabular-nums">
                <MicrosCell v={r.current_micros} />
              </TableCell>
              <TableCell>
                <StatusBadge
                  vsFloor={r.current_vs_floor}
                  vsOfficial={r.current_vs_official}
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

// VariantChips renders the four dimension fields as short chips so the
// user can scan the table without decoding a paragraph. Empty
// dimensions are dropped rather than shown as "—" clutter.
function VariantChips({ row }: { row: PricingFloorSuggestion }) {
  const chips: string[] = [];
  if (row.resolution) chips.push(row.resolution);
  if (row.duration_seconds > 0) chips.push(`${row.duration_seconds}s`);
  if (row.mode) chips.push(`mode=${row.mode}`);
  if (row.audio) chips.push(`audio=${row.audio}`);
  if (row.unit) chips.push(row.unit);
  return (
    <div className="flex flex-wrap gap-1">
      {chips.map((c) => (
        <Badge key={c} variant="outline" className="text-[10px]">
          {c}
        </Badge>
      ))}
    </div>
  );
}

// MicrosCell prints "$0.xxx" or a dash. When the value is null AND a
// reason is provided, it renders the reason inline so the operator
// can debug (e.g. "cost_rule_missing" vs "floor_disabled").
function MicrosCell({ v, reason }: { v: number | null; reason?: string }) {
  if (v === null || v === undefined) {
    return (
      <span className="text-muted-foreground">
        {reason ? <span className="text-[10px]">{reason}</span> : "—"}
      </span>
    );
  }
  return <>{formatUSD(v / 1_000_000)}</>;
}

// OfficialRange collapses low/mid/high into a compact "min – mid – max"
// triplet, with the provider list hovering below. When every value is
// identical (single-provider row) it degrades gracefully to just the
// value.
function OfficialRange({
  low,
  mid,
  high,
  providers,
}: {
  low: number | null;
  mid: number | null;
  high: number | null;
  providers: string[];
}) {
  if (low === null || mid === null || high === null) {
    return <span className="text-muted-foreground">—</span>;
  }
  const same = low === mid && mid === high;
  return (
    <div className="flex flex-col items-end">
      <div>
        {same
          ? formatUSD(mid / 1_000_000)
          : `${formatUSD(low / 1_000_000)} — ${formatUSD(mid / 1_000_000)} — ${formatUSD(high / 1_000_000)}`}
      </div>
      {providers.length > 0 ? (
        <div className="text-[10px] text-muted-foreground">
          {providers.length === 1
            ? providers[0]
            : `${providers.length} providers`}
        </div>
      ) : null}
    </div>
  );
}

// StatusBadge combines the two `current_vs_*` verdicts. A row is "OK"
// when current sits at or above both benchmarks; "warn" when it's
// below floor (§10 warning territory); "info" for the intermediate
// cases; and blank when both verdicts are unknown.
function StatusBadge({
  vsFloor,
  vsOfficial,
}: {
  vsFloor: PricingFloorSuggestion["current_vs_floor"];
  vsOfficial: PricingFloorSuggestion["current_vs_official"];
}) {
  const { t } = useTranslation();
  if (vsFloor === "unknown" && vsOfficial === "unknown") {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  if (vsFloor === "below") {
    return (
      <Badge variant="destructive" className="gap-1">
        <IconAlertTriangle className="h-3 w-3" />
        {t("pricing.floor.status.belowFloor", { defaultValue: "below floor" })}
      </Badge>
    );
  }
  if (vsOfficial === "below") {
    return (
      <Badge variant="secondary" className="gap-1">
        {t("pricing.floor.status.belowMarket", { defaultValue: "below market" })}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="gap-1 border-green-500/40 text-green-700 dark:text-green-400">
      <IconCheck className="h-3 w-3" />
      {t("pricing.floor.status.ok", { defaultValue: "at or above" })}
    </Badge>
  );
}

export const pricingFloorSuggestionsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/pricing/floor-suggestions",
  staticData: { titleKey: "nav.pricingFloor" },
  component: PricingFloorSuggestionsPage,
});
