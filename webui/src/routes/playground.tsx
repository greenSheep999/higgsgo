import { useEffect, useMemo, useRef, useState } from "react";
import { createRoute, useSearch } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconAlertTriangle,
  IconMessageChatbot,
  IconPlayerPlay,
  IconRuler,
} from "@tabler/icons-react";

import { admin, ApiError, type ApiKey, type PlaygroundModel } from "@/lib/api";
import { rootRoute } from "@/routes/root";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import { StatusBadge } from "@/components/ui/status-badge";
import {
  ParamForm,
  type ParamValue,
} from "@/components/playground/param-form";

// Playground — driven by the model's required_params. Two authoring
// modes:
//   1. Form (default): every required param becomes an input widget,
//      classified by ParamForm's name heuristic. This is what the
//      operator uses 95% of the time.
//   2. Raw JSON (advanced tab): a textarea for the whole request body,
//      pre-filled with the form's current values whenever the operator
//      opens the tab. Free-form override for niche params the heuristic
//      doesn't understand yet.
//
// Result panel below the form auto-detects image / video URLs from the
// execute response and renders them inline; falls back to a raw JSON
// dump for anything else (async job pending, unknown shape, errors).

// PLAYGROUND_CHEAP_CAP mirrors domain.PlaygroundCheapCapHundredths (500,
// i.e. 5 credits) so the WebUI can grey out models a "cheap"-scope key
// cannot invoke before the backend re-checks. Kept in credit units here
// because PlaygroundModel.est_cost is already credits (hundredths / 100).
const PLAYGROUND_CHEAP_CAP = 5;

// modelAllowedForScope reports whether a key with the given playground_scope
// may invoke the model. Purely a UI hint — the backend re-enforces the same
// gate per the selected key's real scope on estimate/execute.
function modelAllowedForScope(
  scope: string | undefined,
  estCost: number,
): boolean {
  switch (scope) {
    case "full":
      return true;
    case "cheap":
      return estCost <= PLAYGROUND_CHEAP_CAP;
    default:
      return false; // none / unknown => fail closed
  }
}

