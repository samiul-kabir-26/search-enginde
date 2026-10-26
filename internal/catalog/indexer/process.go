// Package indexer applies catalog events to a tenant's lexical and vector stores: it maps
// products, embeds them through the tenant's embedders and writes both stores. The lexical
// index arbitrates event order (source_version), so stale events never reach the vector store.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Target is where one tenant's catalog is indexed, built from its active config version.
type Target struct {
	Tenant        tenant.ID
	ConfigVersion int
	Mapper        *catalog.Mapper
	// VendorMapper extracts vendor features from product payloads (nil without a
	// catalog.vendor_mapping); Vendors stores them.
	VendorMapper *catalog.VendorMapper
	Vendors      VendorWriter
	Registry     *catalog.Registry
	// TextEmbedder fills the `text` vector from Product.EmbeddingText.
	TextEmbedder pipeline.Embedder
	// ImageEmbedder fills the `image` vector from the main image; nil skips image vectors.
	ImageEmbedder pipeline.Embedder
	Lexical       pipeline.LexicalWriter
	Vectors       pipeline.VectorWriter
	// BYO is set when the tenant uses its own embedders or vector store.
	BYO     bool
	closers []io.Closer
}

// Close releases the target's BYO connections (embedder and store clients).
func (t *Target) Close() error {
	var errs []error
	for _, c := range t.closers {
		errs = append(errs, c.Close())
	}
	for _, e := range []pipeline.Embedder{t.TextEmbedder, t.ImageEmbedder} {
		if c, ok := e.(io.Closer); ok {
			errs = append(errs, c.Close())
		}
	}
	t.closers = nil
	return errors.Join(errs...)
}

// ImageFetcher downloads a product image.
type ImageFetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Result reports what one batch did. UpsertedIDs and DeletedIDs list the products whose state
// changed, in batch order.
type Result struct {
	UpsertedIDs  []string
	DeletedIDs   []string
	Stale        int
	Invalid      int
	MissingImage int
	// Vendors counts vendor feature updates written.
	Vendors int
}

// Add accumulates r2 into r.
func (r *Result) Add(r2 Result) {
	r.UpsertedIDs = append(r.UpsertedIDs, r2.UpsertedIDs...)
	r.DeletedIDs = append(r.DeletedIDs, r2.DeletedIDs...)
	r.Stale += r2.Stale
	r.Invalid += r2.Invalid
	r.MissingImage += r2.MissingImage
	r.Vendors += r2.Vendors
}

// Processor applies batches of catalog events.
type Processor struct {
	images      ImageFetcher
	log         *slog.Logger
	fetchLimit  int
	chunkSize   int
	parallelism int
}

// NewProcessor returns a Processor. fetchLimit bounds concurrent image downloads per chunk and
// parallelism the chunks in flight, which keeps the model server's dynamic batcher fed.
func NewProcessor(images ImageFetcher, log *slog.Logger, fetchLimit, parallelism int) *Processor {
	return &Processor{images: images, log: log, fetchLimit: max(fetchLimit, 1), chunkSize: 64, parallelism: max(parallelism, 1)}
}

// ProcessAll reduces events to the latest per product, then processes chunks of them
// concurrently. Chunks never share a product, so their writes cannot reorder one product's
// changes.
func (p *Processor) ProcessAll(ctx context.Context, t *Target, events []catalog.ProductEvent) (Result, error) {
	events = latestPerProduct(events)
	var parts [][]catalog.ProductEvent
	for i := 0; i < len(events); i += p.chunkSize {
		parts = append(parts, events[i:min(i+p.chunkSize, len(events))])
	}
	results := make([]Result, len(parts))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.parallelism)
	for i, part := range parts {
		g.Go(func() error {
			r, err := p.Process(gctx, t, part)
			results[i] = r
			return err
		})
	}
	err := g.Wait()
	var res Result
	for _, r := range results {
		res.Add(r)
	}
	if err != nil {
		return res, fmt.Errorf("process: %w", err)
	}
	return res, nil
}

