import { useMemo, useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  IconMessageChatbot,
  IconPlayerPlay,
  IconRuler,
} from "@tabler/icons-react";

import { admin, ApiError, type PlaygroundModel } from "@/lib/api";
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

function Playground() {
  const { t } = useTranslation();
  const [output, setOutput] = useState<"all" | "image" | "video" | "audio">(
    "all",
  );
  const [modelId, setModelId] = useState<string>("");
  const [values, setValues] = useState<Record<string, ParamValue>>({});
  const [rawJSON, setRawJSON] = useState<string>("{}");
  const [tab, setTab] = useState<"form" | "raw">("form");
  const [estimate, setEstimate] = useState<unknown>(null);
  const [execResult, setExecResult] = useState<unknown>(null);

  const models = useQuery({
    queryKey: ["playground", "models"],
    queryFn: admin.playgroundModels,
  });

  const filtered = useMemo(() => {
    const all = models.data?.data ?? [];
    return output === "all" ? all : all.filter((m) => m.output === output);
  }, [models.data, output]);

  const selectedModel = useMemo(
    () => filtered.find((m) => m.id === modelId) ?? null,
    [filtered, modelId],
  );

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

  const estimateM = useMutation({
    mutationFn: () => {
      if (!modelId) throw new Error("no model selected");
      const body = buildBody();
      if (!body) throw new Error("invalid JSON body");
      return admin.playgroundEstimate({ model: modelId, params: body });
    },
    onSuccess: (res) => setEstimate(res),
    onError: (err) => {
      setEstimate(null);
      toast.error(errMsg(err));
    },
  });

  const executeM = useMutation({
    mutationFn: () => {
      if (!modelId) throw new Error("no model selected");
      const body = buildBody();
      if (!body) throw new Error("invalid JSON body");
      return admin.playgroundExecute(body);
    },
    onSuccess: (res) => setExecResult(res),
    onError: (err) => {
      setExecResult(null);
      toast.error(errMsg(err));
    },
  });

  return (
    <div className="grid grid-cols-1 gap-4 @5xl/main:grid-cols-3">
      <Card className="@5xl/main:col-span-2">
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <IconMessageChatbot className="size-5" /> {t("playground.title")}
          </CardTitle>
          <CardDescription>{t("playground.description")}</CardDescription>
          <CardAction>
            {models.data ? (
              <StatusBadge tone="info">
                {t("playground.scope", { scope: models.data.scope })}
              </StatusBadge>
            ) : null}
          </CardAction>
        </CardHeader>
        <CardContent className="space-y-4">
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
                <SelectItem value="audio">audio</SelectItem>
              </SelectContent>
            </Select>
            <Select value={modelId} onValueChange={setModel}>
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
                    no models
                  </div>
                ) : (
                  filtered.map((m) => (
                    <SelectItem key={m.id} value={m.id} disabled={!m.allowed}>
                      <ModelOptionLabel model={m} />
                    </SelectItem>
                  ))
                )}
              </SelectContent>
            </Select>
            {selectedModel ? (
              <StatusBadge tone="muted">
                {selectedModel.output} · est{" "}
                {selectedModel.est_cost.toLocaleString(undefined, {
                  maximumFractionDigits: 2,
                })}
              </StatusBadge>
            ) : null}
          </div>

          {selectedModel ? (
            <Tabs
              value={tab}
              onValueChange={(v) => (v === "raw" ? openRaw() : setTab("form"))}
            >
              <TabsList>
                <TabsTrigger value="form">Form</TabsTrigger>
                <TabsTrigger value="raw">Raw JSON</TabsTrigger>
              </TabsList>
              <TabsContent value="form" className="pt-4">
                <ParamForm
                  params={selectedModel.required_params}
                  values={values}
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
              {t("playground.modelPickerPlaceholder")}
            </div>
          )}

          <div className="flex gap-2">
            <Button
              variant="outline"
              onClick={() => estimateM.mutate()}
              disabled={!modelId || estimateM.isPending}
            >
              <IconRuler />
              {estimateM.isPending
                ? t("playground.estimating")
                : t("playground.estimate")}
            </Button>
            <Button
              onClick={() => executeM.mutate()}
              disabled={!modelId || executeM.isPending}
            >
              <IconPlayerPlay />
              {executeM.isPending
                ? t("playground.executing")
                : t("playground.execute")}
            </Button>
          </div>
        </CardContent>
      </Card>

      <div className="space-y-4">
        {estimate ? (
          <ResultCard
            title={t("playground.estimateResult")}
            data={estimate}
          />
        ) : null}
        {execResult ? (
          <ResultCard
            title={t("playground.executeResult")}
            data={execResult}
          />
        ) : null}
        {!estimate && !execResult && models.isLoading ? (
          <Skeleton className="h-40 w-full" />
        ) : null}
      </div>
    </div>
  );
}

function ModelOptionLabel({ model }: { model: PlaygroundModel }) {
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
      {!model.allowed ? (
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

export const playgroundRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/playground",
  staticData: { titleKey: "nav.playground" },
  component: Playground,
});
