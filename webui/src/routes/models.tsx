import { memo, useMemo, useState } from "react";
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconBox,
  IconCopy,
  IconPencil,
  IconPlayerPlay,
  IconRefresh,
  IconReload,
} from "@tabler/icons-react";

import { admin, ApiError, type Group, type PublicModel } from "@/lib/api";
import { rootRoute } from "@/routes/root";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { StatusBadge, type StatusTone } from "@/components/ui/status-badge";
import { Badge } from "@/components/ui/badge";
import { parseAnchoredList } from "@/components/groups/model-multiselect";
import { providerOf, type Provider } from "@/components/models/provider-map";
import { ModelOverrideDialog } from "@/components/models/override-dialog";
import { CodeExamples } from "@/components/models/code-examples";
import { UptimeBar, generateMockSlots } from "@/components/models/uptime-bar";

// Models management — a read-only catalog view over GET /v1/models,
// cross-referenced against the group model filters so an operator can
// see, per model, which groups may route to it. Purely presentational:
// no probing, no per-model billing, no mutation beyond the registry
// hot-reload button (which just re-parses the catalog file).
//
// The tier column reflects the flags dumped from SEALED.json's
// d_class_by_gate + server/src/mapping/{image,video}_models.mjs into
// verified-models.json. Attribution is surfaced via `tier_source` in
// the detail sheet so an operator can trace where each verdict came
// from.

// Column count for skeleton rows / colSpan on empty-state cells.
// Provider, Model, Output, Cost, Max output, Uptime, Groups, Min plan,
// Tags, Actions plus the `#` index column at the left.
const TOTAL_COLS = 11;

// Output → tone. Video / image / audio each get a distinct muted-ish
// hue so the eye can group the table by media type at a glance.
function outputTone(output: string): StatusTone {
  switch (output) {
    case "video":
      return "info";
    case "image":
      return "brand";
    case "audio":
      return "success";
    default:
      return "muted";
  }
}

// planTone maps a min-plan string to the StatusBadge tone the table
// uses for that tier. Free / Starter stay muted; entry paid tiers get
// info; the premium / unlimited-adjacent tiers escalate to brand /
// success so an operator can eye the "how expensive is this gate?"
// question without reading the label. Unknown values fall through to
// muted so a mis-loaded catalog still renders.
function minPlanTone(plan: string): StatusTone {
  switch (plan) {
    case "":
    case "free":
    case "starter":
      return "muted";
    case "basic":
    case "pro":
      return "info";
    case "plus":
    case "ultimate":
      return "brand";
    case "team":
    case "ultra":
    case "scale":
    case "creator":
    case "enterprise":
      return "success";
    default:
      return "muted";
  }
}

// tagTone maps an operational tag slug to its badge tone. The catalogue
// lives in the registry loader (deriveTags); this table has to stay in
// sync — an unmapped tag falls through to `muted` so a new tag renders
// harmlessly until we assign a colour.
function tagTone(tag: string): StatusTone {
  switch (tag) {
    case "starter_locked":
      return "warning";
    case "unlim_endpoint":
      return "info";
    case "credit_gated":
      return "warning";
    case "unstable":
      return "warning";
    case "deprecated":
      return "danger";
    default:
      return "muted";
  }
}

// uptimeTone maps an uptime percentage to a visual tone.
// >= 99% green, >= 95% yellow, < 95% red.
function uptimeTone(pct: number | null | undefined): StatusTone {
  if (pct == null) return "muted";
  if (pct >= 99) return "success";
  if (pct >= 95) return "warning";
  return "danger";
}

// MOCK: remove when real data flows
// Generates a stable pseudo-random uptime between 95-100% for a given
// model JST. Uses a simple string hash so the same model always shows
// the same value across re-renders.
function mockUptime(jst: string): number {
  let hash = 0;
  for (let i = 0; i < jst.length; i++) {
    hash = (hash * 31 + jst.charCodeAt(i)) | 0;
  }
  // Map hash to 95.0 – 100.0 range
  return 95 + (Math.abs(hash) % 500) / 100;
}

// formatMaxOutput renders the "highest fidelity this model can emit"
// hint into one short table cell. Video models get "<res> · <dur>s",
// audio-only ones get "<dur>s", images get "<res>". Pure pixel-form
// resolutions like "1024x1024" are converted to a Unicode times so
// the cell stays scannable. Returns "—" when both fields are empty.
function formatMaxOutput(m: PublicModel): string {
  const res = m.max_resolution ? m.max_resolution.replace(/x/i, "×") : "";
  const dur = m.max_duration_sec > 0 ? `${m.max_duration_sec}s` : "";
  if (res && dur) return `${res} · ${dur}`;
  if (res) return res;
  if (dur) return dur;
  return "—";
}