// Process applies events for t's tenant. Invalid events are logged, counted and skipped; an
// error means a store or embedder failed and the whole batch must be retried (every write is
// idempotent, so a retry after a partial failure is safe).
func (p *Processor) Process(ctx context.Context, t *Target, events []catalog.ProductEvent) (Result, error) {
	var res Result
	var upserts []pipeline.IndexDoc
	var deletes []pipeline.IndexDelete
	vendors := map[string]catalog.Vendor{}
	for _, e := range latestPerProduct(events) {
		if e.TenantID != string(t.Tenant) {
			res.Invalid++
			p.log.Warn("catalog event for another tenant", "tenant", t.Tenant, "event_tenant", e.TenantID, "event_id", e.EventID)
			continue
		}
		if e.Op == catalog.OpDelete {
			deletes = append(deletes, pipeline.IndexDelete{ProductID: e.ProductID, SourceVersion: e.SourceVersion})
			continue
		}
		prod, errs := t.Mapper.Map(string(t.Tenant), e.Product)
		if len(errs) == 0 && prod.ProductID != e.ProductID {
			errs = append(errs, platform.FieldError{Path: "/product_id", Code: "mismatch", Message: fmt.Sprintf("mapped product_id %q differs from envelope %q", prod.ProductID, e.ProductID)})
		}
		if len(errs) > 0 {
			res.Invalid++
			p.log.Warn("invalid product", "tenant", t.Tenant, "product_id", e.ProductID, "event_id", e.EventID, "errors", errs)
			continue
		}
		if v, ok := t.VendorMapper.Extract(e.Product); ok && prod.VendorID != "" {
			v.ID = prod.VendorID
			if errs := v.Validate("/vendor"); len(errs) > 0 {
				p.log.Warn("invalid vendor fields in product skipped", "tenant", t.Tenant, "product_id", e.ProductID, "errors", errs)
			} else {
				vendors[prod.ProductID] = v
			}
		}
		upserts = append(upserts, pipeline.IndexDoc{Product: prod, SourceVersion: e.SourceVersion})
	}

	if len(upserts) > 0 {
		stale, err := t.Lexical.Upsert(ctx, upserts)
		if err != nil {
			return res, err
		}
		res.Stale += len(stale)
		fresh := withoutStale(upserts, stale)
		missing, err := p.writeVectors(ctx, t, fresh)
		if err != nil {
			return res, err
		}
		for _, d := range fresh {
			res.UpsertedIDs = append(res.UpsertedIDs, d.Product.ProductID)
		}
		res.MissingImage += missing
		res.Vendors = p.writeVendors(ctx, t, fresh, vendors)
	}

	if len(deletes) > 0 {
		stale, err := t.Lexical.Delete(ctx, deletes)
		if err != nil {
			return res, err
		}
		res.Stale += len(stale)
		staleSet := toSet(stale)
		ids := make([]string, 0, len(deletes))
		for _, d := range deletes {
			if !staleSet[d.ProductID] {
				ids = append(ids, d.ProductID)
			}
		}
		if err := t.Vectors.Delete(ctx, ids); err != nil {
			return res, err
		}
		res.DeletedIDs = ids
	}
	return res, nil
}

// VendorWriter stores vendor features taken from product payloads.
type VendorWriter interface {
	PutVendors(ctx context.Context, tid tenant.ID, vendors []catalog.Vendor) error
}

// writeVendors stores the vendor fields of freshly indexed products (stale events carry older
// vendor data). A store failure is logged, not retried: vendor features must not stall catalog
// indexing, and the next product or vendor update rewrites them.
func (p *Processor) writeVendors(ctx context.Context, t *Target, fresh []pipeline.IndexDoc, byProduct map[string]catalog.Vendor) int {
	if t.Vendors == nil || len(byProduct) == 0 {
		return 0
	}
	var vs []catalog.Vendor
	for _, d := range fresh {
		if v, ok := byProduct[d.Product.ProductID]; ok {
			vs = append(vs, v)
		}
	}
	if err := t.Vendors.PutVendors(ctx, t.Tenant, vs); err != nil {
		p.log.Warn("vendor features not stored", "tenant", t.Tenant, "vendors", len(vs), "err", err)
		return 0
	}
	return len(vs)
}

