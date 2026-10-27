"use server";

import { redirect } from "next/navigation";

import { api, endAdminSession, endSession, errorsOf, startAdminSession, startSession } from "@/lib/api/server";
import type { FieldError, FormState } from "@/lib/api/types";

function str(form: FormData, key: string): string {
  return String(form.get(key) ?? "").trim();
}

/** Same-origin paths only, so ?next= cannot send the user elsewhere. */
function safeNext(next: string, fallback: string): string {
  return next.startsWith("/") && !next.startsWith("//") ? next : fallback;
}

/** Step 0 (Account): creates the tenant and owner, then starts the wizard. */
export async function signup(_: FormState, form: FormData): Promise<FormState> {
  const body = {
    company_name: str(form, "company_name"),
    slug: str(form, "slug").toLowerCase(),
    region: str(form, "region"),
    plan: str(form, "plan"),
    email: str(form, "email"),
    password: String(form.get("password") ?? ""),
  };
  // Client-side checks are for UX; control-api is the authority and its errors map to the same paths.
  const errors: FieldError[] = [];
  if (!body.company_name) errors.push({ path: "/company_name", code: "required", message: "Enter your company name." });
  if (!/^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(body.slug))
    errors.push({ path: "/slug", code: "invalid", message: "Use 3–63 lowercase letters, digits or hyphens, not starting or ending with a hyphen." });
  if (!body.email.includes("@")) errors.push({ path: "/email", code: "invalid", message: "Enter a valid email address." });
  if (body.password.length < 12) errors.push({ path: "/password", code: "weak", message: "Use at least 12 characters." });
  const values = { company_name: body.company_name, slug: body.slug, region: body.region, plan: body.plan, email: body.email };
  if (errors.length > 0) return { ok: false, errors, values };

  const res = await startSession("/v1/signup", body);
  if (!res.ok) return { ok: false, ...errorsOf(res.problem), values };
  redirect("/onboarding/engine_tour");
}

export async function login(_: FormState, form: FormData): Promise<FormState> {
  const values = { tenant: str(form, "tenant").toLowerCase(), email: str(form, "email") };
  const res = await startSession("/v1/auth/login", { ...values, password: String(form.get("password") ?? "") });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem), values };
  redirect(safeNext(str(form, "next"), "/onboarding"));
}

export async function logout() {
  await endSession();
  redirect("/");
}

export async function resendVerification(): Promise<FormState> {
  const res = await api("/v1/auth/resend-verification", { method: "POST" });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem) };
  return { ok: true, message: "We sent a new verification link. Check your inbox." };
}

export async function adminLogin(_: FormState, form: FormData): Promise<FormState> {
  const values = { email: str(form, "email") };
  const res = await startAdminSession({ ...values, password: String(form.get("password") ?? "") });
  if (!res.ok) return { ok: false, ...errorsOf(res.problem), values };
  redirect("/admin/settings");
}

export async function adminLogout() {
  await endAdminSession();
  redirect("/admin/login");
}
