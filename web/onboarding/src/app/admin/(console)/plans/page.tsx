import { Suspense } from "react";

import { adminApiOrLogin } from "@/lib/api/server";
import type { Plan } from "@/lib/api/types";

import { PlanForm } from "./plan-form";

async function Load() {
  const { items } = await adminApiOrLogin<{ items: (Plan & { id: string })[] }>("/v1/admin/plans");
  return (
    <div className="flex flex-col gap-4">
      {items.map((p) => (
        <PlanForm key={p.id} plan={p} />
      ))}
    </div>
  );
}

export default function PlansPage() {
  return (
    <>
      <h1 className="text-2xl font-semibold tracking-tight">Plans</h1>
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading plans…</p>}>
        <Load />
      </Suspense>
    </>
  );
}
