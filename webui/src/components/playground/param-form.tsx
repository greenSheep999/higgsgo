import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";

// ParamForm renders a shadcn form derived from a model's required_params
// list. The backend doesn't ship a JSON schema — it only tells us the
// param names — so we use a small name-based heuristic to pick a widget
// (textarea for prompts, number input for width / height / seed / steps
// / batch_size / duration, checkbox for boolean-shaped names, plain text
// for everything else). Fields we don't recognise fall back to a text
// input; that's fine because the operator can always drop into the raw
// JSON escape hatch above the form to override anything.
//
// Aspect ratio / resolution / duration numeric hints — those are still
// text inputs; higgsfield's server-side enum names are model-specific
// and we don't have them exposed anywhere the UI can read yet.

export type ParamValue = string | number | boolean | null;

interface Props {
  params: string[]; // required_params from PlaygroundModel
  values: Record<string, ParamValue>;
  onChange: (name: string, value: ParamValue) => void;
}

// Name-based widget classifier. Order matters — the first branch that
// matches wins, so long-form checks (like `prompt` before `_prompt`)
// resolve the more specific case.
type Widget =
  | "textarea"
  | "number"
  | "boolean"
  | "url"
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

function classify(name: string): Widget {
  const lc = name.toLowerCase();
  if (lc === "prompt" || lc.endsWith("_prompt") || lc === "negative_prompt" || lc === "audio_prompt") {
    return "textarea";
  }
  if (NUMBER_HINTS.includes(lc)) return "number";
  if (BOOL_HINTS.includes(lc)) return "boolean";
  if (lc === "url" || lc.startsWith("input_") || lc.endsWith("_url")) {
    return "url";
  }
  return "text";
}

export function ParamForm({ params, values, onChange }: Props) {
  const { t } = useTranslation();
  const rows = useMemo(
    () => params.map((p) => ({ name: p, widget: classify(p) })),
    [params],
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
      {rows.map(({ name, widget }) => (
        <Field
          key={name}
          name={name}
          widget={widget}
          value={values[name]}
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
  onChange,
}: {
  name: string;
  widget: Widget;
  value: ParamValue;
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

  switch (widget) {
    case "textarea":
      return (
        <div className="md:col-span-2 space-y-1">
          {label}
          <Textarea
            id={id}
            value={typeof value === "string" ? value : ""}
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
            checked={value === true}
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
            placeholder="https://…"
            value={typeof value === "string" ? value : ""}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      );
    default:
      return (
        <div className="space-y-1">
          {label}
          <Input
            id={id}
            value={typeof value === "string" ? value : ""}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
      );
  }
}
