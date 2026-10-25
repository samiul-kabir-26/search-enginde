// Text search load test (M2 target: p95 <= 150 ms at 50 RPS locally).
//
//   SEARCH_TOKEN=$(make -s token) k6 run loadtest/search_text.js
//
// Queries are random colour x material x product combinations (about 8,000), so most requests
// miss the result-snapshot cache and exercise the full pipeline. Set CACHED=1 to replay a
// small fixed set instead and measure the cached path.
import http from "k6/http";
import { check } from "k6";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const RATE = Number(__ENV.RATE || 50);
const DURATION = __ENV.DURATION || "1m";

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

function query() {
  if (__ENV.CACHED) {
    return pick(["red leather handbag", "office chair", "stainless steel water bottle", "gold earrings"]);
  }
  return [pick(COLORS), pick(MATERIALS), pick(PRODUCTS)].filter((s) => s).join(" ") + pick(SUFFIXES);
}

export const options = {
  scenarios: {
    steady: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  thresholds: {
    http_req_duration: ["p(95)<150"],
    http_req_failed: ["rate<0.01"],
    checks: ["rate>0.99"],
  },
};

export function setup() {
  if (!__ENV.SEARCH_TOKEN) {
    throw new Error("SEARCH_TOKEN is required: SEARCH_TOKEN=$(make -s token) k6 run loadtest/search_text.js");
  }
}

export default function () {
  const res = http.post(`${BASE_URL}/v1/search`, JSON.stringify({ q: query(), page: { size: 24 } }), {
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${__ENV.SEARCH_TOKEN}` },
    tags: { name: "search_text" },
  });
  check(res, {
    "status is 200": (r) => r.status === 200,
    "not degraded": (r) => r.status === 200 && r.json("degraded") === false,
  });
}
