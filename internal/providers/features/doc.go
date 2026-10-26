// Package features stores online item, vendor and user features in Redis (PLAN.md R9): featurejob,
// the indexer and PUT /v1/vendors write them, and S4 reads them in one pipelined round trip.
package features