// A group "matcher" precompiles a group's allowed / blocked filters
// into a single predicate over an alias. Anchored-list regexes (the
// picker's own output, ^(a|b)$) become O(1) Set lookups; hand-authored
// regexes fall back to RegExp.test so a `veo.*` filter still resolves
// correctly.
interface GroupMatcher {
  group: Group;
  allows: (alias: string) => boolean;
}

function buildMatcher(group: Group): GroupMatcher {
  const allow = compile(group.allowed_models_regex);
  const block = compile(group.blocked_models_regex);
  return {
    group,
    allows: (alias: string) => {
      if (allow && !allow(alias)) return false;
      if (block && block(alias)) return false;
      return true;
    },
  };
}

function compile(regex: string): ((alias: string) => boolean) | null {
  if (!regex) return null;
  const parsed = parseAnchoredList(regex);
  if (parsed) {
    const set = new Set(parsed.aliases);
    return (alias: string) => set.has(alias);
  }
  try {
    const re = new RegExp(regex);
    return (alias: string) => re.test(alias);
  } catch {
    return () => false;
  }
}

// minPlanFilter is the value used by the min-plan Select. "all" is a
// pass-through; the other values are the coarse tier buckets an
// operator actually thinks in when scanning the catalog. The predicate
// below maps the model's `min_plan` string into these buckets.
type MinPlanFilter = "all" | "free" | "starter" | "pro" | "ultra";

// minPlanBucketOf maps a raw plan slug to the coarse filter bucket the
// filter dropdown exposes. Anything Pro-or-above but below Ultra
// collapses to "pro"; Ultra / Ultimate / Team / Scale / Creator /
// Enterprise collapse to "ultra". Starter shares "starter" with free
// because for filter purposes they behave the same.
function minPlanBucketOf(plan: string): MinPlanFilter {
  switch (plan) {
    case "":
    case "free":
      return "free";
    case "starter":
      return "starter";
    case "basic":
    case "pro":
    case "plus":
      return "pro";
    case "ultimate":
    case "team":
    case "ultra":
    case "scale":
    case "creator":
    case "enterprise":
      return "ultra";
    default:
      return "free";
  }
}

