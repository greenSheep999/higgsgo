import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { IconRotate } from "@tabler/icons-react";

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
import { StatusBadge } from "@/components/ui/status-badge";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import {
  admin,
  ApiError,
  type FailoverConfig,
  type FailoverConfigPatch,
} from "@/lib/api";

// FailoverDialog surfaces the /admin/failover/* subsystem: the two
// isolation mechanisms' tunables (mechanism ① consecutive-failure
// auto-disable and mechanism ② throttle window / evict cascade), plus
// a live list of accounts currently isolated with a "recover" button.
// Opens from the sidebar's Pool health indicator.

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function FailoverDialog({ open, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();

  const cfgQ = useQuery({
    queryKey: ["admin", "failover", "config"],
    queryFn: admin.getFailoverConfig,
    enabled: open,
  });
  const isolatedQ = useQuery({
    queryKey: ["admin", "failover", "isolated"],
    queryFn: admin.listIsolatedAccounts,
    enabled: open,
    refetchInterval: open ? 15_000 : undefined,
  });

  const save = useMutation({
    mutationFn: (patch: FailoverConfigPatch) =>
      admin.updateFailoverConfig(patch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "failover"] });
      toast.success(t("failover.toasts.saved"));
    },
    onError: (err) =>
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : String(err),
      ),
  });

  const recover = useMutation({
    mutationFn: (id: string) => admin.recoverAccount(id),
    onSuccess: (_r, id) => {
      qc.invalidateQueries({ queryKey: ["admin", "failover", "isolated"] });
      qc.invalidateQueries({ queryKey: ["admin", "stats", "pool"] });
      toast.success(t("failover.toasts.recovered", { id }));
    },
    onError: (err) =>
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : String(err),
      ),
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("failover.title")}</DialogTitle>
          <DialogDescription>{t("failover.description")}</DialogDescription>
        </DialogHeader>

        <Tabs defaultValue="config">
          <TabsList>
            <TabsTrigger value="config">{t("failover.tabs.config")}</TabsTrigger>
            <TabsTrigger value="isolated">
              {t("failover.tabs.isolated", {
                count: isolatedQ.data?.total ?? 0,
              })}
            </TabsTrigger>
          </TabsList>

          <TabsContent value="config" className="pt-4">
            {cfgQ.isLoading || !cfgQ.data ? (
              <p className="text-sm text-muted-foreground">
                {t("common.loading")}
              </p>
            ) : (
              <ConfigForm
                initial={cfgQ.data}
                onSave={(patch) => save.mutate(patch)}
                saving={save.isPending}
              />
            )}
          </TabsContent>

          <TabsContent value="isolated" className="pt-4">
            {isolatedQ.isLoading ? (
              <p className="text-sm text-muted-foreground">
                {t("common.loading")}
              </p>
            ) : (isolatedQ.data?.total ?? 0) === 0 ? (
              <p className="rounded-md border border-emerald-500/40 bg-emerald-500/5 p-4 text-sm text-emerald-700 dark:text-emerald-400">
                {t("failover.isolated.empty")}
              </p>
            ) : (
              <div className="space-y-2">
                {isolatedQ.data!.data.map((row) => (
                  <div
                    key={row.id}
                    className="flex items-center gap-2 rounded-md border p-3"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <StatusBadge
                          tone={
                            row.status === "throttled" ? "warning" : "danger"
                          }
                        >
                          {row.status}
                        </StatusBadge>
                        <span className="truncate font-medium">{row.email}</span>
                      </div>
                      <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
                        {row.status_reason || "—"}
                        {row.recent_events > 0
                          ? ` · ${t("failover.isolated.recentEvents", { count: row.recent_events })}`
                          : ""}
                        {row.throttled_until
                          ? ` · ${t("failover.isolated.until", { time: row.throttled_until })}`
                          : ""}
                      </div>
                    </div>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => recover.mutate(row.id)}
                      disabled={recover.isPending}
                    >
                      <IconRotate className="size-3.5" />
                      {t("failover.isolated.recover")}
                    </Button>
                  </div>
                ))}
              </div>
            )}
          </TabsContent>
        </Tabs>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ConfigForm holds the live editor for both mechanisms + outage guard.
