package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	opensearchgo "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// sourceFields are the display fields read by Docs (one page at a time).
var sourceFields = []string{
	"title", "brand", "vendor_id", "price_minor", "currency", "image_urls",
	"category_ids", "rating", "rating_count", "listing_completeness", "created_at",
}

// rankFields are read as doc values by Search: everything ranking and CEL policies need,
// without loading _source for hundreds of hits (3–5x faster). Display fields are hydrated per
// page via Docs. Filterable custom fields are added per tenant (rankingFields).
var rankFields = []string{"vendor_id", "brand", "price_minor", "category_ids", "rating", "rating_count",
	"listing_completeness", "created_at", "stock", "status", "ship_regions"}

// rankingFields are rankFields plus the tenant's filterable custom fields, and which of them
// are lists.
func (s *Store) rankingFields() ([]string, map[string]bool) {
	fields := slices.Clone(rankFields)
	lists := map[string]bool{"category_ids": true, "ship_regions": true}
	for _, c := range vectorstore.CustomRankingFields(s.registry) {
		fields = append(fields, c)
		if f, ok := s.registry.Field(c); ok && f.Type == catalog.TypeKeywordArray {
			lists[c] = true
		}
	}
	return fields, lists
}

// totalHitsCap bounds exact total counting; totals above it are reported as the cap.
const totalHitsCap = 10000

// Search runs a BM25 query (or a popularity query) with base filters in the query, request
// filters as a post-filter, and disjunctive facet aggregations.
func (s *Store) Search(ctx context.Context, req pipeline.LexicalSearch) (pipeline.LexicalResult, error) {
	body, err := s.searchBody(req)
	if err != nil {
		return pipeline.LexicalResult{}, err
	}
	var resp searchResponse
	r := opensearchapi.SearchReq{Indices: []string{s.target}, Body: bytes.NewReader(body)}
	if _, err := opensearchgo.Do(ctx, s.client.Client, http.MethodPost, r, &resp); err != nil {
		return pipeline.LexicalResult{}, fmt.Errorf("search %s: %w", s.target, err)
	}
	out := pipeline.LexicalResult{TotalMatches: resp.Hits.Total.Value, Facets: map[string][]pipeline.FacetValue{}}
	_, lists := s.rankingFields()
	for _, h := range resp.Hits.Hits {
		score := h.Score
		if req.Popular && len(h.Sort) > 0 {
			score = float64(len(resp.Hits.Hits) - len(out.Hits))
		}
		out.Hits = append(out.Hits, pipeline.Hit{ProductID: h.ID, Score: score, Doc: h.Fields.doc(lists)})
	}
	for _, f := range req.Facets {
		agg, ok := resp.Aggregations[aggName(f.Field)]
		if !ok {
			continue
		}
		out.Facets[f.Field] = agg.values(f)
	}
	return out, nil
}

func (s *Store) searchBody(req pipeline.LexicalSearch) ([]byte, error) {
	base, err := Translate(req.Base)
	if err != nil {
		return nil, err
	}
	bq := map[string]any{}
	if base != nil {
		bq["filter"] = []any{base}
	}
	fields, _ := s.rankingFields()
	body := map[string]any{"size": req.K, "_source": false, "docvalue_fields": fields, "track_total_hits": totalHitsCap}
	switch {
	case req.Popular:
		bq["must"] = []any{map[string]any{"match_all": map[string]any{}}}
		body["sort"] = []any{
			map[string]any{"rating_count": map[string]any{"order": "desc", "missing": "_last"}},
			map[string]any{"rating": map[string]any{"order": "desc", "missing": "_last"}},
			map[string]any{"_id": "asc"},
		}
	case strings.TrimSpace(req.Text) == "":
		bq["must"] = []any{map[string]any{"match_all": map[string]any{}}}
	default:
		bq["must"] = []any{map[string]any{"multi_match": map[string]any{
			"query": req.Text, "fields": s.boostedFields(req.Boosts), "type": "best_fields", "tie_breaker": 0.3,
		}}}
	}
	body["query"] = map[string]any{"bool": bq}
	if !req.Request.IsZero() {
		pf, err := Translate(req.Request)
		if err != nil {
			return nil, err
		}
		body["post_filter"] = pf
	}
	if len(req.Facets) > 0 {
		aggs, err := s.facetAggs(req)
		if err != nil {
			return nil, err
		}
		body["aggs"] = aggs
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("search body: %w", err)
	}
	return b, nil
}

