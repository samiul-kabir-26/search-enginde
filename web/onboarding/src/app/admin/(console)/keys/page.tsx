import { Suspense } from "react";

import { adminApiOrLogin } from "@/lib/api/server";
import type { SigningKey } from "@/lib/api/types";

import { KeysTable } from "./keys-table";

async function Load() {
  const { items } = await adminApiOrLogin<{ items: SigningKey[] }>("/v1/admin/keys");
  return <KeysTable keys={items} />;
}

export default function KeysPage() {
  return (
    <>
      <h1 className="text-2xl font-semibold tracking-tight">Signing keys</h1>
      <p className="-mt-4 text-sm text-zinc-600 dark:text-zinc-400">
        Access tokens are signed with the active key. Rotating retires it, and it stays published until every token it signed has expired. Revoking a key makes its tokens stop verifying at once.
      </p>
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading keys…</p>}>
        <Load />
      </Suspense>
    </>
  );
}
