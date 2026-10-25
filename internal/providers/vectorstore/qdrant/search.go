package qdrant

import (
	"context"
	"fmt"
	"time"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

// Search runs a filtered kNN query on one named vector. Filters are pushed down as a Qdrant
// payload filter; the payload carries the display fields, so no second lookup is needed.
func (s *Store) Search(ctx context.Context, req pipeline.VectorSearch) ([]pipeline.Hit, error) {
	if len(req.Query) == 0 || req.K <= 0 {
		return nil, nil
	}
	f, err := Translate(req.Filter)
	if err != nil {
		return nil, err
	}
	limit, using := uint64(req.K), req.Vector
	q := &qdrant.QueryPoints{
		CollectionName: s.target,
		Query:          qdrant.NewQueryDense(req.Query),
		Using:          &using,
		Filter:         f,
		Limit:          &limit,
		WithPayload:    qdrant.NewWithPayloadInclude(payloadFields...),
	}
	if req.EF > 0 {
		ef := uint64(req.EF)
		q.Params = &qdrant.SearchParams{HnswEf: &ef}
	}
	points, err := s.client.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("search %s.%s: %w", s.target, req.Vector, err)
	}
	hits := make([]pipeline.Hit, 0, len(points))
	for _, p := range points {
		id, doc := docFromPayload(p.GetPayload())
		if id == "" {
			continue
		}
		hits = append(hits, pipeline.Hit{ProductID: id, Score: float64(p.GetScore()), Doc: doc})
	}
	return hits, nil
}

// payloadFields are what ranking needs; display fields are hydrated per page from the lexical
// index, so they are not read for hundreds of hits.
var payloadFields = []string{
	"product_id", "vendor_id", "price_minor", "category_ids", "rating", "rating_count",
	"listing_completeness", "image_count", "created_at",
}

func docFromPayload(p map[string]*qdrant.Value) (string, *pipeline.Doc) {
	str := func(k string) string { return p[k].GetStringValue() }
	d := &pipeline.Doc{
		Title: str("title"), Brand: str("brand"), VendorID: str("vendor_id"), Currency: str("currency"),
		ImageURL: str("image_url"), PriceMinor: p["price_minor"].GetIntegerValue(),
		RatingCount: p["rating_count"].GetIntegerValue(), ImageCount: int(p["image_count"].GetIntegerValue()),
		ListingCompleteness: p["listing_completeness"].GetDoubleValue(),
	}
	if v, ok := p["rating"]; ok {
		r := v.GetDoubleValue()
		d.Rating = &r
	}
	for _, v := range p["category_ids"].GetListValue().GetValues() {
		d.CategoryIDs = append(d.CategoryIDs, v.GetStringValue())
	}
	if t, err := time.Parse(time.RFC3339, str("created_at")); err == nil {
		d.CreatedAt = &t
	}
	return str("product_id"), d
}

// Translate converts a compiled filter to a Qdrant payload filter (nil matches everything).
func Translate(e filter.Expr) (*qdrant.Filter, error) {
	if e.IsZero() {
		return nil, nil
	}
	c, err := condition(e)
	if err != nil {
		return nil, err
	}
	if f := c.GetFilter(); f != nil {
		return f, nil
	}
	return &qdrant.Filter{Must: []*qdrant.Condition{c}}, nil
}

func condition(e filter.Expr) (*qdrant.Condition, error) {
	switch {
	case e.And != nil || e.Or != nil:
		kids := e.And
		if e.Or != nil {
			kids = e.Or
		}
		conds := make([]*qdrant.Condition, 0, len(kids))
		for _, k := range kids {
			c, err := condition(k)
			if err != nil {
				return nil, err
			}
			conds = append(conds, c)
		}
		if e.And != nil {
			return qdrant.NewFilterAsCondition(&qdrant.Filter{Must: conds}), nil
		}
		return qdrant.NewFilterAsCondition(&qdrant.Filter{Should: conds}), nil
	case e.Cond != nil:
		return leaf(*e.Cond)
	}
	return nil, fmt.Errorf("empty filter expression")
}

func not(c *qdrant.Condition) *qdrant.Condition {
	return qdrant.NewFilterAsCondition(&qdrant.Filter{MustNot: []*qdrant.Condition{c}})
}

func anyOf(cs []*qdrant.Condition) *qdrant.Condition {
	return qdrant.NewFilterAsCondition(&qdrant.Filter{Should: cs})
}

func leaf(c filter.Condition) (*qdrant.Condition, error) {
	f := c.Field
	switch c.Op {
	case filter.Eq, filter.Contains:
		return equals(f, c.Type, c.Value)
	case filter.Ne:
		eq, err := equals(f, c.Type, c.Value)
		if err != nil {
			return nil, err
		}
		return not(eq), nil
	case filter.In, filter.Nin:
		vals := c.Value.([]any)
		var cond *qdrant.Condition
		switch c.Type {
		case catalog.TypeKeyword, catalog.TypeKeywordArray:
			cond = qdrant.NewMatchKeywords(f, toStrings(vals)...)
		case catalog.TypeInt:
			cond = qdrant.NewMatchInts(f, toInts(vals)...)
		default:
			cs := make([]*qdrant.Condition, 0, len(vals))
			for _, v := range vals {
				eq, err := equals(f, c.Type, v)
				if err != nil {
					return nil, err
				}
				cs = append(cs, eq)
			}
			cond = anyOf(cs)
		}
		if c.Op == filter.Nin {
			return not(cond), nil
		}
		return cond, nil
	case filter.Gt, filter.Gte, filter.Lt, filter.Lte:
		b := filter.Bounds{}
		switch c.Op {
		case filter.Gt:
			b.Gt = c.Value
		case filter.Gte:
			b.Gte = c.Value
		case filter.Lt:
			b.Lt = c.Value
		case filter.Lte:
			b.Lte = c.Value
		}
		return rangeCond(f, c.Type, b)
	case filter.Range:
		return rangeCond(f, c.Type, c.Value.(filter.Bounds))
	}
	return nil, fmt.Errorf("filter op %q is not supported by qdrant", c.Op)
}

