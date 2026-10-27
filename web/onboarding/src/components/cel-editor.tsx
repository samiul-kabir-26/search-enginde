"use client";

import { useState } from "react";

import { testPolicies } from "@/app/onboarding/actions";
import type { FieldError, PolicyTestResponse } from "@/lib/api/types";
import { errorAt, errorsUnder, fieldId } from "@/lib/forms";

import { Badge, Button, ErrorSummary, Select, TextArea, cx } from "./ui";

export type Rule = { id: string; kind: "filter" | "boost"; expr: string };

const EXAMPLES: { label: string; rule: Omit<Rule, "id"> }[] = [
  { label: "Hide products under 4 stars", rule: { kind: "filter", expr: "!has(item.rating) || item.rating >= 4.0" } },
  { label: "Boost fast-dispatching sellers", rule: { kind: "boost", expr: "item.vendor.on_time_dispatch_rate > 0.95 ? 0.2 : 0.0" } },
  { label: "Only one brand for a branded query", rule: { kind: "filter", expr: "!query.text.contains('acme') || item.brand == 'Acme'" } },
];

const REFERENCE =
  "item: product_id, vendor_id, brand, price_minor, category_ids, rating, rating_count, listing_completeness, image_count, created_at, stock, status, ship_regions, " +
  "vendor.{score, on_time_dispatch_rate, dispute_rate, response_hours, verified}, custom.<field>. query: text, type, categories, attributes. " +
  "Optional fields may be absent: test with has(item.rating).";

/**
 * Edits the tenant's CEL rules. Server compile errors arrive at /policy/rules/{i}/{field};
 * "Test against sample products" evaluates each rule on the catalog samples without saving.
 */
