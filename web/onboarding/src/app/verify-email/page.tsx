import type { Metadata } from "next";
import Link from "next/link";
import { Suspense } from "react";

import { AuthShell } from "@/components/auth-shell";
import { api } from "@/lib/api/server";

export const metadata: Metadata = { title: "Verify your email · Search Engine" };

async function Verify({ searchParams }: { searchParams: PageProps<"/verify-email">["searchParams"] }) {
  const token = (await searchParams).token;
  if (typeof token !== "string" || !token) {
    return <p role="alert">This link has no verification token. Open the link from the email again.</p>;
  }
  // Verifying is idempotent and sets no cookies, so it can run while rendering the link's page.
  const res = await api("/v1/auth/verify-email", { method: "POST", body: { token }, auth: "none" });
  if (!res.ok) {
    return (
      <div role="alert" className="flex flex-col gap-2">
        <p>{res.problem.detail ?? "This link is invalid or has expired."}</p>
        <p className="text-sm text-zinc-600 dark:text-zinc-400">Sign in and request a new link from the Go live step.</p>
      </div>
    );
  }
  return (
    <div role="status" className="flex flex-col gap-3">
      <p>Your email address is verified.</p>
      <Link className="text-indigo-700 underline dark:text-indigo-300" href="/onboarding">
        Continue setup
      </Link>
    </div>
  );
}

export default function VerifyEmailPage(props: PageProps<"/verify-email">) {
  return (
    <AuthShell title="Verify your email">
      <Suspense fallback={<p className="text-sm text-zinc-500">Verifying…</p>}>
        <Verify searchParams={props.searchParams} />
      </Suspense>
    </AuthShell>
  );
}