function Models() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [search, setSearch] = useState("");
  const [provider, setProvider] = useState<string>("all");
  const [output, setOutput] = useState<string>("all");
  const [minPlan, setMinPlan] = useState<MinPlanFilter>("all");
  // The alias whose detail sheet is currently open. `null` = sheet
  // closed. Keying off alias (not row identity) means catalog reloads
  // / filter changes don't drop the sheet.
  const [detailAlias, setDetailAlias] = useState<string | null>(null);

  // openInPlayground jumps to /playground?model=<alias>. Wrapped in a
  // stable arrow so ModelRow's memoisation still holds. The playground
  // route's validateSearch pins the schema so an unknown alias just
  // lands on the empty playground (no crash, no toast — the intent is
  // clear from the URL).
  const openInPlayground = (alias: string) => {
    navigate({ to: "/playground", search: { model: alias } });
  };

  // Same endpoint + shape as the group model picker, so we share its
  // cache key. Catalog only changes on deploy / reload; use
  // `keepPreviousData` so a background refetch never blanks the table
  // (was causing a full-table flicker on every focus / interaction).
  const catalog = useQuery({
    queryKey: ["v1", "models", "public"],
    queryFn: admin.listPublicModels,
    staleTime: 30_000,
    refetchInterval: 60_000,
    placeholderData: keepPreviousData,
  });

  const groups = useQuery({
    queryKey: ["admin", "groups"],
    queryFn: admin.listGroups,
    staleTime: 30_000,
    placeholderData: keepPreviousData,
  });

  // overrides list — used to decorate the tier column with a small
  // "✎" mark next to models that carry an operator override. Cached
  // separately so the dialog / list share the same fetch.
  const overrides = useQuery({
    queryKey: ["admin", "model-overrides"],
    queryFn: admin.listModelOverrides,
    staleTime: 30_000,
    placeholderData: keepPreviousData,
  });

  const overrideAliases = useMemo(
    () => new Set((overrides.data?.data ?? []).map((o) => o.alias)),
    [overrides.data],
  );

  // Model health — cross-referenced with catalog by JST to show per-
  // model uptime percentage in the table and detail sheet.
  const health = useQuery({
    queryKey: ["admin", "model-health"],
    queryFn: () => admin.listModelHealth(),
    staleTime: 60_000,
    placeholderData: keepPreviousData,
  });

  // Build a jst -> uptime_pct map from health data.
  // MOCK: remove when real data flows — falls back to mock uptime for
  // models that have no health data yet.
  const all = catalog.data?.data ?? [];
  const uptimeMap = useMemo(() => {
    const m = new Map<string, number | null>();
    for (const row of health.data ?? []) {
      if (!m.has(row.jst)) {
        m.set(row.jst, row.uptime_pct ?? null);
      }
    }
    // MOCK: backfill with mock values for any model not in health data
    for (const d of all) {
      if (!m.has(d.jst)) {
        m.set(d.jst, mockUptime(d.jst));
      }
    }
    return m;
  }, [health.data, all]);

  // The Model whose override dialog is currently open. null = dialog
  // closed. Not tied to the detail sheet — the operator opens the
  // dialog on top of the sheet from the "Edit override" button.
  const [editOverride, setEditOverride] = useState<PublicModel | null>(null);

  const reload = useMutation({
    mutationFn: admin.reloadModels,
    onSuccess: (res) => {
      toast.success(
        t("models.toasts.reloaded", {
          previous: res.previous_count,
          current: res.current_count,
        }),
      );
      qc.invalidateQueries({ queryKey: ["v1", "models"] });
    },
    onError: (err) => toast.error(errMsg(err)),
  });

  const matchers = useMemo(
    () => (groups.data ?? []).map(buildMatcher),
    [groups.data],
  );

  // Decorate every model once with its provider + the groups that may
  // route to it. Sorted by (provider name, alias) with alias as the
  // tie-break so pagination / re-render never reshuffles same-provider
  // rows — visible bug in round 1 where alias & provider could
  // momentarily desync while the parent re-rendered.
  const decorated = useMemo(() => {
    const rows = all.map((m) => {
      const prov = providerOf(m.id, m.jst);
      const inGroups = matchers
        .filter((mt) => mt.allows(m.id))
        .map((mt) => mt.group);
      return { model: m, provider: prov, groups: inGroups };
    });
    rows.sort((a, b) => {
      const p = a.provider.name.localeCompare(b.provider.name);
      if (p !== 0) return p;
      return a.model.id.localeCompare(b.model.id);
    });
    return rows;
  }, [all, matchers]);

  const providerOptions = useMemo(() => {
    const seen = new Map<string, string>();
    for (const d of decorated) seen.set(d.provider.key, d.provider.name);
    return Array.from(seen, ([key, name]) => ({ key, name })).sort((a, b) =>
      a.name.localeCompare(b.name),
    );
  }, [decorated]);

  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return decorated.filter((d) => {
      if (provider !== "all" && d.provider.key !== provider) return false;
      if (output !== "all" && d.model.output !== output) return false;
      if (minPlan !== "all" && minPlanBucketOf(d.model.min_plan) !== minPlan)
        return false;
      if (needle) {
        const hay = `${d.model.id} ${d.model.jst}`.toLowerCase();
        if (!hay.includes(needle)) return false;
      }
      return true;
    });
  }, [decorated, search, provider, output, minPlan]);

  // The detail sheet reads from the *decorated* list (not `rows`) so a
  // filter change doesn't close a currently-open sheet. If the alias
  // vanishes (catalog reload dropped it) the sheet quietly closes.
  const detailRow = useMemo(() => {
    if (!detailAlias) return null;
    return decorated.find((d) => d.model.id === detailAlias) ?? null;
  }, [detailAlias, decorated]);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <IconBox className="size-5" /> {t("models.title")}
        </CardTitle>
        <CardDescription>{t("models.description")}</CardDescription>
        <CardAction className="flex flex-wrap items-center gap-2">
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t("models.filters.search")}
            className="h-8 w-48"
          />
          <Select value={provider} onValueChange={setProvider}>
            <SelectTrigger size="sm" className="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                {t("models.filters.allProviders")}
              </SelectItem>
              {providerOptions.map((p) => (
                <SelectItem key={p.key} value={p.key}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select value={output} onValueChange={setOutput}>
            <SelectTrigger size="sm" className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                {t("models.filters.allOutputs")}
              </SelectItem>
              <SelectItem value="image">
                {t("models.output.image")}
              </SelectItem>
              <SelectItem value="video">
                {t("models.output.video")}
              </SelectItem>
              <SelectItem value="audio">
                {t("models.output.audio")}
              </SelectItem>
            </SelectContent>
          </Select>
          <Select
            value={minPlan}
            onValueChange={(v) => setMinPlan(v as MinPlanFilter)}
          >
            <SelectTrigger size="sm" className="w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                {t("models.filters.allMinPlans")}
              </SelectItem>
              <SelectItem value="free">
                {t("models.filters.minPlanFree")}
              </SelectItem>
              <SelectItem value="starter">
                {t("models.filters.minPlanStarter")}
              </SelectItem>
              <SelectItem value="pro">
                {t("models.filters.minPlanPro")}
              </SelectItem>
              <SelectItem value="ultra">
                {t("models.filters.minPlanUltra")}
              </SelectItem>
            </SelectContent>
          </Select>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              catalog.refetch();
              groups.refetch();
            }}
            disabled={catalog.isFetching}
          >
            <IconRefresh /> {t("common.refresh")}
          </Button>
          <Button
            size="sm"
            onClick={() => reload.mutate()}
            disabled={reload.isPending}
            title={t("models.actions.reloadHint")}
          >
            <IconReload />
            {reload.isPending
              ? t("common.loading")
              : t("models.actions.reload")}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <div className="overflow-hidden rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[3%] text-center text-muted-foreground">
                  #
                </TableHead>
                <TableHead className="w-[13%]">
                  {t("models.columns.provider")}
                </TableHead>
                <TableHead className="w-[22%]">
                  {t("models.columns.model")}
                </TableHead>
                <TableHead className="w-[7%] text-center">
                  {t("models.columns.output")}
                </TableHead>
                <TableHead className="w-[8%] text-center">
                  {t("models.columns.cost")}
                </TableHead>
                <TableHead className="w-[9%] text-center">
                  {t("models.columns.maxOutput")}
                </TableHead>
                <TableHead className="w-[7%] text-center">
                  {t("models.columns.uptime")}
                </TableHead>
                <TableHead className="w-[13%] text-center">
                  {t("models.columns.groups")}
                </TableHead>
                <TableHead className="w-[8%] text-center">
                  {t("models.columns.minPlan")}
                </TableHead>
                <TableHead className="w-[11%] text-center">
                  {t("models.columns.tags")}
                </TableHead>
                <TableHead className="w-[6%] text-right">
                  {t("models.columns.actions")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {catalog.isLoading ? (
                <SkeletonRows />
              ) : rows.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={TOTAL_COLS}
                    className="py-8 text-center text-sm text-muted-foreground"
                  >
                    {all.length === 0
                      ? t("models.empty")
                      : t("models.noMatch")}
                  </TableCell>
                </TableRow>
              ) : (
                rows.map((d, i) => (
                  <ModelRow
                    key={d.model.id}
                    index={i + 1}
                    model={d.model}
                    provider={d.provider}
                    groups={d.groups}
                    hasOverride={overrideAliases.has(d.model.id)}
                    uptimePct={uptimeMap.get(d.model.jst) ?? null}
                    onOpen={setDetailAlias}
                    onEdit={setEditOverride}
                    onTest={openInPlayground}
                  />
                ))
              )}
            </TableBody>
          </Table>
        </div>
      </CardContent>

      <Sheet
        open={detailRow !== null}
        onOpenChange={(open) => !open && setDetailAlias(null)}
      >
        <SheetContent side="right" className="w-full sm:max-w-lg">
          {detailRow ? (
            <ModelDetail
              model={detailRow.model}
              provider={detailRow.provider}
              groups={detailRow.groups}
              hasOverride={overrideAliases.has(detailRow.model.id)}
              uptimePct={uptimeMap.get(detailRow.model.jst) ?? null}
              onEditOverride={() => setEditOverride(detailRow.model)}
              onTestInPlayground={() => {
                const alias = detailRow.model.id;
                setDetailAlias(null);
                openInPlayground(alias);
              }}
            />
          ) : null}
        </SheetContent>
      </Sheet>

      <ModelOverrideDialog
        model={editOverride}
        onOpenChange={(open) => {
          if (!open) setEditOverride(null);
        }}
      />
    </Card>
  );
}

