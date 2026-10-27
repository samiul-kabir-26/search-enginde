/**
 * The parts of the tenant config document (docs/TENANT_CONFIG.md §3) the wizard reads. The
 * OpenAPI ConfigDocument is open-ended, so these are partial views; the server stays the
 * authority on the full schema.
 */

export type Scorer = { type: string; weights?: Record<string, number>; model?: string; model_id?: string };

export type TenantConfig = {
  catalog?: { ingestion?: string; field_mapping?: Record<string, string> };
  embedders?: { text?: { provider: string; model?: string; model_id?: string }; image?: { provider: string; model?: string; model_id?: string } };
  stores?: { vector?: { provider: string; connection_id?: string; collection?: string } };
  retrieval?: { text?: { retrievers?: { name: string; k: number; weight: number }[] } };
  policy?: { always_filters?: unknown[]; rules?: { id: string; kind: string; expr: string }[] };
  prerank?: { scorer: Scorer; keep: number };
  rank?: { scorer: Scorer; keep: number };
  rerank?: {
    personalization?: { enabled: boolean; alpha: number; beta: number };
    diversity?: { max_per_vendor: number; window: number };
    model?: Scorer;
  };
  ads?: { enabled: boolean; slots: number[]; relevance_floor: number; auction?: string; reserve_cpc_minor?: number };
};

/** Canonical catalog fields a tenant can map (internal/catalog/fields.go), in display order. */
export const CANONICAL_FIELDS: { name: string; required?: boolean; hint: string }[] = [
  { name: "product_id", required: true, hint: "Your unique product ID" },
  { name: "title", required: true, hint: "Product name" },
  { name: "category_path", required: true, hint: "Category breadcrumb, an array of names" },
  { name: "price_minor", required: true, hint: "Price in minor units (cents)" },
  { name: "currency", required: true, hint: "ISO 4217 code" },
  { name: "vendor_id", required: true, hint: "The seller of the product" },
  { name: "image_urls", required: true, hint: "One or more image URLs" },
  { name: "description", hint: "Long description" },
  { name: "brand", hint: "Brand name" },
  { name: "product_type", hint: "Product type" },
  { name: "attributes", hint: "Attribute object, e.g. colour and material" },
  { name: "moq", hint: "Minimum order quantity" },
  { name: "stock", hint: "Units in stock" },
  { name: "ship_regions", hint: "Regions you ship to" },
  { name: "status", hint: "active or inactive" },
  { name: "rating", hint: "Average rating" },
  { name: "rating_count", hint: "Number of ratings" },
  { name: "created_at", hint: "Listing date" },
];

/** The default_linear rank features (docs/TENANT_CONFIG.md §3) with plain-language labels. */
export const RANK_FEATURES: { name: string; label: string }[] = [
  { name: "rrf_score", label: "Combined retrieval score" },
  { name: "bm25_norm", label: "Keyword match" },
  { name: "cosine_text", label: "Text meaning match" },
  { name: "cosine_image", label: "Image similarity" },
  { name: "ctr_7d", label: "Click-through (7 days)" },
  { name: "cvr_7d", label: "Conversion (7 days)" },
  { name: "rating", label: "Rating" },
  { name: "vendor_score", label: "Seller quality" },
  { name: "policy_boost", label: "Your boost rules" },
];

export const RETRIEVAL_PRESETS: { id: "balanced" | "keyword" | "semantic" | "visual"; label: string; description: string }[] = [
  { id: "balanced", label: "Balanced", description: "Keyword, meaning and image search weighted evenly. The default." },
  { id: "keyword", label: "Keyword-heavy", description: "Favour exact term matches: part numbers, brands, model names." },
  { id: "semantic", label: "Semantic-heavy", description: "Favour meaning: descriptive queries like \"cozy reading chair\"." },
  { id: "visual", label: "Visual-first", description: "Favour image similarity: fashion, decor and other visual catalogs." },
];

/** Which preset a draft's text retriever mix came from, judged by the heaviest retriever. */
export function presetOf(cfg: TenantConfig): "balanced" | "keyword" | "semantic" | "visual" {
  const rs = cfg.retrieval?.text?.retrievers ?? [];
  if (rs.length === 0) return "balanced";
  const top = [...rs].sort((a, b) => b.weight - a.weight);
  if (top.length > 1 && top[0].weight === top[1].weight) return "balanced";
  if (top[0].name === "lexical_bm25") return "keyword";
  if (top[0].name === "vector_text") return "semantic";
  if (top[0].name === "vector_text_to_image") return "visual";
  return "balanced";
}
