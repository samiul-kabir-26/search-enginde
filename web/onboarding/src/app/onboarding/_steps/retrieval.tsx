import { connections, draft, models, platformModels } from "@/lib/api/data";
import { presetOf } from "@/lib/config";

import { RetrievalForm } from "./retrieval-form";

export async function RetrievalStep() {
  const next = "/onboarding/retrieval";
  const [{ config }, mine, conns, platform] = await Promise.all([draft(next), models(next), connections(next), platformModels(next)]);
  const dim = (id: string | undefined, fallback: number) => platform.find((m) => m.id === id)?.dim ?? fallback;
  return (
    <RetrievalForm
      preset={presetOf(config)}
      embedders={config.embedders}
      store={config.stores?.vector}
      embedderModels={mine.filter((m) => m.kind === "embedder" && m.status === "validated")}
      connections={conns.filter((c) => c.status === "validated")}
      platformDims={{ text: dim("bge-small-en-v1.5", 384), image: dim("clip-vit-b32", 512) }}
    />
  );
}
