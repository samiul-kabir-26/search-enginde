"use client";

import { EngineDiagram } from "@/components/engine-diagram";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import type { EngineStage } from "@/lib/api/types";

export function EngineTourForm({ stages }: { stages: EngineStage[] }) {
  const { state, submit, pending } = useStep("engine_tour");
  return (
    <StepShell
      step="engine_tour"
      intro="Every search runs through six stages. Each one starts on a platform default, and the dashed boxes show where your own model, store or rules can plug in. Select a stage to learn more."
    >
      <EngineDiagram stages={stages} linkSteps={false} />
      <StepOutcomeView state={state} />
      <StepFooter step="engine_tour" pending={pending} onContinue={() => submit({})} continueLabel="Finish the tour" />
    </StepShell>
  );
}
