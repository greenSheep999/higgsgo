import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { admin, ApiError } from "@/lib/api";
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

// CreateGroupDialog wraps POST /admin/groups. The form exposes the two
// levers operators reach for most often (route strategy + monthly
// budget) up front; the rest — regex-based model allowlists, owner
// binding — are advanced settings deferred to the detail sheet.

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
}

export function CreateGroupDialog({ open, onOpenChange, onCreated }: Props) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [maxJobs, setMaxJobs] = useState("0");
  const [maxPerAcct, setMaxPerAcct] = useState("0");
  const [budget, setBudget] = useState("0");
  const [route, setRoute] = useState("round_robin");

  const create = useMutation({
    mutationFn: () =>
      admin.createGroup({
        name: name.trim(),
        description: description.trim() || undefined,
        max_concurrent_jobs: parseInt(maxJobs || "0", 10),
        max_concurrent_per_account: parseInt(maxPerAcct || "0", 10),
        monthly_credit_budget: Math.round(parseFloat(budget || "0") * 100),
        route_strategy: route,
      }),
    onSuccess: (res) => {
      toast.success(t("groups.toasts.created", { name: res.name }));
      onCreated();
      onOpenChange(false);
      setName("");
      setDescription("");
      setMaxJobs("0");
      setMaxPerAcct("0");
      setBudget("0");
      setRoute("round_robin");
    },
    onError: (err) => {
      toast.error(
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "create failed",
      );
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("groups.createTitle")}</DialogTitle>
          <DialogDescription>{t("groups.description")}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          <div className="grid gap-2">
            <Label htmlFor="grp-name">{t("groups.form.name")}</Label>
            <Input
              id="grp-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="grp-desc">{t("groups.form.description")}</Label>
            <Textarea
              id="grp-desc"
              rows={2}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="grp-max">
                {t("groups.form.maxConcurrent")}
              </Label>
              <Input
                id="grp-max"
                type="number"
                min={0}
                value={maxJobs}
                onChange={(e) => setMaxJobs(e.target.value)}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="grp-max-per">
                {t("groups.form.maxConcurrentPerAccount")}
              </Label>
              <Input
                id="grp-max-per"
                type="number"
                min={0}
                value={maxPerAcct}
                onChange={(e) => setMaxPerAcct(e.target.value)}
              />
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="grp-budget">
              {t("groups.form.monthlyBudget")}
            </Label>
            <Input
              id="grp-budget"
              type="number"
              min={0}
              step="0.01"
              value={budget}
              onChange={(e) => setBudget(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              {t("groups.form.monthlyBudgetHint")}
            </p>
          </div>
          <div className="grid gap-2">
            <Label>{t("groups.form.routeStrategy")}</Label>
            <Select value={route} onValueChange={setRoute}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="round_robin">round_robin</SelectItem>
                <SelectItem value="least_used">least_used</SelectItem>
                <SelectItem value="priority">priority</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => create.mutate()}
            disabled={!name.trim() || create.isPending}
          >
            {create.isPending ? t("common.loading") : t("groups.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
