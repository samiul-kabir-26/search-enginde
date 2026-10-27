"""Generates the provisioned Grafana dashboards (docs/ARCHITECTURE.md §10).

    python3 deploy/grafana/gen_dashboards.py

Writes deploy/grafana/provisioning/dashboards/{search-overview,search-resilience}.json from the
metrics search-api exports: search_request_duration_seconds{tenant,query_type},
search_stage_duration_seconds{tenant,stage,provider}, search_stage_fallback_total{tenant,stage,
provider,reason}, search_results_empty_total and provider_breaker_state (0 closed, 1 half-open,
2 open), plus the OTel HTTP server histogram. Edit this file, not the JSON. Standard library only.
"""

# PromQL expressions and panel descriptions read better unsplit.
# ruff: noqa: E501

from __future__ import annotations

import json
from pathlib import Path

OUT = Path(__file__).resolve().parent / "provisioning" / "dashboards"
DS = {"type": "prometheus", "uid": "prometheus"}
T = 'tenant=~"$tenant"'


def target(expr: str, legend: str, ref: str = "A") -> dict:
    return {"datasource": DS, "expr": expr, "legendFormat": legend, "refId": ref}


def panel(
    pid: int,
    title: str,
    targets: list[dict],
    x: int,
    y: int,
    w: int = 12,
    h: int = 8,
    unit: str = "short",
    kind: str = "timeseries",
    description: str = "",
    thresholds: list[tuple[str, float | None]] | None = None,
    extra: dict | None = None,
) -> dict:
    steps = [{"color": c, "value": v} for c, v in (thresholds or [("green", None)])]
    p = {
        "id": pid,
        "type": kind,
        "title": title,
        "description": description,
        "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": targets,
        "fieldConfig": {
            "defaults": {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps}},
            "overrides": [],
        },
        "options": {
            "legend": {"displayMode": "table", "placement": "bottom", "calcs": ["mean", "max"]},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
    }
    if thresholds and kind == "timeseries":
        p["fieldConfig"]["defaults"]["custom"] = {"thresholdsStyle": {"mode": "line"}}
    if extra:
        for k, v in extra.items():
            p[k] = v if not isinstance(v, dict) else p.get(k, {}) | v
    return p


def quantile(q: float, metric: str, by: str, where: str = T) -> str:
    return f"histogram_quantile({q}, sum by (le, {by}) (rate({metric}_bucket{{{where}}}[$__rate_interval])))"


def variables(extra: list[dict] | None = None) -> dict:
    tenant = {
        "name": "tenant",
        "label": "Tenant",
        "type": "query",
        "datasource": DS,
        "query": {
            "query": "label_values(search_request_duration_seconds_count, tenant)",
            "refId": "tenant",
        },
        "definition": "label_values(search_request_duration_seconds_count, tenant)",
        "includeAll": True,
        "multi": True,
        "allValue": ".*",
        "current": {"text": "All", "value": "$__all"},
        "refresh": 2,
    }
    return {"list": [tenant] + (extra or [])}


def dashboard(
    uid: str, title: str, description: str, panels: list[dict], extra_vars: list[dict] | None = None
) -> dict:
    return {
        "uid": uid,
        "title": title,
        "description": description,
        "tags": ["search-engine"],
        "timezone": "browser",
        "schemaVersion": 39,
        "version": 1,
        "editable": True,
        "refresh": "10s",
        "time": {"from": "now-30m", "to": "now"},
        "templating": variables(extra_vars),
        "panels": panels,
        "links": [
            {
                "title": "Runbook",
                "url": "https://github.com/samiul-kabir-26/search-engine/blob/main/docs/RUNBOOK.md",
                "type": "link",
                "targetBlank": True,
            }
        ],
    }


def overview() -> dict:
    latency_slo = [("green", None), ("orange", 0.15), ("red", 0.3)]
    panels = [
        panel(
            1,
            "Search requests per second",
            [
                target(
                    f"sum by (tenant, query_type) (rate(search_request_duration_seconds_count{{{T}}}[$__rate_interval]))",
                    "{{tenant}} {{query_type}}",
                ),
            ],
            0,
            0,
            unit="reqps",
        ),
        panel(
            2,
            "Search latency (end to end)",
            [
                target(
                    quantile(0.5, "search_request_duration_seconds", "query_type"),
                    "p50 {{query_type}}",
                    "A",
                ),
                target(
                    quantile(0.95, "search_request_duration_seconds", "query_type"),
                    "p95 {{query_type}}",
                    "B",
                ),
                target(
                    quantile(0.99, "search_request_duration_seconds", "query_type"),
                    "p99 {{query_type}}",
                    "C",
                ),
            ],
            12,
            0,
            unit="s",
            thresholds=latency_slo,
            description="Pipeline latency inside search-api. Target p95 ≤ 150 ms for text (≤ 300 ms with BYO remote rank).",
        ),
        panel(
            3,
            "p95 by tenant",
            [
                target(quantile(0.95, "search_request_duration_seconds", "tenant"), "{{tenant}}"),
            ],
            0,
            8,
            unit="s",
            thresholds=latency_slo,
            description="One slow tenant must not move the others (isolation: bulkheads, per-tenant breakers).",
        ),
        panel(
            4,
            "Stage latency p95",
            [
                target(quantile(0.95, "search_stage_duration_seconds", "stage"), "{{stage}}"),
            ],
            12,
            8,
            unit="s",
            description="Per-stage budgets (default): S1 40, S2 120, S3 10, S4 15, S5 40, S6 30 ms.",
        ),
        panel(
            5,
            "Stage latency p95 by provider",
            [
                target(
                    quantile(0.95, "search_stage_duration_seconds", "stage, provider"),
                    "{{stage}} · {{provider}}",
                ),
            ],
            0,
            16,
            w=24,
            unit="s",
        ),
        panel(
            6,
            "Empty results per second",
            [
                target("sum(rate(search_results_empty_total[$__rate_interval]))", "empty results"),
            ],
            0,
            24,
            unit="reqps",
            description="Searches that returned nothing (all tenants; the counter has no tenant label).",
        ),
        panel(
            7,
            "HTTP responses by status (search-api)",
            [
                target(
                    'sum by (http_response_status_code) (rate(http_server_request_duration_seconds_count{job="search-api", http_route=~"/v1/.*"}[$__rate_interval]))',
                    "{{http_response_status_code}}",
                ),
            ],
            12,
            24,
            unit="reqps",
            description="429: plan rate limit. 503: tenant config or providers unavailable. Search itself degrades rather than failing.",
        ),
        panel(
            8,
            "control-api latency p95 by route",
            [
                target(
                    'histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket{job="control-api"}[$__rate_interval])))',
                    "{{http_route}}",
                ),
            ],
            0,
            32,
            w=24,
            unit="s",
        ),
    ]
    return dashboard(
        "search-overview",
        "Search overview",
        "Request and stage latency, throughput and errors (docs/ARCHITECTURE.md §10).",
        panels,
    )


