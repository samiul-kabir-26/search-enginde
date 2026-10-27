import { NextResponse, type NextRequest } from "next/server";

import { api, tenantId } from "@/lib/api/server";
import type { PresignedUpload } from "@/lib/api/types";

/** ONNX models are refused above this size (docs/MODELS.md §6). */
const MAX_BYTES = 200 << 20;

/** BYO ONNX scorer upload: presigns with control-api and PUTs the model to storage from the server. */
export async function POST(req: NextRequest) {
  const form = await req.formData().catch(() => null);
  const file = form?.get("file");
  if (!(file instanceof File) || file.size === 0) {
    return NextResponse.json({ detail: "Choose an .onnx model file." }, { status: 400 });
  }
  if (file.size > MAX_BYTES) {
    return NextResponse.json({ detail: "ONNX models are limited to 200 MB." }, { status: 413 });
  }
  const tid = await tenantId();
  const presign = await api<PresignedUpload>(`/v1/tenants/${tid}/models/uploads`, { method: "POST", body: { format: "onnx" } });
  if (!presign.ok) {
    return NextResponse.json({ detail: presign.problem.detail ?? "Model uploads are unavailable." }, { status: presign.status });
  }
  const put = await fetch(presign.data.url, { method: "PUT", headers: presign.data.headers, body: await file.arrayBuffer() }).catch(() => null);
  if (!put?.ok) return NextResponse.json({ detail: "Uploading the model to storage failed; try again." }, { status: 502 });
  return NextResponse.json({ upload_id: presign.data.upload_id });
}
