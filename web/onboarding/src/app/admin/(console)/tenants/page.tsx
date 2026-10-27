import { Suspense } from "react";

import { Badge, Card } from "@/components/ui";
import { adminApiOrLogin } from "@/lib/api/server";
import type { Tenant } from "@/lib/api/types";

async function Load() {
  const { items } = await adminApiOrLogin<{ items: Tenant[] }>("/v1/admin/tenants?limit=200");
  return (
    <Card>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[36rem] text-left text-sm">
          <caption className="sr-only">Tenants, newest first</caption>
          <thead className="text-xs uppercase tracking-wide text-zinc-500">
            <tr>
              <th scope="col" className="py-2 pr-3">
                Name
              </th>
              <th scope="col" className="py-2 pr-3">
                Slug
              </th>
              <th scope="col" className="py-2 pr-3">
                Plan
              </th>
              <th scope="col" className="py-2 pr-3">
                Region
              </th>
              <th scope="col" className="py-2 pr-3">
                Status
              </th>
              <th scope="col" className="py-2">
                Created
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
            {items.map((t) => (
              <tr key={t.id}>
                <td className="py-2 pr-3 font-medium">{t.name}</td>
                <td className="py-2 pr-3 font-mono text-xs">{t.slug}</td>
                <td className="py-2 pr-3">{t.plan}</td>
                <td className="py-2 pr-3">{t.region}</td>
                <td className="py-2 pr-3">
                  <Badge tone={t.status === "active" ? "green" : "zinc"}>{t.status}</Badge>
                </td>
                <td className="py-2">{new Date(t.created_at).toLocaleDateString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

export default function TenantsPage() {
  return (
    <>
      <h1 className="text-2xl font-semibold tracking-tight">Tenants</h1>
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading tenants…</p>}>
        <Load />
      </Suspense>
    </>
  );
}