def resilience() -> dict:
    breaker_map = {
        "mappings": [
            {
                "type": "value",
                "options": {
                    "0": {"text": "closed", "color": "green"},
                    "1": {"text": "half-open", "color": "orange"},
                    "2": {"text": "open", "color": "red"},
                },
            }
        ],
    }
    panels = [
        panel(
            1,
            "Fallbacks per second by stage",
            [
                target(
                    f"sum by (stage) (rate(search_stage_fallback_total{{{T}}}[$__rate_interval]))",
                    "{{stage}}",
                ),
            ],
            0,
            0,
            unit="reqps",
            description="A fallback keeps the search answering: a missing retriever, a scorer keeping the previous order, default features.",
        ),
        panel(
            2,
            "Fallback share of searches",
            [
                target(
                    f"sum by (tenant) (rate(search_stage_fallback_total{{{T}}}[$__rate_interval])) / "
                    f"sum by (tenant) (rate(search_request_duration_seconds_count{{{T}}}[$__rate_interval]))",
                    "{{tenant}}",
                ),
            ],
            12,
            0,
            unit="percentunit",
            thresholds=[("green", None), ("orange", 0.01), ("red", 0.05)],
            description="Fallbacks per search (one search can fall back in several stages).",
        ),
        panel(
            3,
            "Fallbacks by provider and reason",
            [
                target(
                    f"sum by (stage, provider, reason) (rate(search_stage_fallback_total{{{T}}}[$__rate_interval]))",
                    "{{stage}} · {{provider}} · {{reason}}",
                ),
            ],
            0,
            8,
            w=24,
            unit="reqps",
            description="Reasons: timeout (stage budget), breaker_open, bulkhead_full (tenant's BYO concurrency), error.",
        ),
        panel(
            4,
            "Circuit breakers",
            [
                target(
                    f"max by (tenant, stage, provider) (provider_breaker_state{{{T}}})",
                    "{{tenant}} · {{stage}} · {{provider}}",
                ),
            ],
            0,
            16,
            w=24,
            h=10,
            kind="state-timeline",
            description="0 closed, 1 half-open, 2 open. An open breaker skips the provider until its cooldown; half-open lets probes through.",
            extra={
                "fieldConfig": {
                    "defaults": {
                        "unit": "short",
                        "mappings": breaker_map["mappings"],
                        "thresholds": {
                            "mode": "absolute",
                            "steps": [
                                {"color": "green", "value": None},
                                {"color": "orange", "value": 1},
                                {"color": "red", "value": 2},
                            ],
                        },
                    },
                    "overrides": [],
                },
                "options": {
                    "showValue": "never",
                    "mergeValues": True,
                    "legend": {"showLegend": True},
                },
            },
        ),
        panel(
            5,
            "Breakers not closed",
            [
                target(f"count(provider_breaker_state{{{T}}} > 0) or vector(0)", "breakers"),
            ],
            0,
            26,
            w=8,
            h=6,
            kind="stat",
            thresholds=[("green", None), ("red", 1)],
        ),
        panel(
            6,
            "Top fallback reasons (last hour)",
            [
                {
                    **target(
                        f"topk(15, sum by (tenant, stage, provider, reason) (increase(search_stage_fallback_total{{{T}}}[1h])))",
                        "",
                    ),
                    "instant": True,
                    "format": "table",
                },
            ],
            8,
            26,
            w=16,
            h=6,
            kind="table",
        ),
    ]
    return dashboard(
        "search-resilience",
        "Search resilience",
        "Fallbacks, timeouts and circuit breakers per tenant, stage and provider (docs/ARCHITECTURE.md §6, §10).",
        panels,
    )


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    for name, d in (("search-overview", overview()), ("search-resilience", resilience())):
        (OUT / f"{name}.json").write_text(json.dumps(d, indent=2) + "\n")
        print(f"wrote {OUT / name}.json")


if __name__ == "__main__":
    main()
