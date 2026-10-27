"use client";

import { useState } from "react";

import { SlotPicker } from "@/components/slot-picker";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import { Card, Field } from "@/components/ui";
import type { TenantConfig } from "@/lib/config";
import { errorAt, errorsUnder } from "@/lib/forms";

type Rerank = NonNullable<TenantConfig["rerank"]>;
type Ads = NonNullable<TenantConfig["ads"]>;

function Slider({ id, label, value, min, max, step, onChange, help, disabled }: { id: string; label: string; value: number; min: number; max: number; step: number; onChange: (v: number) => void; help?: string; disabled?: boolean }) {
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline justify-between">
        <label htmlFor={id} className="text-sm">
          {label}
        </label>
        <output htmlFor={id} className="font-mono text-sm tabular-nums">
          {value.toFixed(2)}
        </output>
      </div>
      <input id={id} type="range" min={min} max={max} step={step} value={value} disabled={disabled} onChange={(e) => onChange(Number(e.target.value))} className="accent-indigo-600" aria-describedby={help ? `${id}-help` : undefined} />
      {help && (
        <p id={`${id}-help`} className="text-xs text-zinc-500">
          {help}
        </p>
      )}
    </div>
  );
}

export function PersonalizationForm(props: { personalization: NonNullable<Rerank["personalization"]>; diversity: NonNullable<Rerank["diversity"]>; ads: Ads; rankKeep: number }) {
  const { state, submit, pending } = useStep("personalization");
  const [pers, setPers] = useState(props.personalization);
  const [div, setDiv] = useState(props.diversity);
  const [ads, setAds] = useState<Ads>(props.ads);

  const onContinue = () =>
    submit({
      personalization: pers,
      diversity: div,
      ads: { enabled: ads.enabled, slots: ads.slots, relevance_floor: ads.relevance_floor, auction: "gsp", ...(ads.reserve_cpc_minor ? { reserve_cpc_minor: ads.reserve_cpc_minor } : {}) },
    });

  const slotError = errorsUnder(state.errors, "/ads/slots")[0]?.message;
  return (
    <StepShell
      step="personalization"
      intro="The last stage adapts results to each shopper, keeps seller variety and places sponsored results. Everything here can change later without re-indexing."
    >
      <Card title="Personalization">
        <div className="flex flex-col gap-4">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={pers.enabled} onChange={(e) => setPers({ ...pers, enabled: e.target.checked })} /> Personalize results for returning shoppers
          </label>
          <div className="grid gap-4 sm:grid-cols-2">
            <Slider id="alpha" label="Category taste strength" value={pers.alpha} min={0} max={1} step={0.05} disabled={!pers.enabled} onChange={(alpha) => setPers({ ...pers, alpha })} help="How much a shopper's favourite categories lift matching products." />
            <Slider id="beta" label="Price band strength" value={pers.beta} min={0} max={1} step={0.05} disabled={!pers.enabled} onChange={(beta) => setPers({ ...pers, beta })} help="How much products in the shopper's usual price range are lifted." />
          </div>
        </div>
      </Card>

      <Card title="Vendor diversity">
        <div className="grid max-w-md gap-4 sm:grid-cols-2">
          <Field path="/rerank/diversity/max_per_vendor" label="Max results per vendor" type="number" min={0} value={div.max_per_vendor} onChange={(e) => setDiv({ ...div, max_per_vendor: Number(e.target.value) })} hint="0 turns diversity off." errors={state.errors} />
          <Field path="/rerank/diversity/window" label="In every window of" type="number" min={1} value={div.window} onChange={(e) => setDiv({ ...div, window: Number(e.target.value) })} hint="Consecutive results." errors={state.errors} />
        </div>
      </Card>

      <Card title="Sponsored results">
        <div className="flex flex-col gap-4">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={ads.enabled} onChange={(e) => setAds({ ...ads, enabled: e.target.checked })} /> Show sponsored results from your vendors&apos; campaigns
          </label>
          {ads.enabled && (
            <>
              <SlotPicker slots={ads.slots} onChange={(slots) => setAds({ ...ads, slots })} max={Math.min(20, props.rankKeep)} error={slotError} />
              <div className="grid gap-4 sm:grid-cols-2">
                <Slider id="floor" label="Relevance floor" value={ads.relevance_floor} min={0} max={1} step={0.05} onChange={(relevance_floor) => setAds({ ...ads, relevance_floor })} help="Ads less relevant than this (0–1, relative to the page) are never shown." />
                <Field
                  path="/ads/reserve_cpc_minor"
                  label="Reserve price per click (minor units)"
                  type="number"
                  min={0}
                  value={ads.reserve_cpc_minor ?? ""}
                  onChange={(e) => setAds({ ...ads, reserve_cpc_minor: e.target.value ? Number(e.target.value) : undefined })}
                  hint="Optional; the platform's bounds apply."
                  errors={state.errors}
                />
              </div>
              {errorAt(state.errors, "/ads/relevance_floor") && <p className="text-xs text-red-600">{errorAt(state.errors, "/ads/relevance_floor")}</p>}
            </>
          )}
        </div>
      </Card>

      <StepOutcomeView state={state} claimed={["/ads/slots", "/ads/relevance_floor", "/ads/reserve_cpc_minor", "/rerank/diversity"]} />
      <StepFooter step="personalization" pending={pending} onContinue={onContinue} skippable />
    </StepShell>
  );
}