function Playground() {
  const { t } = useTranslation();
  // `?model=<alias>` prefill — set from the Models table's "Test in
  // playground" affordance. Consumed exactly once on first mount
  // (guarded by prefillDone) so a later manual model change doesn't
  // snap back to the URL value.
  const search = useSearch({ from: playgroundRoute.id });
  const [output, setOutput] = useState<"all" | "image" | "video" | "audio">(
    "all",
  );
  const [selectedKeyId, setSelectedKeyId] = useState<string>("");
  const [modelId, setModelId] = useState<string>("");
  const [values, setValues] = useState<Record<string, ParamValue>>({});
  const [rawJSON, setRawJSON] = useState<string>("{}");
  const [tab, setTab] = useState<"form" | "raw">("form");
  const [estimate, setEstimate] = useState<unknown>(null);
  const [execResult, setExecResult] = useState<unknown>(null);
  const prefillDone = useRef(false);

  const models = useQuery({
    queryKey: ["playground", "models"],
    queryFn: admin.playgroundModels,
  });

  const keys = useQuery({
    queryKey: ["keys"],
    queryFn: admin.listKeys,
  });

  const selectedKey = useMemo<ApiKey | null>(
    () => keys.data?.find((k) => k.id === selectedKeyId) ?? null,
    [keys.data, selectedKeyId],
  );

  const filtered = useMemo(() => {
    const all = models.data?.data ?? [];
    return output === "all" ? all : all.filter((m) => m.output === output);
  }, [models.data, output]);

  // Prefill from ?model=<alias> once both models and (optionally) the
  // selected key have landed so we can honour the scope gate. We
  // deliberately don't require a key: if the URL alias exists in the
  // catalog, prefill it; the user still has to pick a key before
  // execute/estimate light up. If a key is already selected we
  // additionally check its playground scope so a "cheap" key doesn't
  // land on a model it can't run.
  useEffect(() => {
    if (prefillDone.current) return;
    const requested = search?.model;
    if (!requested) return;
    // Wait until models are loaded — otherwise we'd flag the prefill
    // as "done" before we can honour it.
    if (models.isLoading) return;
    const match = filtered.find((m) => m.id === requested);
    if (!match) {
      // Alias not in catalog (or filtered out by the current output
      // filter — which is "all" on first mount). Mark done so we
      // don't keep looking on subsequent renders.
      prefillDone.current = true;
      return;
    }
    // If a key is already selected, defer to its scope. No key ==
    // "prefill anyway"; the execute button stays disabled until the
    // operator picks one.
    if (selectedKey) {
      const scope = selectedKey.playground_scope;
      if (!modelAllowedForScope(scope, match.est_cost)) {
        prefillDone.current = true;
        return;
      }
    }
    setModelId(requested);
    prefillDone.current = true;
  }, [search?.model, filtered, models.isLoading, selectedKey]);

  const selectedModel = useMemo(
    () => filtered.find((m) => m.id === modelId) ?? null,
    [filtered, modelId],
  );

  // A model is allowed only when a key is selected and its scope permits
  // the model's cost. Without a key everything is gated (forced selection).
  const isModelAllowed = (m: PlaygroundModel): boolean =>
    !!selectedKey && modelAllowedForScope(selectedKey.playground_scope, m.est_cost);

  const hasKey = !!selectedKeyId;

  // Assemble the request body from either the form values or the raw
  // JSON tab. `model` is always forced to the selected id so the two
  // tabs can never drift on which model to hit.
  const buildBody = (): Record<string, unknown> | null => {
    if (tab === "raw") {
      try {
        const parsed = JSON.parse(rawJSON) as Record<string, unknown>;
        return { ...parsed, model: modelId };
      } catch {
        return null;
      }
    }
    const clean: Record<string, unknown> = { model: modelId };
    for (const [k, v] of Object.entries(values)) {
      if (v === null || v === "" || v === undefined) continue;
      clean[k] = v;
    }
    return clean;
  };

  // When the operator flips to the raw tab, seed the textarea with the
  // form's current shape so they don't lose context switching modes.
  const openRaw = () => {
    setRawJSON(JSON.stringify(buildBody() ?? {}, null, 2));
    setTab("raw");
  };

  const setModel = (id: string) => {
    setModelId(id);
    setValues({});
    setEstimate(null);
    setExecResult(null);
  };

  // Changing the acting key resets the model selection and results: the
  // new key's scope may no longer permit the previously chosen model.
  const setKey = (id: string) => {
    setSelectedKeyId(id);
    setModelId("");
    setValues({});
    setEstimate(null);
    setExecResult(null);
  };

  const estimateM = useMutation({
    mutationFn: () => {
      if (!selectedKeyId) throw new Error("no api key selected");
      if (!modelId) throw new Error("no model selected");
      const body = buildBody();
      if (!body) throw new Error("invalid JSON body");
      return admin.playgroundEstimate({
        model: modelId,
        params: body,
        as_api_key_id: selectedKeyId,
      });
    },
    onSuccess: (res) => setEstimate(res),
    onError: (err) => {
      setEstimate(null);
      toast.error(errMsg(err));
    },
  });

  const executeM = useMutation({
    mutationFn: () => {
      if (!selectedKeyId) throw new Error("no api key selected");
      if (!modelId) throw new Error("no model selected");
      const body = buildBody();
      if (!body) throw new Error("invalid JSON body");
      return admin.playgroundExecute({ ...body, as_api_key_id: selectedKeyId });
    },
    onSuccess: (res) => setExecResult(res),
    onError: (err) => {
      setExecResult(null);
      toast.error(errMsg(err));
    },
  });

  return (
    <div className="grid grid-cols-1 gap-4 @3xl/main:grid-cols-2">
      {/* Left column — Input: key, model, params, actions. */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconMessageChatbot className="size-5" /> {t("playground.title")}
          </CardTitle>
          <CardDescription>{t("playground.description")}</CardDescription>
          <CardAction>
            {selectedKey ? (
              <StatusBadge tone="info">
                {t("playground.runningAs", {
                  name: selectedKey.name,
                  scope: selectedKey.playground_scope,
                })}
              </StatusBadge>
            ) : null}
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-4">
          {/* Warning: real credits consumed */}
          <div className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-900 dark:bg-amber-950/50 dark:text-amber-200">
            <IconAlertTriangle className="mt-0.5 size-4 shrink-0" />
            <div>
              <p className="font-medium">{t("playground.warning.title")}</p>
              <p className="mt-0.5 text-amber-700 dark:text-amber-300">{t("playground.warning.description")}</p>
            </div>
          </div>
          {/* 1. Acting API key — required, on top. */}
          <div className="space-y-1.5">
            <Label>{t("playground.keyPickerLabel")}</Label>
            <Select value={selectedKeyId} onValueChange={setKey}>
              <SelectTrigger className="w-full">
                <SelectValue
                  placeholder={t("playground.keyPickerPlaceholder")}
                />
              </SelectTrigger>
              <SelectContent>
                {keys.isLoading ? (
                  <div className="p-2 text-sm text-muted-foreground">
                    {t("common.loading")}
                  </div>
                ) : (keys.data ?? []).length === 0 ? (
                  <div className="p-2 text-sm text-muted-foreground">
                    {t("playground.keyPickerPlaceholder")}
                  </div>
                ) : (
                  (keys.data ?? []).map((k) => (
                    <SelectItem key={k.id} value={k.id}>
                      <KeyOptionLabel apiKey={k} />
                    </SelectItem>
                  ))
                )}
              </SelectContent>
            </Select>
            {!hasKey ? (
              <p className="text-xs text-muted-foreground">
                {t("playground.keyRequired")}
              </p>
            ) : selectedKey?.playground_scope === "none" ? (
              <p className="text-xs text-destructive">
                {t("playground.keyScopeNone")}
              </p>
            ) : null}
          </div>

          {/* 2. Output filter + model picker. */}
          <div className="flex flex-wrap items-center gap-2">
            <Select
              value={output}
              onValueChange={(v) => setOutput(v as typeof output)}
            >
              <SelectTrigger size="sm" className="w-32">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("playground.outputAll")}</SelectItem>
                <SelectItem value="image">
                  {t("playground.outputImage")}
                </SelectItem>
                <SelectItem value="video">
                  {t("playground.outputVideo")}
                </SelectItem>
                <SelectItem value="audio">{t("playground.outputAudio")}</SelectItem>
              </SelectContent>
            </Select>
            <Select value={modelId} onValueChange={setModel} disabled={!hasKey}>
              <SelectTrigger size="sm" className="w-72">
                <SelectValue
                  placeholder={t("playground.modelPickerPlaceholder")}
                />
              </SelectTrigger>
              <SelectContent>
                {models.isLoading ? (
                  <div className="p-2 text-sm text-muted-foreground">
                    {t("common.loading")}
                  </div>
                ) : filtered.length === 0 ? (
                  <div className="p-2 text-sm text-muted-foreground">
                    {t("playground.noModels")}
                  </div>
                ) : (
                  filtered.map((m) => (
                    <SelectItem
                      key={m.id}
                      value={m.id}
                      disabled={!isModelAllowed(m)}
                    >
                      <ModelOptionLabel
                        model={m}
                        allowed={isModelAllowed(m)}
                      />
                    </SelectItem>
                  ))
                )}
              </SelectContent>
            </Select>
          </div>

          {/* 3. Form / Raw JSON authoring. */}
          {selectedModel ? (
            <Tabs
              value={tab}
              onValueChange={(v) => (v === "raw" ? openRaw() : setTab("form"))}
            >
              <TabsList>
                <TabsTrigger value="form">{t("playground.tabForm")}</TabsTrigger>
                <TabsTrigger value="raw">{t("playground.tabRawJson")}</TabsTrigger>
              </TabsList>
              <TabsContent value="form" className="pt-4">
                <ParamForm
                  params={selectedModel.required_params}
                  values={values}
                  exampleBodyJSON={selectedModel.example_body_json}
                  enums={selectedModel.enums}
                  onChange={(k, v) =>
                    setValues((prev) => ({ ...prev, [k]: v }))
                  }
                />
              </TabsContent>
              <TabsContent value="raw" className="pt-4">
                <Textarea
                  value={rawJSON}
                  onChange={(e) => setRawJSON(e.target.value)}
                  className="min-h-56 font-mono text-xs"
                />
              </TabsContent>
            </Tabs>
          ) : (
            <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
              {hasKey
                ? t("playground.modelPickerPlaceholder")
                : t("playground.keyRequired")}
            </div>
          )}

          {/* 4. Cost tag + Actions. */}
          {selectedModel ? (
            <div className="flex items-center gap-2 rounded-md border bg-muted/50 px-3 py-2 text-sm">
              <span className="font-medium text-foreground">{selectedModel.id}</span>
              <span className="text-muted-foreground">·</span>
              <span className="text-muted-foreground">{selectedModel.output}</span>
              <span className="text-muted-foreground">·</span>
              <StatusBadge tone="warning">
                {t("playground.estCost", {
                  value: selectedModel.est_cost.toLocaleString(undefined, {
                    maximumFractionDigits: 2,
                  }),
                })}
              </StatusBadge>
            </div>
          ) : null}
          <div className="flex gap-2">
            <Button
              variant="outline"
              onClick={() => estimateM.mutate()}
              disabled={!hasKey || !modelId || estimateM.isPending}
            >
              <IconRuler />
              {estimateM.isPending
                ? t("playground.estimating")
                : t("playground.estimate")}
            </Button>
            <Button
              onClick={() => executeM.mutate()}
              disabled={!hasKey || !modelId || executeM.isPending}
            >
              <IconPlayerPlay />
              {executeM.isPending
                ? t("playground.executing")
                : t("playground.execute")}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Right column — Output: estimate + execute results. */}
      <div className="space-y-4">
        {estimate ? (
          <ResultCard title={t("playground.estimateResult")} data={estimate} />
        ) : null}
        {execResult ? (
          <ResultCard title={t("playground.executeResult")} data={execResult} />
        ) : null}
        {!estimate && !execResult ? (
          models.isLoading ? (
            <Skeleton className="h-40 w-full" />
          ) : (
            <Card>
              <CardHeader>
                <CardTitle className="text-base">
                  {t("playground.outputTitle")}
                </CardTitle>
              </CardHeader>
              <CardContent>
                <div className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
                  {t("playground.outputEmpty")}
                </div>
              </CardContent>
            </Card>
          )
        ) : null}
      </div>
    </div>
  );
}

