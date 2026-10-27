"use client";

import Link from "next/link";
import { useEffect, useRef } from "react";

import type { EngineStage } from "@/lib/api/types";
import { STEP_TITLES, isWizardStep } from "@/lib/api/types";

import { Badge } from "./ui";

/** The side panel for one engine stage: what it does, what is configurable, the current choice. */
export function StagePanel({ stage, onClose, linkStep = true }: { stage: EngineStage; onClose: () => void; linkStep?: boolean }) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), [stage.id]);
  return (
    <aside className="flex flex-col gap-3 rounded-lg border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950" aria-labelledby={`stage-${stage.id}-title`}>
      <div className="flex items-start justify-between gap-2">
        <h3 id={`stage-${stage.id}-title`} ref={heading} tabIndex={-1} className="text-base font-semibold focus:outline-none">
          {stage.number}. {stage.title}
        </h3>
        <button type="button" onClick={onClose} className="rounded px-2 text-sm text-zinc-500 hover:text-zinc-900 focus-visible:ring-2 focus-visible:ring-indigo-500 dark:hover:text-zinc-100">
          Close<span className="sr-only"> {stage.title} details</span>
        </button>
      </div>
      <p className="text-sm text-zinc-700 dark:text-zinc-300">{stage.summary}</p>
      <div>
        <h4 className="text-xs font-semibold uppercase tracking-wide text-zinc-500">You can configure</h4>
        <ul className="mt-1 list-disc pl-5 text-sm">
          {stage.configurable.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
      </div>
      <dl className="grid gap-2 text-sm">
        <div>
          <dt className="text-xs font-semibold uppercase tracking-wide text-zinc-500">Platform default</dt>
          <dd>{stage.default}</dd>
        </div>
        <div>
          <dt className="text-xs font-semibold uppercase tracking-wide text-zinc-500">Plugs in</dt>
          <dd className="flex items-center gap-2">
            {stage.injection}
            {stage.badge && <Badge tone={stage.badge === "Custom" ? "indigo" : "zinc"}>{stage.badge}</Badge>}
          </dd>
        </div>
        {stage.choice && (
          <div>
            <dt className="text-xs font-semibold uppercase tracking-wide text-zinc-500">Your current choice</dt>
            <dd>{stage.choice}</dd>
          </div>
        )}
      </dl>
      {linkStep && isWizardStep(stage.step) && (
        <Link href={`/onboarding/${stage.step}`} className="text-sm font-medium text-indigo-700 underline dark:text-indigo-300">
          Configure in the {STEP_TITLES[stage.step]} step
        </Link>
      )}
    </aside>
  );
}
