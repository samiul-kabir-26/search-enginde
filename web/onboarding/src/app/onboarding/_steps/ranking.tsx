import { draft, models, platformModels } from "@/lib/api/data";

import { RankingForm } from "./ranking-form";

export async function RankingStep() {
  const next = "/onboarding/ranking";
  const [{ config }, mine, platform] = await Promise.all([draft(next), models(next), platformModels(next)]);
  const scorers = platform.filter((m) => m.kind === "scorer");
  const rank = config.rank?.scorer;
  const prerank = config.prerank?.scorer;
  const mode = rank?.model_id || prerank?.model_id ? "byo" : rank?.model || prerank?.model ? "learned" : "default";
  return (
    <RankingForm
      mode={mode}
      weights={rank?.type === "default_linear" ? (rank.weights ?? {}) : {}}
      learnedAvailable={scorers.length > 0 && scorers.every((m) => m.available)}
      prerankFeatures={scorers.find((m) => m.id === "prerank-gbdt-v1")?.features ?? []}
      rankFeatures={scorers.find((m) => m.id === "rank-gbdt-v1")?.features ?? []}
      prerankModelId={prerank?.model_id}
      rankModelId={rank?.model_id}
      scorerModels={mine.filter((m) => (m.kind === "prerank" || m.kind === "rank") && m.status === "validated")}
    />
  );
}
