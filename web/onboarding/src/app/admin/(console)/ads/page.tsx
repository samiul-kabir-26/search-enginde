import { Suspense } from "react";

import { adminApiOrLogin } from "@/lib/api/server";
import type { PlatformSettings } from "@/lib/api/types";

import { AdsForm } from "./ads-form";

async function Load() {
  const settings = await adminApiOrLogin<PlatformSettings>("/v1/admin/settings");
  return <AdsForm settings={settings} />;
}

export default function AdsPage() {
  return (
    <>
      <h1 className="text-2xl font-semibold tracking-tight">Ad controls</h1>
      <p className="-mt-4 text-sm text-zinc-600 dark:text-zinc-400">What tenants can change in their campaigns, how sponsored results are disclosed, and the limits every tenant&apos;s ads configuration must respect.</p>
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading settings…</p>}>
        <Load />
      </Suspense>
    </>
  );
}
