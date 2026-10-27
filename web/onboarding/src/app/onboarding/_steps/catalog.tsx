import { draft } from "@/lib/api/data";

import { CatalogForm } from "./catalog-form";

export async function CatalogStep() {
  const { config } = await draft("/onboarding/catalog");
  return <CatalogForm initialMapping={config.catalog?.field_mapping ?? {}} initialIngestion={config.catalog?.ingestion === "upload" ? "upload" : "events"} />;
}
