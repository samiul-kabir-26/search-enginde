"use server";

import { refresh } from "next/cache";

import { api, errorsOf } from "@/lib/api/server";
import type { FormState, Plan, PlatformSettings } from "@/lib/api/types";

/** Platform settings are saved whole (auth and ads sections), as PUT /v1/admin/settings takes them. */
export async function saveSettings(settings: PlatformSettings): Promise<FormState> {
  const res = await api<PlatformSettings>("/v1/admin/settings", { method: "PUT", body: settings, auth: "admin" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  refresh();
  return { ok: true, message: "Settings saved. Services pick them up within seconds." };
}

export async function savePlan(plan: Plan & { id: string }): Promise<FormState> {
  const { id, ...body } = plan;
  const res = await api<Plan>(`/v1/admin/plans/${encodeURIComponent(id)}`, { method: "PUT", body, auth: "admin" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  refresh();
  return { ok: true, message: `Plan ${id} saved.` };
}

export async function rotateKey(): Promise<FormState> {
  const res = await api<{ active_kid: string }>("/v1/admin/keys/rotate", { method: "POST", auth: "admin" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  refresh();
  return { ok: true, message: `New signing key ${res.data.active_kid} is active.` };
}

export async function revokeKey(kid: string): Promise<FormState> {
  const res = await api(`/v1/admin/keys/${encodeURIComponent(kid)}/revoke`, { method: "POST", auth: "admin" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  refresh();
  return { ok: true, message: `Key ${kid} revoked; tokens it signed stop verifying.` };
}
