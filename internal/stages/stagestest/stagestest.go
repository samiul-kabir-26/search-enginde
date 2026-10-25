// Package stagestest provides deterministic in-memory providers for stage and pipeline tests:
// a bag-of-words embedder, a lexical index and a vector store over a small fixed catalog, with
// filter evaluation so pushdown can be asserted.
package stagestest

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/scorer/defaultlinear"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// TenantID is the fake tenant.
const TenantID = tenant.ID("01JTESTTENANT00000000000AA")

// Product is a catalog entry of the fake stores.
type Product struct {
	ID          string
	Title       string
	Brand       string
	PriceMinor  int64
	Stock       int64
	Status      string
	Categories  []string
	RatingCount int64
	Rating      float64
}

// Catalog is the fixed test catalog.
func Catalog() []Product {
	return []Product{
		{"p01", "Red leather handbag", "Rivet", 4500, 3, "active", []string{"bags", "handbags"}, 120, 4.6},
		{"p02", "Red leather tote bag", "Rivet", 7900, 5, "active", []string{"bags", "handbags"}, 40, 4.1},
		{"p03", "Black leather handbag", "Solimo", 3900, 0, "active", []string{"bags", "handbags"}, 300, 4.8},
		{"p04", "Red canvas backpack", "Goodthreads", 2500, 9, "active", []string{"bags", "backpacks"}, 75, 3.9},
		{"p05", "Brown leather wallet", "Rivet", 1900, 2, "active", []string{"bags", "wallets"}, 500, 4.4},
		{"p06", "Red running shoes", "Pinzon", 5900, 4, "active", []string{"shoes", "running-shoes"}, 210, 4.2},
		{"p07", "Stainless steel water bottle", "Solimo", 1500, 7, "active", []string{"kitchen", "bottles"}, 900, 4.7},
		{"p08", "Red leather handbag deluxe", "Stone & Beam", 4800, 1, "inactive", []string{"bags", "handbags"}, 10, 4.0},
		{"p09", "Oak wood desk lamp", "Stone & Beam", 3500, 6, "active", []string{"lighting", "lamps"}, 60, 4.3},
		{"p10", "Leather handbag strap", "Pinzon", 900, 12, "active", []string{"bags", "accessories"}, 30, 3.8},
	}
}

func (p Product) doc() *pipeline.Doc {
	r := p.Rating
	return &pipeline.Doc{Title: p.Title, Brand: p.Brand, VendorID: "v_" + strings.ToLower(p.Brand[:1]),
		PriceMinor: p.PriceMinor, Currency: "USD", CategoryIDs: p.Categories, Rating: &r, RatingCount: p.RatingCount,
		ListingCompleteness: 0.8, ImageCount: 1}
}

// Embed is a deterministic, L2-normalized bag-of-words embedding.
func Embed(text string, dim int) []float32 {
	v := make([]float32, dim)
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(tok, ".,")))
		v[h.Sum32()%uint32(dim)] += 1 //nolint:gosec // dim is small and positive
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / math.Sqrt(n))
		}
	}
	return v
}

// Embedder embeds with Embed; Fail makes every call fail.
type Embedder struct {
	Size int
	Fail bool
}

// EmbedText implements pipeline.Embedder.
func (e *Embedder) EmbedText(_ context.Context, texts []string) ([][]float32, error) {
	if e.Fail {
		return nil, errors.New("embedder down")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = Embed(t, e.Size)
	}
	return out, nil
}

// EmbedImage implements pipeline.Embedder.
func (e *Embedder) EmbedImage(_ context.Context, imgs [][]byte) ([][]float32, error) {
	if e.Fail {
		return nil, errors.New("embedder down")
	}
	out := make([][]float32, len(imgs))
	for i, b := range imgs {
		out[i] = Embed(string(b), e.Size)
	}
	return out, nil
}

// Dim implements pipeline.Embedder.
func (e *Embedder) Dim() int { return e.Size }

