import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { admin, ApiError, type Account, type PatchAccountRequest } from "@/lib/api";
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

// EditAccountDialog: editor for the mutable admin-managed columns of
// an account row. Wraps PATCH /admin/accounts/{id} for scalar fields
// and group membership changes via the group bindings endpoints.
//
// Props follow the same pattern as EditKeyDialog: a nullable `account`
// prop controls open state (null = closed, non-null = open with that
// row's values pre-filled).

interface Props {
  account: Account | null;
  onOpenChange: (open: boolean) => void;
}

export function EditAccountDialog({ account, onOpenChange }: Props) {
  const { t } = useTranslation();
  const qc = useQueryClient();

  const [proxyUrl, setProxyUrl] = useState("");
  const [priority, setPriority] = useState("0");
  const [maxConcurrent, setMaxConcurrent] = useState("0");
  const [source, setSource] = useState("");
  const [note, setNote] = useState("");
  // groupIds is the current selection; groupPriority is the priority the
  // user is editing per group. Two maps let the group multi-select stay
  // dumb (just toggles ids) while the priority column beside each chip
  // reads/writes the second map. Default 100 matches the DB default.
  const [groupIds, setGroupIds] = useState<Set<string>>(new Set());
  const [groupPriority, setGroupPriority] = useState<Map<string, number>>(
    new Map(),
  );

  const allGroups = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
    enabled: !!account,
  });

  // Fetch current group memberships plus per-group priority for this
  // account. We fan out one listGroupMembers call per group (already
  // needed to know which groups the account belongs to) and pull the
  // priority out of members_detail in the same response.
  const currentGroups = useQuery({
    queryKey: ["admin", "account-groups", account?.id],
    queryFn: async () => {
      if (!account) return { ids: [] as string[], priority: new Map<string, number>() };
      const groups = allGroups.data ?? [];
      const memberOf: string[] = [];
      const prio = new Map<string, number>();
      for (const g of groups) {
        const res = await admin.listGroupMembers(g.id);
        const hit = res.members_detail.find((m) => m.account_id === account.id);
        if (hit) {
          memberOf.push(g.id);
          prio.set(g.id, hit.priority);
        }
      }
      return { ids: memberOf, priority: prio };
    },
    enabled: !!account && !!allGroups.data,
  });

  const originalGroupIds = useMemo(
    () => new Set(currentGroups.data?.ids ?? []),
    [currentGroups.data],
  );
  const originalGroupPriority = useMemo(
    () => currentGroups.data?.priority ?? new Map<string, number>(),
    [currentGroups.data],
  );

  // Seed form state from the account prop when it changes.
  useEffect(() => {
    if (!account) return;
    setProxyUrl(account.bound_proxy_url ?? "");
    setPriority(String(account.priority ?? 0));
    setMaxConcurrent(String(account.max_concurrent ?? 0));
    setSource(account.source ?? "");
    setNote(account.note ?? "");
  }, [account]);

  useEffect(() => {
    if (!currentGroups.data) return;
    setGroupIds(new Set(currentGroups.data.ids));
    setGroupPriority(new Map(currentGroups.data.priority));
  }, [currentGroups.data]);

  const save = useMutation({
    mutationFn: async () => {
      if (!account) throw new Error("no account");

      // Build patch body — only include fields that changed.
      const patch: PatchAccountRequest = {};
      const newPriority = parseInt(priority, 10) || 0;
      if (newPriority !== (account.priority ?? 0)) {
        patch.priority = newPriority;
      }
      if (proxyUrl !== (account.bound_proxy_url ?? "")) {
        patch.bound_proxy_url = proxyUrl;
      }
      const newMaxConcurrent = parseInt(maxConcurrent, 10) || 0;
      if (newMaxConcurrent !== (account.max_concurrent ?? 0)) {
        patch.max_concurrent = newMaxConcurrent;
      }
      if (note !== (account.note ?? "")) {
        patch.note = note;
      }
      if (source !== (account.source ?? "")) {
        patch.source = source;
      }

      if (Object.keys(patch).length > 0) {
        await admin.patchAccount(account.id, patch);
      }

      // Group membership diff — add/remove/repriority as needed.
      // addGroupMember on the backend is an upsert (ON CONFLICT DO
      // UPDATE), so we can use it for both new bindings and priority
      // updates on already-bound groups.
      for (const gid of groupIds) {
        const desired = groupPriority.get(gid);
        if (!originalGroupIds.has(gid)) {
          await admin.addGroupMember(gid, account.id, desired);
        } else if (desired !== undefined && desired !== originalGroupPriority.get(gid)) {
          await admin.addGroupMember(gid, account.id, desired);
        }
      }
      for (const gid of originalGroupIds) {
        if (!groupIds.has(gid)) {
          await admin.removeGroupMember(gid, account.id);
        }
      }

      return account;
    },
    onSuccess: () => {
      toast.success(t("accounts.edit.saved"));
      qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
      qc.invalidateQueries({ queryKey: ["admin", "account-groups", account?.id] });
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
    <Dialog open={!!account} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("accounts.edit.title")}</DialogTitle>
          <DialogDescription>{t("accounts.edit.description")}</DialogDescription>
        </DialogHeader>

        <div className="space-y-3 py-2">
          <Row label={t("accounts.edit.proxyUrl")}>
            <Input
              value={proxyUrl}
              onChange={(e) => setProxyUrl(e.target.value)}
              placeholder="socks5://user:pass@host:port"
            />
          </Row>

          <Row label={t("accounts.edit.priority")} hint={t("accounts.edit.priorityHint")}>
            <Input
              type="number"
              min={-1000}
              max={1000}
              value={priority}
              onChange={(e) => setPriority(e.target.value)}
            />
          </Row>

          <Row label={t("accounts.edit.maxConcurrent")} hint={t("accounts.edit.maxConcurrentHint")}>
            <Input
              type="number"
              min={0}
              max={20}
              placeholder="0 = default (6)"
              value={maxConcurrent}
              onChange={(e) => setMaxConcurrent(e.target.value)}
            />
          </Row>

          <Row label={t("accounts.edit.groups")}>
            {allGroups.isLoading || currentGroups.isLoading ? (
              <p className="text-xs text-muted-foreground">
                {t("common.loading")}
              </p>
            ) : (allGroups.data ?? []).length === 0 ? (
              <p className="text-xs text-muted-foreground">
                {t("accounts.edit.noGroups")}
              </p>
            ) : (
              <div className="space-y-2">
                <GroupMultiSelect
                  all={allGroups.data ?? []}
                  selected={groupIds}
                  onChange={setGroupIds}
                />
                {/* Per-group priority editor — only shown for currently
                    selected groups. Only visible when the group's
                    route_strategy is 'priority' does it actually affect
                    routing, but the field is always editable so an admin
                    can pre-set values before the strategy switch. */}
                {groupIds.size > 0 ? (
                  <div className="rounded-md border bg-muted/20 p-2 space-y-1.5">
                    <div className="text-[10px] uppercase tracking-wide text-muted-foreground">
                      {t("accounts.edit.groupPriorityLabel")}
                    </div>
                    <p className="text-[10px] text-muted-foreground">
                      {t("accounts.edit.groupPriorityHint")}
                    </p>
                    {Array.from(groupIds).map((gid) => {
                      const g = (allGroups.data ?? []).find(
                        (x) => x.id === gid,
                      );
                      if (!g) return null;
                      const val = groupPriority.get(gid) ?? 100;
                      return (
                        <div
                          key={gid}
                          className="flex items-center gap-2 text-xs"
                        >
                          <span className="min-w-0 flex-1 truncate">
                            {g.name}
                          </span>
                          <Input
                            type="number"
                            min={-1000}
                            max={1000}
                            value={String(val)}
                            className="h-7 w-20 text-xs tabular-nums"
                            onChange={(e) => {
                              const next = new Map(groupPriority);
                              next.set(gid, parseInt(e.target.value, 10) || 0);
                              setGroupPriority(next);
                            }}
                          />
                        </div>
                      );
                    })}
                  </div>
                ) : null}
              </div>
            )}
          </Row>

          <Row label={t("accounts.edit.source")}>
            <Input
              value={source}
              onChange={(e) => setSource(e.target.value)}
              placeholder="manual / imported / registered"
            />
          </Row>

          <Row label={t("accounts.edit.note")}>
            <Textarea
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder={t("accounts.edit.notePlaceholder")}
              rows={3}
            />
          </Row>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => save.mutate()}
            disabled={save.isPending}
          >
            {save.isPending ? t("common.loading") : t("common.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// GroupMultiSelect: Popover-backed multi-select for group assignment.
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

// Row: form line layout (fixed-width label + flexible control).
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
