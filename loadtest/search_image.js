// Image search load test: alternate product photos from the image eval set as queries.
//
//   k6 run loadtest/search_image.js            (from the repo root; RATE, DURATION)
//
// Image embedding runs CLIP on CPU in Triton, so the default rate is lower than text search.
import http from "k6/http";

import { SEARCH_URL, authHeaders, checkSearch, report, tokenFor, warmUp } from "./lib.js";

const RATE = Number(__ENV.RATE || 10);
const DURATION = __ENV.DURATION || "1m";
const API_KEY = __ENV.API_KEY || open("../data/demo_api_key").trim();
const IMAGES = open("../data/eval/image_queries.jsonl")
  .split("\n")
  .filter((l) => l.trim())
  .slice(0, 100)
  .map((l) => JSON.parse(l).image_file)
  .map((f) => ({ name: f.split("/").pop(), bytes: open(`../data/${f}`, "b") }));

export const options = {
  scenarios: {
    image: { executor: "constant-arrival-rate", rate: RATE, timeUnit: "1s", duration: DURATION, preAllocatedVUs: 20, maxVUs: 100 },
  },
  thresholds: {
    "http_req_duration{name:search_image}": ["p(95)<300"],
    "degraded{name:search_image}": ["rate<0.01"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
  summaryTrendStats: ["med", "p(95)", "p(99)", "max", "count"],
};

function search(token, name) {
  const img = IMAGES[Math.floor(Math.random() * IMAGES.length)];
  return http.post(
    `${SEARCH_URL}/v1/search/image`,
    {
      image: http.file(img.bytes, img.name, "image/jpeg"),
      page: JSON.stringify({ size: 24 }),
      // Snapshots are keyed by image and user: a fresh user per request runs the full pipeline.
      ...(__ENV.CACHED ? {} : { user: JSON.stringify({ id: `lt_${Math.floor(Math.random() * 1e9)}` }) }),
    },
    { headers: authHeaders(token), tags: { name } },
  );
}

export function setup() {
  const token = tokenFor(API_KEY);
  warmUp(() => search(token, "warm_up"), 15);
  return { token };
}

export default function (data) {
  checkSearch(search(data.token, "search_image"), "search_image");
}

export const handleSummary = report(
  "search_image",
  `Image search on the demo tenant: ${RATE} RPS for ${DURATION}, 100 alternate product photos (JPEG uploads, ` +
    `CLIP on CPU, ${__ENV.CACHED ? "snapshot cache allowed" : "a fresh user per request, so every request runs the full pipeline"}), ` +
    "page size 24. Target: p95 ≤ 300 ms (the smallest plan budget).",
  [["image search", "name:search_image"]],
);
