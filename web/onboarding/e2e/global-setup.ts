import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";

/**
 * Builds the acceptance test's catalog (docs/ONBOARDING.md §6): the first 100 products of
 * data/products.canonical.jsonl in the demo storefront's JSON shape (the one the default
 * field mapping reads, as cmd/seed renders it), with image URLs on the images `make seed`
 * uploaded for the demo tenant.
 */

const root = path.resolve(__dirname, "../../..");
export const FIXTURES = path.join(__dirname, ".fixtures");
export const CATALOG = path.join(FIXTURES, "catalog-100.jsonl");
export const META = path.join(FIXTURES, "meta.json");

type Canonical = {
  product_id: string;
  title: string;
  description?: string;
  category_path: string[];
  brand?: string;
  product_type?: string;
  price_minor: number;
  currency: string;
  image_files?: string[];
  vendor_id: string;
  attributes?: Record<string, string[]>;
  moq?: number;
  stock?: number;
  ship_regions?: string[];
  status: string;
  rating?: number;
  rating_count?: number;
  created_at?: string;
};

export default function globalSetup() {
  const imageBase = `${(process.env.S3_PUBLIC_URL ?? "http://localhost:9100").replace(/\/$/, "")}/${process.env.S3_BUCKET_IMAGES ?? "catalog-images"}/demo`;
  const lines = readFileSync(path.join(root, "data/products.canonical.jsonl"), "utf8").split("\n");
  const products: Canonical[] = [];
  for (const line of lines) {
    if (!line.trim()) continue;
    const p = JSON.parse(line) as Canonical;
    if (p.status !== "active" || !p.image_files?.length) continue;
    products.push(p);
    if (products.length === 100) break;
  }
  const storefront = products.map((p) => ({
    id: p.product_id,
    name: p.title,
    description: p.description,
    category: { path: p.category_path },
    brand: p.brand,
    product_type: p.product_type,
    price: { amount_minor: p.price_minor, currency: p.currency },
    images: p.image_files!.map((f) => ({ url: `${imageBase}/${f}` })),
    vendor: { id: p.vendor_id },
    attributes: p.attributes,
    moq: p.moq,
    inventory: { stock: p.stock },
    shipping: { regions: p.ship_regions },
    status: p.status,
    reviews: { rating: p.rating, count: p.rating_count },
    created_at: p.created_at,
  }));
  mkdirSync(FIXTURES, { recursive: true });
  writeFileSync(CATALOG, storefront.map((d) => JSON.stringify(d)).join("\n") + "\n");
  // A query word from a product title, and that product's image for the image search.
  const probe = products[0];
  const word = probe.title.split(/\s+/).find((w) => /^[A-Za-z]{4,}$/.test(w)) ?? probe.title.split(/\s+/)[0];
  writeFileSync(META, JSON.stringify({ query: word, image: path.join(root, "data/images", probe.image_files![0]), productId: probe.product_id }));
}
