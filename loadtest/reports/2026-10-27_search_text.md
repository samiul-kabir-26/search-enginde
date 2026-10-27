# Load test: search_text, 2026-10-27

Text search on the demo tenant: 50 RPS for 1m, random colour × material × product queries (a fresh user per request, so every request runs the full pipeline), page size 24. Target: p95 ≤ 150 ms (docs/PROJECT.md §7).

| Requests | Count | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| text search | 3001 | 73.3 | 158.0 | 195.5 | 254.1 |

| Rate | Value |
|---|---|
| HTTP failures | 0.00% |
| Degraded (text search) | 0.00% |
| Checks passed | 100.00% |
| Iterations | 3001 (48.5/s) |
| Dropped iterations (VUs exhausted) | 0 |

Thresholds:

- `http_req_duration{name:search_text}` p(95)<150: **FAIL**
- `checks` rate>0.99: pass
- `http_req_failed` rate<0.01: pass
- `degraded{name:search_text}` rate<0.01: pass
