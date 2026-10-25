-- Shadow-mode comparisons (PLAN.md R8): a sampled request run on the shadow config version next
-- to the active one. Shadow results are never returned to callers.
CREATE TABLE IF NOT EXISTS shadow_results
(
    ts                 DateTime64(3, 'UTC'),
    request_id         String,
    tenant_id          LowCardinality(String),
    query_type         LowCardinality(String),
    query_hash         String,
    active_version     UInt32,
    shadow_version     UInt32,
    active_top         Array(String),
    shadow_top         Array(String),
    overlap_at_10      Float32,
    active_latency_ms  Float32,
    shadow_latency_ms  Float32,
    shadow_degraded    UInt8
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (tenant_id, shadow_version, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY;