function KeyOptionLabel({ apiKey }: { apiKey: ApiKey }) {
  const scopeTone =
    apiKey.playground_scope === "full"
      ? "brand"
      : apiKey.playground_scope === "cheap"
        ? "info"
        : "muted";
  return (
    <span className="flex items-center gap-1.5">
      <span className="text-xs">{apiKey.name}</span>
      {apiKey.key_last4 ? (
        <span className="font-mono text-[10px] text-muted-foreground">
          ···{apiKey.key_last4}
        </span>
      ) : null}
      <StatusBadge tone={scopeTone} className="ml-1">
        {apiKey.playground_scope}
      </StatusBadge>
    </span>
  );
}

function ModelOptionLabel({
  model,
  allowed,
}: {
  model: PlaygroundModel;
  allowed: boolean;
}) {
  const { t } = useTranslation();
  return (
    <span className="flex items-center gap-1.5">
      <span className="font-mono text-xs">{model.id}</span>
      <span className="text-[10px] text-muted-foreground">
        {t("playground.estCost", {
          value: model.est_cost.toLocaleString(undefined, {
            maximumFractionDigits: 2,
          }),
        })}
      </span>
      {model.unstable ? (
        <StatusBadge tone="warning" className="ml-1">
          {t("playground.tagsUnstable")}
        </StatusBadge>
      ) : null}
      {!allowed ? (
        <StatusBadge tone="danger" className="ml-1">
          {t("playground.tagsBlocked")}
        </StatusBadge>
      ) : null}
    </span>
  );
}

