import { cookies } from "next/headers";
import { NextResponse, type NextRequest } from "next/server";

import { UPLOAD, api, tenantId } from "@/lib/api/server";
import type { PresignedUpload } from "@/lib/api/types";

/** Catalog files larger than this go through the event stream instead. */
const MAX_BYTES = 100 << 20;
const MAX_SAMPLES = 20;

/**
 * Batch catalog upload (onboarding step 2): presigns an upload with control-api, PUTs the JSONL
 * to object storage from the server, and remembers the upload for the index build step. The
 * first products come back as samples for the field mapper.
 */
export async function POST(req: NextRequest) {
  const form = await req.formData().catch(() => null);
  const file = form?.get("file");
  if (!(file instanceof File) || file.size === 0) {
    return NextResponse.json({ detail: "Choose a JSONL file with one product per line." }, { status: 400 });
  }
  if (file.size > MAX_BYTES) {
    return NextResponse.json({ detail: `The file is larger than ${MAX_BYTES >> 20} MB; stream it as catalog events instead.` }, { status: 413 });
  }
  const text = await file.text();
  const samples: unknown[] = [];
  let lines = 0;
  for (const [i, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (!line) continue;
    lines++;
    if (samples.length >= MAX_SAMPLES) continue;
    try {
      const doc: unknown = JSON.parse(line);
      if (doc && typeof doc === "object" && !Array.isArray(doc)) samples.push(doc);
    } catch {
      return NextResponse.json({ detail: `Line ${i + 1} is not valid JSON. Each line must be one product object.` }, { status: 400 });
    }
  }
  if (samples.length === 0) {
    return NextResponse.json({ detail: "The file has no product objects." }, { status: 400 });
  }

  const tid = await tenantId();
  const presign = await api<PresignedUpload>(`/v1/tenants/${tid}/catalog/uploads`, { method: "POST", body: { format: "jsonl" } });
  if (!presign.ok) {
    return NextResponse.json({ detail: presign.problem.detail ?? "Uploads are unavailable." }, { status: presign.status });
  }
  const put = await fetch(presign.data.url, { method: "PUT", headers: presign.data.headers, body: text }).catch(() => null);
  if (!put?.ok) {
    return NextResponse.json({ detail: "Uploading the file to storage failed; try again." }, { status: 502 });
  }
  (await cookies()).set(UPLOAD, presign.data.upload_id, {
    httpOnly: true,
    sameSite: "lax",
    secure: process.env.NODE_ENV === "production",
    path: "/",
    maxAge: 24 * 3600,
  });
  return NextResponse.json({ upload_id: presign.data.upload_id, lines, samples });
}
