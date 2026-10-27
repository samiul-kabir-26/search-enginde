"use client";

import { useState } from "react";

import { saveSettings } from "@/app/admin/actions";
import { Button, Card, ErrorSummary, Field } from "@/components/ui";
import type { FormState, PlatformSettings } from "@/lib/api/types";

type Ads = NonNullable<PlatformSettings["ads"]>;

const CAMPAIGN_FIELDS = ["bid_minor", "daily_budget_minor", "status", "targeting", "start_at", "end_at"];

const DEFAULT_ADS: Ads = {
  tenant_fields: CAMPAIGN_FIELDS,
  disclosure: { label: "Sponsored", vendor_name: true, disclosure_link: "" },
  max_slots: 10,
  min_relevance_floor: 0.1,
  reserve_bounds: { min_minor: 1, max_minor: 10000 },
};

/** Platform ad controls (PLAN.md R10), saved with the rest of the platform settings. */
export function AdsForm({ settings }: { settings: PlatformSettings }) {
  const [ads, setAds] = useState<Ads>(settings.ads ?? DEFAULT_ADS);
  const [out, setOut] = useState<FormState | null>(null);
  const [busy, setBusy] = useState(false);

  async function onSave() {
    setBusy(true);
    try {
      setOut(await saveSettings({ ...settings, ads }));
    } finally {
      setBusy(false);
    }
  }

  const toggleField = (f: string) =>
    setAds({ ...ads, tenant_fields: ads.tenant_fields.includes(f) ? ads.tenant_fields.filter((x) => x !== f) : [...ads.tenant_fields, f] });
  const claimed = (p: string) => p.startsWith("/ads/") && p !== "/ads/tenant_fields";

  return (
    <div className="flex flex-col gap-4">
      <ErrorSummary message={out?.ok ? undefined : out?.message} errors={out?.errors?.filter((e) => !claimed(e.path))} />
      <Card title="Limits">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field path="/ads/max_slots" label="Max ad slots per tenant" type="number" min={1} max={50} value={ads.max_slots} onChange={(e) => setAds({ ...ads, max_slots: Number(e.target.value) })} errors={out?.errors} />
          <Field
            path="/ads/min_relevance_floor"
            label="Minimum relevance floor"
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={ads.min_relevance_floor}
            onChange={(e) => setAds({ ...ads, min_relevance_floor: Number(e.target.value) })}
            hint="0 to 1; tenants cannot set a lower floor."
            errors={out?.errors}
          />
          <Field
            path="/ads/reserve_bounds/min_minor"
            label="Lowest reserve price per click (minor units)"
            type="number"
            min={0}
            value={ads.reserve_bounds.min_minor}
            onChange={(e) => setAds({ ...ads, reserve_bounds: { ...ads.reserve_bounds, min_minor: Number(e.target.value) } })}
            errors={out?.errors}
          />
          <Field
            path="/ads/reserve_bounds/max_minor"
            label="Highest reserve price per click (minor units)"
            type="number"
            min={0}
            value={ads.reserve_bounds.max_minor}
            onChange={(e) => setAds({ ...ads, reserve_bounds: { ...ads.reserve_bounds, max_minor: Number(e.target.value) } })}
            errors={out?.errors}
          />
        </div>
      </Card>
      <Card title="Sponsored disclosure">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field path="/ads/disclosure/label" label="Label" value={ads.disclosure.label} onChange={(e) => setAds({ ...ads, disclosure: { ...ads.disclosure, label: e.target.value } })} errors={out?.errors} />
          <Field
            path="/ads/disclosure/disclosure_link"
            label="Disclosure link"
            type="url"
            value={ads.disclosure.disclosure_link}
            onChange={(e) => setAds({ ...ads, disclosure: { ...ads.disclosure, disclosure_link: e.target.value } })}
            errors={out?.errors}
          />
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={ads.disclosure.vendor_name} onChange={(e) => setAds({ ...ads, disclosure: { ...ads.disclosure, vendor_name: e.target.checked } })} /> Show the vendor name on sponsored results
          </label>
        </div>
      </Card>
      <Card title="Campaign fields tenants can see and edit">
        <fieldset className="grid gap-2 sm:grid-cols-3">
          <legend className="sr-only">Tenant-editable campaign fields</legend>
          {CAMPAIGN_FIELDS.map((f) => (
            <label key={f} className="flex items-center gap-2 text-sm">
              <input type="checkbox" checked={ads.tenant_fields.includes(f)} onChange={() => toggleField(f)} /> <code>{f}</code>
            </label>
          ))}
        </fieldset>
      </Card>
      <p aria-live="polite" className="text-sm text-emerald-700 dark:text-emerald-300">
        {out?.ok ? out.message : ""}
      </p>
      <div>
        <Button onClick={onSave} disabled={busy}>
          {busy ? "Saving…" : "Save ad controls"}
        </Button>
      </div>
    </div>
  );
}
