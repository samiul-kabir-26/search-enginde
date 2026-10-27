"use client";

import { useActionState } from "react";

import { adminLogin } from "@/app/actions/auth";
import { Button, ErrorSummary, Field } from "@/components/ui";
import { initialFormState } from "@/lib/api/types";

export function AdminLoginForm() {
  const [state, action, pending] = useActionState(adminLogin, initialFormState);
  const v = state.values ?? {};
  return (
    <form action={action} className="flex flex-col gap-4" noValidate key={JSON.stringify(v)}>
      <ErrorSummary message={state.message} errors={state.errors} />
      <Field path="/email" label="Email" name="email" type="email" autoComplete="email" required defaultValue={v.email} />
      <Field path="/password" label="Password" name="password" type="password" autoComplete="current-password" required />
      <Button type="submit" disabled={pending}>
        {pending ? "Signing in…" : "Sign in"}
      </Button>
    </form>
  );
}
