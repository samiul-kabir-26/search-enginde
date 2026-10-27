// Tenant isolation: a healthy tenant's latency must hold while another tenant's BYO endpoint
// hangs. The demo tenant searches at 40 RPS; at the same time lt-slow searches at 20 RPS with
// its rank model (loadtest/slow_kserve.py) answering after 2 s. The slow tenant's calls time
// out at its rank budget, its breaker opens and its bulkhead bounds them; the demo tenant must
// keep p95 <= 150 ms with nothing degraded.
//
//   python3 loadtest/slow_kserve.py &            (and make loadtest-setup once)
//   k6 run loadtest/isolation.js                 (from the repo root)
import http from "k6/http";
import { Counter } from "k6/metrics";

import { SEARCH_URL, authHeaders, checkSearch, fallbackCounts, report, tenantOf, tokenFor, warmUp } from "./lib.js";
import { body } from "./search_text.js";

const DURATION = __ENV.DURATION || "1m";
const SLOW_URL = __ENV.SLOW_URL || "http://127.0.0.1:9555";
const DELAY_MS = Number(__ENV.DELAY_MS || 2000);
const DEMO_KEY = __ENV.API_KEY || open("../data/demo_api_key").trim();
const TENANTS = JSON.parse(open("./.tenants.json"));

export const options = {
  scenarios: {
    healthy: { executor: "constant-arrival-rate", rate: 40, timeUnit: "1s", duration: DURATION, preAllocatedVUs: 40, maxVUs: 200, exec: "healthy" },
    slow: { executor: "constant-arrival-rate", rate: 20, timeUnit: "1s", duration: DURATION, preAllocatedVUs: 40, maxVUs: 200, exec: "slow" },
  },
  thresholds: {
    "http_req_duration{name:healthy}": ["p(95)<150"],
    "degraded{name:healthy}": ["rate<0.01"],
    // The slow tenant still answers: its rank stage falls back to the pre-rank order.
    "http_req_duration{name:slow}": ["p(95)<300"],
    // Reported, not limited: the slow tenant is expected to degrade.
    "degraded{name:slow}": ["rate<=1"],
    "http_req_failed{name:healthy}": ["rate<0.01"],
    "http_req_failed{name:slow}": ["rate<0.01"],
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
  const demo = tokenFor(DEMO_KEY);
  const slow = tokenFor(TENANTS["lt-slow"].api_key);
  warmUp(() => search(demo, "warm_up"));
  warmUp(() => search(slow, "warm_up"), 10);
  const res = http.post(`${SLOW_URL}/admin/delay?ms=${DELAY_MS}`, null, { tags: { name: "admin" } });
  if (res.status !== 200) throw new Error(`cannot slow down ${SLOW_URL}: is loadtest/slow_kserve.py running?`);
  return { demo, slow, tenants: { healthy: tenantOf(demo), slow: tenantOf(slow) }, before: fallbackCounts() };
}

// Stage fallbacks during the run, by tenant and stage (search-api metrics, after minus before).
const fallbacks = {};
for (const who of ["healthy", "slow"]) {
  for (const stage of ["query_understanding", "retrieval", "prerank", "rank"]) {
    fallbacks[`${who}/${stage}`] = new Counter(`fallbacks_${who}_${stage}`);
  }
}

export function healthy(data) {
  checkSearch(search(data.demo, "healthy"), "healthy");
}

export function slow(data) {
  checkSearch(search(data.slow, "slow"), "slow");
}

export function teardown(data) {
  http.post(`${SLOW_URL}/admin/delay?ms=0`, null, { tags: { name: "admin" } });
  const after = fallbackCounts();
  for (const [who, tid] of Object.entries(data.tenants)) {
    for (const stage of ["query_understanding", "retrieval", "prerank", "rank"]) {
      const key = `${tid}/${stage}`;
      fallbacks[`${who}/${stage}`].add((after[key] || 0) - (data.before[key] || 0));
    }
  }
}

export const handleSummary = report(
  "isolation",
  `Two tenants at once for ${DURATION}: the demo tenant at 40 RPS (healthy), and lt-slow at 20 RPS whose BYO rank ` +
    `endpoint answers after ${DELAY_MS} ms. The slow tenant's rank stage is expected to fall back (its calls time out ` +
    "at the rank budget, then its breaker opens and only half-open probes reach the endpoint); the healthy tenant " +
    "must stay within p95 ≤ 150 ms with nothing degraded and no fallbacks caused by the slow tenant.",
  [
    ["healthy tenant (demo)", "name:healthy"],
    ["slow tenant (lt-slow)", "name:slow"],
  ],
);
