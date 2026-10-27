"use client";

import Link from "next/link";
import { useActionState } from "react";

import { signup } from "@/app/actions/auth";
import { Button, ErrorSummary, Field, Select } from "@/components/ui";
import { initialFormState } from "@/lib/api/types";

const PLANS = [
  { id: "starter", label: "Starter: platform defaults only" },
  { id: "growth", label: "Growth: bring your own models and stores" },
  { id: "enterprise", label: "Enterprise" },
];

const REGIONS = [
  { id: "us", label: "United States" },
  { id: "eu", label: "European Union" },
  { id: "ap", label: "Asia Pacific" },
];

export function SignupForm() {
  const [state, action, pending] = useActionState(signup, initialFormState);
  const v = state.values ?? {};
  const claimed = ["/company_name", "/slug", "/region", "/plan", "/email", "/password"];
  return (
    <form action={action} className="flex flex-col gap-4" noValidate key={JSON.stringify(v)}>
      <ErrorSummary message={state.message} errors={state.errors?.filter((e) => !claimed.includes(e.path))} />
      <Field path="/company_name" label="Company name" name="company_name" autoComplete="organization" required defaultValue={v.company_name} errors={state.errors} />
      <Field
        path="/slug"
        label="Workspace slug"
        name="slug"
        required
        defaultValue={v.slug}
        hint="Used to sign in, e.g. acme-shop. Lowercase letters, digits and hyphens."
        errors={state.errors}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        <Field path="/region" label="Region" errors={state.errors}>
          {(p) => (
            <Select {...p} name="region" defaultValue={v.region ?? "us"}>
              {REGIONS.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field path="/plan" label="Plan" errors={state.errors}>
          {(p) => (
            <Select {...p} name="plan" defaultValue={v.plan ?? "growth"}>
              {PLANS.map((pl) => (
                <option key={pl.id} value={pl.id}>
                  {pl.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </div>
      <Field path="/email" label="Work email" name="email" type="email" autoComplete="email" required defaultValue={v.email} errors={state.errors} />
      <Field
        path="/password"
        label="Password"
        name="password"
        type="password"
        autoComplete="new-password"
        required
        hint="At least 12 characters."
        errors={state.errors}
      />
      <Button type="submit" disabled={pending}>
        {pending ? "Creating your workspace…" : "Create workspace"}
      </Button>
      <p className="text-sm text-zinc-600 dark:text-zinc-400">
        Already have a workspace?{" "}
        <Link className="text-indigo-700 underline dark:text-indigo-300" href="/login">
          Sign in
        </Link>
      </p>
    </form>
  );
}
