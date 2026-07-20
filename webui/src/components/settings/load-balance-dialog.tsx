import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

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
import { Switch } from "@/components/ui/switch";
import {
  admin,
  ApiError,
  type LoadBalanceSettings,
  type LoadBalanceSettingsInput,
} from "@/lib/api";

// LoadBalanceDialog surfaces /admin/settings/load_balance — six knobs
// that tweak the load_balance route strategy's internal ordering
// (tier-aware CASE, unlim / free_quota preference, richer-first tail,
// balance headroom, RANDOM() jitter). Opens from the settings dropdown
// in the site header.
//
// All six flags are live: the refresher populates the
// account_unlim_activations table plus the seven per-family free-quota
// columns on every tick, so prefer_unlim / prefer_free_quota take
// effect immediately after the operator toggles them on.

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const HEADROOM_MIN = 100;
const HEADROOM_MAX = 500;

// defaults() keeps the fallback shape aligned with the backend so the
// dialog can render before the fetch finishes without flashing an
// impossible zero row.
function defaults(): LoadBalanceSettingsInput {
  return {
    tier_aware: true,
    prefer_unlim: false,
    prefer_free_quota: false,
    prefer_richer: false,
    balance_headroom_pct: 120,
    jitter: true,
  };
}

export function LoadBalanceDialog({ open, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();

  const q = useQuery({
    queryKey: ["admin", "settings", "load_balance"],
    queryFn: admin.getLoadBalanceSettings,
    enabled: open,
    staleTime: 30_000,
  });

  const [form, setForm] = useState<LoadBalanceSettingsInput>(defaults());
  const [headroomText, setHeadroomText] = useState<string>("120");

  // Sync the form with the fetched data whenever the dialog opens or
  // the underlying query returns a fresh payload. Keeping headroom as
  // a separate string lets the operator type intermediate values
  // (e.g. "1" before typing "50") without the input snapping back.
  useEffect(() => {
    if (q.data) {
      const { source: _s, ...body } = q.data as LoadBalanceSettings;
      setForm(body);
      setHeadroomText(String(body.balance_headroom_pct));
    }
  }, [q.data]);

  const save = useMutation({
    mutationFn: (body: LoadBalanceSettingsInput) =>
      admin.updateLoadBalanceSettings(body),
    onSuccess: (res) => {
      qc.setQueryData(["admin", "settings", "load_balance"], res);
      toast.success(t("loadBalance.toasts.saved"));
    },
    onError: (err) =>
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : String(err),
      ),
  });

  // Headroom validation runs on the parsed value; the input stays a
  // free-form string so operators can clear the field without the UI
  // fighting them. `pctValid` gates the save button.
  const headroom = Number.parseInt(headroomText, 10);
  const pctValid =
    Number.isFinite(headroom) &&
    headroom >= HEADROOM_MIN &&
    headroom <= HEADROOM_MAX;

  const source = (q.data as LoadBalanceSettings | undefined)?.source ?? null;

  const onSave = () => {
    if (!pctValid) return;
    save.mutate({ ...form, balance_headroom_pct: headroom });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("loadBalance.title")}</DialogTitle>
          <DialogDescription>
            {t("loadBalance.description")}
          </DialogDescription>
        </DialogHeader>

        {q.isLoading ? (
          <p className="text-sm text-muted-foreground">{t("common.loading")}</p>
        ) : (
          <div className="space-y-3">
            {source === "default" ? (
              <div className="rounded-md border border-muted-foreground/20 bg-muted px-3 py-2 text-[11px] text-muted-foreground">
                {t("loadBalance.usingDefault")}
              </div>
            ) : null}

            {/* Tier-aware ordering */}
            <SwitchRow
              checked={form.tier_aware}
              onChange={(v) => setForm({ ...form, tier_aware: v })}
              label={t("loadBalance.tierAware.label")}
              hint={t("loadBalance.tierAware.hint")}
            />

            {/* Prefer unlim */}
            <SwitchRow
              checked={form.prefer_unlim}
              onChange={(v) => setForm({ ...form, prefer_unlim: v })}
              label={t("loadBalance.preferUnlim.label")}
              hint={t("loadBalance.preferUnlim.hint")}
            />

            {/* Prefer free quota */}
            <SwitchRow
              checked={form.prefer_free_quota}
              onChange={(v) => setForm({ ...form, prefer_free_quota: v })}
              label={t("loadBalance.preferFreeQuota.label")}
              hint={t("loadBalance.preferFreeQuota.hint")}
            />

            {/* Prefer richer */}
            <SwitchRow
              checked={form.prefer_richer}
              onChange={(v) => setForm({ ...form, prefer_richer: v })}
              label={t("loadBalance.preferRicher.label")}
              hint={t("loadBalance.preferRicher.hint")}
            />

            {/* Headroom */}
            <div className="rounded-md border p-3">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <div className="text-sm font-medium">
                    {t("loadBalance.headroom.label")}
                  </div>
                  <div className="text-[11px] text-muted-foreground">
                    {t("loadBalance.headroom.hint")}
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <Input
                    type="number"
                    min={HEADROOM_MIN}
                    max={HEADROOM_MAX}
                    step={5}
                    value={headroomText}
                    onChange={(e) => setHeadroomText(e.target.value)}
                    aria-invalid={!pctValid}
                    className="w-24"
                  />
                  <Label className="text-xs text-muted-foreground">%</Label>
                </div>
              </div>
              {!pctValid ? (
                <div className="mt-2 text-[11px] text-destructive">
                  {t("loadBalance.headroom.invalid", {
                    min: HEADROOM_MIN,
                    max: HEADROOM_MAX,
                  })}
                </div>
              ) : null}
            </div>

            {/* Jitter */}
            <SwitchRow
              checked={form.jitter}
              onChange={(v) => setForm({ ...form, jitter: v })}
              label={t("loadBalance.jitter.label")}
              hint={t("loadBalance.jitter.hint")}
            />
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button onClick={onSave} disabled={!pctValid || save.isPending}>
            {save.isPending ? t("common.loading") : t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// SwitchRow is the standard label+hint+switch row shared by the five
// bool knobs.
function SwitchRow({
  checked,
  onChange,
  label,
  hint,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  label: string;
  hint: string;
}) {
  return (
    <label className="flex items-center gap-3 rounded-md border p-3">
      <Switch checked={checked} onCheckedChange={onChange} />
      <div className="flex-1">
        <div className="text-sm font-medium">{label}</div>
        <div className="text-[11px] text-muted-foreground">{hint}</div>
      </div>
    </label>
  );
}
