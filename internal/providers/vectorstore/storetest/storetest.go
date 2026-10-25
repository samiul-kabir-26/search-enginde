// Package storetest is a conformance suite for BYO vector store adapters, run by each
// adapter's integration test against a real store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Tenant is the tenant the suite stores secrets for.
const Tenant = tenant.ID("01JTESTTENANT00000000000AA")

// Dim is the dimension of both test vectors.
const Dim = 4

// Secrets is an in-memory SecretStore.
type Secrets map[string]string

// Put implements security.SecretStore.
func (s Secrets) Put(_ context.Context, _, _, value string) (string, error) {
	ref := fmt.Sprintf("01JTESTSECRET%013d", len(s))
	s[ref] = value
	return ref, nil
}

// Get implements security.SecretStore.
func (s Secrets) Get(_ context.Context, _, ref string) (security.Secret, error) {
	v, ok := s[ref]
	if !ok {
		return security.Secret{}, security.ErrSecretNotFound
	}
	return security.NewSecret(v), nil
}

// BYO returns registry BYO deps that trust endpoint's host (a local container) and hold secret.
func BYO(t *testing.T, endpoint, secret string) (*registry.BYO, string) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	secrets := Secrets{}
	ref := ""
	if secret != "" {
		ref, _ = secrets.Put(context.Background(), string(Tenant), "password", secret)
	}
	return &registry.BYO{Secrets: secrets, Egress: security.NewEgress(security.EgressConfig{Trusted: []string{u.Host}})}, ref
}

// Block is the store block the suite expects: vectors "text" (field emb_text) and "image"
// (emb_image), product IDs in "pid", custom.season stored as "season".
func Block(collection, connID string) tenant.StoreConfig {
	return tenant.StoreConfig{
		Provider: tenant.ProviderBYO, ConnectionID: connID, Collection: collection,
		Vectors: map[string]tenant.VectorSpec{
			pipeline.VectorText:  {Name: "emb_text", Dim: Dim, Metric: "cosine"},
			pipeline.VectorImage: {Name: "emb_image", Dim: Dim, Metric: "cosine"},
		},
		PayloadMapping: map[string]string{"product_id": "pid", "custom.season": "season"},
	}
}

func unit(v ...float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

var created = time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)

func point(id string, price int64, status string, regions []string, text, image []float32) pipeline.VectorPoint {
	p := pipeline.VectorPoint{ProductID: id, Vectors: map[string][]float32{pipeline.VectorText: text}, Payload: map[string]any{
		"vendor_id": "v_" + id, "brand": "Rivet", "price_minor": price, "currency": "USD", "status": status,
		"ship_regions": regions, "category_ids": []string{"bags", "bags/" + id}, "rating": 4.5, "rating_count": int64(12),
		"stock": int64(3), "listing_completeness": 0.8, "image_count": int64(1), "created_at": created.Format(time.RFC3339),
		"custom.season": "summer",
	}}
	if image != nil {
		p.Vectors[pipeline.VectorImage] = image
	}
	return p
}

func cond(field string, op filter.Op, v any, t catalog.FieldType) filter.Expr {
	return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v, Type: t}}
}

func ids(hits []pipeline.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ProductID
	}
	return out
}

