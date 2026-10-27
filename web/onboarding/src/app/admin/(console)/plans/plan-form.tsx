"use client";

import { useState } from "react";

import { savePlan } from "@/app/admin/actions";
import { Button, Card, ErrorSummary, Field } from "@/components/ui";
import type { FormState, Plan } from "@/lib/api/types";

const NUMBERS: { key: keyof Plan; label: string }[] = [
  { key: "max_total_ms", label: "Max search budget (ms)" },
  { key: "search_rps", label: "Searches per second" },
  { key: "search_burst", label: "Search burst" },
  { key: "events_rps", label: "Events per second" },
  { key: "max_custom_fields", label: "Max custom fields" },
  { key: "max_page_depth", label: "Max page depth" },
];

export function PlanForm({ plan: initial }: { plan: Plan & { id: string } }) {
  const [plan, setPlan] = useState(initial);
  const [allow, setAllow] = useState((initial.egress_allowlist ?? []).join(", "));
  const [out, setOut] = useState<FormState | null>(null);
  const [busy, setBusy] = useState(false);
  const p = (k: string) => `/${k}`;

  async function onSave() {
    setBusy(true);
    try {
      const egress = allow
        .split(/[\s,]+/)
        .map((s) => s.trim())
        .filter(Boolean);
      setOut(await savePlan({ ...plan, egress_allowlist: egress }));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title={`${plan.name} (${plan.id})`}>
      <div className="flex flex-col gap-4">
        <ErrorSummary message={out?.ok ? undefined : out?.message} errors={out?.errors} />
        <div className="grid gap-4 sm:grid-cols-3">
          <Field path={`/${plan.id}/name`} label="Name" value={plan.name} onChange={(e) => setPlan({ ...plan, name: e.target.value })} errors={out?.errors?.map((e) => ({ ...e, path: `/${plan.id}${e.path}` }))} />
          {NUMBERS.map((n) => (
            <Field
              key={n.key}
              path={`/${plan.id}${p(n.key)}`}
              label={n.label}
              type="number"
              min={0}
              value={Number(plan[n.key])}
              onChange={(e) => setPlan({ ...plan, [n.key]: Number(e.target.value) })}
              errors={out?.errors?.map((e) => ({ ...e, path: `/${plan.id}${e.path}` }))}
            />
          ))}
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={plan.byo_allowed} onChange={(e) => setPlan({ ...plan, byo_allowed: e.target.checked })} /> Bring-your-own models and stores allowed
        </label>
        <Field
          path={`/${plan.id}/egress_allowlist`}
          label="Egress host allowlist"
          value={allow}
          onChange={(e) => setAllow(e.target.value)}
          hint="Exact hosts or *.suffix, comma separated. Empty allows any public host."
        />
        <p aria-live="polite" className="text-sm text-emerald-700 dark:text-emerald-300">
          {out?.ok ? out.message : ""}
        </p>
        <div>
          <Button variant="secondary" onClick={onSave} disabled={busy}>
            {busy ? "Saving…" : `Save ${plan.name}`}
          </Button>
        </div>
      </div>
    </Card>
  );
}
