// Package retrieval implements S2: run the query type's retrievers in parallel with every
// structured filter pushed down, tolerate individual failures, and fuse the lists with weighted
// RRF. Facet counts and the total match count come from the lexical index.
package retrieval