// ResultCard renders a two-part preview: a media area (image / video)
// when the response carries a recognisable result URL, and always a
// raw JSON dump below it so the operator has the full response to
// inspect / copy.
function ResultCard({ title, data }: { title: string; data: unknown }) {
  const url = findResultURL(data);
  const kind = url ? detectMediaKind(url) : null;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        {url && kind === "image" ? (
          <a href={url} target="_blank" rel="noreferrer">
            <img
              src={url}
              alt="result"
              className="max-h-64 w-full rounded-md border object-contain"
            />
          </a>
        ) : null}
        {url && kind === "video" ? (
          <video
            src={url}
            controls
            className="max-h-64 w-full rounded-md border"
          />
        ) : null}
        {url && kind === "audio" ? (
          <audio src={url} controls className="w-full" />
        ) : null}
        <pre className="max-h-96 overflow-auto rounded-md border bg-muted/40 p-3 text-xs">
          {JSON.stringify(data, null, 2)}
        </pre>
      </CardContent>
    </Card>
  );
}

// findResultURL walks the response looking for a plausible media URL.
// higgsfield's own responses use `result_url`, but async jobs might
// hide URLs under `data[].url` or similar — we cover the common shapes
// without pretending to know every variant.
function findResultURL(data: unknown): string | null {
  if (!data || typeof data !== "object") return null;
  const o = data as Record<string, unknown>;
  const direct = o.result_url ?? o.url ?? o.image_url ?? o.video_url;
  if (typeof direct === "string") return direct;
  if (Array.isArray(o.data) && o.data.length > 0) {
    const first = o.data[0] as Record<string, unknown> | undefined;
    if (first && typeof first === "object") {
      const nested = first.url ?? first.result_url ?? first.image_url;
      if (typeof nested === "string") return nested;
    }
  }
  return null;
}

function detectMediaKind(url: string): "image" | "video" | "audio" | null {
  const lc = url.toLowerCase().split("?")[0]!;
  if (/\.(png|jpe?g|webp|gif|avif)$/.test(lc)) return "image";
  if (/\.(mp4|mov|webm|m4v)$/.test(lc)) return "video";
  if (/\.(mp3|wav|ogg|m4a|flac)$/.test(lc)) return "audio";
  return null;
}

function errMsg(err: unknown): string {
  if (err instanceof ApiError)
    return `${err.status} ${err.type}: ${err.message}`;
  if (err instanceof Error) return err.message;
  return String(err);
}

// PlaygroundSearch is the typed search-param shape for the playground
// route. Only one param today: `model=<alias>` used by the Models
// table's "Test in playground" affordance. Extra params are stripped
// so a stale share-link doesn't inject random state.
interface PlaygroundSearch {
  model?: string;
}

export const playgroundRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/playground",
  staticData: { titleKey: "nav.playground" },
  component: Playground,
  validateSearch: (raw: Record<string, unknown>): PlaygroundSearch => {
    const model = raw?.model;
    return typeof model === "string" && model.length > 0 ? { model } : {};
  },
});