// Local state so operators can tweak several sliders and save once;
// diff against `initial` is trivial (deep-copy pattern) since the
// server accepts a partial patch anyway.
function ConfigForm({
  initial,
  onSave,
  saving,
}: {
  initial: FailoverConfig;
  onSave: (patch: FailoverConfigPatch) => void;
  saving: boolean;
}) {
  const { t } = useTranslation();
  const [cfg, setCfg] = useState<FailoverConfig>(initial);
  useEffect(() => setCfg(initial), [initial]);

  const numChange =
    (setter: (n: number) => void) =>
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const n = Number.parseInt(e.target.value, 10);
      if (Number.isFinite(n) && n >= 0) setter(n);
    };

  return (
    <div className="space-y-5">
      {/* Master enable */}
      <label className="flex items-center gap-3 rounded-md border p-3">
        <Switch
          checked={cfg.enabled}
          onCheckedChange={(v) => setCfg({ ...cfg, enabled: v })}
        />
        <div className="flex-1">
          <div className="text-sm font-medium">
            {t("failover.master.enable")}
          </div>
          <div className="text-[11px] text-muted-foreground">
            {t("failover.master.hint")}
          </div>
        </div>
      </label>

      {/* Mechanism ① consecutive */}
      <section className="space-y-2 rounded-md border p-3">
        <label className="flex items-center gap-3">
          <Switch
            checked={cfg.consecutive.enabled}
            onCheckedChange={(v) =>
              setCfg({
                ...cfg,
                consecutive: { ...cfg.consecutive, enabled: v },
              })
            }
          />
          <div className="flex-1">
            <div className="text-sm font-medium">
              {t("failover.consec.title")}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {t("failover.consec.hint")}
            </div>
          </div>
        </label>
        <div className="grid grid-cols-[10rem_1fr] items-center gap-3 pt-1">
          <Label className="text-xs">{t("failover.consec.failLimit")}</Label>
          <Input
            type="number"
            min={1}
            value={cfg.consecutive.fail_limit}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                consecutive: { ...cfg.consecutive, fail_limit: n },
              }),
            )}
            className="w-28"
          />
        </div>
      </section>

      {/* Mechanism ② throttle */}
      <section className="space-y-2 rounded-md border p-3">
        <label className="flex items-center gap-3">
          <Switch
            checked={cfg.throttle.enabled}
            onCheckedChange={(v) =>
              setCfg({ ...cfg, throttle: { ...cfg.throttle, enabled: v } })
            }
          />
          <div className="flex-1">
            <div className="text-sm font-medium">
              {t("failover.throttle.title")}
            </div>
            <div className="text-[11px] text-muted-foreground">
              {t("failover.throttle.hint")}
            </div>
          </div>
        </label>
        <div className="grid grid-cols-[10rem_1fr] items-center gap-3 pt-1">
          <Label className="text-xs">
            {t("failover.throttle.judgeWindowSec")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.throttle.judge_window_sec}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                throttle: { ...cfg.throttle, judge_window_sec: n },
              }),
            )}
            className="w-28"
          />
          <Label className="text-xs">
            {t("failover.throttle.judgeCount")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.throttle.judge_count}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                throttle: { ...cfg.throttle, judge_count: n },
              }),
            )}
            className="w-28"
          />
          <Label className="text-xs">{t("failover.throttle.cooldownSec")}</Label>
          <Input
            type="number"
            min={1}
            value={cfg.throttle.cooldown_sec}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                throttle: { ...cfg.throttle, cooldown_sec: n },
              }),
            )}
            className="w-28"
          />
          <Label className="text-xs">
            {t("failover.throttle.evictWindowSec")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.throttle.evict_window_sec}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                throttle: { ...cfg.throttle, evict_window_sec: n },
              }),
            )}
            className="w-28"
          />
          <Label className="text-xs">
            {t("failover.throttle.evictCount")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.throttle.evict_count}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                throttle: { ...cfg.throttle, evict_count: n },
              }),
            )}
            className="w-28"
          />
        </div>
      </section>

      {/* Outage guard */}
      <section className="space-y-2 rounded-md border p-3">
        <div className="text-sm font-medium">
          {t("failover.outage.title")}
        </div>
        <div className="text-[11px] text-muted-foreground">
          {t("failover.outage.hint")}
        </div>
        <div className="grid grid-cols-[10rem_1fr] items-center gap-3 pt-1">
          <Label className="text-xs">
            {t("failover.outage.windowSec")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.outage_guard.window_sec}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                outage_guard: { ...cfg.outage_guard, window_sec: n },
              }),
            )}
            className="w-28"
          />
          <Label className="text-xs">
            {t("failover.outage.disableCountLimit")}
          </Label>
          <Input
            type="number"
            min={1}
            value={cfg.outage_guard.disable_count_limit}
            onChange={numChange((n) =>
              setCfg({
                ...cfg,
                outage_guard: {
                  ...cfg.outage_guard,
                  disable_count_limit: n,
                },
              }),
            )}
            className="w-28"
          />
        </div>
      </section>

      <div className="flex justify-end">
        <Button
          onClick={() =>
            onSave({
              enabled: cfg.enabled,
              consecutive: cfg.consecutive,
              throttle: cfg.throttle,
              outage_guard: cfg.outage_guard,
            })
          }
          disabled={saving}
        >
          {saving ? t("common.loading") : t("common.save")}
        </Button>
      </div>
    </div>
  );
}
