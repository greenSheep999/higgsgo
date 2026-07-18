import * as React from "react";
import { cn } from "@/lib/utils";

// StatusBadge is a thin colour-coded wrapper on top of the shadcn Badge
// primitive. shadcn's own Badge variants ({default, secondary,
// destructive, outline}) all draw from a single primary/secondary/
// destructive palette — enough for actions, not enough for the semantic
// pool state (active / paused / banned / warning / info) we surface on
// the dashboard.
//
// Rather than fork the Badge primitive, we spell out the semantic tones
// here using Tailwind + CSS variables that already exist on the theme.
// This keeps shadcn's file untouched (upgrades stay painless) and gives
// every status a stable colour that reads correctly in dark mode.

export type StatusTone =
  | "success" // active, healthy
  | "warning" // suspended, quota pressure
  | "danger" // banned, failed
  | "info" // playground scopes, cheap tier
  | "muted" // none, unknown
  | "brand"; // primary highlight — e.g. unlim

const TONE_CLASSES: Record<StatusTone, string> = {
  success:
    "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
  warning:
    "border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300",
  danger:
    "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-300",
  info:
    "border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300",
  muted:
    "border-border bg-muted text-muted-foreground",
  brand:
    "border-primary/40 bg-primary/10 text-primary",
};

interface Props extends React.HTMLAttributes<HTMLSpanElement> {
  tone: StatusTone;
}

export function StatusBadge({ tone, className, ...props }: Props) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs font-medium",
        TONE_CLASSES[tone],
        className,
      )}
      {...props}
    />
  );
}

// Convenience: map an Account.status string to a StatusBadge tone.
export function accountStatusTone(status: string): StatusTone {
  switch (status) {
    case "active":
      return "success";
    case "suspended":
      return "warning";
    case "banned":
      return "danger";
    default:
      return "muted";
  }
}

// Convenience: map an APIKey.status string to a StatusBadge tone.
export function keyStatusTone(status: string): StatusTone {
  switch (status) {
    case "active":
      return "success";
    case "paused":
      return "warning";
    case "revoked":
      return "danger";
    default:
      return "muted";
  }
}

// Convenience: map a Group.status string to a StatusBadge tone. Groups
// currently support "active" / "paused" — keep parity with keys so
// the eye reads both tables the same way.
export function groupStatusTone(status: string): StatusTone {
  switch (status) {
    case "active":
      return "success";
    case "paused":
      return "warning";
    default:
      return "muted";
  }
}

// Convenience: playground_scope tone (none/cheap/full).
export function playgroundScopeTone(scope: string): StatusTone {
  switch (scope) {
    case "full":
      return "brand";
    case "cheap":
      return "info";
    case "none":
    default:
      return "muted";
  }
}
