import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";

// ParamForm renders a shadcn form for a model's request params. The form
// is now schema-driven when the backend ships an `example_body_json` (a
// working request captured from a real upstream call) and an optional
// `enums` map (param name -> allowed values, sourced from the body
// template's catalogRefs → catalogs/*.json). When neither is present we
// fall back to the legacy name-heuristic classifier so older models and
// slim deployments still render *something* — just without dropdowns or
// pre-filled defaults.
//
// Widget selection order:
//   1. If the param has an entry in `enums`, render a <Select>.
//   2. Else, inspect the value pulled from example_body_json:
//        - boolean literal          → Switch
//        - number literal           → number input
//        - string looking like URL  → url input
//        - long prompt string       → Textarea
//        - object/array             → Textarea holding JSON
//        - anything else            → text input
//   3. Else fall back to the legacy NUMBER_HINTS / BOOL_HINTS name heuristic.
//
// The operator can always drop into the "Raw JSON" tab above the form to
// override anything the schema-driven UI does not surface.

export type ParamValue = string | number | boolean | null | object;

interface Props {
  params: string[]; // required_params from PlaygroundModel
  values: Record<string, ParamValue>;
  onChange: (name: string, value: ParamValue) => void;
  // Optional schema surfaces. Both are safe to omit — the form
  // degrades to the pre-schema heuristic.
  exampleBodyJSON?: string;
  enums?: Record<string, string[]>;
}

// Widget kinds ParamForm knows how to render. `select` and `json` are
// new: `select` fires when the backend ships an enum for the param,
// `json` handles nested objects/arrays from the example body (the
// operator edits the raw JSON string, which we parse back on submit).
type Widget =
  | "textarea"
  | "number"
  | "boolean"
  | "url"
  | "select"
  | "json"
  | "text";

const NUMBER_HINTS = [
  "width",
  "height",
  "seed",
  "steps",
  "batch_size",
  "duration",
  "cfg",
  "cfg_scale",
  "strength",
  "clips_num",
  "guide_scale",
  "guide_scale_2",
  "frames",
  "fps",
  "sample_shift",
  "sample_guide_scale",
  "output_width",
  "output_height",
  "guide_end",
  "boundary",
  "creativity",
  "texture",
  "sharpen",
  "denoise",
  "brightness",
  "style_strength",
];

const BOOL_HINTS = [
  "enhance_prompt",
  "enhance",
  "generate_audio",
  "camera_fixed",
  "fixed_lens",
  "multi_shots",
  "is_storyboard",
  "is_zoom_control",
  "use_unlim",
  "is_end_frame",
  "is_chain",
  "is_draw",
  "use_sage_attention",
  "use_lightx",
  "sound",
  "autoprompt",
  "track_face_crop",
  "enable_color_aligning",
];

// parseExampleBody unwraps the outer `{params: {...}}` envelope from
// the model's example body and returns the inner params map. A
// missing / malformed template returns an empty object so callers
// can defensively `.[name]` without a branch.
function parseExampleBody(json?: string): Record<string, unknown> {
  if (!json) return {};
  try {
    const parsed = JSON.parse(json);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const outer = parsed as Record<string, unknown>;
      const inner = outer.params;
      if (inner && typeof inner === "object" && !Array.isArray(inner)) {
        return inner as Record<string, unknown>;
      }
    }
  } catch {
    /* fall through */
  }
  return {};
}

