"use client";

import { useState } from "react";

import { ConnectionForm, type VectorStoreBlock } from "@/components/connection-form";
import { ModelRegistrationForm } from "@/components/model-registration-form";
import { ProviderChooser, type ProviderChoice } from "@/components/provider-chooser";
import { StepFooter, StepOutcomeView, StepShell, useStep } from "@/components/step-shell";
import { Card, Field, Select, cx } from "@/components/ui";
import type { Connection, Model } from "@/lib/api/types";
import { RETRIEVAL_PRESETS, type TenantConfig } from "@/lib/config";

type Preset = (typeof RETRIEVAL_PRESETS)[number]["id"];

function EmbedderSlot({
  modality,
  platformModel,
  current,
  models,
  onChange,
  errors,
}: {
  modality: "text" | "image";
  platformModel: string;
  current: { provider: ProviderChoice; model_id?: string };
  models: Model[];
  onChange: (v: { provider: ProviderChoice; model_id?: string }) => void;
  errors: Parameters<typeof Field>[0]["errors"];
}) {
  const [registered, setRegistered] = useState<Model[]>([]);
  const options = [...models, ...registered].filter((m) => m.input_schema?.modality === modality);
  return (
    <ProviderChooser
      legend={modality === "text" ? "Text embedder" : "Image embedder"}
      value={current.provider}
      onChange={(p) => onChange({ provider: p, model_id: p === "byo" ? (current.model_id ?? options[0]?.id) : undefined })}
      platformLabel={platformModel}
      platformDetail={modality === "text" ? "Fast English text embeddings, hosted for you." : "Image embeddings with text-to-image search, hosted for you."}
      byoDetail="Your own embedding endpoint (KServe v2 or an OpenAI-compatible API)."
    >
      <div className="flex flex-col gap-4">
        {options.length > 0 && (
          <Field path={`/embedders/${modality}/model_id`} label="Validated model" errors={errors}>
            {(p) => (
              <Select {...p} value={current.model_id ?? ""} onChange={(e) => onChange({ provider: "byo", model_id: e.target.value })}>
                {options.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name} {m.version} · {m.output_schema?.dim} dims
                  </option>
                ))}
              </Select>
            )}
          </Field>
        )}
        <details open={options.length === 0}>
          <summary className="cursor-pointer text-sm font-medium">Register a new {modality} embedder</summary>
          <div className="mt-3">
            <ModelRegistrationForm
              kind="embedder"
              modality={modality}
              types={modality === "text" ? ["kserve_v2", "openai_embeddings"] : ["kserve_v2"]}
              onRegistered={(m) => {
                setRegistered((r) => [...r, m]);
                onChange({ provider: "byo", model_id: m.id });
              }}
            />
          </div>
        </details>
      </div>
    </ProviderChooser>
  );
}