// Matches evaluates a compiled filter against a product.
func Matches(e filter.Expr, p Product) bool {
	switch {
	case e.IsZero():
		return true
	case e.And != nil:
		for _, k := range e.And {
			if !Matches(k, p) {
				return false
			}
		}
		return true
	case e.Or != nil:
		for _, k := range e.Or {
			if Matches(k, p) {
				return true
			}
		}
		return false
	}
	c := e.Cond
	var vals []any
	switch c.Field {
	case "product_id":
		vals = []any{p.ID}
	case "brand":
		vals = []any{p.Brand}
	case "status":
		vals = []any{p.Status}
	case "price_minor":
		vals = []any{p.PriceMinor}
	case "stock":
		vals = []any{p.Stock}
	case "category_ids":
		for _, x := range p.Categories {
			vals = append(vals, x)
		}
	default:
		return false
	}
	cmp := func(want any, ok func(int) bool) bool {
		for _, v := range vals {
			if a, isInt := v.(int64); isInt {
				w := want.(int64)
				switch {
				case a < w:
					if ok(-1) {
						return true
					}
				case a > w:
					if ok(1) {
						return true
					}
				default:
					if ok(0) {
						return true
					}
				}
			} else if v == want && ok(0) {
				return true
			}
		}
		return false
	}
	switch c.Op {
	case filter.Eq, filter.Contains:
		return cmp(c.Value, func(r int) bool { return r == 0 })
	case filter.Ne:
		return !cmp(c.Value, func(r int) bool { return r == 0 })
	case filter.In, filter.Nin:
		in := slices.ContainsFunc(c.Value.([]any), func(w any) bool { return cmp(w, func(r int) bool { return r == 0 }) })
		return in == (c.Op == filter.In)
	case filter.Gt:
		return cmp(c.Value, func(r int) bool { return r > 0 })
	case filter.Gte:
		return cmp(c.Value, func(r int) bool { return r >= 0 })
	case filter.Lt:
		return cmp(c.Value, func(r int) bool { return r < 0 })
	case filter.Lte:
		return cmp(c.Value, func(r int) bool { return r <= 0 })
	case filter.Range:
		b := c.Value.(filter.Bounds)
		ok := true
		if b.Gt != nil {
			ok = ok && cmp(b.Gt, func(r int) bool { return r > 0 })
		}
		if b.Gte != nil {
			ok = ok && cmp(b.Gte, func(r int) bool { return r >= 0 })
		}
		if b.Lt != nil {
			ok = ok && cmp(b.Lt, func(r int) bool { return r < 0 })
		}
		if b.Lte != nil {
			ok = ok && cmp(b.Lte, func(r int) bool { return r <= 0 })
		}
		return ok
	}
	return false
}

// Lexical is a token-overlap index over Catalog. It records the last request.
type Lexical struct {
	Products []Product
	Fail     bool
	Last     pipeline.LexicalSearch
}

// Search implements pipeline.LexicalIndex.
func (l *Lexical) Search(_ context.Context, req pipeline.LexicalSearch) (pipeline.LexicalResult, error) {
	if l.Fail {
		return pipeline.LexicalResult{}, errors.New("opensearch down")
	}
	l.Last = req
	all := filter.All(req.Base, req.Request)
	type scored struct {
		p Product
		s float64
	}
	var hits []scored
	var total int64
	terms := strings.Fields(strings.ToLower(req.Text))
	for _, p := range l.Products {
		if !Matches(req.Base, p) {
			continue
		}
		var s float64
		if req.Popular {
			s = float64(p.RatingCount)
		} else {
			title := strings.ToLower(p.Title + " " + p.Brand)
			for _, t := range terms {
				if strings.Contains(title, t) {
					s++
				}
			}
			if s == 0 && len(terms) > 0 {
				continue
			}
		}
		total++
		if Matches(all, p) {
			hits = append(hits, scored{p, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].s > hits[j].s })
	res := pipeline.LexicalResult{TotalMatches: total, Facets: map[string][]pipeline.FacetValue{}}
	for i, h := range hits {
		if req.K > 0 && i >= req.K {
			break
		}
		res.Hits = append(res.Hits, pipeline.Hit{ProductID: h.p.ID, Score: h.s * 1.7, Doc: h.p.doc()})
	}
	for _, f := range req.Facets {
		if f.Field == "brand" {
			counts := map[string]int64{}
			for _, h := range hits {
				counts[h.p.Brand]++
			}
			keys := make([]string, 0, len(counts))
			for k := range counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				res.Facets["brand"] = append(res.Facets["brand"], pipeline.FacetValue{Value: k, Count: counts[k]})
			}
		}
	}
	return res, nil
}

// Docs implements pipeline.LexicalIndex.
func (l *Lexical) Docs(_ context.Context, ids []string) (map[string]*pipeline.Doc, error) {
	out := map[string]*pipeline.Doc{}
	for _, p := range l.Products {
		if slices.Contains(ids, p.ID) {
			out[p.ID] = p.doc()
		}
	}
	return out, nil
}

// Upsert implements pipeline.LexicalIndex.
func (l *Lexical) Upsert(context.Context, []pipeline.IndexDoc) ([]string, error) { return nil, nil }

// Delete implements pipeline.LexicalIndex.
func (l *Lexical) Delete(context.Context, []pipeline.IndexDelete) ([]string, error) { return nil, nil }

