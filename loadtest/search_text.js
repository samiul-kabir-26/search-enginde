// Text search load test (M10 target: p95 <= 150 ms at 50 RPS locally).
// Usage: SEARCH_TOKEN=<jwt> k6 run loadtest/search_text.js
import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const QUERIES = ["red leather handbag", "running shoes", "stainless steel water bottle", "wooden desk lamp"];

export const options = {
  scenarios: {
    steady: { executor: "constant-arrival-rate", rate: 50, timeUnit: "1s", duration: "1m", preAllocatedVUs: 50 },
  },
  thresholds: { http_req_duration: ["p(95)<150"], http_req_failed: ["rate<0.01"] },
};

export default function () {
  const q = QUERIES[Math.floor(Math.random() * QUERIES.length)];
  const res = http.post(`${BASE_URL}/v1/search`, JSON.stringify({ q, page: { size: 24 } }), {
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${__ENV.SEARCH_TOKEN}` },
  });
  check(res, { "status is 200": (r) => r.status === 200 });
}
