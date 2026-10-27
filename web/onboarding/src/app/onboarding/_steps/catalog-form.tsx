"use client";

import { useId, useState } from "react";

import { JsonPathMapper } from "@/components/jsonpath-mapper";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import { Card, TextArea, cx } from "@/components/ui";
import { saveSamples, useSamples } from "@/lib/samples";

type Ingestion = "events" | "upload";

/** Parses pasted samples: a JSON object, a JSON array of objects, or JSONL. */
function parseSamples(text: string): { samples: unknown[]; error?: string } {
  const t = text.trim();
  if (!t) return { samples: [] };
  try {
    const v: unknown = JSON.parse(t);
    if (Array.isArray(v)) return { samples: v.filter((x) => x && typeof x === "object").slice(0, 20) };
    if (v && typeof v === "object") return { samples: [v] };
  } catch {
    // Not one JSON value: try JSONL below.
  }
  const out: unknown[] = [];
  for (const [i, line] of t.split("\n").entries()) {
    if (!line.trim()) continue;
    try {
      out.push(JSON.parse(line));
    } catch {
      return { samples: [], error: `Line ${i + 1} is not valid JSON.` };
    }
  }
  return { samples: out.slice(0, 20) };
}

export function CatalogForm({ initialMapping, initialIngestion }: { initialMapping: Record<string, string>; initialIngestion: Ingestion }) {
  const { state, submit, pending } = useStep("catalog");
  const [ingestion, setIngestion] = useState<Ingestion>(initialIngestion);
  const [mapping, setMapping] = useState<Record<string, string>>(initialMapping);
  const samples = useSamples();
  const [pasted, setPasted] = useState("");
  const [pasteError, setPasteError] = useState<string>();
  const [upload, setUpload] = useState<{ status: "idle" | "uploading" | "done" | "error"; message?: string }>({ status: "idle" });
  const fileId = useId();

  // Samples are not config: they stay in this browser tab for the policies step's rule tests.
  const applySamples = saveSamples;

  async function onUpload(file: File | undefined) {
    if (!file) return;
    setUpload({ status: "uploading", message: `Uploading ${file.name}…` });
    const body = new FormData();
    body.set("file", file);
    const res = await fetch("/api/catalog/upload", { method: "POST", body });
    const data = (await res.json().catch(() => ({}))) as { detail?: string; lines?: number; samples?: unknown[] };
    if (!res.ok || !data.samples) {
      setUpload({ status: "error", message: data.detail ?? "The upload failed." });
      return;
    }
    applySamples(data.samples);
    setUpload({ status: "done", message: `Uploaded ${data.lines} products. The first ${data.samples.length} are used as samples below.` });
  }

  function onPaste(text: string) {
    setPasted(text);
    const { samples: s, error } = parseSamples(text);
    setPasteError(error);
    if (!error) applySamples(s);
  }

  const onContinue = () => {
    const fieldMapping = Object.fromEntries(Object.entries(mapping).filter(([, v]) => v.trim()));
    submit({ ingestion, field_mapping: fieldMapping, samples: samples.slice(0, 20) });
  };

  return (
    <StepShell
      step="catalog"
      intro="Tell us how products arrive and how your product JSON maps onto the platform's catalog fields. The preview shows each sample as the engine will store it."
    >
      <Card title="How do products arrive?">
        <fieldset className="grid gap-3 sm:grid-cols-2">
          <legend className="sr-only">Ingestion</legend>
          {(
            [
              ["events", "Event stream", "Send product changes to your Kafka topic or webhook as they happen."],
              ["upload", "Batch upload", "Upload a JSONL file, one product per line. Good for a first import."],
            ] as const
          ).map(([value, label, help]) => (
            <label
              key={value}
              className={cx(
                "flex cursor-pointer gap-3 rounded-lg border p-3 text-sm",
                ingestion === value ? "border-indigo-600 bg-indigo-50 dark:bg-indigo-950" : "border-zinc-200 dark:border-zinc-800",
              )}
            >
              <input type="radio" name="ingestion" value={value} checked={ingestion === value} onChange={() => setIngestion(value)} className="mt-1" />
              <span>
                <span className="block font-medium">{label}</span>
                <span className="block text-zinc-600 dark:text-zinc-400">{help}</span>
              </span>
            </label>
          ))}
        </fieldset>
      </Card>

      <Card title={ingestion === "upload" ? "Upload your catalog" : "Paste a sample payload"}>
        {ingestion === "upload" ? (
          <div className="flex flex-col gap-2">
            <label htmlFor={fileId} className="text-sm font-medium">
              Catalog file (JSONL)
            </label>
            <input
              id={fileId}
              type="file"
              accept=".jsonl,.ndjson,application/x-ndjson,application/json"
              onChange={(e) => onUpload(e.target.files?.[0])}
              className="text-sm file:mr-3 file:rounded-md file:border-0 file:bg-indigo-600 file:px-3 file:py-2 file:text-white"
            />
            <p aria-live="polite" className={cx("text-sm", upload.status === "error" ? "text-red-600" : "text-zinc-600 dark:text-zinc-400")}>
              {upload.message}
            </p>
          </div>
        ) : (
          <div className="flex flex-col gap-2">
            <label htmlFor={`${fileId}-paste`} className="text-sm font-medium">
              One product as JSON, a JSON array, or JSONL (up to 20)
            </label>
            <TextArea
              id={`${fileId}-paste`}
              rows={8}
              value={pasted}
              onChange={(e) => onPaste(e.target.value)}
              aria-invalid={Boolean(pasteError)}
              placeholder='{"id": "sku-1", "name": "Red leather bag", ...}'
            />
            {pasteError && <p className="text-xs text-red-600">{pasteError}</p>}
            {samples.length > 0 && !pasteError && <p className="text-xs text-zinc-500">{samples.length} sample(s) ready.</p>}
          </div>
        )}
      </Card>

      <Card>
        <JsonPathMapper mapping={mapping} onChange={setMapping} samples={samples} errors={state.errors} />
      </Card>

      <StepOutcomeView state={state} claimed={["/field_mapping", "/catalog/field_mapping"]} />
      <StepFooter step="catalog" pending={pending} onContinue={onContinue} disabled={samples.length === 0} />
      {samples.length === 0 && <p className="text-right text-xs text-zinc-500">Add at least one sample product to continue.</p>}
    </StepShell>
  );
}
