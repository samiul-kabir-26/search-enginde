"use server";

import { refresh } from "next/cache";
import { cookies } from "next/headers";
import { redirect } from "next/navigation";

import { UPLOAD, api, errorsOf, refreshSession, tenantId } from "@/lib/api/server";
import type {
  Connection,
  ConnectionInput,
  ConnectionTestResult,
  FieldError,
  FormState,
  IndexJob,
  MappingPreviewResponse,
  Model,
  ModelInput,
  ModelValidation,
  PolicyTestResponse,
  SearchResponse,
  StepResult,
  VectorSpec,
  WizardStep,
} from "@/lib/api/types";
import { isWizardStep } from "@/lib/api/types";

export type StepOutcome = FormState & { data?: StepResult["data"]; nextStep?: string };

async function putStep(step: WizardStep, payload: unknown, query = ""): Promise<StepOutcome> {
  const tid = await tenantId();
  const res = await api<StepResult>(`/v1/tenants/${tid}/onboarding/steps/${step}${query}`, { method: "PUT", body: payload ?? {} });
  if (!res.ok) {
    if (res.status === 409) return { ok: false, message: res.problem.detail ?? "Complete the earlier steps first." };
    return { ok: false, ...errorsOf(res.problem) };
  }
  // The layout's stepper shows the onboarding state; refresh it with the step saved.
  refresh();
  return { ok: true, warnings: res.data.warnings, data: res.data.data, nextStep: res.data.next_step };
}

function go(next: string | undefined) {
  redirect(next && isWizardStep(next) ? `/onboarding/${next}` : "/onboarding/go_live");
}

/** Saves a configuration step into the draft and moves to the next step. */
export async function saveStep(step: WizardStep, payload: unknown): Promise<StepOutcome> {
  const out = await putStep(step, payload);
  if (!out.ok) return out;
  go(out.nextStep);
  return out;
}

/** Saves a step and stays on the page with its result (index build, go live). */
export async function saveStepInPlace(step: WizardStep, payload: unknown): Promise<StepOutcome> {
  return putStep(step, payload);
}

/** "Use defaults for the rest": completes retrieval through personalization with platform defaults. */
export async function applyDefaultsForRest(): Promise<StepOutcome> {
  const out = await putStep("retrieval", {}, "?use_defaults=true");
  if (!out.ok) return out;
  redirect("/onboarding/index_build");
}

/** Index build: publishes the draft and queues the index job from the catalog upload, if any. */
export async function publishAndBuild(): Promise<StepOutcome> {
  const upload = (await cookies()).get(UPLOAD)?.value;
  return putStep("index_build", upload ? { upload_id: upload } : {});
}

export async function getIndexJob(id: string): Promise<IndexJob | null> {
  const tid = await tenantId();
  const res = await api<IndexJob>(`/v1/tenants/${tid}/index/jobs/${encodeURIComponent(id)}`);
  return res.ok ? res.data : null;
}

export async function testPolicies(rules: { id: string; kind: string; expr: string }[], samples: unknown[], query?: string) {
  const tid = await tenantId();
  const res = await api<PolicyTestResponse>(`/v1/tenants/${tid}/policy/test`, { method: "POST", body: { rules, samples, query } });
  if (!res.ok) return { ok: false as const, ...errorsOf(res.problem) };
  return { ok: true as const, data: res.data };
}

export type ModelOutcome = FormState & { model?: Model; validation?: ModelValidation };

/** Registers a BYO model (embedder or scorer) and, for remote models, validates it at once. */
export async function registerModel(input: ModelInput & { secret?: string }): Promise<ModelOutcome> {
  const tid = await tenantId();
  const { secret, ...model } = input;
  if (secret) {
    const s = await api<{ secret_ref: string }>(`/v1/tenants/${tid}/secrets`, { method: "POST", body: { kind: "token", value: secret } });
    if (!s.ok) return { ok: false, ...prefixed(errorsOf(s.problem), "/secret") };
    model.secret_ref = s.data.secret_ref;
  }
  const res = await api<Model>(`/v1/tenants/${tid}/models`, { method: "POST", body: model, headers: { "Idempotency-Key": crypto.randomUUID() } });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  if (res.data.status === "validated") return { ok: true, model: res.data };
  return validateModel(res.data.id);
}

