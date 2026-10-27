"use client";

import Link from "next/link";
import { useActionState } from "react";

import { login } from "@/app/actions/auth";
import { Button, ErrorSummary, Field } from "@/components/ui";
import { initialFormState } from "@/lib/api/types";

export function LoginForm({ next }: { next: string }) {
  const [state, action, pending] = useActionState(login, initialFormState);
  const v = state.values ?? {};
  return (
    <form action={action} className="flex flex-col gap-4" noValidate key={JSON.stringify(v)}>
      <ErrorSummary message={state.message} errors={state.errors} />
      <input type="hidden" name="next" value={next} />
      <Field path="/tenant" label="Workspace slug" name="tenant" required defaultValue={v.tenant} autoComplete="organization" />
      <Field path="/email" label="Email" name="email" type="email" autoComplete="email" required defaultValue={v.email} />
      <Field path="/password" label="Password" name="password" type="password" autoComplete="current-password" required />
      <Button type="submit" disabled={pending}>
        {pending ? "Signing in…" : "Sign in"}
      </Button>
      <p className="text-sm text-zinc-600 dark:text-zinc-400">
        New here?{" "}
        <Link className="text-indigo-700 underline dark:text-indigo-300" href="/signup">
          Create a workspace
        </Link>
      </p>
    </form>
  );
}
