import type { Metadata } from "next";
import { notFound, redirect } from "next/navigation";
import { Suspense } from "react";

import { onboardingState } from "@/lib/api/data";
import { STEP_TITLES, isWizardStep, type WizardStep } from "@/lib/api/types";

import { CatalogStep } from "../_steps/catalog";
import { EngineTourStep } from "../_steps/engine-tour";
import { GoLiveStep } from "../_steps/go-live";
import { IndexBuildStep } from "../_steps/index-build";
import { PersonalizationStep } from "../_steps/personalization";
import { PlaygroundStep } from "../_steps/playground";
import { PoliciesStep } from "../_steps/policies";
import { RankingStep } from "../_steps/ranking";
import { RetrievalStep } from "../_steps/retrieval";

export async function generateMetadata({ params }: PageProps<"/onboarding/[step]">): Promise<Metadata> {
  const { step } = await params;
  return { title: `${isWizardStep(step) ? STEP_TITLES[step] : "Setup"} · Search Engine` };
}

const STEPS: Record<WizardStep, () => Promise<React.ReactNode>> = {
  engine_tour: EngineTourStep,
  catalog: CatalogStep,
  retrieval: RetrievalStep,
  policies: PoliciesStep,
  ranking: RankingStep,
  personalization: PersonalizationStep,
  index_build: IndexBuildStep,
  playground: PlaygroundStep,
  go_live: GoLiveStep,
};

/** The step guard: a step that is neither completed nor current redirects to the current step. */
async function Step({ params }: { params: PageProps<"/onboarding/[step]">["params"] }) {
  const { step } = await params;
  if (!isWizardStep(step)) notFound();
  const state = await onboardingState(`/onboarding/${step}`);
  const open = state.current_step === "done" || state.current_step === step || state.completed_steps.includes(step);
  if (!open) redirect(isWizardStep(state.current_step) ? `/onboarding/${state.current_step}` : "/onboarding/go_live");
  return STEPS[step]();
}

export default function StepPage(props: PageProps<"/onboarding/[step]">) {
  return (
    <Suspense fallback={<p className="mx-auto w-full max-w-5xl px-4 py-6 text-sm text-zinc-500">Loading step…</p>}>
      <Step params={props.params} />
    </Suspense>
  );
}
