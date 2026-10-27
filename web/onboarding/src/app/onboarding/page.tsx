import { redirect } from "next/navigation";
import { Suspense } from "react";

import { onboardingState } from "@/lib/api/data";
import { isWizardStep } from "@/lib/api/types";

async function ToCurrentStep(): Promise<null> {
  const state = await onboardingState("/onboarding");
  redirect(isWizardStep(state.current_step) ? `/onboarding/${state.current_step}` : "/onboarding/go_live");
}

export default function OnboardingIndex() {
  return (
    <Suspense fallback={<p className="p-6 text-sm text-zinc-500">Loading your setup…</p>}>
      <ToCurrentStep />
    </Suspense>
  );
}
