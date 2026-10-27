// Text search load test (M2 target: p95 <= 150 ms at 50 RPS locally).
//
//   make loadtest-text            (or: k6 run loadtest/search_text.js from the repo root)
//
// Queries are random colour x material x product combinations (about 8,000), so most requests
// miss the result-snapshot cache and exercise the full pipeline. Set CACHED=1 to replay a
// small fixed set instead and measure the cached path. The demo tenant's API key is read from
// data/demo_api_key (or API_KEY).
import http from "k6/http";

import { SEARCH_URL, authHeaders, checkSearch, report, tokenFor, warmUp } from "./lib.js";

const RATE = Number(__ENV.RATE || 50);
const DURATION = __ENV.DURATION || "1m";
const API_KEY = __ENV.API_KEY || open("../data/demo_api_key").trim();

const COLORS = ["red", "black", "white", "blue", "navy", "green", "grey", "brown", "pink", "gold",
  "silver", "beige", "yellow", "purple", "orange", "", "", "", "", ""];
const MATERIALS = ["leather", "cotton", "wood", "oak", "steel", "stainless steel", "glass", "ceramic", "", ""];
const PRODUCTS = ["chair", "office chair", "sofa", "table", "desk", "lamp", "floor lamp", "rug", "bed frame",
  "dresser", "bookshelf", "earrings", "necklace", "ring", "bracelet", "handbag", "backpack", "wallet",
  "shoes", "boots", "sandals", "phone case", "screen protector", "charger", "water bottle", "mug",
  "plates", "frying pan", "dutch oven", "coffee", "tea", "snack mix", "vitamins", "candle", "vase",
  "planter", "clock", "storage box", "hangers", "umbrella"];
const SUFFIXES = ["", "", "", "", " under $50", " under 100 dollars", " for kids", " set"];

function pick(xs) {
  return xs[Math.floor(Math.random() * xs.length)];
}

export function query() {
  if (__ENV.CACHED) {
    return pick(["red leather handbag", "office chair", "stainless steel water bottle", "gold earrings"]);
  }
  return [pick(COLORS), pick(MATERIALS), pick(PRODUCTS)].filter((s) => s).join(" ") + pick(SUFFIXES);
}

/**
 * A search request body. Unless CACHED is set, each request carries a fresh user: snapshots are
 * keyed by query and user, so repeated queries across runs still run the full pipeline.
 */
export function body() {
  const b = { q: query(), page: { size: 24 } };
  if (!__ENV.CACHED) b.user = { id: `lt_${Math.floor(Math.random() * 1e9)}` };
  return JSON.stringify(b);
}

export const options = {
  scenarios: {
    text: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  thresholds: {
    "http_req_duration{name:search_text}": ["p(95)<150"],
    "degraded{name:search_text}": ["rate<0.01"],
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
  const token = tokenFor(API_KEY);
  warmUp(() => search(token, "warm_up"));
  return { token };
}

export default function (data) {
  checkSearch(search(data.token, "search_text"), "search_text");
}

export const handleSummary = report(
  "search_text",
  `Text search on the demo tenant: ${RATE} RPS for ${DURATION}, random colour × material × product queries ` +
    `(${__ENV.CACHED ? "cached set" : "a fresh user per request, so every request runs the full pipeline"}), page size 24. Target: p95 ≤ 150 ms (docs/PROJECT.md §7).`,
  [["text search", "name:search_text"]],
);
