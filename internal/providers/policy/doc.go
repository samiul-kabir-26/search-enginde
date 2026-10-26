// Package policy compiles and evaluates tenant CEL filter and boost rules (docs/TENANT_CONFIG.md
// §5 rule 7) in a restricted environment: variables `item` and `query`, the standard library
// only, field names checked at compile time, and a cost limit at compile and run time. It
// depends only on cel-go, so config validation and S3 share it.
package policy
