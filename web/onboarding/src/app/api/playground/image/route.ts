import { NextResponse, type NextRequest } from "next/server";

import { api, tenantId } from "@/lib/api/server";

/** Image search in the playground: forwards the multipart form to control-api's playground proxy. */
export async function POST(req: NextRequest) {
  const version = req.nextUrl.searchParams.get("config_version") ?? "draft";
  const form = await req.formData().catch(() => null);
  if (!form || !(form.get("image") instanceof File)) {
    return NextResponse.json({ detail: "Choose a JPEG, PNG or WebP image." }, { status: 400 });
  }
  const tid = await tenantId();
  const res = await api<unknown>(`/v1/tenants/${tid}/playground/search/image?config_version=${encodeURIComponent(version)}`, { method: "POST", raw: form });
  if (!res.ok) return NextResponse.json(res.problem, { status: res.status });
  return NextResponse.json(res.data);
}
