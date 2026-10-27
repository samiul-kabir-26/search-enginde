import { explainer } from "@/lib/api/data";

import { EngineTourForm } from "./engine-tour-form";

export async function EngineTourStep() {
  const e = await explainer("/onboarding/engine_tour");
  return <EngineTourForm stages={e.stages} />;
}
