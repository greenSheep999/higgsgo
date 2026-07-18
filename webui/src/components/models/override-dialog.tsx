import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { IconPlus, IconX } from "@tabler/icons-react";

import {
  admin,
  ApiError,
  type ModelOverride,
  type ModelOverridePatch,
  type PublicModel,
} from "@/lib/api";
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
import { Badge } from "@/components/ui/badge";

// TriState mirrors the wire semantics of every tier flag on the
// ModelOverride row: "spec" = inherit the static catalog value,
// "on"/"off" = explicit override that pins the value regardless of a
// future catalog reclassification. The select uses these string
// values verbatim so the URL param / localStorage roundtrip stays
// human-readable.
type TriState = "spec" | "on" | "off";

// wireToTri converts a nullable wire value into the three-state form
// used by the select. Called on both the initial load and every
// re-render after a save so the dialog stays in sync with the store.
function wireToTri(v: boolean | null | undefined): TriState {
  if (v === null || v === undefined) return "spec";
  return v ? "on" : "off";
}

// triToWire is the inverse — spec becomes null (unset the override),
// on/off become explicit booleans. Passing null on PUT is how a
// caller clears a single flag back to the spec default.
function triToWire(t: TriState): boolean | null {
  if (t === "spec") return null;
  return t === "on";
}

interface Props {
  model: PublicModel | null;
  onOpenChange: (open: boolean) => void;
}

