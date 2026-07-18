// providerOf infers the upstream vendor for a model from its alias / jst.
//
// /v1/models doesn't carry a provider field, so we derive one from the
// naming convention operators already recognise (kling-…, veo-…, flux-…).
// Rules are matched in order against the lower-cased "alias jst" string,
// so the more specific tokens (nano-banana, seedream) sit ahead of the
// broad ones (gpt). Anything that matches nothing is treated as a
// first-party Higgsfield studio model, which is the sensible catch-all.
//
// `key` is a stable slug used for the provider filter dropdown; `name`
// is the (English) label rendered in the table. The `Logo` field points
// at a @lobehub/icons brand component so we render actual vendor marks
// instead of a letter fallback.

import type { ComponentType, CSSProperties } from "react";
import { IconBox } from "@tabler/icons-react";
import {
  Alibaba,
  ByteDance,
  Flux,
  Google,
  Grok,
  Kling,
  Minimax,
  OpenAI,
} from "@lobehub/icons";

// LogoProps mirrors the shared prop shape @lobehub/icons exposes on the
// default (Mono) export of every brand component. We keep the minimal
// surface the app actually consumes so downstream call sites don't
// depend on the library's internals.
export interface LogoProps {
  size?: number | string;
  style?: CSSProperties;
  className?: string;
}

export type LogoComponent = ComponentType<LogoProps>;

export interface Provider {
  name: string;
  key: string;
  Logo: LogoComponent;
}

interface Rule {
  key: string;
  name: string;
  Logo: LogoComponent;
  // Substrings; any hit (via includes) on "alias jst" claims the model.
  match: string[];
}

// Ordered most-specific → most-generic. `gpt` sits last of the OpenAI
// tokens so `gpt-image` resolves before the bare `gpt` fallback, and
// `nano-banana` lands on Google rather than any looser rule.
//
// Provider names are intentionally English-only: the operator survey
// pushed back on mixed-language labels like "快手 Kling" — the brand
// mark plus the English name is enough for both audiences.
const RULES: Rule[] = [
  { key: "kling", name: "Kling", Logo: Kling as LogoComponent, match: ["kling"] },
  { key: "veo", name: "Google Veo", Logo: Google as LogoComponent, match: ["veo"] },
  {
    key: "seedance",
    name: "ByteDance",
    Logo: ByteDance as LogoComponent,
    // seedream (image) & seedance (video) both come from ByteDance.
    match: ["seedance", "seedream"],
  },
  { key: "google", name: "Google", Logo: Google as LogoComponent, match: ["nano-banana"] },
  {
    key: "openai",
    name: "OpenAI",
    Logo: OpenAI as LogoComponent,
    match: ["gpt-image", "openai", "sora", "gpt"],
  },
  { key: "flux", name: "Flux", Logo: Flux as LogoComponent, match: ["flux"] },
  { key: "wan", name: "Alibaba", Logo: Alibaba as LogoComponent, match: ["wan"] },
  {
    key: "minimax",
    name: "MiniMax",
    Logo: Minimax as LogoComponent,
    match: ["hailuo", "minimax"],
  },
  { key: "grok", name: "xAI", Logo: Grok as LogoComponent, match: ["grok"] },
];

// Catch-all for first-party studio models (cinematic-studio,
// marketing-studio, bd-studio, soul, canvas, autosprite, ai-influencer,
// character-sheet, …). @lobehub/icons doesn't ship a Higgsfield mark,
// so we fall back to Tabler's generic IconBox.
// TODO: swap for a project-specific logo once we have one.
export const HIGGSFIELD: Provider = {
  key: "higgsfield",
  name: "Higgsfield",
  Logo: IconBox as unknown as LogoComponent,
};

export function providerOf(alias: string, jst: string): Provider {
  const hay = `${alias} ${jst}`.toLowerCase();
  for (const rule of RULES) {
    if (rule.match.some((m) => hay.includes(m))) {
      return { key: rule.key, name: rule.name, Logo: rule.Logo };
    }
  }
  return HIGGSFIELD;
}

// providerLogo is a thin helper for call sites that only want the icon
// component (e.g. an icon-only badge). Preserved so downstream imports
// don't need to reach into Provider directly.
export function providerLogo(providerKey: string): LogoComponent {
  const rule = RULES.find((r) => r.key === providerKey);
  return rule ? rule.Logo : HIGGSFIELD.Logo;
}
