import { memo, useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { IconCheck, IconChevronDown, IconX } from "@tabler/icons-react";
import { admin, type PublicModel } from "@/lib/api";
import { StatusBadge } from "@/components/ui/status-badge";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";

// ModelMultiSelect is a shared picker for the "allowed / blocked
// models" fields on Group. Operators think in aliases, not regexes,
// so this component owns the alias ↔ regex translation:
//
//   - EMITTED regex is anchored `^(a|b|c)$`, or "" when nothing is
//     selected.
//   - INBOUND regex is parsed back to chips only if it fits that
//     exact shape. Anything else (a hand-crafted `veo.*`) surfaces as
//     a locked "custom regex" chip so we don't silently rewrite what
//     the operator wrote. They can clear it to switch to picker mode.
//
// Keeping the alias catalog fetched here (rather than at each caller)
// means both allowed / blocked pickers share the same cached query.

interface Props {
  value: string; // the regex string on the wire
  onChange: (nextRegex: string) => void;
  placeholder?: string;
}

export function ModelMultiSelect({ value, onChange, placeholder }: Props) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);

  const catalog = useQuery({
    queryKey: ["v1", "models", "public"],
    queryFn: admin.listPublicModels,
    staleTime: 5 * 60_000, // catalog changes on deploy; cache aggressively
  });
  const all = catalog.data?.data ?? [];

  const parsed = parseAnchoredList(value);
  const isCustom = value !== "" && parsed === null;
  // Membership lookups run once per row on every render / keystroke;
  // a Set keeps them O(1) instead of Array.includes' O(n) across 129
  // rows. Memoised on the serialised regex so the identity is stable
  // between renders that don't actually change the selection — which
  // is what lets the memoised Row below skip re-rendering.
  // `parsed` is recomputed every render (parseAnchoredList runs above
  // unmemoised), so `parsed?.aliases` is a fresh array identity on
  // every keystroke — depending on it directly would defeat the memo.
  // Serialising to a stable key keeps the Set stable across renders
  // that don't actually change the selection, which is what lets the
  // memoised Row below skip re-rendering.
  const aliasKey = parsed?.aliases?.join("|") ?? "";
  const selectedSet = useMemo(
    () => new Set(parsed?.aliases ?? []),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed on serialised aliasKey by design
    [aliasKey],
  );

  // Stable callbacks so <Row> (memoised) only re-renders when its own
  // `on` flag flips, not every time the parent re-renders.
  const toggle = useCallback(
    (alias: string) => {
      const set = new Set(selectedSet);
      if (set.has(alias)) set.delete(alias);
      else set.add(alias);
      onChange(serializeAnchoredList(Array.from(set)));
    },
    [selectedSet, onChange],
  );

  const clearOne = useCallback(
    (alias: string) => {
      const set = new Set(selectedSet);
      set.delete(alias);
      onChange(serializeAnchoredList(Array.from(set)));
    },
    [selectedSet, onChange],
  );

  const clearAll = useCallback(() => onChange(""), [onChange]);

  const selectedAliases = useMemo(
    () => Array.from(selectedSet),
    [selectedSet],
  );

  // Custom regex — the operator wrote something the picker can't
  // reason about. Show it as a locked chip with a "clear to edit"
  // affordance so they can consciously switch to picker mode.
  if (isCustom) {
    return (
      <div className="flex min-h-9 w-full items-center gap-1 rounded-md border bg-muted/40 px-2 py-1 text-sm">
        <code className="min-w-0 flex-1 truncate font-mono text-xs">
          {value}
        </code>
        <StatusBadge tone="warning" className="shrink-0">
          {t("groups.modelPicker.customRegex")}
        </StatusBadge>
        <button
          type="button"
          onClick={clearAll}
          className="rounded p-1 hover:bg-background"
          title={t("groups.modelPicker.clearCustom") ?? ""}
        >
          <IconX className="size-3.5" />
        </button>
      </div>
    );
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="flex min-h-9 w-full items-center gap-1 rounded-md border bg-background px-2 py-1 text-left text-sm hover:bg-muted/40"
        >
          <div className="flex flex-1 flex-wrap gap-1">
            {selectedAliases.length === 0 ? (
              <span className="text-muted-foreground">
                {placeholder ?? t("groups.modelPicker.placeholder")}
              </span>
            ) : (
              selectedAliases.map((alias) => (
                <StatusBadge key={alias} tone="muted" className="gap-0.5">
                  <span className="font-mono text-[10px]">{alias}</span>
                  <span
                    role="button"
                    tabIndex={0}
                    onClick={(e) => {
                      e.stopPropagation();
                      clearOne(alias);
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
          <CommandInput
            placeholder={t("groups.modelPicker.search") ?? "Search…"}
          />
          <CommandList className="max-h-72">
            {catalog.isLoading ? (
              <CommandEmpty>{t("common.loading")}</CommandEmpty>
            ) : (
              <CommandEmpty>
                {t("groups.modelPicker.noResults")}
              </CommandEmpty>
            )}
            <CommandGroup>
              {all.map((m: PublicModel) => (
                <ModelRow
                  key={m.id}
                  model={m}
                  on={selectedSet.has(m.id)}
                  onToggle={toggle}
                />
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

// ModelRow is memoised so toggling one alias only re-renders the two
// rows whose `on` flag actually flipped — not all 129. The old version
// mounted a Radix Checkbox per row (each subscribes to context), which
// made every keystroke / toggle re-render the whole list and dropped
// frames. A plain check-mark span carries the same signal for free.
const ModelRow = memo(function ModelRow({
  model,
  on,
  onToggle,
}: {
  model: PublicModel;
  on: boolean;
  onToggle: (alias: string) => void;
}) {
  return (
    <CommandItem
      value={model.id}
      onSelect={() => onToggle(model.id)}
      className="flex items-center gap-2"
    >
      <span
        className={`flex size-4 shrink-0 items-center justify-center rounded border ${
          on
            ? "border-primary bg-primary text-primary-foreground"
            : "border-input"
        }`}
      >
        {on ? <IconCheck className="size-3" /> : null}
      </span>
      <div className="min-w-0 flex-1">
        <div className="truncate font-mono text-xs">{model.id}</div>
        <div className="truncate text-[10px] text-muted-foreground">
          {model.output}
          {model.unstable ? " · unstable" : ""}
          {model.requires_paid ? " · paid" : ""}
        </div>
      </div>
    </CommandItem>
  );
});

// parseAnchoredList recognises regexes we produced ourselves:
//   ^(alias-a|alias-b|alias-c)$
// A single alias also matches: ^alias-a$. Anything else returns null
// so callers can fall back to "custom regex" mode. Aliases are
// [a-z0-9-]+ on the current catalog, but we accept a slightly wider
// pattern to survive future spec changes without wiping picker mode.
export function parseAnchoredList(
  regex: string | null | undefined,
): { aliases: string[] } | null {
  // Guard against null/undefined so callers don't have to sanitise
  // — the API is supposed to return "" for empty, but a null slip-
  // through would otherwise blow up the whole Groups render.
  if (!regex) return { aliases: [] };
  const m = /^\^\(?([A-Za-z0-9_.-]+(?:\|[A-Za-z0-9_.-]+)*)\)?\$$/.exec(
    regex,
  );
  if (!m) return null;
  const aliases = m[1].split("|").filter(Boolean);
  return { aliases };
}

// serializeAnchoredList produces `^(a|b|c)$` from an alias list.
// Empty list → empty string (means "no filter" on the backend).
export function serializeAnchoredList(aliases: string[]): string {
  if (aliases.length === 0) return "";
  if (aliases.length === 1) return `^${aliases[0]}$`;
  return `^(${aliases.join("|")})$`;
}
