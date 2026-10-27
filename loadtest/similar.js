// Similar items load test: GET /v1/products/{id}/similar for products of the image eval set.
//
//   k6 run loadtest/similar.js                 (from the repo root; RATE, DURATION)
import http from "k6/http";

import { SEARCH_URL, authHeaders, checkSearch, report, tokenFor, warmUp } from "./lib.js";

const RATE = Number(__ENV.RATE || 50);
const DURATION = __ENV.DURATION || "1m";
const API_KEY = __ENV.API_KEY || open("../data/demo_api_key").trim();
const PRODUCTS = open("../data/eval/image_queries.jsonl")
  .split("\n")
  .filter((l) => l.trim())
  .map((l) => JSON.parse(l).product_id);

export const options = {
  scenarios: {
    similar: { executor: "constant-arrival-rate", rate: RATE, timeUnit: "1s", duration: DURATION, preAllocatedVUs: 50, maxVUs: 200 },
  },
  thresholds: {
    "http_req_duration{name:similar}": ["p(95)<150"],
    "degraded{name:similar}": ["rate<0.01"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
  summaryTrendStats: ["med", "p(95)", "p(99)", "max", "count"],
};

function similar(token, name) {
  const id = PRODUCTS[Math.floor(Math.random() * PRODUCTS.length)];
  return http.get(`${SEARCH_URL}/v1/products/${encodeURIComponent(id)}/similar?size=24`, {
    headers: authHeaders(token),
    tags: { name },
  });
}

export function setup() {
  const token = tokenFor(API_KEY);
  warmUp(() => similar(token, "warm_up"));
  return { token };
}

export default function (data) {
  checkSearch(similar(data.token, "similar"), "similar");
}

export const handleSummary = report(
  "similar",
  `Similar items on the demo tenant: ${RATE} RPS for ${DURATION}, ${PRODUCTS.length} anchor products (stored text and image ` +
    `vectors, no embedding call), 24 results. Target: p95 ≤ 150 ms.`,
  [["similar items", "name:similar"]],
);