// classify picks a widget for `name`. It consults, in order:
//   1. `enums[name]` — if present, render a Select.
//   2. `example[name]` — infer from the literal value's runtime type.
//   3. name-heuristic fallback — the original NUMBER_HINTS / BOOL_HINTS
//      tables so pre-template models still render something useful.
function classify(
  name: string,
  example: Record<string, unknown>,
  enums: Record<string, string[]> | undefined,
): Widget {
  const lc = name.toLowerCase();
  if (enums && enums[name] && enums[name].length > 0) return "select";

  if (name in example) {
    const v = example[name];
    if (typeof v === "boolean") return "boolean";
    if (typeof v === "number") return "number";
    if (typeof v === "string") {
      if (lc === "prompt" || lc.endsWith("_prompt") ||
          lc === "negative_prompt" || lc === "audio_prompt") {
        return "textarea";
      }
      if (/^https?:\/\//i.test(v)) return "url";
      // Long free-text values render better as a textarea.
      if (v.length >= 80) return "textarea";
      return "text";
    }
    // Objects / arrays — surface as raw JSON so the operator can
    // edit nested structures like input_images / medias / style.
    if (v && typeof v === "object") return "json";
    // null / undefined fall through to the name heuristic.
  }

  // Legacy name heuristic — retained so older models without a body
  // template still classify sensibly.
  if (lc === "prompt" || lc.endsWith("_prompt") ||
      lc === "negative_prompt" || lc === "audio_prompt") {
    return "textarea";
  }
  if (NUMBER_HINTS.includes(lc)) return "number";
  if (BOOL_HINTS.includes(lc)) return "boolean";
  if (lc === "url" || lc.startsWith("input_") || lc.endsWith("_url")) {
    return "url";
  }
  return "text";
}

export function ParamForm({
  params,
  values,
  onChange,
  exampleBodyJSON,
  enums,
}: Props) {
  const { t } = useTranslation();
  const example = useMemo(() => parseExampleBody(exampleBodyJSON), [exampleBodyJSON]);
  const rows = useMemo(
    () =>
      params.map((p) => ({
        name: p,
        widget: classify(p, example, enums),
        // Default = current value if set, else the example body's value.
        // Passed to Field so a textarea placeholder / initial value can
        // reflect the captured template. Kept as unknown because
        // JSON.stringify handles anything.
        exampleValue: example[p],
        options: enums?.[p],
      })),
    [params, example, enums],
  );

  if (rows.length === 0) {
    return (
      <p className="text-xs text-muted-foreground">
        {t("playground.noRequiredParams")}
      </p>
    );
  }

  return (
    <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
      {rows.map(({ name, widget, exampleValue, options }) => (
        <Field
          key={name}
          name={name}
          widget={widget}
          value={values[name]}
          exampleValue={exampleValue}
          options={options}
          onChange={(v) => onChange(name, v)}
        />
      ))}
    </div>
  );
}

function Field({
  name,
  widget,
  value,
  exampleValue,
  options,
  onChange,
}: {
  name: string;
  widget: Widget;
  value: ParamValue;
  exampleValue?: unknown;
  options?: string[];
  onChange: (v: ParamValue) => void;
}) {
  const id = `pg-${name}`;
  const label = (
    <Label htmlFor={id} className="text-xs">
      {name}
      <span className="ml-1 rounded bg-muted px-1 text-[10px] text-muted-foreground">
        {widget}
      </span>
    </Label>
  );
  // Placeholder text: for text-shaped widgets we show the example
  // body's captured value so the operator has a working starting point
  // without needing to fill the field. `String(...)` handles nulls
  // safely; strings render verbatim, numbers as digits, booleans as
  // "true"/"false".
  const placeholder = exampleValue != null && typeof exampleValue !== "object"
    ? `default: ${String(exampleValue)}`
    : undefined;

  switch (widget) {
    case "textarea":
      return (
        <div className="md:col-span-2 space-y-1">
          {label}
          <Textarea
            id={id}
            value={typeof value === "string" ? value : ""}
            placeholder={
              typeof exampleValue === "string" ? exampleValue : undefined
            }
            onChange={(e) => onChange(e.target.value)}
            className="min-h-20"
          />
        </div>
      );
    case "number":
      return (
        <div className="space-y-1">
          {label}
          <Input
            id={id}
            type="number"
            value={value == null ? "" : String(value)}
            placeholder={placeholder}
            onChange={(e) => {
              const v = e.target.value;
              onChange(v === "" ? null : Number(v));
            }}
          />
        </div>
      );
    case "boolean":
      return (
        <div className="flex items-center justify-between gap-2 rounded-md border px-3 py-2">
          {label}
          <Switch
            id={id}
            checked={
              value === true ||
              (value == null && exampleValue === true)
            }
            onCheckedChange={(v) => onChange(v)}
          />
        </div>
      );
    case "url":
      return (
        <div className="md:col-span-2 space-y-1">
          {label}
          <Input
            id={id}
            type="url"
            placeholder={
              typeof exampleValue === "string"
                ? exampleValue
                : "https://…"
            }
            value={typeof value === "string" ? value : ""}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      );
    case "select": {
      // Select fires only when `options` is a non-empty array — the
      // classifier guards for that — so we can safely map over it.
      const opts = options ?? [];
      const current = typeof value === "string" ? value : "";
      return (
        <div className="space-y-1">
          {label}
          <Select
            value={current}
            onValueChange={(v) => onChange(v)}
          >
            <SelectTrigger id={id}>
              <SelectValue placeholder={placeholder ?? "select…"} />
            </SelectTrigger>
            <SelectContent>
              {opts.map((o) => (
                <SelectItem key={o} value={o}>
                  {/* Long UUIDs are unreadable; truncate to the last
                      12 chars so the dropdown stays scannable. */}
                  {o.length > 20 ? `…${o.slice(-12)}` : o}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <p className="text-[10px] text-muted-foreground">
            {opts.length} option{opts.length === 1 ? "" : "s"}
          </p>
        </div>
      );
    }
    case "json": {
      // Object / array shaped params. Store the raw JSON string in
      // local form state; on change we try to parse it back to the
      // structured value so the outbound request body contains the
      // real object, not a string. Parse failures silently keep the
      // last valid value — a red border would be nicer, but the raw
      // JSON tab above the form is the operator's escape hatch when
      // something goes wrong.
      const asString = typeof value === "string"
        ? value
        : value != null
          ? JSON.stringify(value, null, 2)
          : exampleValue != null
            ? JSON.stringify(exampleValue, null, 2)
            : "";
      return (
        <div className="md:col-span-2 space-y-1">
          {label}
          <Textarea
            id={id}
            value={asString}
            placeholder={
              exampleValue != null
                ? JSON.stringify(exampleValue, null, 2)
                : undefined
            }
            onChange={(e) => {
              const raw = e.target.value;
              try {
                const parsed = JSON.parse(raw);
                onChange(parsed as ParamValue);
              } catch {
                // Store as string; caller can still submit via raw
                // JSON tab.
                onChange(raw);
              }
            }}
            className="min-h-24 font-mono text-xs"
          />
        </div>
      );
    }
    default:
      return (
        <div className="space-y-1">
          {label}
          <Input
            id={id}
            value={typeof value === "string" ? value : ""}
            placeholder={placeholder}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      );
  }
}
