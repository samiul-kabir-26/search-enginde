"use client";

import { PlaygroundSearch } from "@/components/playground-search";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";

export function PlaygroundForm({ versions }: { versions: number[] }) {
  const { state, submit, pending } = useStep("playground");
  return (
    <StepShell
      step="playground"
      intro="Search your new version before shoppers do. The explain trace shows what each stage did: candidates in and out, scores, and any fallbacks. Compare it side by side with the draft or another version, then adjust retrieval or ranking and rebuild."
    >
      <PlaygroundSearch versions={versions} />
      <StepOutcomeView state={state} />
      <StepFooter step="playground" pending={pending} onContinue={() => submit({})} continueLabel="Looks good: go live" />
    </StepShell>
  );
}
