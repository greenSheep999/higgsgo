#!/usr/bin/env node
// Dumps the VERIFIED_MODELS map from higgsfield-register/server/src/mapping/verified_models.mjs
// to a stable JSON file that higgsgo reads at startup.
//
// Usage:
//   node scripts/dump-verified-models.mjs [--source /path/to/higgsfield-register] [--out data/reference/verified-models.json]

import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

const args = process.argv.slice(2);
function arg(name, def) {
  const i = args.indexOf(`--${name}`);
  return i >= 0 ? args[i + 1] : def;
}

const source = arg('source', '/Users/danlio/Repositories/daniel/higgsfield-register');
const out = arg('out', 'data/reference/verified-models.json');

const modulePath = resolve(source, 'server/src/mapping/verified_models.mjs');
const mod = await import(`file://${modulePath}`);
const VERIFIED = mod.VERIFIED_MODELS;
if (!VERIFIED || typeof VERIFIED !== 'object') {
  console.error(`no VERIFIED_MODELS export at ${modulePath}`);
  process.exit(1);
}

// Also pull SEALED.json for A/B/D/X classification.
const sealedPath = resolve(source, 'server/data/SEALED.json');
let sealed = null;
try {
  sealed = JSON.parse(readFileSync(sealedPath, 'utf8'));
} catch (err) {
  console.warn(`sealed.json not found (${err.message}); classification will be empty`);
}

const jstClass = new Map();
if (sealed?.models) {
  for (const [jst, meta] of Object.entries(sealed.models)) {
    if (meta && typeof meta.class === 'string') jstClass.set(jst, meta.class);
  }
}

const rows = Object.entries(VERIFIED).map(([alias, spec]) => ({
  alias,
  jst: spec.jobSetType,
  endpoint: spec.endpoint,
  version: spec.version || 'v1-hyphen',
  output: spec.output,
  cost_credits_h: spec.cost != null ? Math.round(Number(spec.cost) * 100) : null,
  required_params: spec.requiredParams || [],
  needs_image:   !!spec.needsImage,
  needs_video:   !!spec.needsVideo,
  needs_audio:   !!spec.needsAudio,
  needs_medias:  !!spec.needsMedias,
  needs_outfit:  !!spec.needsOutfit,
  app_slug:      spec.appSlug || '',
  supports_unlim: !!spec.supportsUnlim,
  unlim_jst:     spec.unlimJobSetType || '',
  media_role:    spec.mediaRole || '',
  classification: jstClass.get(spec.jobSetType) || '',
}));

// Merge in aliases *_unlimited → base
const aliasEntries = [];
for (const r of rows) {
  if (r.unlim_jst) {
    aliasEntries.push({
      alias: r.unlim_jst.replaceAll('_', '-'),
      base_alias: r.alias,
      base_jst: r.jst,
      strategy: 'transparent',
      note: 'Ultra-only endpoint; higgsgo transparently forwards to base model.',
    });
  }
}

writeFileSync(out, JSON.stringify({
  generatedAt: new Date().toISOString(),
  source: modulePath,
  models: rows,
  aliases: aliasEntries,
}, null, 2));

console.log(`wrote ${out}`);
console.log(`  models: ${rows.length}`);
console.log(`  aliases: ${aliasEntries.length}`);
