# Load test: isolation, 2026-10-27

Two tenants at once for 1m: the demo tenant at 40 RPS (healthy), and lt-slow at 20 RPS whose BYO rank endpoint answers after 2000 ms. The slow tenant's rank stage is expected to fall back (its calls time out at the rank budget, then its breaker opens and only half-open probes reach the endpoint); the healthy tenant must stay within p95 ≤ 150 ms with nothing degraded and no fallbacks caused by the slow tenant.

| Requests | Count | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|
| healthy tenant (demo) | 2401 | 128.0 | 192.8 | 233.3 | 316.9 |
| slow tenant (lt-slow) | 1201 | 83.7 | 133.2 | 163.3 | 202.6 |

| Rate | Value |
|---|---|
| HTTP failures | 0.00% |
| Degraded (healthy tenant (demo)) | 0.08% |
| Degraded (slow tenant (lt-slow)) | 0.00% |
| Checks passed | 100.00% |
| Iterations | 3602 (57.6/s) |
| Stage fallbacks: healthy rank | 1 |
| Stage fallbacks: healthy query understanding | 2147 |
| Stage fallbacks: healthy prerank | 20 |
| Stage fallbacks: slow rank | 1196 |
| Stage fallbacks: slow retrieval | 0 |
| Stage fallbacks: slow prerank | 3 |
| Stage fallbacks: slow query understanding | 1060 |
| Stage fallbacks: healthy retrieval | 25 |
| Dropped iterations (VUs exhausted) | 0 |

Thresholds:

- `degraded{name:slow}` rate<=1: pass
- `http_req_duration{name:healthy}` p(95)<150: **FAIL**
- `degraded{name:healthy}` rate<0.01: pass
- `http_req_failed{name:slow}` rate<0.01: pass
- `http_req_failed{name:healthy}` rate<0.01: pass
- `http_req_duration{name:slow}` p(95)<300: pass

## Controlled comparison

The same two-tenant load with the slow endpoint answering at once (`DELAY_MS=0`) is the
control: any difference in the healthy tenant's numbers is what the slow tenant costs it. Runs
alternated on the same laptop (12 cores shared by every service, Triton and k6), 20 s apart.

| Run | Slow endpoint | Healthy p50 / p95 ms | Healthy stage fallbacks (query understanding / retrieval) | Slow tenant rank fallbacks |
|---|---|---|---|---|
| Cache-cold, control | 0 ms | 122.2 / 183.7 | 2231 / 16 | 328 (breaker still open from the previous run, then closed) |
| Cache-cold, slow (this report) | 2000 ms | 128.0 / 192.8 | 2147 / 25 | 1196 of 1201 requests |
| Earlier runs (snapshot cache partly warm), slow ×4 | 2000 ms | p95 119.7, 157.6, 149.0, 108.1 | 16–2171 retrieval | 734–1043 |
| Earlier runs (snapshot cache partly warm), control ×3 | 0 ms | p95 149.8, 140.5, 115.8 | 16–704 retrieval | 88–186 (breaker recovering) |

**Isolation holds**: with the slow tenant's endpoint hanging, the healthy tenant's p95 moves by
+9 ms in the cache-cold pair and not at all on average across the earlier runs (mean 134 ms slow
vs 135 ms control), and the healthy tenant's rank stage never falls back. The slow tenant's rank
calls time out at its 40 ms rank budget, its breaker opens, and only half-open probes reach the
endpoint (83 calls over the first two runs), so its own searches stay fast in pre-rank order.

**Capacity, not isolation, misses the target**: at 60 RPS of fully uncached searches, the demo
tenant's p95 exceeds 150 ms in both conditions. Its query embedders (BGE and the CLIP text
tower in Triton on CPU) miss the 40 ms S1 budget on most requests and fall back, and
retrieval's p95 grows to about 100 ms. See `2026-10-27_search_text.md` for the single-tenant
baseline.
