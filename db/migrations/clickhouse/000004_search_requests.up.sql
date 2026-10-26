-- Request log (PLAN.md M7): one row per served search page, written by search-api (and by
-- `seed -events` for the simulated history) to search.requests.v1. Training joins it with the
-- impressions of the same request_id to recover each request's query (docs/MODELS.md §5).
CREATE TABLE IF NOT EXISTS search_requests
(
    tenant_id       LowCardinality(String),
    request_id      String,
    ts              DateTime64(3, 'UTC'),
    type            LowCardinality(String),
    query           String,
    anchor_id       String,
    user_id         String,
    session_id      String,
    config_version  UInt32,
    page_offset     UInt32,
    results         UInt16,
    ingested_at     DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(ts)
ORDER BY (tenant_id, toDate(ts), request_id)
TTL toDateTime(ts) + INTERVAL 180 DAY;

CREATE TABLE IF NOT EXISTS search_requests_kafka
(
    tenant_id       String,
    request_id      String,
    ts              DateTime64(3, 'UTC'),
    type            String,
    query           String,
    anchor_id       String,
    user_id         String,
    session_id      String,
    config_version  UInt32,
    page_offset     UInt32,
    results         UInt16
)
ENGINE = Kafka
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'search.requests.v1',
         kafka_group_name = 'clickhouse-requests',
         kafka_format = 'JSONEachRow',
         kafka_num_consumers = 1,
         kafka_skip_broken_messages = 1000,
         date_time_input_format = 'best_effort',
         input_format_skip_unknown_fields = 1;

CREATE MATERIALIZED VIEW IF NOT EXISTS search_requests_mv TO search_requests AS
SELECT tenant_id, request_id, ts, type, query, anchor_id, user_id, session_id, config_version, page_offset, results
FROM search_requests_kafka;
