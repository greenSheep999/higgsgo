import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { PricingMatrixRow, PricingOfficialValue } from "@/lib/api";
import { formatUSD } from "@/lib/format";

// PricingMatrixTable renders the operator-facing pricing comparison for
// one model. Columns (left → right):
//   Variant       — resolution/audio/unit; sub-tier mode noted as gray
//   Higgs         — credits/次 (upper) + per-credit cost (lower)
//   国内套餐       — ¥/month → credits/month (upper) + ¥/credit (lower)
//   国内 API       — one line per pricing tier (标准 / 批量 / …)
//   海外套餐       — $/month → credits/month + $/credit
//   海外 API       — one line per pricing tier
//   建议 / 自定义   — server-computed suggested price + click to override
// "接生位置" (JST + endpoint) is NOT its own column — it lives on the
// alias-header row above the matrix as gray helper text + tooltip. That
// keeps the matrix visually focused on the pricing axes.
export function PricingMatrixTable({
  rows,
  loading,
  failed,
  onEditDecision,
  cnPlan,
  intlPlan,
}: {
  rows: PricingMatrixRow[];
  loading: boolean;
  failed: boolean;
  onEditDecision?: (row: PricingMatrixRow) => void;
  // Optional aggregate plan info shown in the plan columns. Passed by
  // the page-level container which reads higgs_plan_rates. When absent
  // the plan columns fall back to "—".
  cnPlan?: { label: string; monthlyCNY: number; creditsPerMonth: number };
  intlPlan?: { label: string; monthlyUSD: number; creditsPerMonth: number };
}) {
  const { t } = useTranslation();
  if (loading) return <Skeleton className="h-28 w-full" />;
  if (failed) {
    return (
      <p className="text-xs text-destructive">
        {t("models.pricingMatrix.failed", { defaultValue: "Could not load pricing data." })}
      </p>
    );
  }
  if (rows.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        {t("models.pricingMatrix.empty", { defaultValue: "No granular pricing data yet." })}
      </p>
    );
  }

  return (
    <div className="overflow-x-auto rounded-md border">
      <Table className="min-w-[960px] text-xs">
        <TableHeader>
          <TableRow>
            <TableHead className="w-[160px]">
              {t("pricing.col.variant", { defaultValue: "Variant" })}
            </TableHead>
            <TableHead className="w-[112px]">
              {t("pricing.col.higgs", { defaultValue: "Higgs 积分" })}
            </TableHead>
            <TableHead className="w-[128px]">
              {t("pricing.col.cnPlan", { defaultValue: "国内套餐" })}
            </TableHead>
            <TableHead className="w-[128px]">
              {t("pricing.col.cnAPI", { defaultValue: "国内 API" })}
            </TableHead>
            <TableHead className="w-[128px]">
              {t("pricing.col.intlPlan", { defaultValue: "海外套餐" })}
            </TableHead>
            <TableHead className="w-[128px]">
              {t("pricing.col.intlAPI", { defaultValue: "海外 API" })}
            </TableHead>
            <TableHead className="w-[144px]">
              {t("pricing.col.suggested", { defaultValue: "建议 / 自定义" })}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row, index) => {
            const intlObs = (row.official_api ?? []).filter((o) => o.region === "intl");
            const cnObs = (row.official_api ?? []).filter((o) => o.region === "cn");
            return (
              <TableRow key={`${matrixSpec(row)}-${index}`}>
                <TableCell className="align-top font-medium">
                  <div>
                    {row.dimensions.resolution ||
                      t("models.pricingMatrix.anyResolution", { defaultValue: "Any resolution" })}
                  </div>
                  <div className="mt-1 flex flex-wrap gap-1 text-[10px] font-normal text-muted-foreground">
                    {row.dimensions.audio && <span>audio: {row.dimensions.audio}</span>}
                    {row.dimensions.duration_seconds > 0 && (
                      <span>{row.dimensions.duration_seconds}s</span>
                    )}
                    <span>{formatPriceUnit(row.dimensions.unit)}</span>
                  </div>
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  <HiggsCell row={row} />
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  {cnPlan ? (
                    <PlanCell
                      currency="CNY"
                      amount={cnPlan.monthlyCNY}
                      credits={cnPlan.creditsPerMonth}
                      label={cnPlan.label}
                    />
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  <OfficialCell obs={cnObs} currency="CNY" unit={row.dimensions.unit} />
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  {intlPlan ? (
                    <PlanCell
                      currency="USD"
                      amount={intlPlan.monthlyUSD}
                      credits={intlPlan.creditsPerMonth}
                      label={intlPlan.label}
                    />
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  <OfficialCell obs={intlObs} currency="USD" unit={row.dimensions.unit} />
                </TableCell>

                <TableCell className="align-top tabular-nums">
                  <SuggestedCell row={row} onEditDecision={onEditDecision} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

// HiggsCell shows credits/次 (top) and Higgs plan unit cost (bottom).
// When Higgs credits row has multiple components (pro/std sub-tiers), we
// display the max — that's what suggested_price uses too, keeping the
// upper number aligned with what the operator will price against.
function HiggsCell({ row }: { row: PricingMatrixRow }) {
  if (row.higgs.length === 0) return <span className="text-muted-foreground">—</span>;
  const maxCredits = Math.max(...row.higgs.map((h) => h.credits));
  const cheapestPlan = row.plan_costs.reduce<number | null>(
    (min, p) => (min === null || p.usd < min ? p.usd : min),
    null,
  );
  return (
    <div>
      <div>{maxCredits.toLocaleString()} cr</div>
      {cheapestPlan !== null && (
        <div className="text-[10px] text-muted-foreground">
          {formatUSD(cheapestPlan / maxCredits)}/cr
        </div>
      )}
      {row.higgs.length > 1 && (
        <div className="mt-0.5 text-[10px] text-muted-foreground">
          {row.higgs.map((h) => h.component).filter(Boolean).join(" · ")}
        </div>
      )}
    </div>
  );
}

// PlanCell shows a subscription-plan summary: monthly amount + credits +
// per-credit rate. Kept minimal — one plan tier at a time, chosen by the
// page container.
function PlanCell({
  currency,
  amount,
  credits,
  label,
}: {
  currency: "USD" | "CNY";
  amount: number;
  credits: number;
  label: string;
}) {
  const symbol = currency === "CNY" ? "¥" : "$";
  const perCredit = credits > 0 ? amount / credits : 0;
  return (
    <div>
      <div>
        {symbol}
        {amount.toLocaleString()} → {credits.toLocaleString()}cr
      </div>
      <div className="text-[10px] text-muted-foreground">
        {symbol}
        {perCredit.toFixed(4)}/cr
      </div>
      <div className="mt-0.5 text-[10px] text-muted-foreground">{label}</div>
    </div>
  );
}

// OfficialCell renders one line per official-API tier. Multiple lines
// happen when the provider publishes tiered pricing (标准/批量/企业) or
// when a region has multi-mode variants (e.g. Kling Omni no_video vs
// video_input).
function OfficialCell({
  obs,
  currency,
  unit,
}: {
  obs: PricingOfficialValue[];
  currency: "USD" | "CNY";
  unit: string;
}) {
  if (obs.length === 0) return <span className="text-muted-foreground">未采集</span>;
  const symbol = currency === "CNY" ? "¥" : "$";
  const unitSuffix = unit === "per_second" ? "/s" : unit === "per_request" ? "/req" : "";
  return (
    <div className="space-y-0.5">
      {obs.map((o, i) => (
        <div key={`${o.provider}-${o.mode ?? ""}-${i}`} className="flex items-baseline gap-1">
          <span>
            {symbol}
            {o.usd.toFixed(4)}
            {unitSuffix}
          </span>
          {o.estimated && (
            <span className="text-[9px] text-amber-600">(估算)</span>
          )}
          {o.mode && (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="text-[9px] text-muted-foreground">· {o.mode}</span>
              </TooltipTrigger>
              <TooltipContent side="top">
                <span className="text-xs">{o.provider}</span>
              </TooltipContent>
            </Tooltip>
          )}
        </div>
      ))}
    </div>
  );
}

// SuggestedCell renders server-suggested price + final decision.
// - Suggested price is advisory (contract §10 floor × markup)
// - Clicking the cell opens the operator's custom-price editor
// - When a final decision exists, that's shown in bold above the suggested
function SuggestedCell({
  row,
  onEditDecision,
}: {
  row: PricingMatrixRow;
  onEditDecision?: (row: PricingMatrixRow) => void;
}) {
  const { t } = useTranslation();
  const suggested = row.suggested_price;
  const final = row.final_price;

  const content = (
    <div className="text-left">
      {final ? (
        <div className="font-semibold">{formatUSD(final.usd)}</div>
      ) : (
        <div className="text-muted-foreground">
          {t("pricing.setPrice", { defaultValue: "Set price" })}
        </div>
      )}
      {suggested && (
        <div className="text-[10px] text-muted-foreground">
          建议 {formatUSD(suggested.usd)} ({suggested.markup_multiplier.toFixed(1)}×)
        </div>
      )}
      {suggested && (
        <div className="text-[9px] text-muted-foreground">
          floor {formatUSD(suggested.floor_usd)}
        </div>
      )}
    </div>
  );

  if (!onEditDecision) return content;
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      className="h-auto min-w-[96px] justify-start px-2 py-1 tabular-nums"
      onClick={() => onEditDecision(row)}
    >
      {content}
    </Button>
  );
}

function matrixSpec(row: PricingMatrixRow): string {
  const d = row.dimensions;
  return [d.resolution, d.duration_seconds, d.mode, d.audio, d.unit].join("|");
}

function formatPriceUnit(unit: string): string {
  if (unit === "per_second") return "/s";
  if (unit === "per_request") return "/request";
  return unit || "unit unspecified";
}
