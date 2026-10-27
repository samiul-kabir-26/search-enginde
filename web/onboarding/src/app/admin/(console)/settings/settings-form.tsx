"use client";

import { useState } from "react";

import { saveSettings } from "@/app/admin/actions";
import { Button, Card, ErrorSummary, Field } from "@/components/ui";
import type { FormState, PlatformSettings } from "@/lib/api/types";

type Auth = PlatformSettings["auth"];

const DURATIONS: { key: keyof Auth; label: string; hint: string }[] = [
  { key: "access_token_ttl", label: "Access token lifetime", hint: "5m to 60m" },
  { key: "refresh_token_ttl", label: "Refresh token lifetime", hint: "24h to 720h" },
  { key: "api_token_ttl", label: "API token lifetime", hint: "5m to 60m" },
  { key: "key_rotation", label: "Signing key rotation", hint: "7 to 90 days, e.g. 720h" },
  { key: "key_retention", label: "Retired key retention", hint: "1 to 30 days, at least every token lifetime" },
  { key: "email_verification_ttl", label: "Email verification link lifetime", hint: "1h to 7 days" },
];

/** Authentication settings (PLAN.md R7). Ads settings are on their own page and saved with these. */
export function SettingsForm({ settings }: { settings: PlatformSettings }) {
  const [auth, setAuth] = useState<Auth>(settings.auth);
  const [out, setOut] = useState<FormState | null>(null);
  const [busy, setBusy] = useState(false);
  const own = (p: string) => p === "/auth/password_min_length" || DURATIONS.some((d) => p === `/auth/${d.key}`);

  async function onSave() {
    setBusy(true);
    try {
      setOut(await saveSettings({ ...settings, auth }));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="Authentication">
      <div className="flex flex-col gap-4">
        <ErrorSummary message={out?.ok ? undefined : out?.message} errors={out?.errors?.filter((e) => !own(e.path))} />
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            path="/auth/password_min_length"
            label="Minimum password length"
            type="number"
            min={8}
            max={64}
            value={auth.password_min_length}
            onChange={(e) => setAuth({ ...auth, password_min_length: Number(e.target.value) })}
            hint="8 to 64 characters"
            errors={out?.errors}
          />
          {DURATIONS.map((d) => (
            <Field
              key={d.key}
              path={`/auth/${d.key}`}
              label={d.label}
              value={String(auth[d.key])}
              onChange={(e) => setAuth({ ...auth, [d.key]: e.target.value })}
              hint={d.hint}
              errors={out?.errors}
            />
          ))}
        </div>
        <p aria-live="polite" className="text-sm text-emerald-700 dark:text-emerald-300">
          {out?.ok ? out.message : ""}
        </p>
        <div>
          <Button onClick={onSave} disabled={busy}>
            {busy ? "Saving…" : "Save settings"}
          </Button>
        </div>
      </div>
    </Card>
  );
}