// ModelRow is memoised: the catalog is ~130 rows and typing in the
// search box re-renders the parent on every keystroke, so rows whose
// props are unchanged bail out of re-render. The `onOpen` callback is
// a stable setter reference (`setDetailAlias`), so the default shallow
// compare is enough.
const ModelRow = memo(function ModelRow({
  index,
  model,
  provider,
  groups,
  hasOverride,
  uptimePct,
  onOpen,
  onEdit,
  onTest,
}: {
  index: number;
  model: PublicModel;
  provider: Provider;
  groups: Group[];
  hasOverride: boolean;
  uptimePct: number | null;
  onOpen: (alias: string) => void;
  onEdit: (model: PublicModel) => void;
  onTest: (alias: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <TableRow
      className="cursor-pointer hover:bg-muted/40"
      onClick={() => onOpen(model.id)}
    >
      <TableCell className="text-center text-xs tabular-nums text-muted-foreground">
        #{index}
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          <ProviderLogo provider={provider} />
          <span className="truncate text-sm">{provider.name}</span>
        </div>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-1">
          <span className="truncate font-medium">{model.id}</span>
          <button
            type="button"
            className="inline-flex size-5 shrink-0 items-center justify-center rounded text-muted-foreground opacity-60 hover:bg-muted hover:opacity-100"
            onClick={(e) => {
              // Row click opens the detail sheet; stop the event so the
              // copy button doesn't trigger both.
              e.stopPropagation();
              void navigator.clipboard.writeText(model.id).then(
                () => toast(t("models.copiedId", { id: model.id })),
                () => toast.error(t("models.copyFailed")),
              );
            }}
            title={t("models.copyId")}
            aria-label={t("models.copyId")}
          >
            <IconCopy className="size-3" />
          </button>
        </div>
        <div className="truncate font-mono text-[10px] text-muted-foreground">
          {model.jst}
        </div>
      </TableCell>
      <TableCell className="text-center">
        <StatusBadge tone={outputTone(model.output)}>
          {t(`models.output.${model.output}`, { defaultValue: model.output })}
        </StatusBadge>
      </TableCell>
      <TableCell className="text-center tabular-nums">
        {model.est_cost > 0 ? model.est_cost.toLocaleString() : "—"}
      </TableCell>
      <TableCell className="text-center text-xs tabular-nums">
        {formatMaxOutput(model)}
      </TableCell>
      <TableCell className="text-center">
        <UptimeCell pct={uptimePct} jst={model.jst} />
      </TableCell>
      <TableCell className="text-center">
        <GroupsCell groups={groups} />
      </TableCell>
      <TableCell className="text-center">
        <MinPlanCell model={model} />
      </TableCell>
      <TableCell className="text-center">
        <TagsCell tags={model.tags} hasOverride={hasOverride} />
      </TableCell>
      {/* Actions cluster: "Test in playground" jumps to the playground
          with the alias pre-selected; "Edit override" opens the
          override dialog. Both stopPropagation so the row click
          (which opens the detail sheet) doesn't also fire. */}
      <TableCell className="text-right">
        <div className="inline-flex items-center gap-1">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onTest(model.id);
            }}
            className="inline-flex size-7 items-center justify-center rounded-md border border-transparent text-muted-foreground hover:bg-muted hover:text-foreground"
            title={t("models.testInPlayground")}
            aria-label={t("models.testInPlayground")}
          >
            <IconPlayerPlay className="size-3.5" />
          </button>
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onEdit(model);
            }}
            className={
              "inline-flex size-7 items-center justify-center rounded-md border text-muted-foreground hover:bg-muted hover:text-foreground " +
              (hasOverride
                ? "border-amber-500/40 text-amber-600"
                : "border-transparent")
            }
            title={
              hasOverride
                ? t("models.detail.editOverride")
                : t("models.detail.addOverride")
            }
            aria-label={
              hasOverride
                ? t("models.detail.editOverride")
                : t("models.detail.addOverride")
            }
          >
            <IconPencil className="size-3.5" />
          </button>
        </div>
      </TableCell>
    </TableRow>
  );
});

