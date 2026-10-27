"use client";

import Link from "next/link";
import { startTransition, useActionState, useEffect, useRef, type ReactNode } from "react";

import { saveStep, applyDefaultsForRest, type StepOutcome } from "@/app/onboarding/actions";
import type { WizardStep } from "@/lib/api/types";
import { STEP_TITLES, WIZARD_STEPS } from "@/lib/api/types";

import { Button, ErrorSummary, Warnings } from "./ui";

/**
 * The frame of every step: a heading that takes focus on step change, the step body and the
 * navigation footer.
 */
export function StepShell({ step, intro, children }: { step: WizardStep; intro: ReactNode; children: ReactNode }) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), [step]);
  const n = WIZARD_STEPS.indexOf(step) + 1;
  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-6 px-4 py-6">
      <header>
        <p className="text-xs font-semibold uppercase tracking-wide text-indigo-700 dark:text-indigo-300">
          Step {n} of {WIZARD_STEPS.length}
        </p>
        <h1 ref={heading} tabIndex={-1} className="text-2xl font-semibold tracking-tight focus:outline-none">
          {STEP_TITLES[step]}
        </h1>
        <div className="mt-1 max-w-3xl text-sm text-zinc-600 dark:text-zinc-400">{intro}</div>
      </header>
      {children}
    </div>
  );
}

/** Submits a step payload with useActionState; errors and warnings come back by JSON Pointer. */
export function useStep(step: WizardStep) {
  const [state, dispatch, pending] = useActionState((_: StepOutcome, payload: unknown) => saveStep(step, payload), { ok: false } as StepOutcome);
  const submit = (payload: unknown) => startTransition(() => dispatch(payload));
  return { state, submit, pending };
}

export function StepFooter({
  step,
  pending,
  onContinue,
  continueLabel = "Save and continue",
  skippable = false,
  disabled = false,
}: {
  step: WizardStep;
  pending: boolean;
  onContinue: () => void;
  continueLabel?: string;
  /** Offer "Use defaults for the rest" (steps after the catalog). */
  skippable?: boolean;
  disabled?: boolean;
}) {
  const prev = WIZARD_STEPS[WIZARD_STEPS.indexOf(step) - 1];
  const [skip, dispatchSkip, skipping] = useActionState(() => applyDefaultsForRest(), { ok: false } as StepOutcome);
  return (
    <div className="flex flex-col gap-3 border-t border-zinc-200 pt-4 dark:border-zinc-800">
      <ErrorSummary message={skip.message} errors={skip.errors} />
      <div className="flex flex-wrap items-center gap-3">
        {prev && (
          <Link href={`/onboarding/${prev}`} className="text-sm text-zinc-600 underline dark:text-zinc-400">
            Back to {STEP_TITLES[prev]}
          </Link>
        )}
        <div className="ml-auto flex flex-wrap gap-3">
          {skippable && (
            <Button variant="secondary" disabled={pending || skipping} onClick={() => startTransition(() => dispatchSkip())}>
              {skipping ? "Applying defaults…" : "Use defaults for the rest"}
            </Button>
          )}
          <Button onClick={onContinue} disabled={pending || skipping || disabled}>
            {pending ? "Saving…" : continueLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}

/** The outcome block shown above the footer. */
export function StepOutcomeView({ state, claimed = [] }: { state: StepOutcome; claimed?: string[] }) {
  const own = (p: string) => claimed.some((c) => p === c || p.startsWith(c + "/"));
  return (
    <>
      <ErrorSummary message={state.message} errors={state.errors?.filter((e) => !own(e.path))} />
      <Warnings warnings={state.warnings} />
    </>
  );
}
