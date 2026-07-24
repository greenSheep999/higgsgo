// Small formatting helpers shared across pages.

export function formatCredits(cents: number): string {
  return (cents / 100).toLocaleString(undefined, {
    maximumFractionDigits: 2,
  });
}

export function formatRelative(iso: string | undefined): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  const now = Date.now();
  const diff = Math.round((now - then) / 1000);
  if (diff < 60) return `${diff}s ago`;
  if (diff < 3600) return `${Math.round(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.round(diff / 3600)}h ago`;
  return `${Math.round(diff / 86400)}d ago`;
}

export function formatDateTime(iso: string | undefined): string {
  if (!iso) return "—";
  return new Date(iso).toLocaleString();
}

// USD money formatting for pricing tables. Kept at 3-6 decimals so the
// per-second Kling rates ($0.084 style) survive rounding while a
// whole-dollar rate still renders neatly ($15.000). Callers pass an
// already-decimal USD number (backend converts price_micros / 1e6).
export function formatUSD(value: number): string {
  return `$${value.toLocaleString(undefined, {
    minimumFractionDigits: 3,
    maximumFractionDigits: 6,
  })}`;
}

// USD ↔ micros conversion. The wire format for pricing decisions uses
// int64 micros so integer arithmetic is exact; the UI works in decimal
// USD everywhere except the request body.
export function usdToMicros(usd: number): number {
  return Math.round(usd * 1_000_000);
}

export function microsToUsd(micros: number): number {
  return micros / 1_000_000;
}