// Run exercises a store built for Block: writes, filtered searches with pushdown, gets,
// counts, scrolls, describe and deletes. settle waits for writes to become searchable.
func Run(t *testing.T, st registry.BYOVectorStore, settle func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := st.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	info, err := st.Describe(ctx)
	if err != nil || !info.Exists || info.Vectors["emb_text"].Dim != Dim {
		t.Fatalf("describe = %+v, %v", info, err)
	}
	p1 := point("p1", 4500, "active", []string{"US", "DE"}, unit(1, 0, 0, 0), unit(0, 1, 0, 0))
	p2 := point("p2", 9900, "inactive", []string{"US"}, unit(0.9, 0.1, 0, 0), unit(0, 0.9, 0.1, 0))
	p3 := point("p3", 2500, "active", []string{"FR"}, unit(0, 0, 1, 0), nil)
	if err := st.Upsert(ctx, []pipeline.VectorPoint{p1, p2, p3}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	settle()

	hits, err := st.Search(ctx, pipeline.VectorSearch{Vector: pipeline.VectorText, Query: unit(1, 0, 0, 0), K: 3})
	if err != nil || len(hits) != 3 || hits[0].ProductID != "p1" || math.Abs(hits[0].Score-1) > 1e-3 {
		t.Fatalf("unfiltered search = %v (%v), %v", ids(hits), hits, err)
	}
	d := hits[0].Doc
	if d.VendorID != "v_p1" || d.PriceMinor != 4500 || d.Rating == nil || *d.Rating != 4.5 || d.RatingCount != 12 ||
		len(d.CategoryIDs) != 2 || d.CreatedAt == nil || !d.CreatedAt.Equal(created) {
		t.Errorf("ranking doc = %+v", d)
	}

	f := filter.All(cond("status", filter.Eq, "active", catalog.TypeKeyword), cond("price_minor", filter.Lte, int64(5000), catalog.TypeInt))
	hits, err = st.Search(ctx, pipeline.VectorSearch{Vector: pipeline.VectorText, Query: unit(1, 0, 0, 0), K: 3, Filter: f})
	if got := ids(hits); err != nil || len(got) != 2 || got[0] != "p1" || got[1] != "p3" {
		t.Errorf("filtered search = %v, %v", got, err)
	}
	f = filter.All(cond("ship_regions", filter.Contains, "US", catalog.TypeKeywordArray), cond("product_id", filter.Ne, "p1", catalog.TypeKeyword),
		cond("custom.season", filter.In, []any{"summer"}, catalog.TypeKeyword))
	hits, err = st.Search(ctx, pipeline.VectorSearch{Vector: pipeline.VectorText, Query: unit(1, 0, 0, 0), K: 3, Filter: f})
	if got := ids(hits); err != nil || len(got) != 1 || got[0] != "p2" {
		t.Errorf("contains/ne/custom search = %v, %v", got, err)
	}
	hits, err = st.Search(ctx, pipeline.VectorSearch{Vector: pipeline.VectorImage, Query: unit(0, 1, 0, 0), K: 1})
	if got := ids(hits); err != nil || len(got) != 1 || got[0] != "p1" {
		t.Errorf("image search = %v, %v", got, err)
	}

	vp, doc, err := st.Get(ctx, "p1")
	if err != nil || len(vp.Vectors[pipeline.VectorText]) != Dim || len(vp.Vectors[pipeline.VectorImage]) != Dim || doc.PriceMinor != 4500 {
		t.Errorf("get = %+v %+v, %v", vp, doc, err)
	}
	if _, _, err := st.Get(ctx, "nope"); !errors.Is(err, pipeline.ErrNotFound) {
		t.Errorf("get missing = %v", err)
	}
	if n, err := st.Count(ctx, 0); err != nil || n != 3 {
		t.Errorf("count = %d, %v", n, err)
	}
	var scrolled []string
	err = st.ScrollVectors(ctx, 0, pipeline.VectorText, func(id string, vec []float32, cats []string) error {
		if len(vec) != Dim || len(cats) != 2 {
			t.Errorf("scroll %s: vec %v cats %v", id, vec, cats)
		}
		scrolled = append(scrolled, id)
		return nil
	})
	sort.Strings(scrolled)
	if err != nil || len(scrolled) != 3 || scrolled[0] != "p1" {
		t.Errorf("scroll = %v, %v", scrolled, err)
	}

	p1.Payload["price_minor"] = int64(4000)
	if err := st.Upsert(ctx, []pipeline.VectorPoint{p1}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := st.Delete(ctx, []string{"p3"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	settle()
	if n, err := st.Count(ctx, 0); err != nil || n != 2 {
		t.Errorf("count after delete = %d, %v", n, err)
	}
	if _, _, err := st.Get(ctx, "p3"); !errors.Is(err, pipeline.ErrNotFound) {
		t.Errorf("deleted product: %v", err)
	}
	if _, doc, err := st.Get(ctx, "p1"); err != nil || doc.PriceMinor != 4000 {
		t.Errorf("upsert replaces: %+v, %v", doc, err)
	}
	f = cond("vendor_id", filter.Eq, "v_p1", catalog.TypeKeyword)
	if _, err := st.Search(ctx, pipeline.VectorSearch{Vector: pipeline.VectorText, Query: unit(1, 0, 0, 0), K: 1, Filter: f}); err != nil && info.AnyField {
		t.Errorf("schemaless stores filter on any field: %v", err)
	}
}
