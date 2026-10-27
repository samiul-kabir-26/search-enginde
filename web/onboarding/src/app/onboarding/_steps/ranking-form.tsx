"use client";

import { useState } from "react";

import { ModelRegistrationForm } from "@/components/model-registration-form";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import { Card, Field, Select, cx } from "@/components/ui";
import { WeightSliders } from "@/components/weight-sliders";
import type { Model } from "@/lib/api/types";

type Mode = "default" | "learned" | "byo";

/** Default weights of default_linear in the rank stage (docs/TENANT_CONFIG.md §3). */
const DEFAULT_WEIGHTS: Record<string, number> = {
  rrf_score: 1.0,
  bm25_norm: 0.4,
  cosine_text: 0.6,
  cosine_image: 0.6,
  ctr_7d: 0.3,
  cvr_7d: 0.5,
  rating: 0.2,
  vendor_score: 0.3,
  policy_boost: 1.0,
};

function ScorerPicker({
  kind,
  models,
  value,
  onChange,
  defaultFeatures,
  errors,
}: {
  kind: "prerank" | "rank";
  models: Model[];
  value: string;
  onChange: (id: string) => void;
  defaultFeatures: string[];
  errors: Parameters<typeof Field>[0]["errors"];
}) {
  const [extra, setExtra] = useState<Model[]>([]);
  const options = [...models, ...extra].filter((m) => m.kind === kind);
  return (
    <div className="flex flex-col gap-3">
      <Field path={`/${kind}_model_id`} label={kind === "prerank" ? "Pre-rank model (stage 4)" : "Rank model (stage 5)"} errors={errors}>
        {(p) => (
          <Select {...p} value={value} onChange={(e) => onChange(e.target.value)}>
            <option value="">Keep the default scorer</option>
            {options.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name} {m.version} ({m.type}, {m.input_schema?.features?.length ?? 0} features)
              </option>
            ))}
          </Select>
        )}
      </Field>
      <details>
        <summary className="cursor-pointer text-sm font-medium">Register a {kind === "prerank" ? "pre-rank" : "rank"} model</summary>
        <div className="mt-3">
          <ModelRegistrationForm
            kind={kind}
            types={kind === "prerank" ? ["onnx", "kserve_v2"] : ["kserve_v2", "onnx"]}
            defaultFeatures={defaultFeatures}
            onRegistered={(m) => {
              setExtra((x) => [...x, m]);
              onChange(m.id);
            }}
          />
        </div>
      </details>
    </div>
  );
}

export function RankingForm(props: {
  mode: Mode;
  weights: Record<string, number>;
  learnedAvailable: boolean;
  prerankFeatures: string[];
  rankFeatures: string[];
  prerankModelId?: string;
  rankModelId?: string;
  scorerModels: Model[];
}) {
  const { state, submit, pending } = useStep("ranking");
  const [mode, setMode] = useState<Mode>(props.mode);
  const [weights, setWeights] = useState<Record<string, number>>({ ...DEFAULT_WEIGHTS, ...props.weights });
  const [prerankId, setPrerankId] = useState(props.prerankModelId ?? "");
  const [rankId, setRankId] = useState(props.rankModelId ?? "");

  const onContinue = () => {
    if (mode === "default") submit({ mode, weights });
    else if (mode === "learned") submit({ mode });
    else submit({ mode, prerank_model_id: prerankId || undefined, rank_model_id: rankId || undefined });
  };

  const modes: { id: Mode; label: string; detail: string; disabled?: string }[] = [
    { id: "default", label: "Default scorer", detail: "A weighted formula over relevance, quality, seller and popularity signals. Tune the weights below." },
    {
      id: "learned",
      label: "Platform learned model",
      detail: "LightGBM models trained on shopper clicks, carts and orders.",
      disabled: props.learnedAvailable ? undefined : "Available once enough interaction events exist and the platform models are trained.",
    },
    { id: "byo", label: "Your own models", detail: "Upload an ONNX pre-rank model and/or connect a remote ranking model." },
  ];

  return (
    <StepShell
      step="ranking"
      intro="Stage 4 quickly scores thousands of candidates; stage 5 ranks the top few hundred precisely. Start with the default scorer and switch to a learned model once you have traffic."
    >
      <Card title="Ranking approach">
        <fieldset className="grid gap-3 md:grid-cols-3">
          <legend className="sr-only">Ranking approach</legend>
          {modes.map((m) => (
            <label
              key={m.id}
              className={cx(
                "flex gap-3 rounded-lg border p-3 text-sm",
                m.disabled ? "cursor-not-allowed opacity-60" : "cursor-pointer",
                mode === m.id ? "border-indigo-600 bg-indigo-50 dark:bg-indigo-950" : "border-zinc-200 dark:border-zinc-800",
              )}
            >
              <input type="radio" name="mode" value={m.id} checked={mode === m.id} disabled={Boolean(m.disabled)} onChange={() => setMode(m.id)} className="mt-1" />
              <span>
                <span className="block font-medium">{m.label}</span>
                <span className="block text-zinc-600 dark:text-zinc-400">{m.disabled ?? m.detail}</span>
              </span>
            </label>
          ))}
        </fieldset>
      </Card>

      {mode === "default" && (
        <Card title="Tune the default scorer">
          <WeightSliders weights={weights} onChange={setWeights} errors={state.errors} />
        </Card>
      )}
      {mode === "byo" && (
        <Card title="Your scoring models">
          <div className="grid gap-6 lg:grid-cols-2">
            <ScorerPicker kind="prerank" models={props.scorerModels} value={prerankId} onChange={setPrerankId} defaultFeatures={props.prerankFeatures} errors={state.errors} />
            <ScorerPicker kind="rank" models={props.scorerModels} value={rankId} onChange={setRankId} defaultFeatures={props.rankFeatures} errors={state.errors} />
          </div>
        </Card>
      )}

      <StepOutcomeView state={state} claimed={["/weights", "/rank/scorer/weights", "/prerank_model_id", "/rank_model_id"]} />
      <StepFooter step="ranking" pending={pending} onContinue={onContinue} skippable disabled={mode === "byo" && !prerankId && !rankId} />
    </StepShell>
  );
}