// boostedFields maps field_boosts keys to index fields: brand and searchable keyword custom
// fields search their analyzed `.text` sub-field, attributes cover every attribute key.
func (s *Store) boostedFields(boosts map[string]float64) []string {
	if len(boosts) == 0 {
		boosts = map[string]float64{"title": 3, "brand": 2, "attributes": 1.5, "description": 1}
	}
	keys := make([]string, 0, len(boosts))
	for k := range boosts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		field := k
		switch {
		case k == "brand":
			field = "brand.text"
		case k == "attributes":
			field = "attributes.*"
		case strings.HasPrefix(k, catalog.CustomPrefix):
			if f, ok := s.registry.Field(k); ok && f.Type != catalog.TypeText {
				field = k + ".text"
			}
		}
		out = append(out, fmt.Sprintf("%s^%g", field, boosts[k]))
	}
	return out
}

func aggName(field string) string { return "facet_" + strings.ReplaceAll(field, ".", "__") }

// facetAggs builds one filtered aggregation per facet. Each applies every request condition
// except those on its own field, so selecting a brand still shows the other brands' counts.
func (s *Store) facetAggs(req pipeline.LexicalSearch) (map[string]any, error) {
	byField, rest := filter.SplitByField(req.Request)
	aggs := map[string]any{}
	for _, f := range req.Facets {
		var parts []filter.Expr
		parts = append(parts, rest...)
		for field, conds := range byField {
			if field != f.Field {
				parts = append(parts, conds...)
			}
		}
		clause := map[string]any{"match_all": map[string]any{}}
		if e := filter.All(parts...); !e.IsZero() {
			c, err := Translate(e)
			if err != nil {
				return nil, err
			}
			clause = c
		}
		var inner map[string]any
		if len(f.Ranges) > 0 {
			ranges := make([]map[string]any, len(f.Ranges))
			for i, r := range f.Ranges {
				m := map[string]any{"key": rangeKey(r)}
				if r.From != nil {
					m["from"] = *r.From
				}
				if r.To != nil {
					m["to"] = *r.To
				}
				ranges[i] = m
			}
			inner = map[string]any{"range": map[string]any{"field": f.Field, "ranges": ranges}}
		} else {
			size := f.Size
			if size <= 0 {
				size = 10
			}
			inner = map[string]any{"terms": map[string]any{"field": f.Field, "size": size}}
		}
		aggs[aggName(f.Field)] = map[string]any{"filter": clause, "aggs": map[string]any{"values": inner}}
	}
	return aggs, nil
}

func rangeKey(r pipeline.FacetRange) string {
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	return f(r.From) + "-" + f(r.To)
}

type searchResponse struct {
	Hits struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID     string      `json:"_id"`
			Score  float64     `json:"_score"`
			Sort   []any       `json:"sort"`
			Fields docValueDoc `json:"fields"`
		} `json:"hits"`
	} `json:"hits"`
	Aggregations map[string]facetAgg `json:"aggregations"`
}

type facetAgg struct {
	Values struct {
		Buckets json.RawMessage `json:"buckets"`
	} `json:"values"`
}

func (a facetAgg) values(f pipeline.FacetRequest) []pipeline.FacetValue {
	type bucket struct {
		Key      any      `json:"key"`
		DocCount int64    `json:"doc_count"`
		From     *float64 `json:"from"`
		To       *float64 `json:"to"`
	}
	var bs []bucket
	if err := json.Unmarshal(a.Values.Buckets, &bs); err != nil {
		return nil
	}
	out := make([]pipeline.FacetValue, 0, len(bs))
	for i, b := range bs {
		v := pipeline.FacetValue{Count: b.DocCount, From: b.From, To: b.To}
		switch k := b.Key.(type) {
		case string:
			v.Value = k
		case float64:
			v.Value = strconv.FormatFloat(k, 'f', -1, 64)
		}
		if len(f.Ranges) > 0 && i < len(f.Ranges) {
			v.Value = rangeKey(f.Ranges[i])
		}
		out = append(out, v)
	}
	return out
}

type sourceDoc struct {
	Title               string     `json:"title"`
	Brand               string     `json:"brand"`
	VendorID            string     `json:"vendor_id"`
	PriceMinor          int64      `json:"price_minor"`
	Currency            string     `json:"currency"`
	ImageURLs           []string   `json:"image_urls"`
	CategoryIDs         []string   `json:"category_ids"`
	Rating              *float64   `json:"rating"`
	RatingCount         int64      `json:"rating_count"`
	ListingCompleteness float64    `json:"listing_completeness"`
	CreatedAt           *time.Time `json:"created_at"`
}

