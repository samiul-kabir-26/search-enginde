import { draft } from "@/lib/api/data";

import { PoliciesForm } from "./policies-form";

/** The template rule the server appends for "verified vendors only". */
export const VERIFIED_TEMPLATE = "template-verified-vendors";

export async function PoliciesStep() {
  const { config } = await draft("/onboarding/policies");
  const filters = (config.policy?.always_filters ?? []) as { field?: string; op?: string; value?: unknown }[];
  const rules = config.policy?.rules ?? [];
  const region = filters.find((f) => f.field === "ship_regions")?.value;
  return (
    <PoliciesForm
      inStock={filters.some((f) => f.field === "stock")}
      shipToRegion={typeof region === "string" ? region : ""}
      verifiedVendors={rules.some((r) => r.id === VERIFIED_TEMPLATE)}
      rules={rules.filter((r) => r.id !== VERIFIED_TEMPLATE)}
    />
  );
}