func (p *Processor) writeVectors(ctx context.Context, t *Target, docs []pipeline.IndexDoc) (int, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Product.EmbeddingText()
	}
	var textVecs, imageVecs [][]float32
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		textVecs, err = t.TextEmbedder.EmbedText(gctx, texts)
		if err != nil {
			return fmt.Errorf("embed text: %w", err)
		}
		return nil
	})
	if t.ImageEmbedder != nil {
		g.Go(func() error {
			var err error
			imageVecs, err = p.embedImages(gctx, t, docs)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return 0, fmt.Errorf("embed: %w", err)
	}

	missing := 0
	points := make([]pipeline.VectorPoint, len(docs))
	for i, d := range docs {
		vecs := map[string][]float32{pipeline.VectorText: textVecs[i]}
		if t.ImageEmbedder != nil {
			if imageVecs[i] != nil {
				vecs[pipeline.VectorImage] = imageVecs[i]
			} else {
				missing++
			}
		}
		points[i] = pipeline.VectorPoint{ProductID: d.Product.ProductID, Vectors: vecs, Payload: vectorstore.Payload(d.Product, t.Registry)}
	}
	return missing, t.Vectors.Upsert(ctx, points)
}

// embedImages returns one vector per doc, nil where the product has no usable main image.
func (p *Processor) embedImages(ctx context.Context, t *Target, docs []pipeline.IndexDoc) ([][]float32, error) {
	blobs := make([][]byte, len(docs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.fetchLimit)
	for i, d := range docs {
		if len(d.Product.ImageURLs) == 0 {
			continue
		}
		g.Go(func() error {
			b, err := p.images.Fetch(gctx, d.Product.ImageURLs[0])
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("image fetch failed", "tenant", t.Tenant, "product_id", d.Product.ProductID, "err", err)
				return nil
			}
			blobs[i] = b
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("fetch images: %w", err)
	}

	var idx []int
	var batch [][]byte
	for i, b := range blobs {
		if b != nil {
			idx = append(idx, i)
			batch = append(batch, b)
		}
	}
	out := make([][]float32, len(docs))
	vecs, err := t.ImageEmbedder.EmbedImage(ctx, batch)
	if errors.Is(err, pipeline.ErrInvalidInput) {
		vecs, err = p.embedEach(ctx, t, docs, idx, batch)
	}
	if err != nil {
		return nil, fmt.Errorf("embed images: %w", err)
	}
	for j, i := range idx {
		out[i] = vecs[j]
	}
	return out, nil
}

// embedEach isolates undecodable images after a batch was rejected, leaving their vector nil.
func (p *Processor) embedEach(ctx context.Context, t *Target, docs []pipeline.IndexDoc, idx []int, batch [][]byte) ([][]float32, error) {
	out := make([][]float32, len(batch))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for j, b := range batch {
		g.Go(func() error {
			v, err := t.ImageEmbedder.EmbedImage(gctx, [][]byte{b})
			if errors.Is(err, pipeline.ErrInvalidInput) {
				p.log.Warn("image rejected by embedder", "tenant", t.Tenant, "product_id", docs[idx[j]].Product.ProductID, "err", err)
				return nil
			}
			if err != nil {
				return err
			}
			mu.Lock()
			out[j] = v[0]
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("embed images one by one: %w", err)
	}
	return out, nil
}

// latestPerProduct keeps one event per product in arrival order: the highest source_version,
// where an unversioned event (0, such as a tombstone) wins by arriving later.
func latestPerProduct(events []catalog.ProductEvent) []catalog.ProductEvent {
	pos := map[string]int{}
	var out []catalog.ProductEvent
	for _, e := range events {
		key := e.TenantID + ":" + e.ProductID
		i, seen := pos[key]
		switch {
		case !seen:
			pos[key] = len(out)
			out = append(out, e)
		case e.SourceVersion == 0 || out[i].SourceVersion == 0 || e.SourceVersion >= out[i].SourceVersion:
			out[i] = e
		}
	}
	return out
}

func withoutStale(docs []pipeline.IndexDoc, stale []string) []pipeline.IndexDoc {
	if len(stale) == 0 {
		return docs
	}
	set := toSet(stale)
	out := docs[:0:0]
	for _, d := range docs {
		if !set[d.Product.ProductID] {
			out = append(out, d)
		}
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
