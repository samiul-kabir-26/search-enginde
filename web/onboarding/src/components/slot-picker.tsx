"use client";

import { cx } from "./ui";

/** Sponsored slot positions on the results page: toggle buttons over the first `max` positions. */
export function SlotPicker({ slots, onChange, max = 20, limit, error }: { slots: number[]; onChange: (s: number[]) => void; max?: number; limit?: number; error?: string }) {
  const toggle = (n: number) => onChange(slots.includes(n) ? slots.filter((s) => s !== n) : [...slots, n].sort((a, b) => a - b));
  return (
    <fieldset aria-describedby="slot-picker-help" className="flex flex-col gap-2">
      <legend className="text-sm font-medium">Sponsored positions</legend>
      <p id="slot-picker-help" className="text-xs text-zinc-500">
        Select where ads may appear in the result list{limit ? ` (at most ${limit})` : ""}. Selected: {slots.length > 0 ? slots.join(", ") : "none"}.
      </p>
      <div className="grid grid-cols-5 gap-2 sm:grid-cols-10">
        {Array.from({ length: max }, (_, i) => i + 1).map((n) => {
          const on = slots.includes(n);
          return (
            <button
              key={n}
              type="button"
              aria-pressed={on}
              aria-label={`Position ${n}`}
              disabled={!on && limit !== undefined && slots.length >= limit}
              onClick={() => toggle(n)}
              className={cx(
                "h-10 rounded-md border text-sm font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-indigo-500 disabled:opacity-40",
                on ? "border-amber-500 bg-amber-100 text-amber-900 dark:bg-amber-900 dark:text-amber-100" : "border-zinc-300 dark:border-zinc-700",
              )}
            >
              {n}
            </button>
          );
        })}
      </div>
      {error && <p className="text-xs text-red-600">{error}</p>}
    </fieldset>
  );
}
