-- Interaction events (docs/API.md §1 POST /v1/events). ReplacingMergeTree
-- collapses duplicate event_id rows on merge, so query with FINAL for exact dedup.
-- The Kafka engine table and materialized view that feed this table land in M6.
CREATE TABLE IF NOT EXISTS events
(
    tenant_id    LowCardinality(String),
    event_id     UUID,
    type         Enum8('impression' = 1, 'click' = 2, 'add_to_cart' = 3, 'order' = 4, 'rfq' = 5),
    ts           DateTime64(3, 'UTC'),
    request_id   String,
    product_id   String,
    position     UInt16,
    user_id      String,
    session_id   String,
    sponsored    Bool,
    order_id     Nullable(String),
    quantity     Nullable(UInt32),
    value_minor  Nullable(Int64),
    ingested_at  DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(ts)
ORDER BY (tenant_id, type, toDate(ts), event_id);
