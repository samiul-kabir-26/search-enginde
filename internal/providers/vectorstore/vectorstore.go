// Package vectorstore holds what the vector store adapters share: the canonical payload of a
// product, the ranking fields read back by retrievers, and the BYO collection spec (named
// vectors and payload mapping) with the describe-and-check used by connection tests.
package vectorstore

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Metrics a BYO vector may declare (docs/TENANT_CONFIG.md §4).
const (
	MetricCosine = "cosine"
	MetricDot    = "dot"
	MetricL2     = "l2"
)

// ProductIDField is the canonical payload field holding the product ID.
const ProductIDField = "product_id"

// RankingFields are the payload fields retrievers read back for ranking; display fields are
// hydrated per page from the lexical index.
var RankingFields = []string{
	ProductIDField, "vendor_id", "price_minor", "category_ids", "rating", "rating_count",
	"listing_completeness", "image_count", "created_at",
}

// Payload is the stored payload of p under canonical field names: every filterable field
// (canonical and custom) plus the display fields search results need without a second lookup.
func Payload(p catalog.Product, r *catalog.Registry) map[string]any {
	out := map[string]any{
		"vendor_id":            p.VendorID,
		"title":                p.Title,
		"category_path":        p.CategoryPath,
		"category_ids":         p.CategoryIDs,
		"price_minor":          p.PriceMinor,
		"currency":             p.Currency,
		"moq":                  p.MOQ,
		"status":               p.Status,
		"rating_count":         p.RatingCount,
		"listing_completeness": p.ListingCompleteness,
		"image_count":          len(p.ImageURLs),
	}
	setIf := func(k string, ok bool, v any) {
		if ok {
			out[k] = v
		}
	}
	setIf("brand", p.Brand != "", p.Brand)
	setIf("product_type", p.ProductType != "", p.ProductType)
	setIf("ship_regions", len(p.ShipRegions) > 0, p.ShipRegions)
	setIf("image_url", len(p.ImageURLs) > 0, firstOr(p.ImageURLs))
	if p.Stock != nil {
		out["stock"] = *p.Stock
	}
	if p.Rating != nil {
		out["rating"] = *p.Rating
	}
	if p.CreatedAt != nil {
		out["created_at"] = p.CreatedAt.UTC().Format(time.RFC3339)
	}
	for _, f := range r.Filterable() {
		if !f.Custom {
			continue
		}
		key := strings.TrimPrefix(f.Name, catalog.CustomPrefix)
		if v, ok := p.Custom[key]; ok {
			if t, isTime := v.(time.Time); isTime {
				v = t.UTC().Format(time.RFC3339)
			}
			out[f.Name] = v
		}
	}
	return out
}

func firstOr(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

// DocFromMap builds the ranking doc from payload values keyed by canonical names, accepting
// the value types store drivers decode to (int32/int64/float32/float64/json numbers, string
// lists as []string or []any, times as time.Time or RFC 3339 strings).
func DocFromMap(m map[string]any) (string, *pipeline.Doc) {
	d := &pipeline.Doc{
		VendorID: AsString(m["vendor_id"]), PriceMinor: AsInt(m["price_minor"]), RatingCount: AsInt(m["rating_count"]),
		ImageCount: int(AsInt(m["image_count"])), ListingCompleteness: AsFloat(m["listing_completeness"]),
		CategoryIDs: AsStrings(m["category_ids"]),
	}
	if v, ok := m["rating"]; ok && v != nil {
		r := AsFloat(v)
		d.Rating = &r
	}
	switch t := m["created_at"].(type) {
	case time.Time:
		d.CreatedAt = &t
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			d.CreatedAt = &ts
		}
	case int64:
		ts := time.Unix(t, 0).UTC()
		d.CreatedAt = &ts
	}
	return AsString(m[ProductIDField]), d
}

// AsString converts a decoded value to a string ("" for nil).
func AsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

// AsInt converts a decoded number to int64.
func AsInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(math.Round(x))
	case float32:
		return int64(math.Round(float64(x)))
	}
	return 0
}

// AsFloat converts a decoded number to float64.
func AsFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64, int32, int16, int8, int:
		return float64(AsInt(x))
	}
	return 0
}

// AsStrings converts a decoded list to []string.
func AsStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, AsString(e))
		}
		return out
	}
	return nil
}

