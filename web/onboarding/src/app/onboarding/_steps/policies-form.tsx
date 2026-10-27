"use client";

import { useState } from "react";

import { CelEditor, type Rule } from "@/components/cel-editor";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import { Card, Field } from "@/components/ui";
import { useSamples } from "@/lib/samples";

export function PoliciesForm({
  inStock: initialInStock,
  shipToRegion: initialRegion,
  verifiedVendors: initialVerified,
  rules: initialRules,
}: {
  inStock: boolean;
  shipToRegion: string;
  verifiedVendors: boolean;
  rules: { id: string; kind: string; expr: string }[];
}) {
  const { state, submit, pending } = useStep("policies");
  const [inStock, setInStock] = useState(initialInStock);
  const [region, setRegion] = useState(initialRegion);
  const [verified, setVerified] = useState(initialVerified);
  const [rules, setRules] = useState<Rule[]>(initialRules.map((r) => ({ ...r, kind: r.kind === "boost" ? "boost" : "filter" })));
  const samples = useSamples();

  const onContinue = () =>
    submit({
      in_stock: inStock,
      ship_to_region: region.trim() || undefined,
      verified_vendors: verified,
      rules: rules.map((r) => ({ id: r.id.trim(), kind: r.kind, expr: r.expr.trim() })),
    });

  return (
    <StepShell
      step="policies"
      intro="Rules decide which products may appear and which get a lift. Templates cover the common cases; custom rules use CEL and are compiled and cost-checked on save, so a typo never reaches your shoppers."
    >
      <Card title="Always-on filters">
        <div className="flex flex-col gap-3">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={inStock} onChange={(e) => setInStock(e.target.checked)} /> In-stock products only
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} /> Verified vendors only
          </label>
          <div className="max-w-xs">
            <Field
              path="/ship_to_region"
              label="Only products that ship to"
              value={region}
              onChange={(e) => setRegion(e.target.value)}
              placeholder="e.g. US"
              hint="Leave empty to show products for every region."
              errors={state.errors}
            />
          </div>
        </div>
      </Card>
      <Card title="Custom rules">
        <CelEditor rules={rules} onChange={setRules} errors={state.errors} samples={samples} />
      </Card>
      <StepOutcomeView state={state} claimed={["/policy/rules", "/ship_to_region"]} />
      <StepFooter step="policies" pending={pending} onContinue={onContinue} skippable />
    </StepShell>
  );
}
