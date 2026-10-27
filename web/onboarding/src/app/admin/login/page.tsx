import type { Metadata } from "next";

import { AuthShell } from "@/components/auth-shell";

import { AdminLoginForm } from "./admin-login-form";

export const metadata: Metadata = { title: "Platform admin · Search Engine" };

export default function AdminLoginPage() {
  return (
    <AuthShell title="Platform admin" subtitle="Settings, plans, signing keys, ad controls and tenants.">
      <AdminLoginForm />
    </AuthShell>
  );
}