// ProviderLogo renders the @lobehub/icons brand mark for the given
// provider. Fixed-square container so an SVG of any intrinsic aspect
// ratio lands in the same footprint as the previous letter fallback.
// Height/width via CSS classes so the SVG can respect currentColor for
// the monochrome variants we ship.
function ProviderLogo({ provider }: { provider: Provider }) {
  const Logo = provider.Logo;
  return (
    <span className="flex size-6 shrink-0 items-center justify-center rounded-md border bg-background text-foreground">
      <Logo size={16} />
    </span>
  );
}

const MAX_INLINE_GROUPS = 2;

function GroupsCell({ groups }: { groups: Group[] }) {
  const { t } = useTranslation();
  if (groups.length === 0) {
    return (
      <span className="text-[10px] text-muted-foreground">
        {t("models.groupsNone")}
      </span>
    );
  }
  const shown = groups.slice(0, MAX_INLINE_GROUPS);
  const overflow = groups.slice(MAX_INLINE_GROUPS);
  return (
    <div className="flex flex-wrap justify-center gap-1">
      {shown.map((g) => (
        <StatusBadge key={g.id} tone="muted">
          {g.name}
        </StatusBadge>
      ))}
      {overflow.length > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex cursor-help items-center rounded-md border bg-muted/60 px-1.5 py-0.5 text-xs font-medium text-muted-foreground">
              +{overflow.length}
            </span>
          </TooltipTrigger>
          <TooltipContent>
            <div className="max-w-56 space-y-0.5">
              {overflow.map((g) => (
                <div key={g.id} className="text-xs">
                  {g.name}
                </div>
              ))}
            </div>
          </TooltipContent>
        </Tooltip>
      ) : null}
    </div>
  );
}

