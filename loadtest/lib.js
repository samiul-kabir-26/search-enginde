// Shared helpers for the k6 scenarios: API key → token exchange, request helpers, and the
// markdown + JSON report written to loadtest/reports/ by handleSummary.
import http from "k6/http";
import { check } from "k6";
import encoding from "k6/encoding";
import { Rate } from "k6/metrics";

export const SEARCH_URL = __ENV.BASE_URL || "http://localhost:8080";
export const CONTROL_URL = __ENV.CONTROL_URL || "http://localhost:8081";

/**
 * Share of 200 responses flagged degraded: a stage failed outright (for example every retriever
 * failed). A provider that falls back (a scorer keeping the previous order, one retriever
 * missing) does not set it; fallbacks are counted from search-api's metrics instead.
 */
export const degraded = new Rate("degraded");

/** search-api's search_stage_fallback_total, summed by tenant and stage. */
export function fallbackCounts() {
  const res = http.get(`${SEARCH_URL}/metrics`, { tags: { name: "metrics" } });
  const out = {};
  for (const line of res.body.split("\n")) {
    if (!line.startsWith("search_stage_fallback_total{")) continue;
    const tenant = /tenant="([^"]*)"/.exec(line)?.[1];
    const stage = /stage="([^"]*)"/.exec(line)?.[1];
    const value = Number(line.slice(line.lastIndexOf(" ") + 1));
    const key = `${tenant}/${stage}`;
    out[key] = (out[key] || 0) + value;
  }
  return out;
}

/** The tenant ID in a search token. */
export function tenantOf(token) {
  return JSON.parse(encoding.b64decode(token.split(".")[1], "rawurl", "s")).tid;
}

/** Exchanges an API key for a search token (tokens last auth.api_token_ttl, 15 min by default). */
export function tokenFor(apiKey) {
  const res = http.post(`${CONTROL_URL}/v1/auth/token`, JSON.stringify({ api_key: apiKey }), {
    headers: { "Content-Type": "application/json" },
    tags: { name: "token" },
  });
  if (res.status !== 200) throw new Error(`token exchange failed: ${res.status} ${res.body}`);
  return res.json("access_token");
}

export function authHeaders(token, extra = {}) {
  return { Authorization: `Bearer ${token}`, ...extra };
}

/** Checks a search response: 200 and not degraded (the degraded rate is tracked separately). */
export function checkSearch(res, name) {
  const ok = res.status === 200;
  if (ok) degraded.add(res.json("degraded") === true, { name });
  check(res, { "status is 200": () => ok }, { name });
  return ok;
}

/** One warm-up pass: the first queries after a restart can exceed their budgets (PLAN.md D38). */
export function warmUp(fn, n = 30) {
  for (let i = 0; i < n; i++) fn();
}

function fmt(v) {
  return v === undefined ? "–" : v.toFixed(1);
}

function row(label, m) {
  if (!m) return null;
  const v = m.values;
  return `| ${label} | ${v.count ?? "–"} | ${fmt(v.med)} | ${fmt(v["p(95)"])} | ${fmt(v["p(99)"])} | ${fmt(v.max)} |`;
}

/**
 * Writes loadtest/reports/{date}_{name}.md and .json. `subs` lists the tagged request groups to
 * report: [[label, "name:demo"], ...]; each needs a threshold on its submetric so k6 keeps it.
 */
export function report(name, description, subs) {
  return (data) => {
    const date = new Date().toISOString().slice(0, 10);
    const m = data.metrics;
    const lines = [
      `# Load test: ${name}, ${date}`,
      "",
      description,
      "",
      "| Requests | Count | p50 ms | p95 ms | p99 ms | max ms |",
      "|---|---|---|---|---|---|",
    ];
    for (const [label, tag] of subs) {
      const r = row(label, m[`http_req_duration{${tag}}`]);
      if (r) lines.push(r);
    }
    lines.push("", "| Rate | Value |", "|---|---|");
    const rate = (k) => (m[k] ? `${(m[k].values.rate * 100).toFixed(2)}%` : "–");
    lines.push(`| HTTP failures | ${rate("http_req_failed")} |`);
    for (const [label, tag] of subs) {
      if (m[`degraded{${tag}}`]) lines.push(`| Degraded (${label}) | ${rate(`degraded{${tag}}`)} |`);
    }
    lines.push(`| Checks passed | ${rate("checks")} |`);
    const its = m.iterations?.values;
    if (its) lines.push(`| Iterations | ${its.count} (${its.rate.toFixed(1)}/s) |`);
    for (const [k, v] of Object.entries(m)) {
      if (k.startsWith("fallbacks_")) lines.push(`| Stage fallbacks: ${k.slice(10).replace(/_/g, " ")} | ${v.values.count} |`);
    }
    const dropped = m.dropped_iterations?.values.count ?? 0;
    lines.push(`| Dropped iterations (VUs exhausted) | ${dropped} |`);
    lines.push("", "Thresholds:", "");
    for (const [metric, v] of Object.entries(m)) {
      for (const [expr, t] of Object.entries(v.thresholds ?? {})) {
        lines.push(`- \`${metric}\` ${expr}: ${t.ok ? "pass" : "**FAIL**"}`);
      }
    }
    lines.push("");
    const md = lines.join("\n");
    return {
      stdout: md + "\n",
      [`loadtest/reports/${date}_${name}.md`]: md,
      // setup_data holds the search tokens: never write it.
      [`loadtest/reports/${date}_${name}.json`]: JSON.stringify({ ...data, setup_data: undefined }, null, 2),
    };
  };
}