func (d sourceDoc) doc() *pipeline.Doc {
	out := &pipeline.Doc{
		Title: d.Title, Brand: d.Brand, VendorID: d.VendorID, PriceMinor: d.PriceMinor, Currency: d.Currency,
		CategoryIDs: d.CategoryIDs, Rating: d.Rating, RatingCount: d.RatingCount,
		ListingCompleteness: d.ListingCompleteness, ImageCount: len(d.ImageURLs), CreatedAt: d.CreatedAt,
	}
	if len(d.ImageURLs) > 0 {
		out.ImageURL = d.ImageURLs[0]
	}
	return out
}

// docValueDoc is the `fields` section of a hit: every doc value comes back as an array.
type docValueDoc map[string][]any

// doc converts doc values to the ranking doc: list fields keep their arrays, others take the
// first value.
func (d docValueDoc) doc(lists map[string]bool) *pipeline.Doc {
	m := make(map[string]any, len(d))
	for k, vs := range d {
		switch {
		case lists[k]:
			m[k] = vs
		case len(vs) > 0:
			m[k] = vs[0]
		}
	}
	_, doc := vectorstore.DocFromMap(m)
	return doc
}

// Docs fetches stored product data by ID through the alias.
func (s *Store) Docs(ctx context.Context, ids []string) (map[string]*pipeline.Doc, error) {
	out := make(map[string]*pipeline.Doc, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	body, err := json.Marshal(map[string]any{"ids": ids})
	if err != nil {
		return nil, fmt.Errorf("mget body: %w", err)
	}
	var resp struct {
		Docs []struct {
			ID     string    `json:"_id"`
			Found  bool      `json:"found"`
			Source sourceDoc `json:"_source"`
		} `json:"docs"`
	}
	req := opensearchapi.MGetReq{Index: s.target, Body: bytes.NewReader(body),
		Params: opensearchapi.MGetParams{SourceIncludes: sourceFields}}
	if _, err := opensearchgo.Do(ctx, s.client.Client, http.MethodPost, req, &resp); err != nil {
		return nil, fmt.Errorf("mget %s: %w", s.target, err)
	}
	for _, d := range resp.Docs {
		if d.Found {
			out[d.ID] = d.Source.doc()
		}
	}
	return out, nil
}

// Translate converts a compiled filter to an OpenSearch query clause (nil matches everything).
func Translate(e filter.Expr) (map[string]any, error) {
	switch {
	case e.IsZero():
		return nil, nil
	case e.And != nil || e.Or != nil:
		kids := e.And
		if e.Or != nil {
			kids = e.Or
		}
		clauses := make([]any, 0, len(kids))
		for _, k := range kids {
			c, err := Translate(k)
			if err != nil {
				return nil, err
			}
			if c != nil {
				clauses = append(clauses, c)
			}
		}
		if e.And != nil {
			return map[string]any{"bool": map[string]any{"filter": clauses}}, nil
		}
		return map[string]any{"bool": map[string]any{"should": clauses, "minimum_should_match": 1}}, nil
	}
	c := *e.Cond
	not := func(q map[string]any) map[string]any {
		return map[string]any{"bool": map[string]any{"must_not": []any{q}}}
	}
	val := func(v any) any {
		if t, ok := v.(time.Time); ok {
			return t.Format(time.RFC3339)
		}
		return v
	}
	switch c.Op {
	case filter.Eq, filter.Contains:
		return map[string]any{"term": map[string]any{c.Field: val(c.Value)}}, nil
	case filter.Ne:
		return not(map[string]any{"term": map[string]any{c.Field: val(c.Value)}}), nil
	case filter.In, filter.Nin:
		vals := c.Value.([]any)
		out := make([]any, len(vals))
		for i, v := range vals {
			out[i] = val(v)
		}
		q := map[string]any{"terms": map[string]any{c.Field: out}}
		if c.Op == filter.Nin {
			return not(q), nil
		}
		return q, nil
	case filter.Gt, filter.Gte, filter.Lt, filter.Lte:
		return map[string]any{"range": map[string]any{c.Field: map[string]any{string(c.Op): val(c.Value)}}}, nil
	case filter.Range:
		b := c.Value.(filter.Bounds)
		r := map[string]any{}
		for k, v := range map[string]any{"gt": b.Gt, "gte": b.Gte, "lt": b.Lt, "lte": b.Lte} {
			if v != nil {
				r[k] = val(v)
			}
		}
		return map[string]any{"range": map[string]any{c.Field: r}}, nil
	}
	return nil, fmt.Errorf("filter op %q is not supported by opensearch", c.Op)
}