func equals(field string, t catalog.FieldType, v any) (*qdrant.Condition, error) {
	switch t {
	case catalog.TypeKeyword, catalog.TypeKeywordArray:
		return qdrant.NewMatchKeyword(field, v.(string)), nil
	case catalog.TypeInt:
		return qdrant.NewMatchInt(field, v.(int64)), nil
	case catalog.TypeBool:
		return qdrant.NewMatchBool(field, v.(bool)), nil
	case catalog.TypeFloat, catalog.TypeDate:
		return rangeCond(field, t, filter.Bounds{Gte: v, Lte: v})
	}
	return nil, fmt.Errorf("field %s: type %s cannot be matched", field, t)
}

func rangeCond(field string, t catalog.FieldType, b filter.Bounds) (*qdrant.Condition, error) {
	if t == catalog.TypeDate {
		ts := func(v any) *timestamppb.Timestamp {
			if v == nil {
				return nil
			}
			return timestamppb.New(v.(time.Time))
		}
		return qdrant.NewDatetimeRange(field, &qdrant.DatetimeRange{Gt: ts(b.Gt), Gte: ts(b.Gte), Lt: ts(b.Lt), Lte: ts(b.Lte)}), nil
	}
	num := func(v any) *float64 {
		switch x := v.(type) {
		case int64:
			f := float64(x)
			return &f
		case float64:
			return &x
		}
		return nil
	}
	if t != catalog.TypeInt && t != catalog.TypeFloat {
		return nil, fmt.Errorf("field %s: range needs a numeric or date field", field)
	}
	return qdrant.NewRange(field, &qdrant.Range{Gt: num(b.Gt), Gte: num(b.Gte), Lt: num(b.Lt), Lte: num(b.Lte)}), nil
}

func toStrings(vs []any) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.(string)
	}
	return out
}

func toInts(vs []any) []int64 {
	out := make([]int64, len(vs))
	for i, v := range vs {
		out[i] = v.(int64)
	}
	return out
}

// ScrollVectors streams every point of version v with its `vector` and category IDs, for
// offline jobs such as the category centroid builder.
func (s *Store) ScrollVectors(ctx context.Context, v int, vector string, fn func(productID string, vec []float32, categories []string) error) error {
	limit := uint32(512)
	var offset *qdrant.PointId
	for {
		points, next, err := s.client.ScrollAndOffset(ctx, &qdrant.ScrollPoints{
			CollectionName: s.CollectionName(v),
			Offset:         offset,
			Limit:          &limit,
			WithPayload:    qdrant.NewWithPayloadInclude("product_id", "category_ids"),
			WithVectors:    qdrant.NewWithVectorsInclude(vector),
		})
		if err != nil {
			return fmt.Errorf("scroll %s: %w", s.CollectionName(v), err)
		}
		for _, p := range points {
			out := p.GetVectors().GetVectors().GetVectors()[vector]
			vec := out.GetDense().GetData()
			if len(vec) == 0 {
				vec = out.GetData() //nolint:staticcheck // servers before the dense field fill data
			}
			if len(vec) == 0 {
				continue
			}
			var cats []string
			for _, c := range p.GetPayload()["category_ids"].GetListValue().GetValues() {
				cats = append(cats, c.GetStringValue())
			}
			if err := fn(p.GetPayload()["product_id"].GetStringValue(), vec, cats); err != nil {
				return err
			}
		}
		if next == nil || len(points) == 0 {
			return nil
		}
		offset = next
	}
}

// Get returns a product's stored named vectors and ranking doc (similar-items anchors).
func (s *Store) Get(ctx context.Context, productID string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	points, err := s.client.Get(ctx, &qdrant.GetPoints{
		CollectionName: s.target,
		Ids:            []*qdrant.PointId{qdrant.NewID(catalog.PointID(productID))},
		WithVectors:    qdrant.NewWithVectors(true),
		WithPayload:    qdrant.NewWithPayloadInclude(payloadFields...),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get %s from %s: %w", productID, s.target, err)
	}
	if len(points) == 0 {
		return nil, nil, fmt.Errorf("product %s: %w", productID, pipeline.ErrNotFound)
	}
	p := points[0]
	vecs := map[string][]float32{}
	for name, out := range p.GetVectors().GetVectors().GetVectors() {
		v := out.GetDense().GetData()
		if len(v) == 0 {
			v = out.GetData() //nolint:staticcheck // servers before the dense field fill data
		}
		vecs[name] = v
	}
	_, doc := docFromPayload(p.GetPayload())
	return &pipeline.VectorPoint{ProductID: productID, Vectors: vecs}, doc, nil
}
