"use client";

import { startTransition, useActionState, useState } from "react";

import { resendVerification } from "@/app/actions/auth";
import { goLive, startShadow, type StepOutcome } from "@/app/onboarding/actions";
import { StepOutcomeView, StepShell } from "@/components/step-shell";
import { Button, Card, ErrorSummary } from "@/components/ui";
import type { FormState } from "@/lib/api/types";

export function GoLiveForm({ latest, active, controlUrl, searchUrl }: { latest?: number; active?: number; controlUrl: string; searchUrl: string }) {
  const [state, dispatch, pending] = useActionState(() => goLive(), { ok: false } as StepOutcome);
  const [resent, dispatchResend, resending] = useActionState(resendVerification, { ok: false } as FormState);
  const [shadow, setShadow] = useState<FormState | null>(null);
  const [rate, setRate] = useState(0.1);
  const [copied, setCopied] = useState(false);

  const unverified = state.errors?.some((e) => e.code === "email_unverified");
  const apiKey = state.ok ? (state.data?.api_key as string | undefined) : undefined;
  const liveVersion = state.ok ? latest : active;

  const snippet = `# Exchange the API key for a short-lived token (server side; keep the key secret).
TOKEN=$(curl -s -X POST ${controlUrl}/v1/auth/token \\
  -H 'Content-Type: application/json' \\
  -d '{"api_key": "'"$SEARCH_API_KEY"'"}' | jq -r .access_token)

# Search.
curl -s -X POST ${searchUrl}/v1/search \\
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \\
  -d '{"q": "red leather bag", "page": {"size": 24}}'`;

  return (
    <StepShell
      step="go_live"
      intro="Activating sends live traffic to the version you built. You can also run it in shadow mode first, which compares it against the active version on a sample of real queries without changing what shoppers see."
    >
      {active !== undefined && latest !== undefined && latest !== active && !state.ok && (
        <Card title="Shadow mode first (optional)">
          <div className="flex flex-col gap-3">
            <p className="text-sm text-zinc-600 dark:text-zinc-400">
              Version {active} is live. Run version {latest} on a sample of live queries and compare the results before activating it.
            </p>
            <label className="flex items-center gap-3 text-sm">
              Sample rate
              <input type="range" min={0.01} max={1} step={0.01} value={rate} onChange={(e) => setRate(Number(e.target.value))} className="accent-indigo-600" aria-valuetext={`${Math.round(rate * 100)}%`} />
              <output className="font-mono">{Math.round(rate * 100)}%</output>
            </label>
            <div>
              <Button variant="secondary" onClick={async () => setShadow(await startShadow(latest, rate))}>
                Start shadow mode
              </Button>
            </div>
            <div aria-live="polite">
              {shadow?.ok && <p className="text-sm text-emerald-700">{shadow.message}</p>}
              <ErrorSummary message={shadow?.ok ? undefined : shadow?.message} errors={shadow?.errors} />
            </div>
          </div>
        </Card>
      )}

      <Card title={state.ok ? "You're live" : `Activate version ${latest ?? ""}`}>
        <div className="flex flex-col gap-4">
          {!state.ok && (
            <>
              <StepOutcomeView state={state} />
              {unverified && (
                <div className="flex flex-col gap-2 rounded-md bg-amber-50 p-3 text-sm dark:bg-amber-950">
                  <p>Verify your email address first: open the link we sent when you signed up.</p>
                  <div>
                    <Button variant="secondary" disabled={resending} onClick={() => startTransition(() => dispatchResend())}>
                      {resending ? "Sending…" : "Send a new link"}
                    </Button>
                  </div>
                  <p aria-live="polite">{resent.ok ? resent.message : resent.message}</p>
                </div>
              )}
              <div>
                <Button onClick={() => startTransition(() => dispatch())} disabled={pending || latest === undefined}>
                  {pending ? "Activating…" : "Activate and go live"}
                </Button>
              </div>
            </>
          )}
          {state.ok && (
            <div role="status" className="flex flex-col gap-3">
              <p className="text-sm">Version {liveVersion} is serving live traffic.</p>
              {apiKey ? (
                <div className="flex flex-col gap-2">
                  <p className="text-sm font-medium">Your production API key (shown once; store it in your secrets manager now):</p>
                  <div className="flex flex-wrap items-center gap-2">
                    <code data-testid="api-key" className="break-all rounded bg-zinc-900 px-3 py-2 text-sm text-zinc-100">
                      {apiKey}
                    </code>
                    <Button
                      variant="secondary"
                      onClick={async () => {
                        await navigator.clipboard.writeText(apiKey).catch(() => undefined);
                        setCopied(true);
                      }}
                    >
                      {copied ? "Copied" : "Copy"}
                    </Button>
                  </div>
                </div>
              ) : (
                <p className="text-sm text-zinc-600 dark:text-zinc-400">Your workspace already has an API key; manage keys in the console.</p>
              )}
            </div>
          )}
        </div>
      </Card>

      <Card title="Integrate">
        <p className="mb-2 text-sm text-zinc-600 dark:text-zinc-400">Call search from your backend with a token exchanged for the API key:</p>
        <pre className="overflow-x-auto rounded-md bg-zinc-900 p-3 text-xs text-zinc-100">{snippet}</pre>
      </Card>
    </StepShell>
  );
}
