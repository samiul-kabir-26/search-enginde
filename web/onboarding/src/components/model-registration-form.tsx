"use client";

import { useId, useState } from "react";

import { registerModel, type ModelOutcome } from "@/app/onboarding/actions";
import type { Model, ModelInput } from "@/lib/api/types";

import { Checks } from "./checks";
import { Button, ErrorSummary, Field, Select, TextArea } from "./ui";

type Kind = ModelInput["kind"];
type Type = ModelInput["type"];

const TYPE_LABELS: Record<Type, string> = {
  kserve_v2: "KServe v2 endpoint (Triton, KServe, Seldon…)",
  openai_embeddings: "OpenAI-compatible embeddings API",
  onnx: "ONNX file (runs in-process)",
};

/**
 * Registers a BYO model and shows its validation checks: an embedder (remote) or a scorer
 * (remote kserve_v2, or an uploaded ONNX file dry-run against its declared features).
 */
export function ModelRegistrationForm({
  kind,
  modality,
  types,
  defaultFeatures = [],
  onRegistered,
}: {
  kind: Kind;
  modality?: "text" | "image";
  types: Type[];
  defaultFeatures?: string[];
  onRegistered: (m: Model) => void;
}) {
  const fileId = useId();
  const [type, setType] = useState<Type>(types[0]);
  const [busy, setBusy] = useState(false);
  const [out, setOut] = useState<ModelOutcome | null>(null);

  async function onSubmit(form: HTMLFormElement) {
    const f = new FormData(form);
    const get = (k: string) => String(f.get(k) ?? "").trim();
    setBusy(true);
    setOut(null);
    try {
      const input: ModelInput & { secret?: string } = {
        kind,
        type,
        name: get("name"),
        version: get("version"),
        input_schema: {},
      };
      if (kind === "embedder") {
        input.input_schema.modality = modality;
        input.output_schema = { dim: Number(get("dim")) };
        if (modality === "image") input.supports_text_to_image = f.get("t2i") === "on";
      } else {
        input.input_schema.features = get("features")
          .split(/[\s,]+/)
          .filter(Boolean);
      }
      if (type === "onnx") {
        const file = (f.get("file") as File | null) ?? null;
        if (!file || file.size === 0) {
          setOut({ ok: false, errors: [{ path: "/upload_id", code: "required", message: "Choose an .onnx file." }] });
          return;
        }
        const body = new FormData();
        body.set("file", file);
        const res = await fetch("/api/models/upload", { method: "POST", body });
        const data = (await res.json().catch(() => ({}))) as { upload_id?: string; detail?: string };
        if (!res.ok || !data.upload_id) {
          setOut({ ok: false, message: data.detail ?? "The upload failed." });
          return;
        }
        input.upload_id = data.upload_id;
      } else {
        input.endpoint = get("endpoint");
        if (get("secret")) input.secret = get("secret");
      }
      const res = await registerModel(input);
      setOut(res);
      if (res.ok && res.model) onRegistered(res.model);
    } finally {
      setBusy(false);
    }
  }

  const errs = out?.errors;
  return (
    <form
      className="flex flex-col gap-3"
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        void onSubmit(e.currentTarget);
      }}
    >
      {types.length > 1 && (
        <Field path="/type" label="Model type" errors={errs}>
          {(p) => (
            <Select {...p} value={type} onChange={(e) => setType(e.target.value as Type)}>
              {types.map((t) => (
                <option key={t} value={t}>
                  {TYPE_LABELS[t]}
                </option>
              ))}
            </Select>
          )}
        </Field>
      )}
      <div className="grid gap-3 sm:grid-cols-2">
        <Field path="/name" label={type === "openai_embeddings" ? "Model (the API's model field)" : "Model name"} name="name" required errors={errs} />
        <Field path="/version" label="Version" name="version" hint="Optional" errors={errs} />
      </div>
      {type === "onnx" ? (
        <Field path="/upload_id" label="ONNX file (≤ 200 MB)" errors={errs}>
          {(p) => <input {...p} id={fileId} name="file" type="file" accept=".onnx" className="text-sm" />}
        </Field>
      ) : (
        <>
          <Field path="/endpoint" label="Endpoint URL" name="endpoint" type="url" placeholder="https://models.example.com" required errors={errs} hint="Public HTTPS only." />
          <Field path="/secret/value" label="Bearer token" name="secret" type="password" autoComplete="off" hint="Optional. Stored encrypted; never shown again." errors={errs} />
        </>
      )}
      {kind === "embedder" ? (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field path="/output_schema/dim" label="Embedding dimension" name="dim" type="number" min={1} max={8192} required errors={errs} />
          {modality === "image" && (
            <label className="flex items-center gap-2 self-end pb-2 text-sm">
              <input type="checkbox" name="t2i" /> Has a text tower (text-to-image search)
            </label>
          )}
        </div>
      ) : (
        <Field path="/input_schema/features" label="Input features, in model order" errors={errs} hint="Feature catalog names separated by spaces, commas or new lines (docs/MODELS.md §4).">
          {(p) => <TextArea {...p} name="features" rows={3} defaultValue={defaultFeatures.join("\n")} />}
        </Field>
      )}
      <ErrorSummary message={out?.ok ? undefined : out?.message} errors={errs} />
      <div aria-live="polite">
        {out?.validation && <Checks checks={out.validation.checks} />}
        {out?.ok && out.model && (
          <p className="text-sm text-emerald-700 dark:text-emerald-300">
            {out.model.name} is {out.model.status}.
          </p>
        )}
      </div>
      <div>
        <Button type="submit" variant="secondary" disabled={busy}>
          {busy ? "Registering and validating…" : "Register and validate"}
        </Button>
      </div>
    </form>
  );
}
