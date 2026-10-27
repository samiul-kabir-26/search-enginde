import { draft } from "@/lib/api/data";

import { PersonalizationForm } from "./personalization-form";

export async function PersonalizationStep() {
  const { config } = await draft("/onboarding/personalization");
  return (
    <PersonalizationForm
      personalization={config.rerank?.personalization ?? { enabled: true, alpha: 0.15, beta: 0.05 }}
      diversity={config.rerank?.diversity ?? { max_per_vendor: 2, window: 10 }}
      ads={config.ads ?? { enabled: false, slots: [1, 5, 10], relevance_floor: 0.35 }}
      rankKeep={config.rank?.keep ?? 100}
    />
  );
}
