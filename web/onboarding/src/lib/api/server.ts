import "server-only";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";

import type { FieldError, Problem, SessionResponse } from "./types";

/**
 * Server-side access to control-api. Tokens live in httpOnly cookies set by Server Actions and
 * Route Handlers; client JS never sees them (docs/ONBOARDING.md §5).
 */

export const CONTROL_API_URL = process.env.CONTROL_API_URL ?? "http://localhost:8081";

const ACCESS = "se_at";
const REFRESH = "se_rt";
const TENANT = "se_tid";
const ADMIN = "se_admin_at";
/** The catalog upload the index build step backfills from. */
export const UPLOAD = "se_upload";

const secure = process.env.NODE_ENV === "production";

export type ApiResult<T> =
  | { ok: true; status: number; data: T }
  | { ok: false; status: number; problem: Problem };

type Auth = "tenant" | "admin" | "none";

type Options = {
  method?: string;
  body?: unknown;
  auth?: Auth;
  headers?: Record<string, string>;
  /** A raw body (multipart, bytes) sent as is; `body` is JSON-encoded otherwise. */
  raw?: BodyInit;
};

function problemOf(status: number, detail: string): Problem {
  return { type: "about:blank", title: "Error", status, detail };
}

async function send<T>(path: string, opts: Options, token?: string): Promise<{ res: Response | null; result: ApiResult<T> }> {
  const headers: Record<string, string> = { Accept: "application/json", ...opts.headers };
  if (token) headers.Authorization = `Bearer ${token}`;
  let body: BodyInit | undefined = opts.raw;
  if (body === undefined && opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  let res: Response;
  try {
    res = await fetch(CONTROL_API_URL + path, { method: opts.method ?? "GET", headers, body, cache: "no-store" });
  } catch {
    return { res: null, result: { ok: false, status: 503, problem: problemOf(503, "The control API is unreachable.") } };
  }
  const text = await res.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
  }
  if (res.ok) return { res, result: { ok: true, status: res.status, data: parsed as T } };
  const problem = (parsed as Problem | undefined) ?? problemOf(res.status, res.statusText || "Request failed");
  return { res, result: { ok: false, status: res.status, problem } };
}

/**
 * Calls control-api as the signed-in tenant user (or admin). From a Server Action or Route
 * Handler an expired access token is refreshed and the rotated cookies are stored; from a
 * Server Component, which cannot set cookies, the caller is sent through /auth/refresh instead
 * (refreshing without storing the rotated token would trip reuse detection).
 */
export async function api<T>(path: string, opts: Options = {}): Promise<ApiResult<T>> {
  const auth = opts.auth ?? "tenant";
  const jar = await cookies();
  const token = auth === "none" ? undefined : jar.get(auth === "admin" ? ADMIN : ACCESS)?.value;
  const first = await send<T>(path, opts, token);
  if (first.result.status !== 401 || auth !== "tenant") return first.result;
  const refreshed = await refreshSession();
  if (!refreshed) return first.result;
  return (await send<T>(path, opts, refreshed)).result;
}

/** For Server Components: the tenant API call, or a redirect to sign in again. */
export async function apiOrLogin<T>(path: string, next: string, opts: Options = {}): Promise<T> {
  const jar = await cookies();
  const token = jar.get(ACCESS)?.value;
  if (!token) {
    redirect(jar.get(REFRESH) ? `/auth/refresh?next=${encodeURIComponent(next)}` : "/login");
  }
  const { result } = await send<T>(path, opts, token);
  if (result.ok) return result.data;
  if (result.status === 401) redirect(`/auth/refresh?next=${encodeURIComponent(next)}`);
  throw new Error(`${path}: ${result.status} ${result.problem.detail ?? result.problem.title}`);
}

/** The signed-in tenant ID, or a redirect to /login. */
export async function tenantId(): Promise<string> {
  const tid = (await cookies()).get(TENANT)?.value;
  if (!tid) redirect("/login");
  return tid;
}

export async function hasAdminSession(): Promise<boolean> {
  return Boolean((await cookies()).get(ADMIN)?.value);
}

function refreshFrom(res: Response): string | undefined {
  for (const c of res.headers.getSetCookie()) {
    const m = /^se_refresh=([^;]*)/.exec(c);
    if (m) return m[1];
  }
  return undefined;
}

/** Stores a new session: the access token, control-api's rotating refresh token and the tenant. */
async function storeSession(s: SessionResponse, refresh: string | undefined) {
  const jar = await cookies();
  const base = { httpOnly: true, secure, sameSite: "lax" as const, path: "/" };
  jar.set(ACCESS, s.access_token, { ...base, maxAge: s.expires_in });
  if (refresh) jar.set(REFRESH, refresh, { ...base, maxAge: 30 * 24 * 3600 });
  if (s.tenant_id) jar.set(TENANT, s.tenant_id, { ...base, maxAge: 30 * 24 * 3600 });
}

/** Sign-up and login: the session response plus the refresh cookie control-api set. */
export async function startSession(path: "/v1/signup" | "/v1/auth/login", body: unknown, idemKey?: string): Promise<ApiResult<SessionResponse>> {
  const { res, result } = await send<SessionResponse>(path, {
    method: "POST",
    body,
    headers: idemKey ? { "Idempotency-Key": idemKey } : undefined,
  });
  if (result.ok && res) await storeSession(result.data, refreshFrom(res));
  return result;
}

/** Rotates the refresh token; returns the new access token, or undefined (signed out). */
export async function refreshSession(): Promise<string | undefined> {
  const jar = await cookies();
  const rt = jar.get(REFRESH)?.value;
  if (!rt) return undefined;
  const { res, result } = await send<SessionResponse>("/v1/auth/refresh", {
    method: "POST",
    headers: { Cookie: `se_refresh=${rt}` },
  });
  if (!result.ok || !res) {
    await clearSession();
    return undefined;
  }
  await storeSession(result.data, refreshFrom(res));
  return result.data.access_token;
}

export async function endSession() {
  const jar = await cookies();
  const rt = jar.get(REFRESH)?.value;
  if (rt) await send("/v1/auth/logout", { method: "POST", headers: { Cookie: `se_refresh=${rt}` } });
  await clearSession();
}

async function clearSession() {
  const jar = await cookies();
  for (const name of [ACCESS, REFRESH, TENANT, UPLOAD]) jar.delete(name);
}

export async function startAdminSession(body: unknown): Promise<ApiResult<SessionResponse>> {
  const { result } = await send<SessionResponse>("/v1/admin/login", { method: "POST", body });
  if (result.ok) {
    (await cookies()).set(ADMIN, result.data.access_token, {
      httpOnly: true,
      secure,
      sameSite: "lax",
      path: "/",
      maxAge: result.data.expires_in,
    });
  }
  return result;
}

export async function endAdminSession() {
  (await cookies()).delete(ADMIN);
}

/** Field errors from a failed call, with a form-level message for anything without a path. */
export function errorsOf(problem: Problem): { message?: string; errors: FieldError[] } {
  const errors = problem.errors ?? [];
  const message = errors.length > 0 ? undefined : (problem.detail ?? problem.title);
  return { message, errors };
}

/** For admin Server Components: the admin API call, or a redirect to the admin login. */
export async function adminApiOrLogin<T>(path: string): Promise<T> {
  const token = (await cookies()).get(ADMIN)?.value;
  if (!token) redirect("/admin/login");
  const { result } = await send<T>(path, {}, token);
  if (result.ok) return result.data;
  if (result.status === 401 || result.status === 403) redirect("/admin/login");
  throw new Error(`${path}: ${result.status} ${result.problem.detail ?? result.problem.title}`);
}
