import { versions } from "@/lib/api/data";
import { CONTROL_API_URL } from "@/lib/api/server";

import { GoLiveForm } from "./go-live-form";

export async function GoLiveStep() {
  const vs = await versions("/onboarding/go_live");
  const sorted = [...vs].sort((a, b) => b.version - a.version);
  return (
    <GoLiveForm
      latest={sorted[0]?.version}
      active={sorted.find((v) => v.state === "active")?.version}
      controlUrl={process.env.PUBLIC_CONTROL_API_URL ?? CONTROL_API_URL}
      searchUrl={process.env.PUBLIC_SEARCH_API_URL ?? "http://localhost:8080"}
    />
  );
}
