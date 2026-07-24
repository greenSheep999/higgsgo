import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { createRoute, Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { IconCurrencyDollar, IconReload } from "@tabler/icons-react";

import { admin, type PricingMatrixRow, type PublicModel } from "@/lib/api";
import { rootRoute } from "@/routes/root";
import { PricingMatrixTable } from "@/components/pricing/matrix-table";
import { EditPricingDecisionModal } from "@/components/pricing/edit-modal";
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
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// Pricing is the operator surface for the 4-layer price system:
//   1. Higgs credits (from costsync /job-sets/costs → model_cost_rules)
//   2. Higgs plan cost (credits × plan_credit_rates unit cost)
//   3. Official upstream API price (official_price_observations)
//   4. Our sell price (model_price_decisions, editable)
//
// The list on the left is every catalog model; clicking one opens the
// full pricing matrix on the right. From the matrix a per-variant Edit
// modal appends a new decision — the underlying table keeps history so
// operators can review reasoning trails.
function Pricing() {
  const { t } = useTranslation();

  const [search, setSearch] = useState("");
  const [outputFilter, setOutputFilter] = useState("all");
  const [selectedAlias, setSelectedAlias] = useState<string | null>(null);
  const [editVariant, setEditVariant] = useState<PricingMatrixRow | null>(null);

  const modelsQ = useQuery({
    queryKey: ["v1", "models"],
    queryFn: () => admin.listPublicModels(),
    staleTime: 60_000,
  });

  // Fetch the pricing matrix only when the sheet is open. `retry: false`
  // matches the Models-page behavior — a 503 (pricing store not
  // configured) should surface immediately rather than back off.
  const matrixQ = useQuery({
    queryKey: ["admin", "model-pricing-matrix", selectedAlias ?? ""],
    queryFn: () => admin.getModelPricingMatrix(selectedAlias!),
    enabled: selectedAlias !== null,
    staleTime: 30_000,
    retry: false,
  });

  const models = modelsQ.data?.data ?? [];

  const outputTypes = useMemo(() => {
    const set = new Set<string>();
    for (const m of models) if (m.output) set.add(m.output);
    return Array.from(set).sort();
  }, [models]);

  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return models.filter((m) => {
      if (outputFilter !== "all" && m.output !== outputFilter) return false;
      if (!needle) return true;
      return (
        m.id.toLowerCase().includes(needle) ||
        m.jst.toLowerCase().includes(needle)
      );
    });
  }, [models, outputFilter, search]);

  const selectedModel = useMemo(
    () => models.find((m) => m.id === selectedAlias) ?? null,
    [models, selectedAlias],
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconCurrencyDollar className="size-5" />{" "}
          {t("pricing.title", { defaultValue: "Pricing" })}
        </CardTitle>
        <CardDescription>
          {t("pricing.description", {
            defaultValue:
              "Higgs credits, plan cost, upstream official API, and our sell price for every catalog model.",
          })}
        </CardDescription>
        <CardAction className="flex flex-wrap items-center gap-2">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t("pricing.filters.search", {
              defaultValue: "Search model or JST",
            })}
            className="h-8 w-48"
          />
          <Select value={outputFilter} onValueChange={setOutputFilter}>
            <SelectTrigger size="sm" className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                {t("pricing.filters.allTypes", { defaultValue: "All types" })}
              </SelectItem>
              {outputTypes.map((v) => (
                <SelectItem key={v} value={v}>
                  {v}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            size="sm"
            variant="outline"
            onClick={() => modelsQ.refetch()}
            disabled={modelsQ.isFetching}
          >
            <IconReload className="size-4" />
          </Button>
          <Button asChild size="sm" variant="secondary">
            <Link to="/pricing/floor-suggestions">
              {t("pricing.floor.linkLabel", {
                defaultValue: "Floor suggestions",
              })}
            </Link>
          </Button>
          <Button asChild size="sm" variant="secondary">
            <Link to="/pricing/purchase-batches">
              {t("pricing.batches.linkLabel", {
                defaultValue: "Purchase batches",
              })}
            </Link>
          </Button>
        </CardAction>
      </CardHeader>

      <CardContent>
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[80px] text-center">#</TableHead>
                <TableHead>
                  {t("pricing.columns.model", { defaultValue: "Model" })}
                </TableHead>
                <TableHead>
                  {t("pricing.columns.jst", { defaultValue: "Endpoint" })}
                </TableHead>
                <TableHead>
                  {t("pricing.columns.type", { defaultValue: "Type" })}
                </TableHead>
                <TableHead>
                  {t("pricing.columns.minPlan", { defaultValue: "Min plan" })}
                </TableHead>
                <TableHead className="text-right">
                  {t("pricing.columns.estCost", {
                    defaultValue: "Est. credits",
                  })}
                </TableHead>
                <TableHead className="w-[140px] text-right">
                  {t("pricing.columns.action", { defaultValue: "Action" })}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {modelsQ.isLoading ? (
                <>
                  {Array.from({ length: 6 }).map((_, i) => (
                    <TableRow key={i}>
                      <TableCell colSpan={7}>
                        <Skeleton className="h-6 w-full" />
                      </TableCell>
                    </TableRow>
                  ))}
                </>
              ) : filtered.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={7}
                    className="text-center text-sm text-muted-foreground"
                  >
                    {t("pricing.emptyList", {
                      defaultValue: "No models match the current filter.",
                    })}
                  </TableCell>
                </TableRow>
              ) : (
                filtered.map((m, index) => (
                  <PricingRow
                    key={m.id}
                    index={index + 1}
                    model={m}
                    onOpen={setSelectedAlias}
                  />
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </CardContent>

      <Sheet
        open={selectedAlias !== null}
        onOpenChange={(open) => {
          if (!open) {
            setSelectedAlias(null);
            setEditVariant(null);
          }
        }}
      >
        <SheetContent
          side="right"
          className="w-full sm:w-[min(92vw,72rem)] sm:max-w-6xl"
        >
          <SheetHeader>
            <SheetTitle>
              <span className="font-mono">{selectedAlias}</span>
            </SheetTitle>
            <SheetDescription>
              {selectedModel ? (
                <span className="flex flex-wrap items-center gap-2 text-xs">
                  <Badge variant="outline">{selectedModel.output}</Badge>
                  {/* "接生位置": JST + endpoint. Gray helper text so
                      it does not compete with the pricing matrix. */}
                  <span
                    className="font-mono text-muted-foreground"
                    title={t("pricing.jstTooltip", {
                      defaultValue: "Upstream job set type on higgsfield",
                    })}
                  >
                    jst: {selectedModel.jst}
                  </span>
                  <span className="text-muted-foreground">
                    · {selectedModel.min_plan || "no gate"}
                  </span>
                </span>
              ) : null}
            </SheetDescription>
          </SheetHeader>

          <div className="p-4">
            <PricingMatrixTable
              rows={matrixQ.data?.rows ?? []}
              loading={matrixQ.isLoading}
              failed={matrixQ.isError}
              onEditDecision={(row) => setEditVariant(row)}
            />
          </div>
        </SheetContent>
      </Sheet>

      {selectedAlias ? (
        <EditPricingDecisionModal
          alias={selectedAlias}
          variant={editVariant}
          onOpenChange={(open) => {
            if (!open) setEditVariant(null);
          }}
        />
      ) : null}
    </Card>
  );
}

// PricingRow keeps each row's click target isolated so a large catalog
// re-renders efficiently. Rendered by the table body in Pricing above.
function PricingRow({
  index,
  model,
  onOpen,
}: {
  index: number;
  model: PublicModel;
  onOpen: (alias: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <TableRow
      className="cursor-pointer hover:bg-muted/40"
      onClick={() => onOpen(model.id)}
    >
      <TableCell className="text-center text-xs tabular-nums text-muted-foreground">
        #{index}
      </TableCell>
      <TableCell>
        <span className="font-medium">{model.id}</span>
      </TableCell>
      <TableCell className="font-mono text-xs text-muted-foreground">
        {model.jst}
      </TableCell>
      <TableCell>
        <Badge variant="outline">{model.output}</Badge>
      </TableCell>
      <TableCell className="text-xs">
        {model.min_plan || (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-right tabular-nums text-xs">
        {model.est_cost > 0 ? `${model.est_cost} cr` : "—"}
      </TableCell>
      <TableCell className="text-right">
        <Button
          size="sm"
          variant="outline"
          onClick={(e) => {
            e.stopPropagation();
            onOpen(model.id);
          }}
        >
          {t("pricing.viewAndEdit", { defaultValue: "View & edit" })}
        </Button>
      </TableCell>
    </TableRow>
  );
}

export const pricingRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/pricing",
  staticData: { titleKey: "nav.pricing" },
  component: Pricing,
});