// ModelOverrideDialog is the operator surface for the model_overrides
// table (migration 015). It reads the current override for the given
// model on open and offers three actions: save (upsert), reset (delete
// the row, all fields fall back to spec), and cancel. The tier flags
// are exposed as three-state selects; min_credits and note are plain
// inputs. ExtraAliases is a tag input — press Enter to commit a chip,
// click the x to remove.
export function ModelOverrideDialog({ model, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const alias = model?.id ?? "";
  const open = model !== null;

  // Fetch the current override lazily. A 404 lands in `error` — we
  // treat that as "no override yet" (all fields inherit spec).
  const overrideQ = useQuery({
    queryKey: ["admin", "model-override", alias],
    queryFn: () => admin.getModelOverride(alias),
    enabled: open,
    retry: false,
    staleTime: 0,
  });

  const [starter, setStarter] = useState<TriState>("spec");
  const [paid, setPaid] = useState<TriState>("spec");
  const [ultra, setUltra] = useState<TriState>("spec");
  const [unlim, setUnlim] = useState<TriState>("spec");
  const [minCredits, setMinCredits] = useState<string>("");
  const [aliases, setAliases] = useState<string[]>([]);
  const [aliasDraft, setAliasDraft] = useState("");
  const [note, setNote] = useState("");

  // Reset the local form state when the dialog opens on a new model
  // or the fetched override lands. A 404 (no row yet) still triggers
  // this branch — we fall back to spec defaults for every field.
  useEffect(() => {
    if (!open) return;
    const o: ModelOverride | undefined =
      overrideQ.data ?? overrideFromError(overrideQ.error);
    if (o) {
      setStarter(wireToTri(o.starter_locked));
      setPaid(wireToTri(o.requires_paid));
      setUltra(wireToTri(o.requires_ultra));
      setUnlim(wireToTri(o.requires_unlim));
      setMinCredits(
        o.min_credits_hundredths !== null && o.min_credits_hundredths > 0
          ? String(o.min_credits_hundredths / 100)
          : "",
      );
      setAliases(o.extra_aliases ?? []);
      setNote(o.note ?? "");
    } else {
      setStarter("spec");
      setPaid("spec");
      setUltra("spec");
      setUnlim("spec");
      setMinCredits("");
      setAliases([]);
      setNote("");
    }
    setAliasDraft("");
  }, [open, overrideQ.data, overrideQ.error]);

  const save = useMutation({
    mutationFn: async () => {
      const trimmed = minCredits.trim();
      let credits: number | null = null;
      if (trimmed !== "") {
        const n = Number(trimmed);
        if (!Number.isFinite(n) || n < 0) {
          throw new Error(t("models.override.errors.minCredits"));
        }
        // Store back in *100 units; 0 clears the override.
        credits = n === 0 ? null : Math.round(n * 100);
      }
      const patch: ModelOverridePatch = {
        starter_locked: triToWire(starter),
        requires_paid: triToWire(paid),
        requires_ultra: triToWire(ultra),
        requires_unlim: triToWire(unlim),
        min_credits_hundredths: credits,
        extra_aliases: aliases,
        note: note.trim(),
      };
      return admin.updateModelOverride(alias, patch);
    },
    onSuccess: () => {
      toast.success(t("models.override.toasts.saved", { alias }));
      // Invalidate everything downstream so the catalog + this dialog
      // both refetch with the new merged view.
      qc.invalidateQueries({ queryKey: ["v1", "models"] });
      qc.invalidateQueries({ queryKey: ["admin", "model-override"] });
      qc.invalidateQueries({ queryKey: ["admin", "model-overrides"] });
      onOpenChange(false);
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const reset = useMutation({
    mutationFn: () => admin.deleteModelOverride(alias),
    onSuccess: () => {
      toast.success(t("models.override.toasts.reset", { alias }));
      qc.invalidateQueries({ queryKey: ["v1", "models"] });
      qc.invalidateQueries({ queryKey: ["admin", "model-override"] });
      qc.invalidateQueries({ queryKey: ["admin", "model-overrides"] });
      onOpenChange(false);
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  // Add-alias flow: Enter commits the draft (deduped), backspace on
  // empty removes the last chip so the tag input feels native.
  function commitAlias() {
    const s = aliasDraft.trim();
    if (!s) return;
    if (aliases.includes(s)) {
      setAliasDraft("");
      return;
    }
    setAliases([...aliases, s]);
    setAliasDraft("");
  }
  function onAliasKey(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      commitAlias();
    } else if (e.key === "Backspace" && aliasDraft === "" && aliases.length > 0) {
      e.preventDefault();
      setAliases(aliases.slice(0, -1));
    }
  }

  const busy = save.isPending || reset.isPending;

  // A row is "dirty enough" for the reset button to make sense
  // whenever the server has a stored override — we don't hide it on
  // an empty form because the user might have JUST cleared it.
  const hasStored = useMemo(
    () => Boolean(overrideQ.data?.updated_at),
    [overrideQ.data],
  );

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onOpenChange(false)}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {t("models.override.title", { alias })}
          </DialogTitle>
          <DialogDescription>
            {t("models.override.description")}
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-3">
          <TierRow
            label={t("models.tier.starterLocked")}
            value={starter}
            onChange={setStarter}
          />
          <TierRow
            label={t("models.tier.paid")}
            value={paid}
            onChange={setPaid}
          />
          <TierRow
            label={t("models.tier.ultra")}
            value={ultra}
            onChange={setUltra}
          />
          <TierRow
            label={t("models.tier.unlim")}
            value={unlim}
            onChange={setUnlim}
          />

          <div className="flex items-center gap-2">
            <Label className="w-32 shrink-0 text-xs" htmlFor="mc">
              {t("models.override.minCredits")}
            </Label>
            <Input
              id="mc"
              className="h-8 flex-1"
              type="number"
              min="0"
              inputMode="decimal"
              placeholder={t("models.override.minCreditsHint")}
              value={minCredits}
              onChange={(e) => setMinCredits(e.target.value)}
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <Label className="text-xs" htmlFor="extra-aliases">
              {t("models.override.extraAliases")}
            </Label>
            <div className="flex flex-wrap items-center gap-1 rounded-md border px-2 py-1.5">
              {aliases.map((a) => (
                <Badge
                  key={a}
                  variant="secondary"
                  className="gap-1 pr-1 font-mono text-[11px]"
                >
                  {a}
                  <button
                    type="button"
                    className="inline-flex size-3.5 items-center justify-center rounded hover:bg-muted-foreground/20"
                    onClick={() =>
                      setAliases(aliases.filter((x) => x !== a))
                    }
                    aria-label={t("common.remove")}
                  >
                    <IconX className="size-2.5" />
                  </button>
                </Badge>
              ))}
              <input
                id="extra-aliases"
                className="min-w-24 flex-1 bg-transparent px-1 text-xs outline-none"
                value={aliasDraft}
                onChange={(e) => setAliasDraft(e.target.value)}
                onKeyDown={onAliasKey}
                onBlur={commitAlias}
                placeholder={t("models.override.extraAliasesHint")}
              />
              {aliasDraft && (
                <button
                  type="button"
                  className="inline-flex size-5 shrink-0 items-center justify-center rounded text-muted-foreground hover:bg-muted"
                  onClick={commitAlias}
                  aria-label={t("common.add")}
                >
                  <IconPlus className="size-3" />
                </button>
              )}
            </div>
            <p className="text-[10px] text-muted-foreground">
              {t("models.override.extraAliasesHelp")}
            </p>
          </div>

          <div className="flex flex-col gap-1.5">
            <Label className="text-xs" htmlFor="note">
              {t("models.override.note")}
            </Label>
            <Textarea
              id="note"
              className="min-h-16 text-xs"
              placeholder={t("models.override.noteHint")}
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
        </div>

        <DialogFooter className="flex-col-reverse gap-2 sm:flex-row sm:justify-between">
          <Button
            variant="ghost"
            className="text-destructive hover:text-destructive"
            onClick={() => reset.mutate()}
            disabled={busy || !hasStored}
          >
            {t("models.override.reset")}
          </Button>
          <div className="flex gap-2">
            <Button
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={busy}
            >
              {t("common.cancel")}
            </Button>
            <Button onClick={() => save.mutate()} disabled={busy}>
              {save.isPending ? t("common.loading") : t("common.save")}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// TierRow renders one three-state select. The label sits in a
// fixed-width column so every row lines up regardless of translation
// length.
function TierRow({
  label,
  value,
  onChange,
}: {
  label: string;
  value: TriState;
  onChange: (v: TriState) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="flex items-center gap-2">
      <Label className="w-32 shrink-0 text-xs">{label}</Label>
      <Select value={value} onValueChange={(v) => onChange(v as TriState)}>
        <SelectTrigger size="sm" className="w-40">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="spec">
            {t("models.override.tri.spec")}
          </SelectItem>
          <SelectItem value="on">
            {t("models.override.tri.on")}
          </SelectItem>
          <SelectItem value="off">
            {t("models.override.tri.off")}
          </SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}

// Extract a ModelOverride from a 404 shape returned by request().
// The admin.getModelOverride path throws ApiError{status:404} when no
// row exists; treat that as "no override yet" without surfacing the
// error to the user.
function overrideFromError(err: unknown): ModelOverride | undefined {
  if (err instanceof ApiError && err.status === 404) {
    return undefined;
  }
  return undefined;
}

function errMsg(err: unknown): string {
  if (err instanceof ApiError)
    return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}
