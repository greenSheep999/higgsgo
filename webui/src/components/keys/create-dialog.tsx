import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation } from "@tanstack/react-query";
import { toast } from "sonner";
import { admin, ApiError, type ApiKey } from "@/lib/api";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  ToggleGroup,
  ToggleGroupItem,
} from "@/components/ui/toggle-group";

// CreateKeyDialog wraps POST /admin/keys. The server issues both the row
// and a one-shot plaintext; the plaintext bubbles up through onCreated
// so the parent can hand it to PlaintextRevealDialog. We never persist
// or log the plaintext anywhere else — it dies as soon as that reveal
// dialog is dismissed.

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (res: ApiKey & { plaintext_key: string }) => void;
}

export function CreateKeyDialog({ open, onOpenChange, onCreated }: Props) {
  const { t } = useTranslation();
  const [kind, setKind] = useState<"default" | "project">("project");
  const [name, setName] = useState("");
  const [createdBy, setCreatedBy] = useState("");
  const [quotaCredits, setQuotaCredits] = useState("0");
  const [markup, setMarkup] = useState("1.0");
  const [scope, setScope] = useState<"none" | "cheap" | "full">("none");

  // Kind switch: when the operator picks a kind, we pre-fill sensible
  // defaults for it so they don't have to remember which knobs match
  // which kind. Default keys are the operator's own — broad access,
  // no cap; project keys are downstream — tight scope, sane quota.
  const pickKind = (k: "default" | "project") => {
    setKind(k);
    if (k === "default") {
      setScope("full");
      setQuotaCredits("0");
    } else {
      setScope("none");
      setQuotaCredits("5000");
    }
  };

  const create = useMutation({
    mutationFn: () =>
      admin.createKey({
        name: name.trim(),
        created_by: createdBy.trim() || undefined,
        monthly_quota: Math.round(parseFloat(quotaCredits || "0") * 100),
        markup_pct: parseFloat(markup || "1"),
        playground_scope: scope,
        kind,
      }),
    onSuccess: (res) => {
      toast.success(t("keys.toasts.created", { name: res.name }));
      onCreated(res);
      onOpenChange(false);
      setKind("project");
      setName("");
      setCreatedBy("");
      setQuotaCredits("5000");
      setMarkup("1.0");
      setScope("none");
    },
    onError: (err) => {
      const msg =
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "create failed";
      toast.error(msg);
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("keys.createTitle")}</DialogTitle>
          <DialogDescription>{t("keys.createDescription")}</DialogDescription>
        </DialogHeader>

        <div className="grid gap-4 py-2">
          {/* Kind picker up top — the choice steers every default
              below (playground scope, quota) so making it the first
              interaction keeps the rest of the form sensible. */}
          <div className="grid gap-2">
            <Label>{t("keys.columns.kind")}</Label>
            <ToggleGroup
              type="single"
              variant="outline"
              value={kind}
              onValueChange={(v) => {
                if (v === "default" || v === "project") pickKind(v);
              }}
              className="w-full"
            >
              <ToggleGroupItem
                value="default"
                className="flex-1 flex-col items-start gap-0 py-3"
              >
                <span className="font-medium">
                  {t("keys.kind.default")}
                </span>
                <span className="text-[10px] text-muted-foreground">
                  {t("keys.kind.defaultHint")}
                </span>
              </ToggleGroupItem>
              <ToggleGroupItem
                value="project"
                className="flex-1 flex-col items-start gap-0 py-3"
              >
                <span className="font-medium">
                  {t("keys.kind.project")}
                </span>
                <span className="text-[10px] text-muted-foreground">
                  {t("keys.kind.projectHint")}
                </span>
              </ToggleGroupItem>
            </ToggleGroup>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="key-name">{t("keys.form.name")}</Label>
            <Input
              id="key-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("keys.form.namePlaceholder")}
              required
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="key-createdby">{t("keys.form.createdBy")}</Label>
            <Input
              id="key-createdby"
              value={createdBy}
              onChange={(e) => setCreatedBy(e.target.value)}
              placeholder={t("keys.form.createdByPlaceholder")}
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="key-quota">{t("keys.form.monthlyQuota")}</Label>
              <Input
                id="key-quota"
                type="number"
                min={0}
                step="0.01"
                value={quotaCredits}
                onChange={(e) => setQuotaCredits(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                {t("keys.form.monthlyQuotaHint")}
              </p>
            </div>
            <div className="grid gap-2">
              <Label htmlFor="key-markup">{t("keys.form.markupPct")}</Label>
              <Input
                id="key-markup"
                type="number"
                min={0}
                step="0.05"
                value={markup}
                onChange={(e) => setMarkup(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                {t("keys.form.markupPctHint")}
              </p>
            </div>
          </div>
          <div className="grid gap-2">
            <Label>{t("keys.form.playgroundScope")}</Label>
            <Select
              value={scope}
              onValueChange={(v) => setScope(v as typeof scope)}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">{t("keys.form.scopeNone")}</SelectItem>
                <SelectItem value="cheap">{t("keys.form.scopeCheap")}</SelectItem>
                <SelectItem value="full">{t("keys.form.scopeFull")}</SelectItem>
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
            {create.isPending ? t("common.loading") : t("keys.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
