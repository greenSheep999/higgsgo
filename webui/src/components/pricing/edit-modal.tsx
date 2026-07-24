import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import {
  admin,
  ApiError,
  type PricingDecisionWarning,
  type PricingMatrixRow,
} from "@/lib/api";
import { formatUSD, microsToUsd, usdToMicros } from "@/lib/format";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

// Props: pricing decisions target one variant at a time. `alias` +
// `variant` together identify the row that the modal is editing. Passing
// variant = null closes the dialog (so parents can render a single
// controlled instance in the tree).
interface Props {
  alias: string;
  variant: PricingMatrixRow | null;
  onOpenChange: (open: boolean) => void;
}

// EditPricingDecisionModal appends one row to model_price_decisions via
// POST /admin/models/{alias}/pricing-decisions. It shows the upstream
// cost signals (Higgs plan costs, official API price) inline so the
// operator can decide without another tab.
export function EditPricingDecisionModal({
  alias,
  variant,
  onOpenChange,
}: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const open = variant !== null;

  // Seed the USD input with the current final price if it exists — an
  // empty string when the row has never had a decision. Reset every
  // time the modal opens for a different variant so we don't leak the
  // previous target's number.
  const initialUsd = variant?.final_price
    ? String(variant.final_price.usd)
    : "";
  const [usd, setUsd] = useState(initialUsd);
  const [rationale, setRationale] = useState(variant?.final_price?.rationale ?? "");
  // Warnings from the last save. Populated on 201 responses that carried
  // a soft signal (retail_below_floor / cost_rule_missing). Cleared as
  // soon as the operator edits any field so stale warnings don't linger
  // past a corrective save.
  const [warnings, setWarnings] = useState<PricingDecisionWarning[]>([]);

  useEffect(() => {
    setUsd(variant?.final_price ? String(variant.final_price.usd) : "");
    setRationale(variant?.final_price?.rationale ?? "");
    setWarnings([]);
  }, [variant?.final_price?.usd, variant?.final_price?.rationale, variant]);

  const parsedUsd = parseFloat(usd);
  const invalid =
    usd.trim() === "" || Number.isNaN(parsedUsd) || parsedUsd < 0;

  const save = useMutation({
    mutationFn: async () => {
      if (!variant) throw new Error("no variant selected");
      if (invalid) throw new Error("invalid usd");
      return admin.createPricingDecision(alias, {
        currency: "USD",
        unit: variant.dimensions.unit || "per_request",
        price_micros: usdToMicros(parsedUsd),
        resolution: variant.dimensions.resolution,
        duration_seconds: variant.dimensions.duration_seconds,
        mode: variant.dimensions.mode,
        audio: variant.dimensions.audio,
        rationale: rationale.trim(),
      });
    },
    onSuccess: (view) => {
      const w = view.warnings ?? [];
      // Always invalidate — the row is durable regardless of warnings.
      qc.invalidateQueries({
        queryKey: ["admin", "model-pricing-matrix", alias],
      });
      if (w.length === 0) {
        toast.success(
          t("pricing.editModal.saved", {
            alias,
            price: formatUSD(microsToUsd(view.price_micros)),
            defaultValue: `Saved ${alias} at ${formatUSD(microsToUsd(view.price_micros))}`,
          }),
        );
        onOpenChange(false);
        return;
      }
      // Soft warning path: row wrote successfully but tripped a signal
      // (contract §10). Hold the modal open so the operator sees the
      // detail inline and can decide to revise (another save) or accept
      // (Close). The toast still confirms the write so the audit trail
      // is unambiguous.
      setWarnings(w);
      toast.warning(
        t("pricing.editModal.savedWithWarning", {
          alias,
          price: formatUSD(microsToUsd(view.price_micros)),
          defaultValue: `Saved ${alias} at ${formatUSD(microsToUsd(view.price_micros))} — review warnings`,
        }),
      );
    },
    onError: (err) => {
      const msg =
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "save failed";
      toast.error(msg);
    },
  });

  const specSummary = useMemo(() => {
    if (!variant) return "";
    const parts = [
      variant.dimensions.resolution || t("models.pricingMatrix.anyResolution", { defaultValue: "any resolution" }),
      variant.dimensions.mode,
      variant.dimensions.audio ? `audio ${variant.dimensions.audio}` : "",
      variant.dimensions.duration_seconds > 0
        ? `${variant.dimensions.duration_seconds}s`
        : "",
      variant.dimensions.unit,
    ].filter(Boolean);
    return parts.join(" · ");
  }, [variant, t]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {t("pricing.editModal.title", { defaultValue: "Set our sell price" })}
          </DialogTitle>
          <DialogDescription>
            <span className="block font-mono text-xs">{alias}</span>
            <span className="block text-xs text-muted-foreground">{specSummary}</span>
          </DialogDescription>
        </DialogHeader>

        {variant ? (
          <div className="space-y-4">
            <ReferencePrices variant={variant} />
            {warnings.length > 0 ? <WarningsPanel warnings={warnings} /> : null}

            <div className="space-y-1.5">
              <Label htmlFor="pricing-usd">
                {t("pricing.editModal.usdLabel", { defaultValue: "Sell price (USD)" })}
              </Label>
              <Input
                id="pricing-usd"
                type="number"
                step="0.001"
                min="0"
                value={usd}
                onChange={(e) => {
                  setUsd(e.target.value);
                  if (warnings.length > 0) setWarnings([]);
                }}
                placeholder="0.150"
                autoFocus
              />
              <p className="text-[11px] text-muted-foreground">
                {t("pricing.editModal.hint", {
                  defaultValue:
                    "Appends a new decision row — the previous price is kept as history.",
                })}
              </p>
            </div>

            <div className="space-y-1.5">
              <Label htmlFor="pricing-rationale">
                {t("pricing.editModal.rationaleLabel", { defaultValue: "Rationale" })}
              </Label>
              <Textarea
                id="pricing-rationale"
                value={rationale}
                onChange={(e) => setRationale(e.target.value)}
                placeholder="60% margin over Kling 720p standard rate"
                rows={3}
              />
            </div>
          </div>
        ) : null}

        <DialogFooter>
          <Button
            variant="ghost"
            type="button"
            onClick={() => onOpenChange(false)}
            disabled={save.isPending}
          >
            {t("common.cancel", { defaultValue: "Cancel" })}
          </Button>
          <Button
            type="button"
            onClick={() => save.mutate()}
            disabled={invalid || save.isPending}
          >
            {save.isPending
              ? t("common.saving", { defaultValue: "Saving…" })
              : t("pricing.editModal.save", { defaultValue: "Save decision" })}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ReferencePrices displays the upstream signals the operator should
// weigh: the Higgs plan costs (credits × plan unit) and the official
// upstream API price. Rendering them again inside the modal saves the
// operator from cross-referencing the sheet.
function ReferencePrices({ variant }: { variant: PricingMatrixRow }) {
  const { t } = useTranslation();
  const hasSignals =
    variant.official_api != null ||
    variant.plan_costs.length > 0 ||
    variant.higgs.length > 0;
  if (!hasSignals) return null;
  return (
    <div className="rounded-md border bg-muted/40 p-3 text-xs">
      <div className="mb-2 font-medium text-muted-foreground">
        {t("pricing.editModal.reference", {
          defaultValue: "Reference prices",
        })}
      </div>
      <div className="space-y-2">
        {variant.official_api ? (
          <div className="flex justify-between">
            <span className="text-muted-foreground">
              {variant.official_api.provider}
            </span>
            <span className="tabular-nums">
              {formatUSD(variant.official_api.usd)}
            </span>
          </div>
        ) : null}
        {variant.plan_costs.map((plan) => (
          <div
            key={`${plan.plan_type}-${plan.component}`}
            className="flex justify-between"
          >
            <span className="text-muted-foreground">
              {plan.plan_name}
              {plan.component ? ` · ${plan.component}` : ""}
            </span>
            <span className="tabular-nums">{formatUSD(plan.usd)}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

// WarningsPanel renders soft signals returned by POST decisions
// (contract §10). The row was already written — this panel exists so the
// operator sees the reason, not to gate the save. `retail_below_floor`
// gets a compact floor-vs-retail breakdown; other codes fall through to
// the plain message.
function WarningsPanel({ warnings }: { warnings: PricingDecisionWarning[] }) {
  const { t } = useTranslation();
  return (
    <div className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-xs text-destructive">
      <div className="mb-2 font-medium">
        {t("pricing.editModal.warningsHeader", {
          defaultValue: "Saved with warnings",
        })}
      </div>
      <ul className="space-y-2">
        {warnings.map((w, i) => (
          <li key={`${w.code}-${i}`}>
            <div className="font-semibold">{w.code}</div>
            <div className="mt-0.5 text-[11px] opacity-90">{w.message}</div>
            {w.code === "retail_below_floor" &&
            w.floor_micros != null &&
            w.retail_micros != null ? (
              <div className="mt-1 flex gap-4 text-[11px] tabular-nums opacity-80">
                <span>
                  {t("pricing.editModal.warningRetail", {
                    defaultValue: "retail",
                  })}
                  : {formatUSD(w.retail_micros / 1_000_000)}
                </span>
                <span>
                  {t("pricing.editModal.warningFloor", {
                    defaultValue: "floor",
                  })}
                  : {formatUSD(w.floor_micros / 1_000_000)}
                </span>
              </div>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}
