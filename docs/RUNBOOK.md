# RUNBOOK.md — Operating the search engine

What to do when something is wrong, and how to do the routine operations safely. Commands are for
the dev stack (`make …` from the repo root); production runs the same binaries and APIs.

Dashboards: Grafana **Search overview** and **Search resilience** (`make up-obs`, then
<http://localhost:3001>, folder *Search Engine*). Traces: Jaeger <http://localhost:16686>.

## 1. Start-up

```bash
make up-models && make migrate      # infra (MySQL, Redis, OpenSearch, Qdrant, Redpanda, ClickHouse, S3, Mailpit) + Triton
make up-obs                         # optional: OTel collector, Prometheus, Grafana, Jaeger
make run-control-api                # :8081  (each in its own terminal)
make run-search-api                 # :8080
make run-indexer                    # :8082
make run-featurejob                 # online features every 10 min, centroids hourly
make run-web                        # :3000  onboarding wizard and /admin
```

- Readiness: `curl localhost:8081/readyz` and `curl localhost:8080/readyz` return 200 once MySQL,
  Redis and the signing keys are reachable. Kafka is not a readiness dependency of search-api
  (impressions are dropped and counted in `events_dropped_total` while it is down).
- **Warm up before trusting latency.** After a restart, the first searches of each tenant open
  connections and warm OpenSearch, Qdrant and Triton; they can exceed their stage budgets and
  trip breakers for a few seconds (the search still answers, from lexical results). Send a few
  dozen searches, or wait for the breakers to close (3 s open, then a half-open probe).
- First time on a machine: `make setup`, `make data && make models`, `make seed` (demo tenant,
  catalog, platform admin from `PLATFORM_ADMIN_EMAIL`/`PASSWORD`), `make seed-events && make features`.

## 2. Reading a bad search

1. Run it with `"explain": true` (an API key with `search:debug`) or in the onboarding
   playground. The trace lists every stage: provider, latency, candidates in and out, and any
   `fallback_reason` or `error`.
2. `degraded: true` means a stage failed outright (for example every retriever failed); the
   response is still 200. A **fallback** is not degraded: a provider was skipped and the stage
   answered another way (see §3).
3. Compare with another version without activating anything: the playground, or
   `make eval VERSION=n` (all eval suites through the playground proxy, as the platform admin).

## 3. Fallbacks and circuit breakers

Every provider call (embedders, stores, scorers, online features, BYO endpoints) runs through
`resilience.Call` with a breaker per **(tenant, stage, provider)**:

| State | `provider_breaker_state` | Meaning |
|---|---|---|
| closed | 0 | Calls go through. |
| open | 2 | Tripped: ≥ 50% failures over ≥ 20 calls in a 10 s window, or 10 consecutive failures. Calls are skipped (`breaker_open`) for 3 s. |
| half-open | 1 | One probe call goes through; success closes the breaker, failure reopens it. |

A call that fails, times out at the stage budget (keeping 25% of the remaining budget for the
fallback), or is refused by a full BYO bulkhead (`bulkhead_full`, `BYO_MAX_CONCURRENCY` per
tenant, default 32) takes the stage's fallback:

| Stage | Fallback |
|---|---|
| S1 query understanding | Without the failed embedder: its vector retrievers are skipped |
| S2 retrieval | The remaining retrievers are fused; all failing → `degraded`, empty results |
| S3 policies | Rules that error keep the candidate; candidates not evaluated in time pass unchanged |
| S4 / S5 scorers | The previous order is kept (pre-rank, then RRF) |
| Online features | Declared feature defaults |

**Diagnosis.** In *Search resilience*, *Fallbacks by provider and reason* names the provider:
- `timeout` on platform embedders or Qdrant across tenants: the shared infrastructure is
  saturated or warming up (§1). Check Triton (`docker compose logs triton`), Qdrant and
  OpenSearch health, and CPU. The load-test reports show where this machine saturates.
- `timeout`/`error`/`breaker_open` on one `byo:…` provider: that tenant's endpoint or store is
  slow or down. Only that tenant is affected (the isolation test in `loadtest/reports/`); tell
  the tenant, or switch their stage back to the platform default by activating an earlier
  version (§6).
- `bulkhead_full`: the tenant sends more concurrent BYO calls than its bulkhead allows; their
  endpoint is too slow for their traffic.

Breakers reset on their own; there is nothing to reset by hand. Restarting search-api clears
them but also drops warm connections (§1).

## 4. Common failures

| Symptom | Likely cause | Action |
|---|---|---|
| search-api `/readyz` 503 | MySQL, Redis or the JWKS unreachable | `make ps`; check `JWKS_URL` and control-api |
| 401 on every search | Token expired (API tokens last `auth.api_token_ttl`, 15 min) or signed by a revoked key | Exchange the API key again (`POST /v1/auth/token`) |
| 429 | The tenant's plan rate limit (`search_rps`/`search_burst`, `events_rps`) | Honour `Retry-After`; raise the plan in `/admin/plans` if warranted |
| 403 `tenant is unknown or inactive` | No active config version | Publish and activate (`go_live` step or `/config/versions/{v}/activate`) |
| Results missing new products | Indexer not running, or events stuck in Kafka | `make run-indexer`; check consumer lag on `catalog.product.v1` (Redpanda console / `rpk group describe indexer`) |
| Index job `queued` forever | No indexer running | Start one; jobs are claimed with `FOR UPDATE SKIP LOCKED`, so several indexers are safe |
| Index job `failed` | Invalid upload lines, unreachable image hosts, store errors | Read `error` on `GET /index/jobs/{id}`; fix and run the index build again |
| Category prediction missing in traces | No centroids for the active index version | `make centroids TENANT=<slug>` |
| Personalization/CTR features all defaults | featurejob not running, or no events | `make features TENANT=<slug>`; check ClickHouse `events` |
| Go-live says `email_unverified` | Email not verified, or a token issued before verifying | Verify via the emailed link (dev: Mailpit <http://localhost:8025>); sign in again |
| ONNX uploads refused (503) | ONNX Runtime not loaded | `make onnxruntime`; check `ONNXRUNTIME_LIB` |
| Image search far worse than text | Learned rankers trained on text-only sessions applied to image queries | See `tools/eval/reports/2026-10-27_v5.md` vs `_v6.md`; prefer `default_linear` until the models handle image queries |

## 5. Re-indexing

A config change that alters indexed fields (mapping, custom field types, embedders) needs a new
index version; everything else is an online change.

- **From the catalog topic** (the tenant streams events): `make backfill TENANT=<slug>` builds
  `v{n+1}` from `catalog.product.v1` and swaps the aliases when it completes. Search keeps
  serving the old index until the swap.
- **From an upload**: `POST /v1/tenants/{tid}/catalog/uploads` (presigned PUT, JSONL), then
  `POST /v1/tenants/{tid}/index/jobs {config_version, upload_id}`; poll `GET /index/jobs/{id}`.
- After a re-index: `make centroids TENANT=<slug>` (category prediction reads the active index's
  centroids), then a few playground searches.
- Old index versions are not deleted automatically; remove them once the new one is verified.

## 6. Rolling back a config change

Every published config is an immutable version; rolling back is activating an older one.

```bash
# As the tenant (or a platform admin): list versions, then activate the previous one.
curl -s localhost:8081/v1/tenants/$TID/config/versions -H "Authorization: Bearer $TOKEN"
curl -s -X POST localhost:8081/v1/tenants/$TID/config/versions/$V/activate -H "Authorization: Bearer $TOKEN"
```

- Activation retires the current version and publishes `tenant.config.activated`; search-api
  instances serve the new version within milliseconds (a 30 s reconcile loop covers lost
  pub/sub messages; `TENANT_CONFIG_TTL` is the last backstop).
- Activation needs a verified email (platform admins are exempt).
- Before activating a risky version, run it in **shadow mode** first
  (`POST /config/versions/{v}/shadow {sample_rate: 0.05}`; comparisons land in ClickHouse
  `shadow_results`), and compare offline with `make eval VERSION=<v>`.
- A version whose index differs from the active one must be re-indexed before activation (§5).

## 7. Signing keys

control-api signs access tokens with the active Ed25519 key and publishes the JWKS that
search-api verifies against.

- **Scheduled rotation** runs in control-api every `auth.key_rotation` (default 30 days), under
  a MySQL lock so only one instance rotates. The retired key stays in the JWKS for
  `auth.key_retention` (default 7 days), longer than any token lifetime, so no token is cut off.
- **Rotate now** (for example after an operator leaves): `/admin/keys` → *Rotate now*, or
  `POST /v1/admin/keys/rotate` with a platform-admin token.
- **Compromised key**: rotate, then revoke the retired key (`/admin/keys` → *Revoke*, or
  `POST /v1/admin/keys/{kid}/revoke`). Tokens it signed stop verifying at once: every session
  signed with it, the admin's included, must sign in again, and API clients must exchange their
  keys again.
- Tenant API keys are separate: revoke one with `DELETE /v1/tenants/{tid}/api-keys/{id}`.

## 8. Platform settings

`/admin/settings` and `/admin/ads` edit `platform_settings` (auth TTLs and password policy; ad
controls). Saving validates every bound and publishes a change that every service picks up
within seconds. Plans (rate limits, BYO, egress allowlist, page depth) are edited in
`/admin/plans`.

## 9. Evaluation and load testing

- `make eval` — text, image and ranking suites on the demo tenant's active version →
  `tools/eval/reports/{date}_v{version}.md`; `make eval VERSION=n` for a published version.
- `make loadtest` (text, 50 RPS), `make loadtest-all` (text, image, similar, BYO remote rank,
  isolation) → `loadtest/reports/`. The BYO and isolation scenarios need `make loadtest-setup`
  once, `python3 loadtest/slow_kserve.py` running, and control-api and search-api started with
  `EGRESS_TRUSTED_HOSTS=localhost,127.0.0.1`. Warm the stack up first (§1), and run on a quiet
  machine: the dev stack shares every core with Triton and k6.
