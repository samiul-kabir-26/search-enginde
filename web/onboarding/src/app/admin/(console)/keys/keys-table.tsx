"use client";

import { useState } from "react";

import { revokeKey, rotateKey } from "@/app/admin/actions";
import { Badge, Button, Card, ErrorSummary } from "@/components/ui";
import type { FormState, SigningKey } from "@/lib/api/types";

export function KeysTable({ keys }: { keys: SigningKey[] }) {
  const [out, setOut] = useState<FormState | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<string | null>(null);

  async function run(id: string, fn: () => Promise<FormState>) {
    setBusy(id);
    try {
      setOut(await fn());
    } finally {
      setBusy(null);
      setConfirm(null);
    }
  }

  return (
    <Card>
      <div className="flex flex-col gap-4">
        <ErrorSummary message={out?.ok ? undefined : out?.message} errors={out?.errors} />
        <p aria-live="polite" className="text-sm text-emerald-700 dark:text-emerald-300">
          {out?.ok ? out.message : ""}
        </p>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[32rem] text-left text-sm">
            <thead className="text-xs uppercase tracking-wide text-zinc-500">
              <tr>
                <th scope="col" className="py-2 pr-3">
                  Key ID
                </th>
                <th scope="col" className="py-2 pr-3">
                  Status
                </th>
                <th scope="col" className="py-2 pr-3">
                  Created
                </th>
                <th scope="col" className="py-2">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
              {keys.map((k) => (
                <tr key={k.kid}>
                  <td className="py-2 pr-3 font-mono text-xs">{k.kid}</td>
                  <td className="py-2 pr-3">
                    <Badge tone={k.status === "active" ? "green" : k.status === "retired" ? "amber" : "zinc"}>{k.status}</Badge>
                  </td>
                  <td className="py-2 pr-3">{new Date(k.created_at).toLocaleString()}</td>
                  <td className="py-2 text-right">
                    {k.status === "retired" &&
                      (confirm === k.kid ? (
                        <span className="inline-flex gap-2">
                          <Button variant="danger" disabled={busy !== null} onClick={() => run(k.kid, () => revokeKey(k.kid))}>
                            Confirm revoke
                          </Button>
                          <Button variant="ghost" onClick={() => setConfirm(null)}>
                            Cancel
                          </Button>
                        </span>
                      ) : (
                        <Button variant="ghost" onClick={() => setConfirm(k.kid)}>
                          Revoke<span className="sr-only"> {k.kid}</span>
                        </Button>
                      ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div>
          <Button onClick={() => run("rotate", rotateKey)} disabled={busy !== null}>
            {busy === "rotate" ? "Rotating…" : "Rotate now"}
          </Button>
        </div>
      </div>
    </Card>
  );
}
