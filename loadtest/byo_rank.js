// BYO remote rank: text search on a tenant whose rank stage is its own kserve_v2 model
// (Triton's rank_gbdt_v1 registered as the tenant's endpoint; same egress client, bulkhead and
// breaker as a public endpoint). Target: p95 <= 300 ms with BYO remote rank (docs/PROJECT.md §7).
//
//   make loadtest-setup                          (once: creates lt-byo; see loadtest/setup_tenants.py)
//   k6 run loadtest/byo_rank.js                  (from the repo root; RATE, DURATION)
import http from "k6/http";

import { SEARCH_URL, authHeaders, checkSearch, report, tokenFor, warmUp } from "./lib.js";
import { body } from "./search_text.js";

const RATE = Number(__ENV.RATE || 30);
const DURATION = __ENV.DURATION || "1m";
const TENANTS = JSON.parse(open("./.tenants.json"));

export const options = {
  scenarios: {
    byo: { executor: "constant-arrival-rate", rate: RATE, timeUnit: "1s", duration: DURATION, preAllocatedVUs: 40, maxVUs: 200 },
  },
  thresholds: {
    "http_req_duration{name:byo_rank}": ["p(95)<300"],
    "degraded{name:byo_rank}": ["rate<0.01"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
  summaryTrendStats: ["med", "p(95)", "p(99)", "max", "count"],
};

function search(token, name) {
  return http.post(`${SEARCH_URL}/v1/search`, body(), {
    headers: authHeaders(token, { "Content-Type": "application/json" }),
    tags: { name },
  });
}

export function setup() {
  const token = tokenFor(TENANTS["lt-byo"].api_key);
  warmUp(() => search(token, "warm_up"));
  return { token };
}

export default function (data) {
  checkSearch(search(data.token, "byo_rank"), "byo_rank");
}

export const handleSummary = report(
  "byo_rank",
  `Text search on lt-byo, whose rank stage is a BYO kserve_v2 endpoint (Triton rank_gbdt_v1 over REST, 22 features ` +
    `for 100 candidates per call): ${RATE} RPS for ${DURATION}, 500 products. Target: p95 ≤ 300 ms with BYO remote rank.`,
  [["BYO remote rank", "name:byo_rank"]],
);
