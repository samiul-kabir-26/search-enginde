// Package featurejob aggregates a tenant's ClickHouse interaction events into Redis online
// features (PLAN.md R9, docs/MODELS.md §4): the examination propensity per position estimated
// from randomized traffic, position-debiased CTR, CVR and order counts per item, and category
// affinities and a price band per user. cmd/featurejob runs it on a schedule.
package featurejob
