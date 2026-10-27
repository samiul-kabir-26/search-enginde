import type { Metadata } from "next";

import { AuthShell } from "@/components/auth-shell";

import { SignupForm } from "./signup-form";

export const metadata: Metadata = { title: "Create your workspace · Search Engine" };

export default function SignupPage() {
  return (
    <AuthShell title="Create your workspace" subtitle="Step 0 of setup: your account. Everything after this can start from platform defaults.">
      <SignupForm />
    </AuthShell>
  );
}
