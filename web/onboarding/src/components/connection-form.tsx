"use client";

import { useState } from "react";

import { createConnection, type ConnectionOutcome } from "@/app/onboarding/actions";
import type { Connection, VectorSpec } from "@/lib/api/types";

import { Checks } from "./checks";
import { Button, ErrorSummary, Field, Select } from "./ui";

type StoreType = "qdrant" | "pgvector" | "milvus";
type Metric = VectorSpec["metric"];

export type VectorStoreBlock = { connection_id: string; collection: string; vectors: Record<string, VectorSpec> };

const PLACEHOLDERS: Record<StoreType, string> = {
  qdrant: "https://qdrant.example.com:6334",
  pgvector: "postgres://search@db.example.com:5432/catalog?sslmode=verify-full",
  milvus: "https://milvus.example.com:19530",
};

/** Registers a BYO vector store connection and runs its test against the collection. */
export function ConnectionForm({
  textDim,
  imageDim,
  onConnected,
}: {
  textDim: number;
  imageDim: number;
  onConnected: (c: Connection, block: VectorStoreBlock) => void;
}) {
  const [type, setType] = useState<StoreType>("qdrant");
  const [withImage, setWithImage] = useState(true);
  const [busy, setBusy] = useState(false);
  const [out, setOut] = useState<ConnectionOutcome | null>(null);

  async function onSubmit(form: HTMLFormElement) {
    const f = new FormData(form);
    const get = (k: string) => String(f.get(k) ?? "").trim();
    const metric = (get("metric") || "cosine") as Metric;
    const vectors: Record<string, VectorSpec> = { text: { dim: Number(get("text_dim")), metric } };
    if (withImage) vectors.image = { dim: Number(get("image_dim")), metric };
    setBusy(true);
    setOut(null);
    try {
      const settings = type === "milvus" ? { username: get("username") || undefined, database: get("database") || undefined } : undefined;
      const collection = get("collection");
      const res = await createConnection({ kind: "vector_store", type, endpoint: get("endpoint"), secret: get("secret") || undefined, settings, collection, vectors });
      setOut(res);
      if (res.ok && res.connection) onConnected(res.connection, { connection_id: res.connection.id, collection, vectors });
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
      <div className="grid gap-3 sm:grid-cols-2">
        <Field path="/type" label="Store" errors={errs}>
          {(p) => (
            <Select {...p} value={type} onChange={(e) => setType(e.target.value as StoreType)}>
              <option value="qdrant">Qdrant</option>
              <option value="pgvector">PostgreSQL + pgvector</option>
              <option value="milvus">Milvus</option>
            </Select>
          )}
        </Field>
        <Field path="/collection" label={type === "pgvector" ? "Table ([schema.]table)" : "Collection"} name="collection" required errors={errs} />
      </div>
      <Field path="/endpoint" label="Endpoint" name="endpoint" placeholder={PLACEHOLDERS[type]} required errors={errs} hint="Public addresses only; credentials go in the field below, never in the URL." />
      {type === "milvus" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field path="/settings/username" label="Milvus user" name="username" errors={errs} />
          <Field path="/settings/database" label="Milvus database" name="database" errors={errs} />
        </div>
      )}
      <Field
        path="/secret/value"
        label={type === "qdrant" ? "API key" : type === "pgvector" ? "Password" : "Password or token"}
        name="secret"
        type="password"
        autoComplete="off"
        hint="Stored encrypted; never shown again."
        errors={errs}
      />
      <div className="grid gap-3 sm:grid-cols-3">
        <Field path="/vectors/text/dim" label="Text vector dimension" name="text_dim" type="number" defaultValue={textDim} errors={errs} />
        <Field path="/vectors/image/dim" label="Image vector dimension" name="image_dim" type="number" defaultValue={imageDim} disabled={!withImage} errors={errs} />
        <Field path="/vectors/text/metric" label="Metric" errors={errs}>
          {(p) => (
            <Select {...p} name="metric" defaultValue="cosine">
              <option value="cosine">Cosine</option>
              <option value="dot">Dot product</option>
              <option value="l2">L2 distance</option>
            </Select>
          )}
        </Field>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={withImage} onChange={(e) => setWithImage(e.target.checked)} /> The collection also stores image vectors
      </label>
      <ErrorSummary message={out?.ok ? undefined : out?.message} errors={errs} />
      <div aria-live="polite">
        {out?.test && <Checks checks={out.test.checks} />}
        {out?.ok && <p className="text-sm text-emerald-700 dark:text-emerald-300">Connection validated.</p>}
      </div>
      <div>
        <Button type="submit" variant="secondary" disabled={busy}>
          {busy ? "Testing connection…" : "Save and test connection"}
        </Button>
      </div>
    </form>
  );
}
