"use client";

import { useEffect, useId, useMemo, useState } from "react";

import { previewMapping } from "@/app/onboarding/actions";
import type { FieldError, MappingPreviewResponse } from "@/lib/api/types";
import { CANONICAL_FIELDS } from "@/lib/config";
import { fieldId } from "@/lib/forms";

import { Badge, Select, cx } from "./ui";

/** JSONPaths found in a sample document: objects by key, arrays of objects as [*]. */
export function discoverPaths(doc: unknown, prefix = "$", out: string[] = []): string[] {
  if (Array.isArray(doc)) {
    const first = doc.find((x) => x !== null && x !== undefined);
    if (first && typeof first === "object" && !Array.isArray(first)) discoverPaths(first, `${prefix}[*]`, out);
    else out.push(prefix);
    return out;
  }
  if (doc && typeof doc === "object") {
    for (const [k, v] of Object.entries(doc as Record<string, unknown>)) {
      const key = /^[A-Za-z_][A-Za-z0-9_]*$/.test(k) ? `${prefix}.${k}` : `${prefix}['${k.replace(/'/g, "\\'")}']`;
      if (v && typeof v === "object" && !Array.isArray(v) && Object.keys(v).length > 0) {
        out.push(key);
        discoverPaths(v, key, out);
      } else if (Array.isArray(v)) {
        out.push(key);
        const first = v.find((x) => x !== null && x !== undefined);
        if (first && typeof first === "object" && !Array.isArray(first)) discoverPaths(first, `${key}[*]`, out);
      } else {
        out.push(key);
      }
    }
  }
  return out;
}

/** Server errors for a mapped field arrive at /field_mapping/x or /catalog/field_mapping/x. */
function mappingError(errors: FieldError[] | undefined, name: string): string | undefined {
  return errors?.find((e) => e.path === `/field_mapping/${name}` || e.path === `/catalog/field_mapping/${name}`)?.message;
}

export function JsonPathMapper({
  mapping,
  onChange,
  samples,
  errors,
}: {
  mapping: Record<string, string>;
  onChange: (m: Record<string, string>) => void;
  samples: unknown[];
  errors?: FieldError[];
}) {
  const listId = useId();
  const paths = useMemo(() => (samples[0] ? discoverPaths(samples[0]) : []), [samples]);
  const [sampleIdx, setSampleIdx] = useState(0);
  const [preview, setPreview] = useState<MappingPreviewResponse | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // Live preview: the server maps the samples with the current mapping, debounced.
  useEffect(() => {
    if (samples.length === 0) return;
    const m = Object.fromEntries(Object.entries(mapping).filter(([, v]) => v.trim()));
    const t = setTimeout(async () => {
      const res = await previewMapping(m, samples);
      if (res.ok) {
        setPreview(res.data);
        setPreviewError(null);
      } else {
        setPreview(null);
        setPreviewError(res.message ?? res.errors.map((e) => `${e.path}: ${e.message}`).join("; "));
      }
    }, 400);
    return () => clearTimeout(t);
  }, [mapping, samples]);

  const result = preview?.results[Math.min(sampleIdx, (preview?.results.length ?? 1) - 1)];

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1 text-sm font-semibold">Map your fields</legend>
        <p className="text-xs text-zinc-500">Each canonical field reads a JSONPath in your product payload. Suggestions come from your sample.</p>
        <datalist id={listId}>
          {paths.map((p) => (
            <option key={p} value={p} />
          ))}
        </datalist>
        <div className="flex flex-col divide-y divide-zinc-100 rounded-md border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
          {CANONICAL_FIELDS.map((f) => {
            const id = fieldId(`/field_mapping/${f.name}`);
            const err = mappingError(errors, f.name);
            return (
              <div key={f.name} className="grid grid-cols-1 items-center gap-1 px-3 py-2 sm:grid-cols-[11rem_1fr]">
                <label htmlFor={id} className="flex items-center gap-2 text-sm">
                  <code>{f.name}</code>
                  {f.required && <Badge tone="amber">required</Badge>}
                </label>
                <div>
                  <input
                    id={id}
                    list={listId}
                    value={mapping[f.name] ?? ""}
                    placeholder={f.hint}
                    aria-invalid={Boolean(err)}
                    aria-describedby={err ? `${id}-error` : undefined}
                    onChange={(e) => onChange({ ...mapping, [f.name]: e.target.value })}
                    className={cx(
                      "w-full rounded border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900",
                      err && "border-red-500",
                    )}
                  />
                  {err && (
                    <p id={`${id}-error`} className="mt-0.5 text-xs text-red-600">
                      {err}
                    </p>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      </fieldset>

      <section aria-labelledby={`${listId}-preview`} className="flex flex-col gap-2">
        <div className="flex items-center justify-between gap-2">
          <h3 id={`${listId}-preview`} className="text-sm font-semibold">
            Canonical product preview
          </h3>
          {samples.length > 1 && (
            <label className="flex items-center gap-2 text-xs">
              Sample
              <Select value={sampleIdx} onChange={(e) => setSampleIdx(Number(e.target.value))} className="w-auto py-1 text-xs">
                {samples.map((_, i) => (
                  <option key={i} value={i}>
                    {i + 1}
                  </option>
                ))}
              </Select>
            </label>
          )}
        </div>
        <div aria-live="polite">
          {samples.length === 0 && <p className="text-sm text-zinc-500">Add a sample product to see how it maps.</p>}
          {previewError && <p className="text-sm text-red-600">{previewError}</p>}
          {result?.errors && result.errors.length > 0 && (
            <ul className="mb-2 list-disc pl-5 text-xs text-red-600">
              {result.errors.map((e, i) => (
                <li key={i}>
                  <code>{e.path}</code> {e.message}
                </li>
              ))}
            </ul>
          )}
          {result && (
            <pre className="max-h-[32rem] overflow-auto rounded-md bg-zinc-900 p-3 text-xs text-zinc-100">{JSON.stringify(result.product, null, 2)}</pre>
          )}
        </div>
      </section>
    </div>
  );
}