// Health implements pipeline.LexicalIndex.
func (l *Lexical) Health(context.Context) error { return nil }

// Vectors scores the catalog by cosine against Embed(title) (`text`) or Embed(title+" photo")
// (`image`), applying the filter.
type Vectors struct {
	Products []Product
	Dim      int
	Fail     bool
}

// Search implements pipeline.VectorStore.
func (v *Vectors) Search(_ context.Context, req pipeline.VectorSearch) ([]pipeline.Hit, error) {
	if v.Fail {
		return nil, errors.New("qdrant down")
	}
	var hits []pipeline.Hit
	for _, p := range v.Products {
		if !Matches(req.Filter, p) {
			continue
		}
		text := p.Title
		if req.Vector == pipeline.VectorImage {
			text += " photo"
		}
		e := Embed(text, v.Dim)
		var dot float64
		for i := range e {
			dot += float64(e[i] * req.Query[i])
		}
		hits = append(hits, pipeline.Hit{ProductID: p.ID, Score: math.Round(dot*1e4) / 1e4, Doc: p.doc()})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > req.K {
		hits = hits[:req.K]
	}
	return hits, nil
}

// Get implements pipeline.VectorStore.
func (v *Vectors) Get(_ context.Context, id string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	for _, p := range v.Products {
		if p.ID == id {
			return &pipeline.VectorPoint{ProductID: id, Vectors: map[string][]float32{
				pipeline.VectorText: Embed(p.Title, v.Dim), pipeline.VectorImage: Embed(p.Title+" photo", v.Dim),
			}}, p.doc(), nil
		}
	}
	return nil, nil, pipeline.ErrNotFound
}

// Upsert implements pipeline.VectorStore.
func (v *Vectors) Upsert(context.Context, []pipeline.VectorPoint) error { return nil }

// Delete implements pipeline.VectorStore.
func (v *Vectors) Delete(context.Context, []string) error { return nil }

// Health implements pipeline.VectorStore.
func (v *Vectors) Health(context.Context) error { return nil }

// Dim is the fake embedding dimension.
const Dim = 32

// Providers returns fake providers over Catalog, with centroids built from the catalog.
func Providers() (*pipeline.Providers, *Lexical, *Vectors) {
	prods := Catalog()
	lex := &Lexical{Products: prods}
	vec := &Vectors{Products: prods, Dim: Dim}
	b := centroids.NewBuilder()
	for _, p := range prods {
		_ = b.Add(Embed(p.Title, Dim), p.Categories)
	}
	return &pipeline.Providers{
		TextEmbedder: &Embedder{Size: Dim}, ImageEmbedder: &Embedder{Size: Dim},
		Lexical: lex, Vectors: vec, Centroids: b.Build(),
		Prerank: defaultlinear.New(defaultlinear.DefaultPrerankWeights()),
		Rank:    defaultlinear.New(defaultlinear.DefaultRankWeights()),
		Names: map[string]string{
			"text_embedder": "fake:text", "image_embedder": "fake:image", "lexical": "fake:lexical",
			"vector": "fake:vector", "prerank": "default_linear", "rank": "default_linear",
		},
	}, lex, vec
}

// Resolved returns the default config for the fake tenant, with small keeps so cuts show.
func Resolved() *tenant.Resolved {
	cfg, err := tenant.ParseConfig(tenant.DefaultDocument())
	if err != nil {
		panic(err)
	}
	cfg.Budgets = tenant.Budgets{TotalMS: 5000, S1MS: 1000, S2MS: 1000, S3MS: 500, S4MS: 500, S5MS: 1000, S6MS: 500}
	cfg.Prerank.Keep, cfg.Rank.Keep = 6, 4
	reg, _ := catalog.NewRegistry(nil)
	var always []filter.Expr
	for _, f := range cfg.Policy.AlwaysFilters {
		c, _ := filter.Compile(f, reg, "")
		always = append(always, c)
	}
	return &tenant.Resolved{
		Tenant:        tenant.Tenant{ID: TenantID, Slug: "test", Plan: "growth", Status: "active"},
		Plan:          tenant.Plan{ID: "growth", MaxTotalMS: 600, SearchRPS: 100, SearchBurst: 200, MaxPageDepth: 1000},
		ConfigVersion: 3, Config: cfg, Registry: reg, AlwaysFilters: filter.All(always...),
	}
}

// Breakers returns breakers on a no-op meter.
func Breakers() *resilience.Breakers {
	b, err := resilience.NewBreakers(noop.NewMeterProvider().Meter("test"), resilience.DefaultSettings())
	if err != nil {
		panic(err)
	}
	return b
}

// FixedNow is the clock used by golden tests.
var FixedNow = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
