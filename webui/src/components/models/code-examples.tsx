import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { IconCopy, IconCheck } from "@tabler/icons-react";
import { toast } from "sonner";

import type { PublicModel } from "@/lib/api";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

// Language keys for the tabbed code blocks.
const LANGS = ["curl", "python_openai", "python_genai", "nodejs", "go"] as const;
type Lang = (typeof LANGS)[number];

// Determine which languages are relevant for a model based on its output type.
function relevantLangs(model: PublicModel): Lang[] {
  if (model.output === "image") {
    // Image models get the google genai SDK tab
    return ["curl", "python_openai", "python_genai", "nodejs", "go"];
  }
  // Video/audio models: skip the genai SDK (it's image-specific)
  return ["curl", "python_openai", "nodejs", "go"];
}

// Generate example code for a given model + language.
function generateCode(model: PublicModel, lang: Lang): string {
  const alias = model.id;
  const isImage = model.output === "image";
  const isVideo = model.output === "video";

  // Determine the API path based on output type. For video we surface the
  // new-api compatible path (/v1/video/generations, singular) so copied
  // examples drop straight into OneAPI-style stacks; the legacy plural
  // /v1/videos/generations is still routed to the same handler on the
  // server side for older integrations.
  const apiPath = isImage
    ? "/v1/images/generations"
    : isVideo
      ? "/v1/video/generations"
      : "/v1/audio/speech";

  switch (lang) {
    case "curl":
      return generateCurl(alias, apiPath, isImage);
    case "python_openai":
      return generatePythonOpenAI(alias, apiPath, isImage);
    case "python_genai":
      return generatePythonGenAI(alias);
    case "nodejs":
      return generateNodeJS(alias, apiPath, isImage);
    case "go":
      return generateGo(alias, apiPath, isImage);
  }
}

function generateCurl(alias: string, path: string, isImage: boolean): string {
  const body = isImage
    ? `{"model": "${alias}", "prompt": "A cat sitting on a windowsill"}`
    : `{"model": "${alias}", "prompt": "A cinematic shot of a sunset over the ocean"}`;
  return `curl -X POST https://your-instance${path} \\
  -H "Authorization: Bearer YOUR_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`;
}

function generatePythonOpenAI(
  alias: string,
  path: string,
  isImage: boolean,
): string {
  const funcName = isImage ? "images.generate" : "images.generate";
  const prompt = isImage
    ? "A cat sitting on a windowsill"
    : "A cinematic shot of a sunset over the ocean";
  // For video models the path differs but the SDK call pattern is similar
  if (path.includes("video")) {
    return `from openai import OpenAI

client = OpenAI(
    api_key="YOUR_API_KEY",
    base_url="https://your-instance/v1"
)

# Video generation (poll for result)
import requests, time

resp = requests.post(
    "https://your-instance${path}",
    headers={"Authorization": "Bearer YOUR_API_KEY"},
    json={"model": "${alias}", "prompt": "${prompt}"}
)
job = resp.json()
print(f"Job ID: {job['id']}")`;
  }
  return `from openai import OpenAI

client = OpenAI(
    api_key="YOUR_API_KEY",
    base_url="https://your-instance/v1"
)

response = client.${funcName}(
    model="${alias}",
    prompt="${prompt}",
    n=1
)

print(response.data[0].url)`;
}

function generatePythonGenAI(alias: string): string {
  return `from google import genai
from google.genai import types

client = genai.Client(
    api_key="YOUR_API_KEY",
    http_options=types.HttpOptions(base_url="https://your-instance/v1")
)

response = client.models.generate_images(
    model="${alias}",
    prompt="A cat sitting on a windowsill"
)

# response.generated_images[0].image.image_bytes
print(f"Generated {len(response.generated_images)} image(s)")`;
}

function generateNodeJS(
  alias: string,
  // path unused — the OpenAI SDK abstracts the URL path; kept in the
  // signature so all generateXxx() share the same shape.
  _path: string,
  isImage: boolean,
): string {
  const prompt = isImage
    ? "A cat sitting on a windowsill"
    : "A cinematic shot of a sunset over the ocean";
  return `import OpenAI from "openai";

const client = new OpenAI({
  apiKey: "YOUR_API_KEY",
  baseURL: "https://your-instance/v1",
});

const response = await client.images.generate({
  model: "${alias}",
  prompt: "${prompt}",
  n: 1,
});

console.log(response.data[0].url);`;
}

function generateGo(alias: string, path: string, isImage: boolean): string {
  const prompt = isImage
    ? "A cat sitting on a windowsill"
    : "A cinematic shot of a sunset over the ocean";
  return `package main

import (
\t"bytes"
\t"encoding/json"
\t"fmt"
\t"net/http"
)

func main() {
\tbody, _ := json.Marshal(map[string]any{
\t\t"model":  "${alias}",
\t\t"prompt": "${prompt}",
\t\t"n":      1,
\t})

\treq, _ := http.NewRequest("POST",
\t\t"https://your-instance${path}", bytes.NewReader(body))
\treq.Header.Set("Authorization", "Bearer YOUR_API_KEY")
\treq.Header.Set("Content-Type", "application/json")

\tresp, err := http.DefaultClient.Do(req)
\tif err != nil {
\t\tpanic(err)
\t}
\tdefer resp.Body.Close()
\tfmt.Println("Status:", resp.Status)
}`;
}

// CopyButton — a small icon button that copies text to clipboard
// and briefly shows a checkmark.
function CopyButton({ text }: { text: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  return (
    <button
      type="button"
      className="absolute right-2 top-2 inline-flex size-7 items-center justify-center rounded-md border bg-background/80 text-muted-foreground backdrop-blur-sm hover:bg-muted hover:text-foreground"
      title={t("models.codeExamples.copy")}
      aria-label={t("models.codeExamples.copy")}
      onClick={() => {
        void navigator.clipboard.writeText(text).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
          toast(t("models.codeExamples.copied"));
        });
      }}
    >
      {copied ? (
        <IconCheck className="size-3.5" />
      ) : (
        <IconCopy className="size-3.5" />
      )}
    </button>
  );
}

export function CodeExamples({ model }: { model: PublicModel }) {
  const { t } = useTranslation();
  const langs = useMemo(() => relevantLangs(model), [model]);

  const codeBlocks = useMemo(() => {
    const blocks: Record<string, string> = {};
    for (const lang of langs) {
      blocks[lang] = generateCode(model, lang);
    }
    return blocks;
  }, [model, langs]);

  return (
    <div>
      <div className="mb-1.5 text-xs font-semibold text-muted-foreground">
        {t("models.codeExamples.title")}
      </div>
      <Tabs defaultValue={langs[0]} className="w-full">
        <TabsList variant="line" className="w-full justify-start">
          {langs.map((lang) => (
            <TabsTrigger key={lang} value={lang} className="text-xs">
              {t(`models.codeExamples.lang.${lang}`)}
            </TabsTrigger>
          ))}
        </TabsList>
        {langs.map((lang) => (
          <TabsContent key={lang} value={lang}>
            <div className="relative mt-2 rounded-md border bg-muted/40">
              <CopyButton text={codeBlocks[lang]} />
              <pre className="max-h-72 overflow-auto p-3 font-mono text-[11px] leading-relaxed">
                {codeBlocks[lang]}
              </pre>
            </div>
          </TabsContent>
        ))}
      </Tabs>
    </div>
  );
}
