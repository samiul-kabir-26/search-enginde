"use client";

import { useEffect, useState } from "react";

import { getIndexJob } from "@/app/onboarding/actions";
import type { IndexJob } from "@/lib/api/types";

import { Badge } from "./ui";

const POLL_MS = 2000;

/** Polls an index job every 2 s until it finishes (docs/ONBOARDING.md §4, step 7). */
export function IndexProgress({ job: initial, onDone }: { job: IndexJob; onDone?: (job: IndexJob) => void }) {
  const [job, setJob] = useState(initial);
  const finished = job.status === "succeeded" || job.status === "failed";

  useEffect(() => {
    if (finished) {
      onDone?.(job);
      return;
    }
    const t = setTimeout(async () => {
      const next = await getIndexJob(job.id);
      if (next) setJob(next);
    }, POLL_MS);
    return () => clearTimeout(t);
  }, [job, finished, onDone]);

  const pct = job.total > 0 ? Math.min(100, Math.round(((job.done + job.failed) / job.total) * 100)) : finished ? 100 : 0;
  const tone = job.status === "succeeded" ? "green" : job.status === "failed" ? "red" : job.status === "running" ? "indigo" : "zinc";
  const status =
    job.status === "succeeded"
      ? `Index built: ${job.done} products indexed${job.failed ? `, ${job.failed} failed` : ""}.`
      : job.status === "failed"
        ? `The index build failed${job.error ? `: ${job.error}` : "."}`
        : job.status === "running"
          ? `Indexing ${job.done} of ${job.total || "…"} products.`
          : "Waiting for an indexer to pick up the job…";

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-sm">
        <span className="font-medium">Version {job.config_version}</span>
        <Badge tone={tone}>{job.status}</Badge>
        {job.index_version !== undefined && <span className="text-zinc-500">index v{job.index_version}</span>}
      </div>
      <div role="progressbar" aria-label="Index build progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} className="h-2.5 w-full rounded-full bg-zinc-200 dark:bg-zinc-800">
        <div className={job.status === "failed" ? "h-2.5 rounded-full bg-red-500" : "h-2.5 rounded-full bg-indigo-600 transition-[width]"} style={{ width: `${pct}%` }} />
      </div>
      <p aria-live="polite" className="text-sm text-zinc-700 dark:text-zinc-300" data-testid="index-status">
        {status}
      </p>
    </div>
  );
}
