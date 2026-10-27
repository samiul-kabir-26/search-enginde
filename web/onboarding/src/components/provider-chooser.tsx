"use client";

import { useId, type ReactNode } from "react";

import { Badge, cx } from "./ui";

export type ProviderChoice = "platform" | "byo";

/**
 * Platform default vs bring-your-own for one slot. The platform default is the primary path;
 * the BYO panel only renders once chosen (docs/ONBOARDING.md §1).
 */
export function ProviderChooser({
  legend,
  value,
  onChange,
  platformLabel,
  platformDetail,
  byoLabel = "Bring your own",
  byoDetail,
  byoDisabledReason,
  children,
}: {
  legend: string;
  value: ProviderChoice;
  onChange: (v: ProviderChoice) => void;
  platformLabel: string;
  platformDetail?: ReactNode;
  byoLabel?: string;
  byoDetail?: ReactNode;
  /** When set, BYO cannot be chosen and this explains why. */
  byoDisabledReason?: string;
  /** The BYO configuration, shown when BYO is selected. */
  children?: ReactNode;
}) {
  const name = useId();
  const option = (v: ProviderChoice, label: string, detail: ReactNode, disabled = false) => (
    <label
      className={cx(
        "flex gap-3 rounded-lg border p-3 text-sm",
        disabled ? "cursor-not-allowed opacity-60" : "cursor-pointer",
        value === v ? "border-indigo-600 bg-indigo-50 dark:bg-indigo-950" : "border-zinc-200 dark:border-zinc-800",
      )}
    >
      <input type="radio" name={name} value={v} checked={value === v} disabled={disabled} onChange={() => onChange(v)} className="mt-1" />
      <span className="flex flex-col gap-0.5">
        <span className="flex items-center gap-2 font-medium">
          {label}
          {v === "platform" ? <Badge>Default</Badge> : <Badge tone="indigo">Custom</Badge>}
        </span>
        {detail && <span className="text-zinc-600 dark:text-zinc-400">{detail}</span>}
      </span>
    </label>
  );
  return (
    <fieldset className="flex flex-col gap-3">
      <legend className="mb-1 text-sm font-semibold">{legend}</legend>
      <div className="grid gap-3 sm:grid-cols-2">
        {option("platform", platformLabel, platformDetail)}
        {option("byo", byoLabel, byoDisabledReason ?? byoDetail, Boolean(byoDisabledReason))}
      </div>
      {value === "byo" && children && <div className="rounded-lg border border-dashed border-indigo-300 p-3 dark:border-indigo-800">{children}</div>}
    </fieldset>
  );
}
