import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { admin, ApiError, type Group } from "@/lib/api";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ModelMultiSelect } from "@/components/groups/model-multiselect";

// EditGroupDialog wraps PUT /admin/groups/{id}. Mirrors EditKeyDialog:
// single-column form with a fixed-label / flex-input row layout so
// every input line starts on the same x. Only sends fields that have
// diverged from the row's snapshot, so callers accidentally clearing
// a textarea don't blow away the server value for other fields.

interface Props {
  group: Group | null;
  onOpenChange: (open: boolean) => void;
}

export function EditGroupDialog({ group, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [maxJobs, setMaxJobs] = useState("0");
  const [maxPerAcct, setMaxPerAcct] = useState("0");
  const [budgetCredits, setBudgetCredits] = useState("");
  const [route, setRoute] = useState("round_robin");
  const [allowed, setAllowed] = useState("");
  const [blocked, setBlocked] = useState("");

  // "0" on the wire is the sentinel for "no cap" on all three numeric
  // fields (concurrency / per-account concurrency / budget). Render
  // that as an EMPTY input so the placeholder "无限制" shows through;
  // saves convert empty → 0 back to the wire. Keeps a real cap value
  // visible unchanged.
  useEffect(() => {
    if (!group) return;
    setName(group.name);
    setDescription(group.description ?? "");
    setMaxJobs(group.max_concurrent_jobs > 0 ? String(group.max_concurrent_jobs) : "");
    setMaxPerAcct(
      group.max_concurrent_per_account > 0 ? String(group.max_concurrent_per_account) : "",
    );
    // credits stored as *100 on the wire; render as human credits.
    setBudgetCredits(
      group.monthly_credit_budget > 0
        ? (group.monthly_credit_budget / 100).toString()
        : "",
    );
    setRoute(group.route_strategy || "round_robin");
    setAllowed(group.allowed_models_regex ?? "");
    setBlocked(group.blocked_models_regex ?? "");
  }, [group]);

  const save = useMutation({
    mutationFn: async () => {
      if (!group) throw new Error("no group");
      const patch: Record<string, unknown> = {};
      const trimmedName = name.trim();
      if (trimmedName !== group.name) patch.name = trimmedName;
      if (description !== (group.description ?? ""))
        patch.description = description;
      const maxJobsNum = parseInt(maxJobs || "0", 10);
      if (maxJobsNum !== group.max_concurrent_jobs)
        patch.max_concurrent_jobs = maxJobsNum;
      const maxPerAcctNum = parseInt(maxPerAcct || "0", 10);
      if (maxPerAcctNum !== group.max_concurrent_per_account)
        patch.max_concurrent_per_account = maxPerAcctNum;
      const budgetH = Math.round(parseFloat(budgetCredits || "0") * 100);
      if (budgetH !== group.monthly_credit_budget)
        patch.monthly_credit_budget = budgetH;
      if (route !== group.route_strategy) patch.route_strategy = route;
      if (allowed !== (group.allowed_models_regex ?? ""))
        patch.allowed_models_regex = allowed;
      if (blocked !== (group.blocked_models_regex ?? ""))
        patch.blocked_models_regex = blocked;
      if (Object.keys(patch).length === 0) return group;
      return admin.updateGroup(group.id, patch);
    },
    onSuccess: () => {
      toast.success(t("groups.toasts.updated", { name: name.trim() }));
      qc.invalidateQueries({ queryKey: ["admin", "groups"] });
      onOpenChange(false);
    },
    onError: (err) => {
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "save failed",
      );
    },
  });

  return (
    <Dialog open={!!group} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("groups.editTitle")}</DialogTitle>
          <DialogDescription>{t("groups.editDescription")}</DialogDescription>
        </DialogHeader>

        <div className="space-y-3 py-2">
          <Row label={t("groups.form.name")}>
            <Input value={name} onChange={(e) => setName(e.target.value)} />
          </Row>

          <Row label={t("groups.form.description")}>
            <Textarea
              rows={2}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </Row>

          <Row
            label={t("groups.form.maxConcurrent")}
            hint={t("groups.form.maxConcurrentHint")}
          >
            <Input
              type="number"
              min={0}
              placeholder={t("common.unlimited")}
              value={maxJobs}
              onChange={(e) => setMaxJobs(e.target.value)}
            />
          </Row>

          <Row
            label={t("groups.form.maxConcurrentPerAccount")}
            hint={t("groups.form.maxConcurrentPerAccountHint")}
          >
            <Input
              type="number"
              min={0}
              placeholder={t("common.unlimited")}
              value={maxPerAcct}
              onChange={(e) => setMaxPerAcct(e.target.value)}
            />
          </Row>

          <Row
            label={t("groups.form.monthlyBudget")}
            hint={t("groups.form.monthlyBudgetHint")}
          >
            <Input
              type="number"
              min={0}
              step="0.01"
              placeholder={t("common.unlimited")}
              value={budgetCredits}
              onChange={(e) => setBudgetCredits(e.target.value)}
            />
          </Row>

          <Row label={t("groups.form.routeStrategy")}>
            <Select value={route} onValueChange={setRoute}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="round_robin">load_balance</SelectItem>
                <SelectItem value="priority">priority</SelectItem>
              </SelectContent>
            </Select>
          </Row>

          <Row
            label={t("groups.form.allowedRegex")}
            hint={t("groups.form.allowedRegexHint")}
          >
            <ModelMultiSelect value={allowed} onChange={setAllowed} />
          </Row>

          <Row
            label={t("groups.form.blockedRegex")}
            hint={t("groups.form.blockedRegexHint")}
          >
            <ModelMultiSelect value={blocked} onChange={setBlocked} />
          </Row>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => save.mutate()}
            disabled={save.isPending || !name.trim()}
          >
            {save.isPending ? t("common.loading") : t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Row({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid grid-cols-[7rem_1fr] items-start gap-3">
      <div className="flex h-9 items-center">
        <Label className="text-xs text-muted-foreground">{label}</Label>
      </div>
      <div className="min-w-0">
        {children}
        {hint ? (
          <p className="mt-1 text-[10px] text-muted-foreground">{hint}</p>
        ) : null}
      </div>
    </div>
  );
}
