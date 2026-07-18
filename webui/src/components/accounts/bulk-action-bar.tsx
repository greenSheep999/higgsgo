import { useTranslation } from "react-i18next";
import {
  IconChecklist,
  IconPlayerPause,
  IconPlayerPlay,
  IconTrash,
  IconX,
} from "@tabler/icons-react";
import { Button } from "@/components/ui/button";

// BulkActionBar sits above the account grid whenever the user has at
// least one card checked. It stays sticky-ish inside the Accounts card
// (not window-sticky) so the operator can scroll the grid without
// losing the toolbar. Actions fan out to the parent as callbacks — the
// bar itself owns no state beyond the visual busy flag.

interface Props {
  count: number;
  total: number;
  busy: boolean;
  onSelectAll: () => void;
  onClear: () => void;
  onPause: () => void;
  onResume: () => void;
  onBan: () => void;
}

export function BulkActionBar({
  count,
  total,
  busy,
  onSelectAll,
  onClear,
  onPause,
  onResume,
  onBan,
}: Props) {
  const { t } = useTranslation();
  if (count === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 p-2">
      <div className="flex items-center gap-2 text-sm font-medium">
        <IconChecklist className="size-4" />
        {t("accounts.bulk.selectedCount", { count })}
      </div>
      <Button
        variant="ghost"
        size="sm"
        onClick={onSelectAll}
        disabled={count === total}
      >
        {t("accounts.bulk.selectAll")}
      </Button>
      <Button variant="ghost" size="sm" onClick={onClear}>
        <IconX /> {t("accounts.bulk.clear")}
      </Button>
      <div className="mx-1 h-4 w-px bg-border" />
      <Button variant="outline" size="sm" disabled={busy} onClick={onPause}>
        <IconPlayerPause /> {t("accounts.bulk.pause")}
      </Button>
      <Button variant="outline" size="sm" disabled={busy} onClick={onResume}>
        <IconPlayerPlay /> {t("accounts.bulk.resume")}
      </Button>
      <Button
        variant="outline"
        size="sm"
        disabled={busy}
        onClick={onBan}
        className="text-destructive"
      >
        <IconTrash /> {t("accounts.bulk.ban")}
      </Button>
    </div>
  );
}