export async function validateModel(id: string): Promise<ModelOutcome> {
  const tid = await tenantId();
  const res = await api<ModelValidation>(`/v1/tenants/${tid}/models/${encodeURIComponent(id)}/validate`, { method: "POST" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  const model = await api<Model>(`/v1/tenants/${tid}/models/${encodeURIComponent(id)}`);
  return {
    ok: res.data.ok,
    validation: res.data,
    model: model.ok ? model.data : undefined,
    message: res.data.ok ? undefined : "Validation failed; see the checks below.",
  };
}

export type ConnectionOutcome = FormState & { connection?: Connection; test?: ConnectionTestResult };

/** Stores the credential as a secret, registers the connection and tests it against the collection. */
export async function createConnection(
  input: Omit<ConnectionInput, "secret_ref"> & { secret?: string; collection: string; vectors: Record<string, VectorSpec> },
): Promise<ConnectionOutcome> {
  const tid = await tenantId();
  let secretRef: string | undefined;
  if (input.secret) {
    const s = await api<{ secret_ref: string }>(`/v1/tenants/${tid}/secrets`, { method: "POST", body: { kind: "api_key", value: input.secret } });
    if (!s.ok) return { ok: false, ...prefixed(errorsOf(s.problem), "/secret") };
    secretRef = s.data.secret_ref;
  }
  const body: ConnectionInput = { kind: "vector_store", type: input.type, endpoint: input.endpoint, secret_ref: secretRef, settings: input.settings };
  const c = await api<Connection>(`/v1/tenants/${tid}/connections`, { method: "POST", body, headers: { "Idempotency-Key": crypto.randomUUID() } });
  if (!c.ok) return { ok: false, ...errorsOf(c.problem) };
  const t = await api<ConnectionTestResult>(`/v1/tenants/${tid}/connections/${encodeURIComponent(c.data.id)}/test`, {
    method: "POST",
    body: { collection: input.collection, vectors: input.vectors },
  });
  if (!t.ok) return { ok: false, connection: c.data, ...errorsOf(t.problem) };
  return { ok: t.data.ok, connection: c.data, test: t.data, message: t.data.ok ? undefined : "The connection test failed; see the checks below." };
}

function prefixed(e: { message?: string; errors: FieldError[] }, prefix: string) {
  return { message: e.message, errors: e.errors.map((x) => ({ ...x, path: prefix + x.path })) };
}

/** Go live: activates the latest version and creates the first API key (shown once). */
export async function goLive(): Promise<StepOutcome> {
  const out = await putStep("go_live", { mode: "activate" });
  // Email verification is a claim in the access token, so a token issued before the user
  // verified still reads unverified: refresh it and try once more.
  if (!out.ok && out.errors?.some((e) => e.code === "email_unverified") && (await refreshSession())) {
    return putStep("go_live", { mode: "activate" });
  }
  return out;
}

export async function startShadow(version: number, sampleRate: number): Promise<FormState> {
  const tid = await tenantId();
  const res = await api(`/v1/tenants/${tid}/config/versions/${version}/shadow`, { method: "POST", body: { sample_rate: sampleRate } });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  return { ok: true, message: `Version ${version} now shadows ${Math.round(sampleRate * 100)}% of live traffic.` };
}

/** Applies a field mapping to the samples without saving (the mapper's live preview). */
export async function previewMapping(fieldMapping: Record<string, string>, samples: unknown[]) {
  const tid = await tenantId();
  const res = await api<MappingPreviewResponse>(`/v1/tenants/${tid}/catalog/mapping/preview`, {
    method: "POST",
    body: { field_mapping: fieldMapping, samples: samples.slice(0, 20) },
  });
  if (!res.ok) return { ok: false as const, ...errorsOf(res.problem) };
  return { ok: true as const, data: res.data };
}

/** Text search in the playground against a version or the draft, with the explain trace. */
export async function playgroundSearch(configVersion: number | "draft", q: string) {
  const tid = await tenantId();
  const res = await api<SearchResponse>(`/v1/tenants/${tid}/playground/search`, {
    method: "POST",
    body: { config_version: configVersion, q, page: { size: 20 } },
  });
  if (!res.ok) return { ok: false as const, ...errorsOf(res.problem) };
  return { ok: true as const, data: res.data };
}
