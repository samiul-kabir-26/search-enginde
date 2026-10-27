import "server-only";

import type { TenantConfig } from "../config";

import { apiOrLogin, tenantId } from "./server";
import type { Connection, DraftView, EngineExplainer, IndexJob, Model, OnboardingState, PlatformModel } from "./types";

/** Server Component data access for the signed-in tenant. `next` is where to return after a re-login. */

export async function onboardingState(next: string) {
  const tid = await tenantId();
  return apiOrLogin<OnboardingState>(`/v1/tenants/${tid}/onboarding`, next);
}

export async function draft(next: string): Promise<{ view: DraftView; config: TenantConfig }> {
  const tid = await tenantId();
  const view = await apiOrLogin<DraftView>(`/v1/tenants/${tid}/config/draft`, next);
  return { view, config: view.document as unknown as TenantConfig };
}

export async function explainer(next: string) {
  const tid = await tenantId();
  return apiOrLogin<EngineExplainer>(`/v1/tenants/${tid}/engine-explainer`, next);
}

export async function platformModels(next: string) {
  return (await apiOrLogin<{ items: PlatformModel[] }>("/v1/platform/models", next)).items;
}

export async function models(next: string) {
  const tid = await tenantId();
  return (await apiOrLogin<{ items: Model[] }>(`/v1/tenants/${tid}/models?limit=100`, next)).items;
}

export async function connections(next: string) {
  const tid = await tenantId();
  return (await apiOrLogin<{ items: Connection[] }>(`/v1/tenants/${tid}/connections?limit=100`, next)).items;
}

export async function versions(next: string) {
  const tid = await tenantId();
  return (await apiOrLogin<{ items: { version: number; state: string; created_at: string }[] }>(`/v1/tenants/${tid}/config/versions?limit=20`, next)).items;
}

export async function indexJobs(next: string) {
  const tid = await tenantId();
  return (await apiOrLogin<{ items: IndexJob[] }>(`/v1/tenants/${tid}/index/jobs`, next)).items;
}
