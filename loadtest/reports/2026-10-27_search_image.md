# Load test: search_image, 2026-10-27

Image search on the demo tenant: 10 RPS for 1m, 100 alternate product photos (JPEG uploads, CLIP on CPU, a fresh user per request, so every request runs the full pipeline), page size 24. Target: p95 ≤ 300 ms (the smallest plan budget).

| Requests | Count | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| image search | 601 | 77.9 | 84.0 | 99.4 | 118.7 |

| Rate | Value |
|---|---|
| HTTP failures | 0.00% |
| Degraded (image search) | 0.00% |
| Checks passed | 100.00% |
| Iterations | 601 (9.7/s) |
| Dropped iterations (VUs exhausted) | 0 |

Thresholds:

- `http_req_failed` rate<0.01: pass
- `http_req_duration{name:search_image}` p(95)<300: pass
- `degraded{name:search_image}` rate<0.01: pass
- `checks` rate>0.99: pass
