import { cookies } from "next/headers";

import { indexJobs } from "@/lib/api/data";
import { UPLOAD } from "@/lib/api/server";

import { IndexBuildForm } from "./index-build-form";

export async function IndexBuildStep() {
  const jobs = await indexJobs("/onboarding/index_build");
  const latest = [...jobs].sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
  const hasUpload = Boolean((await cookies()).get(UPLOAD)?.value);
  return <IndexBuildForm job={latest} hasUpload={hasUpload} />;
}
