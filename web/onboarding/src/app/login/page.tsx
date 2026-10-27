import type { Metadata } from "next";
import { Suspense } from "react";

import { AuthShell } from "@/components/auth-shell";

import { LoginForm } from "./login-form";

export const metadata: Metadata = { title: "Sign in · Search Engine" };

async function Form({ searchParams }: { searchParams: PageProps<"/login">["searchParams"] }) {
  const next = (await searchParams).next;
  return <LoginForm next={typeof next === "string" ? next : "/onboarding"} />;
}

export default function LoginPage(props: PageProps<"/login">) {
  return (
    <AuthShell title="Sign in" subtitle="Continue setting up your search engine.">
      <Suspense fallback={<p className="text-sm text-zinc-500">Loading…</p>}>
        <Form searchParams={props.searchParams} />
      </Suspense>
    </AuthShell>
  );
}
