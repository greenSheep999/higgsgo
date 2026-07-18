import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

export interface UptimeSlot {
  time: string; // ISO timestamp of the slot start
  total: number; // total probes in this slot
  passed: number; // successful probes
}

export interface UptimeBarProps {
  slots: UptimeSlot[];
  size?: "mini" | "sm" | "md"; // mini for table cell (few blocks), sm for compact, md for sheet (stretch full width)
}

function slotColor(slot: UptimeSlot): string {
  if (slot.total === 0) return "bg-muted";
  const ratio = slot.passed / slot.total;
  if (ratio >= 1) return "bg-green-500";
  if (ratio >= 0.8) return "bg-yellow-500";
  return "bg-red-500";
}

function formatSlotTime(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return iso;
  }
}

function formatSlotRate(slot: UptimeSlot): string {
  if (slot.total === 0) return "No data";
  const pct = ((slot.passed / slot.total) * 100).toFixed(1);
  return `${pct}% (${slot.passed}/${slot.total})`;
}

export function UptimeBar({ slots, size = "sm" }: UptimeBarProps) {
  const isMd = size === "md";
  const isMini = size === "mini";

  return (
    <div
      className={`flex items-center ${isMd ? "w-full gap-0.5" : isMini ? "gap-px" : "gap-px"}`}
      role="img"
      aria-label="Uptime status bar"
    >
      {slots.map((slot, i) => (
        <Tooltip key={i}>
          <TooltipTrigger asChild>
            <div
              className={`rounded-[1px] ${slotColor(slot)} ${isMd ? "h-5 flex-1" : isMini ? "h-3 w-[3px] shrink-0" : "h-4 w-1 shrink-0"}`}
            />
          </TooltipTrigger>
          <TooltipContent side="top" className="text-xs">
            <div className="font-medium">{formatSlotTime(slot.time)}</div>
            <div className="text-muted-foreground">
              {formatSlotRate(slot)}
            </div>
          </TooltipContent>
        </Tooltip>
      ))}
    </div>
  );
}

// generateEmptySlots returns `count` slots, each with total=0. The
// UptimeBar renders these as muted-gray blocks with a "No data" tooltip
// so the operator sees a real "we haven't probed this model yet" state
// instead of a fabricated pseudo-random line.
//
// Time stamps are still spread across the requested window so the bar
// visually anchors to a plausible time axis; a caller that wants to
// display an explicit "no data" ribbon should hide the bar entirely
// instead. See docs/ROADMAP.md P2-7 for why the previous mock
// implementation was removed.
export function generateEmptySlots(count: number): UptimeSlot[] {
  const now = Date.now();
  const slotDuration = count <= 24 ? 60 * 60 * 1000 : 24 * 60 * 60 * 1000;
  const slots: UptimeSlot[] = [];
  for (let i = count - 1; i >= 0; i--) {
    slots.push({
      time: new Date(now - i * slotDuration).toISOString(),
      total: 0,
      passed: 0,
    });
  }
  return slots;
}
