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

// MOCK: remove when backend returns time-series health data
// Generates deterministic mock slot data based on the model JST string.
// Produces mostly green slots with occasional yellow/red for realism.
export function generateMockSlots(jst: string, count: number): UptimeSlot[] {
  // Simple deterministic hash seeded from the JST string
  let hash = 0;
  for (let i = 0; i < jst.length; i++) {
    hash = (hash * 31 + jst.charCodeAt(i)) | 0;
  }

  const now = Date.now();
  const slotDuration = count <= 24 ? 60 * 60 * 1000 : 24 * 60 * 60 * 1000; // 1h or 1d

  const slots: UptimeSlot[] = [];
  for (let i = count - 1; i >= 0; i--) {
    const time = new Date(now - i * slotDuration).toISOString();
    // Use hash to get a pseudo-random value per slot
    hash = (hash * 1103515245 + 12345) | 0;
    const rand = Math.abs(hash) % 100;

    let total = 10;
    let passed: number;
    if (rand < 75) {
      // 75% chance: all probes pass
      passed = total;
    } else if (rand < 90) {
      // 15% chance: mostly pass (80-99%)
      passed = 8 + (Math.abs(hash >> 8) % 2);
    } else if (rand < 97) {
      // 7% chance: partial failures
      passed = 5 + (Math.abs(hash >> 4) % 3);
    } else {
      // 3% chance: severe failure
      passed = Math.abs(hash >> 6) % 4;
    }

    slots.push({ time, total, passed });
  }

  return slots;
}
