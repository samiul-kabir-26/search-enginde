"use client";

import { useId, useState } from "react";

import { playgroundSearch } from "@/app/onboarding/actions";
import type { SearchResponse } from "@/lib/api/types";
import { formatMinor } from "@/lib/forms";

import { ExplainTrace } from "./explain-trace";
import { Badge, Button, Card, Select } from "./ui";

type Target = number | "draft";
type Side = { target: Target; res?: SearchResponse; error?: string };

function label(t: Target) {
  return t === "draft" ? "Draft" : `Version ${t}`;
}

/**
 * Text and image search against a published version or the draft, with the explain trace,
 * and an optional side-by-side comparison with another version.
 */
export function PlaygroundSearch({ versions }: { versions: number[] }) {
  const ids = useId();
  const targets: Target[] = [...versions, "draft"];
  const [primary, setPrimary] = useState<Target>(versions[0] ?? "draft");
  const [compare, setCompare] = useState<Target | "">("");
  const [q, setQ] = useState("");
  const [busy, setBusy] = useState(false);
  const [sides, setSides] = useState<Side[]>([]);

  async function runText(e: React.FormEvent) {
    e.preventDefault();
    if (!q.trim()) return;
    setBusy(true);
    try {
      const run = async (t: Target): Promise<Side> => {
        const r = await playgroundSearch(t, q.trim());
        return r.ok ? { target: t, res: r.data } : { target: t, error: r.message ?? r.errors.map((x) => x.message).join("; ") };
      };
      setSides(await Promise.all([primary, ...(compare === "" ? [] : [compare])].map(run)));
    } finally {
      setBusy(false);
    }
  }

  async function runImage(file: File | undefined) {
    if (!file) return;
    setBusy(true);
    try {
      const run = async (t: Target): Promise<Side> => {
        const body = new FormData();
        body.set("image", file);
        const res = await fetch(`/api/playground/image?config_version=${t}`, { method: "POST", body });
        const data = (await res.json().catch(() => ({}))) as SearchResponse & { detail?: string };
        return res.ok ? { target: t, res: data } : { target: t, error: data.detail ?? "Image search failed." };
      };
      setSides(await Promise.all([primary, ...(compare === "" ? [] : [compare])].map(run)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <div className="flex flex-col gap-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-sm font-medium">
              Search
              <Select value={String(primary)} onChange={(e) => setPrimary(e.target.value === "draft" ? "draft" : Number(e.target.value))}>
                {targets.map((t) => (
                  <option key={String(t)} value={String(t)}>
                    {label(t)}
                  </option>
                ))}
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-sm font-medium">
              Compare side by side with
              <Select value={String(compare)} onChange={(e) => setCompare(e.target.value === "" ? "" : e.target.value === "draft" ? "draft" : Number(e.target.value))}>
                <option value="">No comparison</option>
                {targets
                  .filter((t) => t !== primary)
                  .map((t) => (
                    <option key={String(t)} value={String(t)}>
                      {label(t)}
                    </option>
                  ))}
              </Select>
            </label>
          </div>
          <form onSubmit={runText} className="flex flex-col gap-2 sm:flex-row" role="search">
            <label htmlFor={`${ids}-q`} className="sr-only">
              Search query
            </label>
            <input
              id={`${ids}-q`}
              type="search"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Try a query your shoppers would type"
              className="flex-1 rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
            />
            <Button type="submit" disabled={busy || !q.trim()}>
              {busy ? "Searching…" : "Search"}
            </Button>
          </form>
          <div className="flex flex-col gap-1">
            <label htmlFor={`${ids}-img`} className="text-sm font-medium">
              Or search by image
            </label>
            <input id={`${ids}-img`} type="file" accept="image/jpeg,image/png,image/webp" disabled={busy} onChange={(e) => runImage(e.target.files?.[0])} className="text-sm" />
          </div>
        </div>
      </Card>

      <div aria-live="polite" aria-busy={busy} className={sides.length > 1 ? "grid gap-4 lg:grid-cols-2" : "flex flex-col gap-4"}>
        {sides.map((s) => (
          <ResultsColumn key={String(s.target)} side={s} />
        ))}
      </div>
    </div>
  );
}

function ResultsColumn({ side }: { side: Side }) {
  if (side.error) {
    return (
      <Card title={label(side.target)}>
        <p role="alert" className="text-sm text-red-600">
          {side.error}
        </p>
      </Card>
    );
  }
  const res = side.res!;
  return (
    <Card title={`${label(side.target)} · ${res.results.length} results${res.degraded ? " (degraded)" : ""}`}>
      <div className="flex flex-col gap-4">
        {res.results.length === 0 ? (
          <p className="text-sm text-zinc-500">No results.</p>
        ) : (
          <ol className="grid gap-2 sm:grid-cols-2" data-testid="playground-results">
            {res.results.map((r) => (
              <li key={`${r.position}-${r.product_id}`} className="flex gap-3 rounded-md border border-zinc-200 p-2 dark:border-zinc-800">
                {r.product?.image_url ? (
                  // Tenant image hosts are arbitrary; next/image would need each one configured.
                  // eslint-disable-next-line @next/next/no-img-element
                  <img src={r.product.image_url} alt="" className="h-16 w-16 shrink-0 rounded object-cover" loading="lazy" />
                ) : (
                  <div className="h-16 w-16 shrink-0 rounded bg-zinc-100 dark:bg-zinc-800" aria-hidden />
                )}
                <div className="min-w-0 text-sm">
                  <p className="flex items-center gap-1 text-xs text-zinc-500">
                    #{r.position}
                    {r.sponsored && <Badge tone="amber">Sponsored</Badge>}
                  </p>
                  <p className="truncate font-medium">{r.product?.title ?? r.product_id}</p>
                  <p className="text-xs text-zinc-600 dark:text-zinc-400">
                    {r.product ? formatMinor(r.product.price_minor, r.product.currency) : ""} · score {r.score.toFixed(3)}
                  </p>
                </div>
              </li>
            ))}
          </ol>
        )}
        {res.trace && (
          <details open>
            <summary className="cursor-pointer text-sm font-semibold">Explain trace</summary>
            <div className="mt-2">
              <ExplainTrace stages={res.trace.stages} />
            </div>
          </details>
        )}
      </div>
    </Card>
  );
}
