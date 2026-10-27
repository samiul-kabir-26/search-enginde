"use client";

import type { FieldError } from "@/lib/api/types";
import { RANK_FEATURES } from "@/lib/config";
import { fieldId } from "@/lib/forms";

/** default_linear rank weights, each in [0, 5]. Server errors arrive at …/weights/{feature}. */
export function WeightSliders({
  weights,
  onChange,
  errors,
}: {
  weights: Record<string, number>;
  onChange: (w: Record<string, number>) => void;
  errors?: FieldError[];
}) {
  return (
    <fieldset className="grid gap-x-6 gap-y-3 sm:grid-cols-2">
      <legend className="mb-2 text-sm font-semibold">Feature weights</legend>
      {RANK_FEATURES.map((f) => {
        const id = fieldId(`/weights/${f.name}`);
        const v = weights[f.name] ?? 0;
        const err = errors?.find((e) => e.path.endsWith(`/weights/${f.name}`))?.message;
        return (
          <div key={f.name} className="flex flex-col gap-1">
            <div className="flex items-baseline justify-between gap-2">
              <label htmlFor={id} className="text-sm">
                {f.label} <code className="text-xs text-zinc-500">{f.name}</code>
              </label>
              <output htmlFor={id} className="font-mono text-sm tabular-nums">
                {v.toFixed(1)}
              </output>
            </div>
            <input
              id={id}
              type="range"
              min={0}
              max={5}
              step={0.1}
              value={v}
              aria-invalid={Boolean(err)}
              aria-valuetext={`${v.toFixed(1)} of 5`}
              onChange={(e) => onChange({ ...weights, [f.name]: Number(e.target.value) })}
              className="accent-indigo-600"
            />
            {err && <p className="text-xs text-red-600">{err}</p>}
          </div>
        );
      })}
    </fieldset>
  );
}
