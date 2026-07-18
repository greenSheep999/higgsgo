import { useState } from "react";
import { IconCalendar } from "@tabler/icons-react";
import { format } from "date-fns";
import type { DateRange } from "react-day-picker";
import {
  ToggleGroup,
  ToggleGroupItem,
} from "@/components/ui/toggle-group";
import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  customWindow,
  presetWindow,
  type TimeWindow,
  type WindowPreset,
} from "@/lib/time-window";

// TimeWindowPicker is a toolbar with four preset chips (24h/7d/15d/30d)
// plus a popover calendar for custom ranges. It owns no state beyond the
// popover open flag — the parent component holds the active TimeWindow.

const PRESETS: WindowPreset[] = ["24h", "7d", "15d", "30d"];

interface Props {
  value: TimeWindow;
  onChange: (w: TimeWindow) => void;
  activePreset: WindowPreset | "custom";
  onPresetChange: (p: WindowPreset | "custom") => void;
}

export function TimeWindowPicker({
  value,
  onChange,
  activePreset,
  onPresetChange,
}: Props) {
  const [range, setRange] = useState<DateRange | undefined>();

  return (
    <div className="flex flex-wrap items-center gap-2">
      <ToggleGroup
        type="single"
        size="sm"
        variant="outline"
        value={activePreset === "custom" ? "" : activePreset}
        onValueChange={(v) => {
          if (!v) return;
          const p = v as WindowPreset;
          onPresetChange(p);
          onChange(presetWindow(p));
        }}
      >
        {PRESETS.map((p) => (
          <ToggleGroupItem key={p} value={p}>
            {p}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>

      <Popover>
        <PopoverTrigger asChild>
          <Button
            size="sm"
            variant={activePreset === "custom" ? "default" : "outline"}
          >
            <IconCalendar />
            {activePreset === "custom"
              ? `${format(new Date(value.since), "MMM d")} – ${format(
                  new Date(value.until),
                  "MMM d",
                )}`
              : "Custom"}
          </Button>
        </PopoverTrigger>
        <PopoverContent align="end" className="w-auto p-0">
          <Calendar
            mode="range"
            selected={range}
            onSelect={(r) => {
              setRange(r);
              if (r?.from && r?.to) {
                onPresetChange("custom");
                onChange(customWindow(r.from, r.to));
              }
            }}
            numberOfMonths={2}
          />
        </PopoverContent>
      </Popover>
    </div>
  );
}
