import { Suspense } from "react";

import { adminApiOrLogin } from "@/lib/api/server";
import type { PlatformSettings } from "@/lib/api/types";

import { SettingsForm } from "./settings-form";

async function Load() {
  const settings = await adminApiOrLogin<PlatformSettings>("/v1/admin/settings");
  return <SettingsForm settings={settings} />;
}

export default function SettingsPage() {
  return (
    <>
      <h1 className="text-2xl font-semibold tracking-tight">Platform settings</h1>
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading settings…</p>}>
        <Load />
      </Suspense>
    </>
  );
}
