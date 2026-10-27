"use client";

import Link from "next/link";
import { startTransition, useActionState, useCallback, useState } from "react";

import { publishAndBuild, type StepOutcome } from "@/app/onboarding/actions";
import { IndexProgress } from "@/components/index-progress";
import { StepOutcomeView, StepShell } from "@/components/step-shell";
import { Button, Card } from "@/components/ui";
import type { IndexJob } from "@/lib/api/types";

export function IndexBuildForm({ job: existing, hasUpload }: { job?: IndexJob; hasUpload: boolean }) {
  const [state, dispatch, pending] = useActionState(() => publishAndBuild(), { ok: false } as StepOutcome);
  const [done, setDone] = useState<IndexJob | null>(existing?.status === "succeeded" ? existing : null);
  const job = (state.ok ? (state.data?.job as IndexJob | undefined) : undefined) ?? existing;
  const version = state.ok ? (state.data?.job as IndexJob | undefined)?.config_version : undefined;
  const onDone = useCallback((j: IndexJob) => setDone(j.status === "succeeded" ? j : null), []);

  return (
    <StepShell
      step="index_build"
      intro="Publishing validates your whole configuration and freezes it as a version. The build then indexes your catalog into it. Your live search is untouched until you activate the version."
    >
      <Card title="Publish & build">
        <div className="flex flex-col gap-4">
          <p className="text-sm text-zinc-600 dark:text-zinc-400">
            {hasUpload ? "The build indexes the catalog file you uploaded." : "The build indexes the products you sent to your catalog stream."}
          </p>
          <StepOutcomeView state={state} />
          {version !== undefined && (
            <p role="status" className="text-sm">
              Published version {version}.
            </p>
          )}
          {job && <IndexProgress key={job.id} job={job} onDone={onDone} />}
          <div className="flex flex-wrap gap-3">
            <Button onClick={() => startTransition(() => dispatch())} disabled={pending || (Boolean(job) && !done && job?.status !== "failed")} variant={job ? "secondary" : "primary"}>
              {pending ? "Publishing…" : job ? "Publish & rebuild" : "Publish & build"}
            </Button>
            {done && (
              <Link href="/onboarding/playground" className="inline-flex items-center rounded-md bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-500">
                Try it in the playground
              </Link>
            )}
          </div>
        </div>
      </Card>
    </StepShell>
  );
}
