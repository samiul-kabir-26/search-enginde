import { versions } from "@/lib/api/data";

import { PlaygroundForm } from "./playground-form";

export async function PlaygroundStep() {
  const vs = await versions("/onboarding/playground");
  return <PlaygroundForm versions={vs.map((v) => v.version).sort((a, b) => b - a)} />;
}
