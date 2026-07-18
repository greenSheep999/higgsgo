import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconCheck,
  IconCopy,
  IconLoader2,
  IconRefresh,
} from "@tabler/icons-react";

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
import { Checkbox } from "@/components/ui/checkbox";
import {
  admin,
  ApiError,
  type BearerSettings,
  type RotateBearerResponse,
} from "@/lib/api";
import { setBearer } from "@/lib/auth-store";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // current is passed in so the dialog can render "current: ····abcd"
  // without a second network round-trip. May be null while the header
  // metadata query is still loading — in that case the dialog degrades
  // to "current key unknown" but stays usable.
  current: BearerSettings | null;
}

// BearerDialog is the rotate flow for the /admin/* bearer. Two steps:
//
//   1. Form: pick a mode (manual entry vs random-generate), enter the
//      current bearer, submit. The server verifies the current bearer
//      against both the active value and the still-in-grace previous
//      value (see internal/core/bearer.Manager.Accepts) — that's the
//      guard against a stolen browser session locking the operator
//      out of their own deploy.
//   2. Success: the new plaintext bearer is shown exactly once, along
//      with a "copy" button and a "I've saved it" checkbox. The done
//      button unlocks only after the operator ticks the checkbox so
//      the plaintext cannot be dismissed by accident.
//
// On success we call setBearer() to swap the localStorage token so
// the SPA's next XHR authenticates against the new value. The 30s
// grace window on the server keeps any in-flight XHR that raced
// with the rotate from 401'ing.
export function BearerDialog({ open, onOpenChange, current }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();

  const [mode, setMode] = useState<"manual" | "generate">("generate");
  const [newBearer, setNewBearer] = useState("");
  const [currentBearer, setCurrentBearer] = useState("");
  const [busy, setBusy] = useState(false);
  const [errorMsg, setErrorMsg] = useState<string | null>(null);
  const [result, setResult] = useState<RotateBearerResponse | null>(null);
  const [copied, setCopied] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);

  function reset() {
    setMode("generate");
    setNewBearer("");
    setCurrentBearer("");
    setBusy(false);
    setErrorMsg(null);
    setResult(null);
    setCopied(false);
    setAcknowledged(false);
  }

  function close() {
    reset();
    onOpenChange(false);
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErrorMsg(null);
    if (!currentBearer.trim()) {
      setErrorMsg(t("settings.bearerDialog.errorCurrentRequired"));
      return;
    }
    if (mode === "manual") {
      if (!newBearer.trim()) {
        setErrorMsg(t("settings.bearerDialog.errorNewRequired"));
        return;
      }
      if (newBearer.length < 16) {
        setErrorMsg(t("settings.bearerDialog.errorTooShort"));
        return;
      }
      if (/\s/.test(newBearer)) {
        setErrorMsg(t("settings.bearerDialog.errorWhitespace"));
        return;
      }
    }
    setBusy(true);
    try {
      const body: { current_bearer: string; new_bearer?: string } = {
        current_bearer: currentBearer.trim(),
      };
      if (mode === "manual") {
        body.new_bearer = newBearer;
      }
      const res = await admin.rotateBearer(body);
      // Flip the local bearer immediately so the next XHR (including
      // the metadata refetch below) authenticates with the new value.
      // The server's grace window keeps any request still in flight
      // with the *old* token from 401'ing for another 30s.
      setBearer(res.new_bearer);
      setResult(res);
      // Invalidate every admin query so a future re-render pulls fresh
      // data through the new token. We do NOT await this — it's a
      // background refresh, and the dialog is still open showing the
      // plaintext.
      qc.invalidateQueries({ queryKey: ["admin"] });
    } catch (err) {
      const msg = errorMessage(err, t);
      setErrorMsg(msg);
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    if (!result) return;
    try {
      await navigator.clipboard.writeText(result.new_bearer);
      setCopied(true);
      toast.success(t("settings.bearerDialog.copiedToast"));
      window.setTimeout(() => setCopied(false), 1500);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  const currentLabel = current
    ? t("settings.bearerDialog.currentSummary", {
        last4: current.last_4 || "—",
        source:
          current.source === "db"
            ? t("settings.sourceDb")
            : t("settings.sourceToml"),
      })
    : t("settings.bearerDialog.currentUnknown");

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) close();
        else onOpenChange(o);
      }}
    >
      <DialogContent className="sm:max-w-lg">
        {result ? (
          <SuccessStep
            result={result}
            copied={copied}
            onCopy={copy}
            acknowledged={acknowledged}
            onAcknowledgeChange={setAcknowledged}
            onDone={close}
          />
        ) : (
          <form onSubmit={submit} className="space-y-4">
            <DialogHeader>
              <DialogTitle>{t("settings.bearerDialog.title")}</DialogTitle>
              <DialogDescription>
                {t("settings.bearerDialog.description")}
              </DialogDescription>
            </DialogHeader>

            <div className="rounded-md bg-muted px-3 py-2 text-xs text-muted-foreground">
              {currentLabel}
            </div>

            <fieldset className="space-y-2">
              <legend className="text-sm font-medium">
                {t("settings.bearerDialog.modeLabel")}
              </legend>
              <label className="flex items-start gap-2 text-sm">
                <input
                  type="radio"
                  name="rotate-mode"
                  value="generate"
                  checked={mode === "generate"}
                  onChange={() => setMode("generate")}
                  className="mt-1"
                />
                <span>{t("settings.bearerDialog.modeGenerate")}</span>
              </label>
              <label className="flex items-start gap-2 text-sm">
                <input
                  type="radio"
                  name="rotate-mode"
                  value="manual"
                  checked={mode === "manual"}
                  onChange={() => setMode("manual")}
                  className="mt-1"
                />
                <span>{t("settings.bearerDialog.modeManual")}</span>
              </label>
            </fieldset>

            {mode === "manual" && (
              <div className="space-y-2">
                <Label htmlFor="new-bearer">
                  {t("settings.bearerDialog.newKey")}
                </Label>
                <Input
                  id="new-bearer"
                  type="text"
                  autoComplete="off"
                  spellCheck={false}
                  value={newBearer}
                  onChange={(e) => setNewBearer(e.target.value)}
                  placeholder={t("settings.bearerDialog.newKeyPlaceholder")}
                />
              </div>
            )}

            <div className="space-y-2">
              <Label htmlFor="current-bearer">
                {t("settings.bearerDialog.verifyCurrent")}
              </Label>
              <Input
                id="current-bearer"
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={currentBearer}
                onChange={(e) => setCurrentBearer(e.target.value)}
                placeholder={t("settings.bearerDialog.verifyCurrentPlaceholder")}
              />
            </div>

            {errorMsg && (
              <div className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {errorMsg}
              </div>
            )}

            <DialogFooter>
              <Button type="button" variant="outline" onClick={close}>
                {t("settings.bearerDialog.cancel")}
              </Button>
              <Button type="submit" disabled={busy}>
                {busy && <IconLoader2 className="size-4 animate-spin" />}
                {mode === "generate" && !busy && <IconRefresh className="size-4" />}
                {t("settings.bearerDialog.saveCta")}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}

interface SuccessStepProps {
  result: RotateBearerResponse;
  copied: boolean;
  onCopy: () => void;
  acknowledged: boolean;
  onAcknowledgeChange: (v: boolean) => void;
  onDone: () => void;
}

// SuccessStep is the "here is your new bearer, save it now" screen. The
// Done button is gated behind an explicit acknowledgement checkbox so
// the operator has to consciously confirm they wrote the value down —
// otherwise a stray ESC/click-outside would dismiss the only surface
// that ever shows the plaintext.
function SuccessStep({
  result,
  copied,
  onCopy,
  acknowledged,
  onAcknowledgeChange,
  onDone,
}: SuccessStepProps) {
  const { t } = useTranslation();
  return (
    <div className="space-y-4">
      <DialogHeader>
        <DialogTitle>{t("settings.bearerDialog.successTitle")}</DialogTitle>
        <DialogDescription>
          {t("settings.bearerDialog.successHint")}
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-2">
        <div className="text-sm font-medium">
          {t("settings.bearerDialog.newKey")}
        </div>
        <div className="flex items-stretch gap-2">
          <code className="flex-1 overflow-x-auto break-all rounded-md border bg-muted px-3 py-2 font-mono text-xs">
            {result.new_bearer}
          </code>
          <Button variant="outline" onClick={onCopy}>
            {copied ? <IconCheck /> : <IconCopy />}
            {copied
              ? t("settings.bearerDialog.copiedToast")
              : t("settings.bearerDialog.copy")}
          </Button>
        </div>
      </div>
      <label className="flex items-start gap-2 text-sm">
        <Checkbox
          id="ack"
          checked={acknowledged}
          onCheckedChange={(v) => onAcknowledgeChange(v === true)}
          className="mt-0.5"
        />
        <span>{t("settings.bearerDialog.savedAcknowledge")}</span>
      </label>
      <DialogFooter>
        <Button onClick={onDone} disabled={!acknowledged}>
          {t("settings.bearerDialog.doneCta")}
        </Button>
      </DialogFooter>
    </div>
  );
}

// errorMessage collapses the ApiError envelope into a translated
// message. Known error.type ids get a locale-specific string; anything
// else falls back to the raw message so the operator has some hook to
// grep the server logs by.
function errorMessage(err: unknown, t: (k: string) => string): string {
  if (err instanceof ApiError) {
    switch (err.type) {
      case "invalid_current_bearer":
        return t("settings.bearerDialog.errorInvalidCurrent");
      case "bearer_too_short":
        return t("settings.bearerDialog.errorTooShort");
      case "bearer_whitespace":
        return t("settings.bearerDialog.errorWhitespace");
      case "invalid_new_bearer":
        return t("settings.bearerDialog.errorNewRequired");
    }
    return err.message;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}