// MinPlanCell renders the single "minimum plan tier" badge. The
// backend derives this from the same requires_* / starter_locked
// flags accountCanRun consults, so a cell reading "pro" tells the
// operator "any paid plan from Pro up will work" without them having
// to eyeball three separate booleans.
function MinPlanCell({ model }: { model: PublicModel }) {
  const { t } = useTranslation();
  const key = model.min_plan || "free";
  const label = t(`models.minPlan.${key}`, { defaultValue: key });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-block cursor-help">
          <StatusBadge tone={minPlanTone(key)}>{label}</StatusBadge>
        </span>
      </TooltipTrigger>
      <TooltipContent>
        {model.min_plan
          ? t("models.minPlan.hint", { plan: label })
          : t("models.minPlan.freeHint")}
      </TooltipContent>
    </Tooltip>
  );
}

// TagsCell renders the operational tags returned by the loader as a
// stack of tone-coded badges. Empty tags collapse to an em-dash so
// the column doesn't leave a hole. The hasOverride marker still lives
// here so operators can spot patched rows without opening the sheet.
function TagsCell({
  tags,
  hasOverride,
}: {
  tags: string[];
  hasOverride: boolean;
}) {
  const { t } = useTranslation();
  if (!tags.length && !hasOverride) {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  return (
    <div className="flex flex-wrap justify-center gap-1">
      {tags.map((tag) => (
        <StatusBadge key={tag} tone={tagTone(tag)}>
          {t(`models.tags.${tag}`, { defaultValue: tag })}
        </StatusBadge>
      ))}
      {hasOverride ? <OverrideMarker key="override" /> : null}
    </div>
  );
}

// OverrideMarker is the small ✎ badge rendered on the tier column when
// a model carries an operator override. Wrapped in a tooltip so a
// hover explains the marker without leaving the table.
function OverrideMarker() {
  const { t } = useTranslation();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className="inline-flex size-5 cursor-help items-center justify-center rounded-md border border-amber-500/40 bg-amber-500/10 text-[10px] font-semibold text-amber-700 dark:text-amber-400"
          aria-label={t("models.override.marker")}
        >
          <IconPencil className="size-3" />
        </span>
      </TooltipTrigger>
      <TooltipContent>{t("models.override.marker")}</TooltipContent>
    </Tooltip>
  );
}

// UptimeCell renders a mini uptime status bar in the table.
// MOCK: remove when backend returns time-series health data
function UptimeCell({ pct, jst }: { pct: number | null; jst: string }) {
  const slots = useMemo(() => generateMockSlots(jst, 12), [jst]);
  return (
    <div className="flex items-center gap-1.5">
      <UptimeBar slots={slots} size="mini" />
      {pct != null ? (
        <span className="text-[10px] tabular-nums text-muted-foreground">
          {pct.toFixed(0)}%
        </span>
      ) : null}
    </div>
  );
}

// UptimeBarDetail renders the larger uptime bar in the detail sheet.
// MOCK: remove when backend returns time-series health data
function UptimeBarDetail({ jst, uptimePct }: { jst: string; uptimePct: number }) {
  const slots = useMemo(() => generateMockSlots(jst, 48), [jst]);
  return (
    <div className="w-full space-y-2">
      <UptimeBar slots={slots} size="md" />
      <div className="flex items-center gap-2">
        <StatusBadge tone={uptimeTone(uptimePct)}>
          {uptimePct.toFixed(1)}%
        </StatusBadge>
        <span className="text-xs text-muted-foreground">
          uptime in the last 48h
        </span>
      </div>
    </div>
  );
}

