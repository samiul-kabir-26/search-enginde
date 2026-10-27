"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

import type { OnboardingState } from "@/lib/api/types";
import { STEP_TITLES, WIZARD_STEPS } from "@/lib/api/types";

import { cx } from "./ui";

/**
 * Wizard progress. Completed steps and the current step are links (a completed step can be
 * revisited); later steps are locked. The live region announces the step on every change.
 */
export function Stepper({ state }: { state: OnboardingState }) {
  const path = usePathname();
  const here = WIZARD_STEPS.find((s) => path === `/onboarding/${s}`);
  const index = here ? WIZARD_STEPS.indexOf(here) : -1;
  const done = new Set<string>(state.completed_steps);
  const reachable = (s: string) => done.has(s) || s === state.current_step || state.current_step === "done";

  return (
    <nav aria-label="Setup progress" className="border-b border-zinc-200 bg-white px-4 py-3 dark:border-zinc-800 dark:bg-zinc-950">
      <p aria-live="polite" className="sr-only">
        {here ? `Step ${index + 1} of ${WIZARD_STEPS.length}: ${STEP_TITLES[here]}` : ""}
      </p>
      {/* Compact progress under 768 px. */}
      <div className="md:hidden">
        <p className="text-sm font-medium">
          {here ? `Step ${index + 1} of ${WIZARD_STEPS.length}: ${STEP_TITLES[here]}` : "Setup"}
        </p>
        <div className="mt-2 h-1.5 rounded-full bg-zinc-200 dark:bg-zinc-800" aria-hidden>
          <div className="h-1.5 rounded-full bg-indigo-600" style={{ width: `${((done.size - 1) / WIZARD_STEPS.length) * 100}%` }} />
        </div>
      </div>
      <ol className="hidden flex-wrap gap-1 md:flex">
        {WIZARD_STEPS.map((s, i) => {
          const current = s === here;
          const label = (
            <>
              <span
                className={cx(
                  "flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs font-semibold",
                  done.has(s) ? "bg-indigo-600 text-white" : current ? "border-2 border-indigo-600 text-indigo-700" : "border border-zinc-300 text-zinc-500",
                )}
              >
                {done.has(s) ? <span aria-hidden>✓</span> : i + 1}
              </span>
              <span>{STEP_TITLES[s]}</span>
              {done.has(s) && <span className="sr-only">(completed)</span>}
            </>
          );
          const cls = cx(
            "flex items-center gap-2 rounded-md px-2 py-1 text-sm",
            current ? "bg-indigo-50 font-semibold text-indigo-800 dark:bg-indigo-950 dark:text-indigo-200" : "text-zinc-700 dark:text-zinc-300",
          );
          return (
            <li key={s}>
              {reachable(s) ? (
                <Link href={`/onboarding/${s}`} aria-current={current ? "step" : undefined} className={cx(cls, "hover:bg-zinc-100 dark:hover:bg-zinc-900")}>
                  {label}
                </Link>
              ) : (
                <span className={cx(cls, "opacity-60")} aria-disabled>
                  {label}
                  <span className="sr-only">(locked)</span>
                </span>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
