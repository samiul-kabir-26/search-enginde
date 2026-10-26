-- Interaction events stream from Kafka into `events` (PLAN.md M6): the Kafka engine table reads
-- search.events.v1 (JSONEachRow, written by search-api and `seed --events`) and the materialized
-- view inserts every block into the ReplacingMergeTree, which collapses resent event_ids.
-- Undecodable messages are skipped (at most 1000 per block) instead of stopping the stream.
-- `randomized` marks impressions shown in a randomized order, used by featurejob to estimate
-- position propensities (docs/DATA.md §5).
ALTER TABLE events ADD COLUMN IF NOT EXISTS randomized Bool DEFAULT false AFTER sponsored;

CREATE TABLE IF NOT EXISTS events_kafka
(
    tenant_id    String,
    event_id     UUID,
    type         Enum8('impression' = 1, 'click' = 2, 'add_to_cart' = 3, 'order' = 4, 'rfq' = 5),
    ts           DateTime64(3, 'UTC'),
    request_id   String,
    product_id   String,
    position     UInt16,
    user_id      String,
    session_id   String,
    sponsored    Bool,
    randomized   Bool,
    order_id     Nullable(String),
    quantity     Nullable(UInt32),
    value_minor  Nullable(Int64)
)
ENGINE = Kafka
SETTINGS kafka_broker_list = 'redpanda:9092',
         kafka_topic_list = 'search.events.v1',
         kafka_group_name = 'clickhouse-events',
         kafka_format = 'JSONEachRow',
         kafka_num_consumers = 1,
         kafka_skip_broken_messages = 1000,
         date_time_input_format = 'best_effort',
         input_format_skip_unknown_fields = 1;

CREATE MATERIALIZED VIEW IF NOT EXISTS events_mv TO events AS
SELECT tenant_id, event_id, type, ts, request_id, product_id, position, user_id, session_id,
       sponsored, randomized, order_id, quantity, value_minor
FROM events_kafka;