export function CelEditor({
  rules,
  onChange,
  errors,
  samples,
}: {
  rules: Rule[];
  onChange: (rules: Rule[]) => void;
  errors?: FieldError[];
  samples: unknown[];
}) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<{ data?: PolicyTestResponse; message?: string; errors?: FieldError[] } | null>(null);

  const update = (i: number, patch: Partial<Rule>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const nextId = () => {
    let n = rules.length + 1;
    while (rules.some((r) => r.id === `rule-${n}`)) n++;
    return `rule-${n}`;
  };
  const add = (rule: Omit<Rule, "id">) => onChange([...rules, { id: nextId(), ...rule }]);

  async function runTest() {
    setTesting(true);
    try {
      const res = await testPolicies(rules, samples);
      setResult(res.ok ? { data: res.data } : { message: res.message, errors: res.errors });
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <p className="text-xs text-zinc-600 dark:text-zinc-400">{REFERENCE}</p>
      {rules.length === 0 && <p className="text-sm text-zinc-500">No custom rules yet.</p>}
      <ol className="flex flex-col gap-3">
        {rules.map((r, i) => {
          const base = `/policy/rules/${i}`;
          const exprErr = errorAt(errors, `${base}/expr`);
          const other = errorsUnder(errors, base).filter((e) => e.path !== `${base}/expr`);
          const tested = result?.data?.rules[i];
          return (
            <li key={i} className="rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
              <div className="grid gap-3 sm:grid-cols-[1fr_9rem_auto]">
                <div className="flex flex-col gap-1">
                  <label htmlFor={fieldId(`${base}/id`)} className="text-xs font-medium">
                    Rule ID
                  </label>
                  <input
                    id={fieldId(`${base}/id`)}
                    value={r.id}
                    onChange={(e) => update(i, { id: e.target.value })}
                    className="rounded border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label htmlFor={fieldId(`${base}/kind`)} className="text-xs font-medium">
                    Kind
                  </label>
                  <Select id={fieldId(`${base}/kind`)} value={r.kind} onChange={(e) => update(i, { kind: e.target.value as Rule["kind"] })} className="py-1 text-xs">
                    <option value="filter">Filter (hide when false)</option>
                    <option value="boost">Boost (adds −1…1)</option>
                  </Select>
                </div>
                <Button variant="ghost" className="self-end" onClick={() => onChange(rules.filter((_, j) => j !== i))}>
                  Remove<span className="sr-only"> rule {r.id}</span>
                </Button>
              </div>
              <div className="mt-2 flex flex-col gap-1">
                <label htmlFor={fieldId(`${base}/expr`)} className="text-xs font-medium">
                  CEL expression
                </label>
                <TextArea
                  id={fieldId(`${base}/expr`)}
                  rows={2}
                  spellCheck={false}
                  value={r.expr}
                  onChange={(e) => update(i, { expr: e.target.value })}
                  aria-invalid={Boolean(exprErr)}
                  aria-describedby={exprErr ? `${fieldId(`${base}/expr`)}-error` : undefined}
                  placeholder={r.kind === "filter" ? "item.stock > 0" : "item.rating >= 4.5 ? 0.3 : 0.0"}
                  className="text-xs"
                />
                {exprErr && (
                  <p id={`${fieldId(`${base}/expr`)}-error`} className="text-xs text-red-600">
                    {exprErr}
                  </p>
                )}
                {other.map((e, k) => (
                  <p key={k} className="text-xs text-red-600">
                    {e.message}
                  </p>
                ))}
              </div>
              {tested && <RuleResults rule={tested} />}
            </li>
          );
        })}
      </ol>
      <div className="flex flex-wrap gap-2">
        <Button variant="secondary" onClick={() => add({ kind: "filter", expr: "" })}>
          Add a rule
        </Button>
        {EXAMPLES.map((ex) => (
          <Button key={ex.label} variant="ghost" onClick={() => add(ex.rule)}>
            + {ex.label}
          </Button>
        ))}
      </div>
      <div className="flex flex-col gap-2 border-t border-zinc-200 pt-3 dark:border-zinc-800">
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="secondary" onClick={runTest} disabled={testing || rules.length === 0 || samples.length === 0}>
            {testing ? "Testing…" : "Test against sample products"}
          </Button>
          <span className="text-xs text-zinc-500">
            {samples.length > 0 ? `${samples.length} sample product(s) from the catalog step.` : "Add sample products in the catalog step to test rules."}
          </span>
        </div>
        <div aria-live="polite">
          <ErrorSummary message={result?.message} errors={result?.errors} />
          {result?.data && <p className="text-sm text-zinc-600 dark:text-zinc-400">Results are shown under each rule.</p>}
        </div>
      </div>
    </div>
  );
}

function RuleResults({ rule }: { rule: PolicyTestResponse["rules"][number] }) {
  if (rule.errors?.length) {
    return (
      <ul className="mt-2 list-disc pl-5 text-xs text-red-600">
        {rule.errors.map((e, i) => (
          <li key={i}>{e.message}</li>
        ))}
      </ul>
    );
  }
  const kept = rule.results.filter((r) => r.keep !== false && !r.mapping_error).length;
  return (
    <div className="mt-2">
      <p className="text-xs font-medium">
        {rule.kind === "filter" ? `Keeps ${kept} of ${rule.results.length} samples` : `Boosts on ${rule.results.length} samples`}
      </p>
      <ul className="mt-1 flex flex-wrap gap-1">
        {rule.results.map((r, i) => (
          <li key={`${r.product_id}-${i}`}>
            <span className={cx("inline-flex items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-[11px]", "border-zinc-200 dark:border-zinc-700")}>
              {r.product_id || `sample ${i + 1}`}
              {r.mapping_error ? (
                <Badge tone="amber">unmapped</Badge>
              ) : r.failed ? (
                <Badge tone="amber">error: kept</Badge>
              ) : rule.kind === "filter" ? (
                <Badge tone={r.keep ? "green" : "red"}>{r.keep ? "kept" : "hidden"}</Badge>
              ) : (
                <Badge tone="indigo">{(r.boost ?? 0) >= 0 ? "+" : ""}{(r.boost ?? 0).toFixed(2)}</Badge>
              )}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}
