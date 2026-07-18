import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
import { Checkbox } from "@/components/ui/checkbox";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { StatusBadge } from "@/components/ui/status-badge";
import { IconChevronDown, IconX } from "@tabler/icons-react";

// EditKeyDialog: the one-stop editor for an API key. Wraps
// PATCH /admin/keys/{id} (name / quota / markup),
// POST /admin/keys/{id}/playground_scope (scope), and
// POST/DELETE /admin/groups/{gid}/bindings (group bindings). The
// three endpoints are called sequentially so a partial failure
// doesn't leave the UI thinking everything saved; each failure
// surfaces its own toast.
//
// Layout: single-column form with a fixed label width so every row's
// input starts on the same x. Groups is a wrapped checkbox list so
// binding many groups doesn't need a modal-within-modal.

interface Props {
  keyRow: ApiKey | null;
  onOpenChange: (open: boolean) => void;
}

export function EditKeyDialog({ keyRow, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [quotaCredits, setQuotaCredits] = useState("");
  const [markup, setMarkup] = useState("");
  const [groupIds, setGroupIds] = useState<Set<string>>(new Set());

  const allGroups = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
    enabled: !!keyRow,
  });
  const currentBinding = useQuery({
    queryKey: ["admin", "keys", keyRow?.id, "groups"],
    queryFn: () => admin.keyGroups(keyRow!.id),
    enabled: !!keyRow,
  });

  const originalGroupIds = useMemo(
    () => new Set((currentBinding.data ?? []).map((g) => g.id)),
    [currentBinding.data],
  );

  useEffect(() => {
    if (!keyRow) return;
    setName(keyRow.name);
    setQuotaCredits((keyRow.monthly_quota / 100).toString());
    setMarkup(keyRow.markup_pct.toString());
  }, [keyRow]);

  useEffect(() => {
    if (!currentBinding.data) return;
    setGroupIds(new Set(currentBinding.data.map((g) => g.id)));
  }, [currentBinding.data]);

  const save = useMutation({
    mutationFn: async () => {
      if (!keyRow) throw new Error("no key");
      // 1. Patch the name / quota / markup fields together, if any
      //    of them drifted from the row's snapshot.
      const patchBody: {
        name?: string;
        monthly_quota?: number;
        markup_pct?: number;
      } = {};
      if (name.trim() !== keyRow.name) patchBody.name = name.trim();
      const qNum = Math.round(parseFloat(quotaCredits || "0") * 100);
      if (qNum !== keyRow.monthly_quota) patchBody.monthly_quota = qNum;
      const mNum = parseFloat(markup || "1");
      if (mNum !== keyRow.markup_pct) patchBody.markup_pct = mNum;
      if (Object.keys(patchBody).length > 0) {
        await admin.patchKey(keyRow.id, patchBody);
      }
      // Playground scope is intentionally NOT here — it belongs on the
      // Playground page (that's where the operator decides what a key
      // is allowed to run at test-time), not in the generic key editor.
      // 2. Group bindings — walk the diff and fire per-change.
      for (const gid of groupIds) {
        if (!originalGroupIds.has(gid)) {
          await admin.bindKeyToGroup(gid, keyRow.id);
        }
      }
      for (const gid of originalGroupIds) {
        if (!groupIds.has(gid)) {
          await admin.unbindKeyFromGroup(gid, keyRow.id);
        }
      }
      return keyRow;
    },
    onSuccess: () => {
      toast.success(t("keys.toasts.updated", { name: name.trim() }));
      qc.invalidateQueries({ queryKey: ["admin", "keys"] });
      if (keyRow) {
        qc.invalidateQueries({
          queryKey: ["admin", "keys", keyRow.id, "groups"],
        });
      }
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
    <Dialog open={!!keyRow} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("keys.editTitle")}</DialogTitle>
          <DialogDescription>{t("keys.editDescription")}</DialogDescription>
        </DialogHeader>

        {/* Single column, every row is `label (fixed 96px) : input (flex)`
            so field starts align vertically. */}
        <div className="space-y-3 py-2">
          <Row label={t("keys.form.name")}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Row>

          <Row label={t("keys.form.monthlyQuota")} hint={t("keys.form.monthlyQuotaHint")}>
            <Input
              type="number"
              min={0}
              step="0.01"
              value={quotaCredits}
              onChange={(e) => setQuotaCredits(e.target.value)}
            />
          </Row>

          <Row label={t("keys.form.markupPct")} hint={t("keys.form.markupPctHint")}>
            <Input
              type="number"
              min={0}
              step="0.05"
              value={markup}
              onChange={(e) => setMarkup(e.target.value)}
            />
          </Row>

          <Row label={t("keys.editGroupsLabel")}>
            {allGroups.isLoading || currentBinding.isLoading ? (
              <p className="text-xs text-muted-foreground">
                {t("common.loading")}
              </p>
            ) : (allGroups.data ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">
                {t("keys.editGroupsEmpty")}
              </p>
            ) : (
              <GroupMultiSelect
                all={allGroups.data ?? []}
                selected={groupIds}
                onChange={setGroupIds}
              />
            )}
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

// Row lays out one form line: a fixed-width label column on the left,
// flexible control on the right. `min-h-9` on the label matches the
// default shadcn Input height so single-line rows and multi-line rows
// (e.g. the group checkbox wrap) both keep the label vertically
// centred to the FIRST line of the control.
// GroupMultiSelect is a Popover-backed multi-select: the trigger
// shows the selected groups as removable pill badges (or a
// placeholder), and clicking it opens a Command palette with a
// search box + full group list. Clicks inside the palette toggle
// membership without closing it, so an operator can pick several
// groups in one go.
function GroupMultiSelect({
  all,
  selected,
  onChange,
}: {
  all: { id: string; name: string; description?: string }[];
  selected: Set<string>;
  onChange: (next: Set<string>) => void;
}) {
  const [open, setOpen] = useState(false);
  const chips = all.filter((g) => selected.has(g.id));

  const toggle = (id: string) => {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    onChange(next);
  };

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="flex min-h-9 w-full items-center gap-1 rounded-md border bg-background px-2 py-1 text-left text-sm hover:bg-muted/40"
        >
          <div className="flex flex-1 flex-wrap gap-1">
            {chips.length === 0 ? (
              <span className="text-muted-foreground">Select groups…</span>
            ) : (
              chips.map((g) => (
                <StatusBadge key={g.id} tone="muted" className="gap-0.5">
                  {g.name}
                  <span
                    role="button"
                    tabIndex={0}
                    onClick={(e) => {
                      e.stopPropagation();
                      toggle(g.id);
                    }}
                    className="ml-0.5 rounded-full p-0.5 hover:bg-background"
                  >
                    <IconX className="size-3" />
                  </span>
                </StatusBadge>
              ))
            )}
          </div>
          <IconChevronDown className="size-4 shrink-0 opacity-50" />
        </button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-[--radix-popover-trigger-width] p-0"
      >
        <Command>
          <CommandInput placeholder="Search groups…" />
          <CommandList>
            <CommandEmpty>No groups found.</CommandEmpty>
            <CommandGroup>
              {all.map((g) => {
                const on = selected.has(g.id);
                return (
                  <CommandItem
                    key={g.id}
                    value={g.name}
                    onSelect={() => toggle(g.id)}
                    className="flex items-center gap-2"
                  >
                    <Checkbox checked={on} className="pointer-events-none" />
                    <div className="min-w-0 flex-1">
                      <div className="truncate">{g.name}</div>
                      {g.description ? (
                        <div className="truncate text-[10px] text-muted-foreground">
                          {g.description}
                        </div>
                      ) : null}
                    </div>
                  </CommandItem>
                );
              })}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

// Row is a single form line: fixed-width label column left, control
// column right. Both cells are min-h-9 (matches shadcn Input height)
// with `items-center`, so on rows where the control is a single-line
// input the label sits perfectly centred; on multi-line controls
// (the groups checkbox wrap) the label stays anchored to the top of
// the first row instead of drifting down with the content.
// Row lays out a form line. Label sits on a fixed 36px (h-9) box
// aligned to the TOP of the row, so every label's baseline is at the
// same y regardless of whether the row has a hint underneath the
// control or a taller control (like the group multi-select trigger).
// The row itself grows vertically with `items-start`, keeping the
// label anchored to the first line of the control.
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
      {/* Label lives in a 36px flex box centred vertically — same
          height as a shadcn Input, so single-line rows show label
          and control on the same visual centreline. */}
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
