// Command indexer consumes catalog.product.v1 events, maps products to the
// canonical schema, embeds them and upserts to the lexical and vector stores.
// It also runs backfill jobs.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "indexer: not implemented yet (milestone M1, see PLAN.md)")
	os.Exit(1)
}