// Spec is a BYO collection as declared in the tenant config's vector store block.
type Spec struct {
	Collection string
	// Vectors maps a pipeline vector (text, image) to the store's vector field.
	Vectors map[string]VectorSpec
	// Fields maps canonical payload fields (product_id, brand, custom.season, ...) to store
	// fields; unmapped fields keep their canonical names.
	Fields map[string]string
}

// VectorSpec is one named vector of a BYO collection.
type VectorSpec struct {
	Name   string
	Dim    int
	Metric string
}

// Field returns the store field for a canonical field.
func (s Spec) Field(canonical string) string {
	if f, ok := s.Fields[canonical]; ok && f != "" {
		return f
	}
	return canonical
}

// Vector returns the store's vector field for a pipeline vector, and whether it is declared.
func (s Spec) Vector(name string) (VectorSpec, bool) {
	v, ok := s.Vectors[name]
	if ok && v.Name == "" {
		v.Name = name
	}
	return v, ok
}

// Rename returns a copy of a canonical payload with store field names.
func (s Spec) Rename(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[s.Field(k)] = v
	}
	return out
}

// Canonical maps store field names back to canonical names for the given canonical fields.
func (s Spec) Canonical(stored map[string]any, fields []string) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := stored[s.Field(f)]; ok {
			out[f] = v
		}
	}
	return out
}

// Info is what a store reports about a BYO collection.
type Info struct {
	Exists bool
	// Vectors are the collection's vector fields by store name.
	Vectors map[string]VectorInfo
	// Fields are the payload fields the store can filter on. AnyField is set for schemaless
	// payloads (Qdrant, or Milvus with the dynamic field enabled).
	Fields   map[string]bool
	AnyField bool
}

// VectorInfo is a stored vector field's size and metric ("" when the store cannot tell, such
// as a pgvector column without an ANN index).
type VectorInfo struct {
	Dim    int
	Metric string
}

// Describer is implemented by BYO adapters, for connection tests and validation.
type Describer interface {
	Describe(ctx context.Context) (Info, error)
	// Metrics lists the metrics the adapter supports.
	Metrics() []string
}

// Problem is one mismatch between the declared spec and the collection.
type Problem struct {
	Path    string
	Code    string
	Message string
}

// Check compares a declared spec with a described collection: vectors exist with the declared
// dimension and metric, the metric is supported, and the given canonical fields (the ones
// filters must be pushed down on) exist in the store.
func Check(spec Spec, info Info, supported []string, filterFields []string) []Problem {
	var out []Problem
	if !info.Exists {
		return []Problem{{Path: "/collection", Code: "collection_missing", Message: fmt.Sprintf("collection %q does not exist", spec.Collection)}}
	}
	names := make([]string, 0, len(spec.Vectors))
	for n := range spec.Vectors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v, _ := spec.Vector(n)
		path := "/vectors/" + n
		if !contains(supported, v.Metric) {
			out = append(out, Problem{path + "/metric", "unsupported_metric", fmt.Sprintf("metric %q is not supported (use %s)", v.Metric, strings.Join(supported, ", "))})
		}
		got, ok := info.Vectors[v.Name]
		switch {
		case !ok:
			out = append(out, Problem{path + "/name", "vector_missing", fmt.Sprintf("the collection has no vector field %q", v.Name)})
		case got.Dim != v.Dim:
			out = append(out, Problem{path + "/dim", "dim_mismatch", fmt.Sprintf("vector %q has %d dimensions, declared %d", v.Name, got.Dim, v.Dim)})
		case got.Metric != "" && got.Metric != v.Metric:
			out = append(out, Problem{path + "/metric", "metric_mismatch", fmt.Sprintf("vector %q uses %s, declared %s", v.Name, got.Metric, v.Metric)})
		}
	}
	if info.AnyField {
		return out
	}
	for _, f := range append([]string{ProductIDField}, filterFields...) {
		if !info.Fields[spec.Field(f)] {
			out = append(out, Problem{"/payload_mapping/" + f, "field_missing",
				fmt.Sprintf("the collection has no field %q for %s, so it cannot be stored or filtered on", spec.Field(f), f)})
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, e := range xs {
		if e == x {
			return true
		}
	}
	return false
}