// ModelDetail is the side sheet rendered on row click. Shows the
// full ModelSpec surface (endpoint, version, media_role, app_slug,
// example body, override extension) plus a footer button that opens
// the override editor. Every optional field is hidden when empty so
// the sheet stays scannable across text-only and image/video models.
function ModelDetail({
  model,
  provider,
  groups,
  hasOverride,
  uptimePct,
  onEditOverride,
  onTestInPlayground,
}: {
  model: PublicModel;
  provider: Provider;
  groups: Group[];
  hasOverride: boolean;
  uptimePct: number | null;
  onEditOverride: () => void;
  onTestInPlayground: () => void;
}) {
  const { t } = useTranslation();
  // MOCK: remove when real data flows
  const displayUptime = uptimePct ?? mockUptime(model.jst);
  return (
    <>
      <SheetHeader>
        <SheetTitle className="flex items-center gap-2">
          <ProviderLogo provider={provider} />
          <span className="truncate">{model.id}</span>
        </SheetTitle>
        <SheetDescription className="font-mono text-xs">
          {model.jst}
        </SheetDescription>
      </SheetHeader>

      <div className="flex flex-col gap-6 overflow-y-auto p-4 text-sm">
        {/* Section 1: Basic Info */}
        <div>
          <h4 className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">
            {t("models.detail.sectionBasic", { defaultValue: "Basic Info" })}
          </h4>
          <div className="grid grid-cols-2 gap-y-2">
            <span className="text-xs text-muted-foreground">{t("models.detail.provider")}</span>
            <span className="text-right text-sm">{provider.name}</span>

            <span className="text-xs text-muted-foreground">{t("models.detail.model", { defaultValue: "Model" })}</span>
            <span className="text-right font-medium text-sm">{model.id}</span>

            <span className="text-xs text-muted-foreground">{t("models.detail.output")}</span>
            <span className="text-right">
              <StatusBadge tone={outputTone(model.output)}>
                {t(`models.output.${model.output}`, { defaultValue: model.output })}
              </StatusBadge>
            </span>

            {model.version ? (
              <>
                <span className="text-xs text-muted-foreground">{t("models.detail.version")}</span>
                <span className="text-right font-mono text-xs uppercase">{model.version}</span>
              </>
            ) : null}

            {model.endpoint ? (
              <>
                <span className="text-xs text-muted-foreground">{t("models.detail.endpoint")}</span>
                <span className="text-right">
                  <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-[11px]">
                    {model.endpoint}
                  </code>
                </span>
              </>
            ) : null}
          </div>
        </div>

        <Separator />

        {/* Section 2: Pricing & Limits */}
        <div>
          <h4 className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">
            {t("models.detail.sectionPricing", { defaultValue: "Pricing & Limits" })}
          </h4>
          <div className="grid grid-cols-2 gap-y-2">
            <span className="text-xs text-muted-foreground">{t("models.detail.estCost")}</span>
            <span className="text-right tabular-nums text-sm">
              {model.est_cost > 0 ? `${model.est_cost.toLocaleString()} cr` : "—"}
            </span>

            <span className="text-xs text-muted-foreground">{t("models.detail.minPlan")}</span>
            <span className="text-right">
              <StatusBadge tone={minPlanTone(model.min_plan || "free")}>
                {t(`models.minPlan.${model.min_plan || "free"}`, {
                  defaultValue: model.min_plan || "free",
                })}
              </StatusBadge>
            </span>

            <span className="text-xs text-muted-foreground">{t("models.detail.maxOutput")}</span>
            <span className="text-right tabular-nums text-sm">{formatMaxOutput(model)}</span>

            <span className="text-xs text-muted-foreground">{t("models.detail.requiredParams")}</span>
            <span className="text-right">
              {model.required_params && model.required_params.length > 0 ? (
                <span className="flex flex-wrap justify-end gap-1">
                  {model.required_params.map((p) => (
                    <code
                      key={p}
                      className="rounded bg-muted px-1.5 py-0.5 font-mono text-[11px]"
                    >
                      {p}
                    </code>
                  ))}
                </span>
              ) : (
                <span className="text-xs text-muted-foreground">—</span>
              )}
            </span>
          </div>
          {model.tier_source ? (
            <div className="mt-2 text-[11px] text-muted-foreground">
              {t("models.tier.sourceLabel")}:{" "}
              <span className="font-mono">{model.tier_source}</span>
            </div>
          ) : null}
          {model.min_credits_hundredths > 0 ? (
            <div className="mt-1 text-[11px] text-muted-foreground">
              {t("models.tier.minCredits", {
                credits: (model.min_credits_hundredths / 100).toLocaleString(),
              })}
            </div>
          ) : null}
        </div>

        <Separator />

        {/* Section 3: Access Control */}
        <div>
          <h4 className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">
            {t("models.detail.sectionAccess", { defaultValue: "Access Control" })}
          </h4>
          <div className="space-y-3">
            <div>
              <div className="mb-1 text-xs text-muted-foreground">
                {t("models.detail.callableBy")}
              </div>
              {groups.length === 0 ? (
                <span className="text-xs text-muted-foreground">
                  {t("models.groupsNone")}
                </span>
              ) : (
                <div className="flex flex-wrap gap-1">
                  {groups.map((g) => (
                    <StatusBadge key={g.id} tone="muted">
                      {g.name}
                    </StatusBadge>
                  ))}
                </div>
              )}
            </div>
            <div>
              <div className="mb-1 text-xs text-muted-foreground">
                {t("models.detail.tags")}
              </div>
              {model.tags && model.tags.length > 0 ? (
                <div className="flex flex-wrap gap-1">
                  {model.tags.map((tag) => (
                    <StatusBadge key={tag} tone={tagTone(tag)}>
                      {t(`models.tags.${tag}`, { defaultValue: tag })}
                    </StatusBadge>
                  ))}
                </div>
              ) : (
                <span className="text-xs text-muted-foreground">
                  {t("models.detail.tagsNone")}
                </span>
              )}
            </div>
            {model.extra_aliases && model.extra_aliases.length > 0 ? (
              <div>
                <div className="mb-1 text-xs text-muted-foreground">
                  {t("models.detail.extraAliases")}
                </div>
                <div className="flex flex-wrap gap-1">
                  {model.extra_aliases.map((a) => (
                    <Badge
                      key={a}
                      variant="secondary"
                      className="font-mono text-[11px]"
                    >
                      {a}
                    </Badge>
                  ))}
                </div>
                <div className="mt-1 text-[10px] text-muted-foreground">
                  {t("models.detail.extraAliasesHint")}
                </div>
              </div>
            ) : null}
          </div>
        </div>

        <Separator />

        {/* Section 4: Health */}
        <div>
          <h4 className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">
            {t("models.detail.sectionHealth", { defaultValue: "Health" })}
          </h4>
          {/* MOCK: remove when backend returns time-series health data */}
          <UptimeBarDetail jst={model.jst} uptimePct={displayUptime} />
        </div>

        <Separator />

        {/* Section 5: Code Examples */}
        <div>
          <h4 className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">
            {t("models.detail.sectionCodeExamples", { defaultValue: "Code Examples" })}
          </h4>
          <CodeExamples model={model} />
        </div>

        {/* Section 6: Example Body JSON (collapsible) */}
        {model.example_body_json ? (
          <>
            <Separator />
            <details className="rounded-md border">
              <summary className="cursor-pointer bg-muted/40 px-3 py-1.5 text-xs font-semibold">
                {t("models.detail.exampleBody")}
              </summary>
              <pre className="max-h-64 overflow-auto p-2 font-mono text-[10px] leading-snug">
                {formatExampleBody(model.example_body_json)}
              </pre>
            </details>
          </>
        ) : null}

        {/* Section 7: Operator Note */}
        {model.note ? (
          <>
            <Separator />
            <div>
              <h4 className="mb-2 text-xs font-bold uppercase tracking-wide text-muted-foreground">
                {t("models.detail.note")}
              </h4>
              <p className="whitespace-pre-wrap rounded-md border bg-muted/40 p-2 text-xs">
                {model.note}
              </p>
            </div>
          </>
        ) : null}
      </div>

      <SheetFooter className="flex-col items-stretch gap-2 border-t bg-muted/30">
        <div className="flex flex-wrap gap-2">
          <Button
            size="sm"
            variant="outline"
            onClick={onTestInPlayground}
            className="flex-1"
          >
            <IconPlayerPlay className="size-3.5" />
            {t("models.testInPlayground")}
          </Button>
          <Button
            size="sm"
            variant={hasOverride ? "default" : "outline"}
            onClick={onEditOverride}
            className="flex-1"
          >
            <IconPencil className="size-3.5" />
            {hasOverride
              ? t("models.detail.editOverride")
              : t("models.detail.addOverride")}
          </Button>
        </div>
        <p className="text-[11px] text-muted-foreground">
          {t("models.detail.readonlyHint")}
        </p>
      </SheetFooter>
    </>
  );
}

// formatExampleBody pretty-prints a JSON string. If the payload isn't
// valid JSON we render it verbatim so a legacy string / mis-recorded
// body still lands on the sheet instead of throwing.
function formatExampleBody(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function SkeletonRows() {
  return (
    <>
      {Array.from({ length: 6 }).map((_, i) => (
        <TableRow key={i}>
          <TableCell colSpan={TOTAL_COLS}>
            <Skeleton className="h-6 w-full" />
          </TableCell>
        </TableRow>
      ))}
    </>
  );
}

function errMsg(err: unknown): string {
  if (err instanceof ApiError)
    return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}

export const modelsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/models",
  staticData: { titleKey: "nav.models" },
  component: Models,
});

