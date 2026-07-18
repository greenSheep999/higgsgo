import { useState } from "react";
import { useTranslation } from "react-i18next";
import { IconCheck, IconCopy } from "@tabler/icons-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { toast } from "sonner";

// PlaintextRevealDialog is the *only* place a freshly-minted or rotated
// API key is ever surfaced to the operator. There is no state on the
// server after this — closing the dialog is a one-way door. We show a
// copy button front-and-centre and a soft warning so the operator can't
// miss the urgency.

interface Props {
  open: boolean;
  label: string;
  secret: string;
  onOpenChange: (open: boolean) => void;
}

export function PlaintextRevealDialog({
  open,
  label,
  secret,
  onOpenChange,
}: Props) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
      toast.success(t("keys.copied"));
      window.setTimeout(() => setCopied(false), 1500);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("keys.plaintextTitle")}</DialogTitle>
          <DialogDescription>
            {t("keys.plaintextDescription")}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-2 py-2">
          <div className="text-sm font-medium">{label}</div>
          <div className="flex items-stretch gap-2">
            <code className="flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 font-mono text-xs">
              {secret}
            </code>
            <Button variant="outline" onClick={copy}>
              {copied ? <IconCheck /> : <IconCopy />}
              {copied ? t("keys.copied") : t("keys.copy")}
            </Button>
          </div>
        </div>

        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>
            {t("keys.close")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
