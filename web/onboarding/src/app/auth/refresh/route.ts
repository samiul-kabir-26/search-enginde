import { NextResponse, type NextRequest } from "next/server";

import { refreshSession } from "@/lib/api/server";

/**
 * Server Components cannot store a rotated refresh token, so they send an expired session here:
 * this handler rotates it, stores the new cookies and returns to the page.
 */
export async function GET(req: NextRequest) {
  const raw = req.nextUrl.searchParams.get("next") ?? "/onboarding";
  const next = raw.startsWith("/") && !raw.startsWith("//") ? raw : "/onboarding";
  const token = await refreshSession();
  const target = token ? next : `/login?next=${encodeURIComponent(next)}`;
  return NextResponse.redirect(new URL(target, req.url));
}