export function RetrievalForm({
  preset: initialPreset,
  embedders,
  store,
  embedderModels,
  connections,
  platformDims,
}: {
  preset: Preset;
  embedders: TenantConfig["embedders"];
  store: NonNullable<TenantConfig["stores"]>["vector"];
  embedderModels: Model[];
  connections: Connection[];
  platformDims: { text: number; image: number };
}) {
  const { state, submit, pending } = useStep("retrieval");
  const [preset, setPreset] = useState<Preset>(initialPreset);
  const [text, setText] = useState<{ provider: ProviderChoice; model_id?: string }>({
    provider: embedders?.text?.provider === "byo" ? "byo" : "platform",
    model_id: embedders?.text?.model_id,
  });
  const [image, setImage] = useState<{ provider: ProviderChoice; model_id?: string }>({
    provider: embedders?.image?.provider === "byo" ? "byo" : "platform",
    model_id: embedders?.image?.model_id,
  });
  const [storeChoice, setStoreChoice] = useState<ProviderChoice>(store?.provider === "byo" ? "byo" : "platform");
  const [block, setBlock] = useState<VectorStoreBlock | null>(null);
  const [knownConns, setKnownConns] = useState<Connection[]>(connections);

  const dimOf = (sel: { provider: ProviderChoice; model_id?: string }, platform: number) =>
    sel.provider === "byo" ? (embedderModels.find((m) => m.id === sel.model_id)?.output_schema?.dim ?? platform) : platform;

  const onContinue = () => {
    const emb = (sel: { provider: ProviderChoice; model_id?: string }, platformModel: string) =>
      sel.provider === "byo" ? { provider: "byo", model_id: sel.model_id ?? "" } : { provider: "platform", model: platformModel };
    const vector =
      storeChoice === "byo" && block
        ? { provider: "byo", connection_id: block.connection_id, collection: block.collection, vectors: block.vectors }
        : { provider: "platform" };
    submit({
      preset,
      embedders: { text: emb(text, "bge-small-en-v1.5"), image: emb(image, "clip-vit-b32") },
      stores: { lexical: { provider: "platform" }, vector },
    });
  };

  return (
    <StepShell
      step="retrieval"
      intro="Choose how products are found: the embedding models behind meaning and image search, where vectors are stored, and how keyword, meaning and image results are mixed. The platform defaults work out of the box."
    >
      <Card title="Retriever mix">
        <fieldset className="grid gap-3 sm:grid-cols-2">
          <legend className="sr-only">Preset</legend>
          {RETRIEVAL_PRESETS.map((p) => (
            <label
              key={p.id}
              className={cx(
                "flex cursor-pointer gap-3 rounded-lg border p-3 text-sm",
                preset === p.id ? "border-indigo-600 bg-indigo-50 dark:bg-indigo-950" : "border-zinc-200 dark:border-zinc-800",
              )}
            >
              <input type="radio" name="preset" value={p.id} checked={preset === p.id} onChange={() => setPreset(p.id)} className="mt-1" />
              <span>
                <span className="block font-medium">{p.label}</span>
                <span className="block text-zinc-600 dark:text-zinc-400">{p.description}</span>
              </span>
            </label>
          ))}
        </fieldset>
      </Card>

      <Card title="Embedding models">
        <div className="flex flex-col gap-6">
          <EmbedderSlot modality="text" platformModel="bge-small-en-v1.5" current={text} models={embedderModels} onChange={setText} errors={state.errors} />
          <EmbedderSlot modality="image" platformModel="clip-vit-b32" current={image} models={embedderModels} onChange={setImage} errors={state.errors} />
        </div>
      </Card>

      <Card title="Vector store">
        <ProviderChooser
          legend="Where product vectors live"
          value={storeChoice}
          onChange={setStoreChoice}
          platformLabel="Platform-hosted"
          platformDetail="A managed vector store, scoped to your workspace."
          byoDetail="Your own Qdrant, pgvector or Milvus collection. The keyword index stays platform-hosted."
        >
          <div className="flex flex-col gap-4">
            {knownConns.length > 0 && !block && (
              <p className="text-sm text-zinc-600 dark:text-zinc-400">
                You have {knownConns.length} validated connection(s). Test one again below with the collection to use, or register a new one.
              </p>
            )}
            {block && (
              <p className="text-sm text-emerald-700 dark:text-emerald-300" role="status">
                Using collection <code>{block.collection}</code>.
              </p>
            )}
            <ConnectionForm
              textDim={dimOf(text, platformDims.text)}
              imageDim={dimOf(image, platformDims.image)}
              onConnected={(c, b) => {
                setKnownConns((cs) => [...cs.filter((x) => x.id !== c.id), c]);
                setBlock(b);
              }}
            />
          </div>
        </ProviderChooser>
      </Card>

      <StepOutcomeView state={state} claimed={["/embedders/text/model_id", "/embedders/image/model_id"]} />
      <StepFooter step="retrieval" pending={pending} onContinue={onContinue} skippable disabled={storeChoice === "byo" && !block} />
    </StepShell>
  );
}
