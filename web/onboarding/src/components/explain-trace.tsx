"use client";

import type { StageTrace } from "@/lib/api/types";

import { Badge } from "./ui";

const STAGE_NAMES: Record<string, string> = {
  query_understanding: "1. Understand query",
  retrieval: "2. Find candidates",
  filter: "3. Apply your rules",
  prerank: "4. Quick scoring",
  rank: "5. Precise ranking",
  rerank: "6. Personalize and place ads",
};

/** Per-stage explain trace: provider, latency, candidates in and out, fallbacks and top scores. */
export function ExplainTrace({ stages }: { stages: StageTrace[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[36rem] text-left text-sm">
        <caption className="sr-only">Explain trace: what each pipeline stage did for this query</caption>
        <thead className="text-xs uppercase tracking-wide text-zinc-500">
          <tr>
            <th scope="col" className="py-2 pr-3">
              Stage
            </th>
            <th scope="col" className="py-2 pr-3">
              Provider
            </th>
            <th scope="col" className="py-2 pr-3 text-right">
              ms
            </th>
            <th scope="col" className="py-2 pr-3 text-right">
              In → out
            </th>
            <th scope="col" className="py-2">
              Status
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
          {stages.map((s) => (
            <tr key={s.stage} className="align-top">
              <th scope="row" className="py-2 pr-3 font-medium">
                {STAGE_NAMES[s.stage] ?? s.stage}
                {s.candidates && s.candidates.length > 0 && (
                  <details className="mt-1 font-normal">
                    <summary className="cursor-pointer text-xs text-indigo-700 dark:text-indigo-300">Top candidates</summary>
                    <ol className="mt-1 max-h-48 overflow-auto text-xs">
                      {s.candidates.slice(0, 10).map((c) => (
                        <li key={c.product_id} className="font-mono">
                          {c.product_id}{" "}
                          <span className="text-zinc-500">
                            {Object.entries(c.scores)
                              .slice(0, 4)
                              .map(([k, v]) => `${k}=${v.toFixed(3)}`)
                              .join(" ")}
                          </span>
                        </li>
                      ))}
                    </ol>
                  </details>
                )}
              </th>
              <td className="py-2 pr-3 font-mono text-xs">{s.provider}</td>
              <td className="py-2 pr-3 text-right font-mono tabular-nums">{s.latency_ms.toFixed(1)}</td>
              <td className="py-2 pr-3 text-right font-mono tabular-nums">
                {s.in_count} → {s.out_count}
              </td>
              <td className="py-2">
                {s.error ? (
                  <Badge tone="red">error: {s.error}</Badge>
                ) : s.fallback ? (
                  <Badge tone="amber">fallback{s.fallback_reason ? `: ${s.fallback_reason}` : ""}</Badge>
                ) : (
                  <Badge tone="green">ok</Badge>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
