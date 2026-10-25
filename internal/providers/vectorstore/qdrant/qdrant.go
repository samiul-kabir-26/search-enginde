package qdrant

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Collection parameters from docs/DATA.md §6.
const (
	hnswM           = 16
	hnswEfConstruct = 128
	// indexingThresholdKB builds HNSW even for small segments: Qdrant's default (10 MB) leaves a
	// 10K-product tenant on brute force, where filtered top-500 searches take ~100 ms instead of ~9 ms.
	indexingThresholdKB = 1000
)

// VectorDims are the named vectors of a platform collection and their sizes.
type VectorDims map[string]int

// NewClient connects to Qdrant's gRPC API at addr (host:port).
func NewClient(addr string) (*qdrant.Client, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("qdrant addr %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("qdrant port %q: %w", port, err)
	}
	c, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: p, SkipCompatibilityCheck: true})
	if err != nil {
		return nil, fmt.Errorf("qdrant client: %w", err)
	}
	return c, nil
}

// Store is a tenant's platform-default vector collection. Writes target the alias
// `t_{tenant}_products` unless bound to a physical version with Version.
type Store struct {
	client   *qdrant.Client
	alias    string
	target   string
	dims     VectorDims
	registry *catalog.Registry
}

var (
	_ pipeline.VectorStore   = (*Store)(nil)
	_ pipeline.IndexVersions = (*Store)(nil)
)

// New returns the tenant's store. dims sizes the named vectors of new versions; the registry
// selects the payload fields to index.
func New(client *qdrant.Client, t tenant.ID, dims VectorDims, registry *catalog.Registry) *Store {
	alias := t.ResourcePrefix() + "_products"
	return &Store{client: client, alias: alias, target: alias, dims: dims, registry: registry}
}

// CollectionName is the physical collection for version v.
func (s *Store) CollectionName(v int) string { return s.alias + "_v" + strconv.Itoa(v) }

// Version returns a copy of the store that writes to physical version v.
func (s *Store) Version(v int) *Store {
	c := *s
	c.target = s.CollectionName(v)
	return &c
}

// Upsert writes points keyed by the UUIDv5 of their product ID, waiting until applied.
func (s *Store) Upsert(ctx context.Context, points []pipeline.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	ps := make([]*qdrant.PointStruct, len(points))
	for i, p := range points {
		vecs := make(map[string]*qdrant.Vector, len(p.Vectors))
		for name, v := range p.Vectors {
			vecs[name] = qdrant.NewVector(v...)
		}
		payload := make(map[string]any, len(p.Payload)+1)
		for k, v := range p.Payload {
			payload[k] = v
		}
		payload["product_id"] = p.ProductID
		value, err := toValueMap(payload)
		if err != nil {
			return fmt.Errorf("payload %s: %w", p.ProductID, err)
		}
		ps[i] = &qdrant.PointStruct{Id: qdrant.NewID(catalog.PointID(p.ProductID)), Vectors: qdrant.NewVectorsMap(vecs), Payload: value}
	}
	wait := true
	if _, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: s.target, Wait: &wait, Points: ps}); err != nil {
		return fmt.Errorf("upsert %s: %w", s.target, err)
	}
	return nil
}

// Delete removes points by product ID.
func (s *Store) Delete(ctx context.Context, productIDs []string) error {
	if len(productIDs) == 0 {
		return nil
	}
	ids := make([]*qdrant.PointId, len(productIDs))
	for i, id := range productIDs {
		ids[i] = qdrant.NewID(catalog.PointID(id))
	}
	wait := true
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{CollectionName: s.target, Wait: &wait, Points: qdrant.NewPointsSelectorIDs(ids)}); err != nil {
		return fmt.Errorf("delete from %s: %w", s.target, err)
	}
	return nil
}

// Health checks that Qdrant answers.
func (s *Store) Health(ctx context.Context) error {
	if _, err := s.client.HealthCheck(ctx); err != nil {
		return fmt.Errorf("qdrant health: %w", err)
	}
	return nil
}

func (s *Store) versionOf(name string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(name, s.alias+"_v"))
	return n, err == nil && strings.HasPrefix(name, s.alias+"_v")
}

// Active returns the version behind the alias, or 0.
func (s *Store) Active(ctx context.Context) (int, error) {
	aliases, err := s.client.ListAliases(ctx)
	if err != nil {
		return 0, fmt.Errorf("list aliases: %w", err)
	}
	for _, a := range aliases {
		if a.GetAliasName() == s.alias {
			if v, ok := s.versionOf(a.GetCollectionName()); ok {
				return v, nil
			}
		}
	}
	return 0, nil
}

// Latest returns the highest existing version, or 0.
func (s *Store) Latest(ctx context.Context) (int, error) {
	names, err := s.client.ListCollections(ctx)
	if err != nil {
		return 0, fmt.Errorf("list collections: %w", err)
	}
	latest := 0
	for _, n := range names {
		if v, ok := s.versionOf(n); ok {
			latest = max(latest, v)
		}
	}
	return latest, nil
}

// Create creates version v with cosine named vectors, HNSW m=16/ef_construct=128 and a
// payload index on every filterable field.
func (s *Store) Create(ctx context.Context, v int) error {
	name := s.CollectionName(v)
	params := make(map[string]*qdrant.VectorParams, len(s.dims))
	for vec, dim := range s.dims {
		if dim <= 0 {
			return fmt.Errorf("create %s: vector %q has dimension %d", name, vec, dim)
		}
		params[vec] = &qdrant.VectorParams{Size: uint64(dim), Distance: qdrant.Distance_Cosine}
	}
	m, ef, threshold := uint64(hnswM), uint64(hnswEfConstruct), uint64(indexingThresholdKB)
	inRAM := qdrant.Memory_Cached
	err := s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName:   name,
		VectorsConfig:    qdrant.NewVectorsConfigMap(params),
		HnswConfig:       &qdrant.HnswConfigDiff{M: &m, EfConstruct: &ef},
		OptimizersConfig: &qdrant.OptimizersConfigDiff{IndexingThreshold: &threshold},
		Payload:          &qdrant.PayloadStorageParams{Memory: &inRAM},
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	wait := true
	for _, f := range s.registry.Filterable() {
		t, ok := payloadIndexType(f.Type)
		if !ok {
			continue
		}
		_, err := s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: name, FieldName: f.Name, FieldType: &t, Wait: &wait,
		})
		if err != nil {
			return fmt.Errorf("index %s.%s: %w", name, f.Name, err)
		}
	}
	return nil
}

func payloadIndexType(t catalog.FieldType) (qdrant.FieldType, bool) {
	switch t {
	case catalog.TypeKeyword, catalog.TypeKeywordArray:
		return qdrant.FieldType_FieldTypeKeyword, true
	case catalog.TypeInt:
		return qdrant.FieldType_FieldTypeInteger, true
	case catalog.TypeFloat:
		return qdrant.FieldType_FieldTypeFloat, true
	case catalog.TypeBool:
		return qdrant.FieldType_FieldTypeBool, true
	case catalog.TypeDate:
		return qdrant.FieldType_FieldTypeDatetime, true
	}
	return 0, false
}

// Promote points the alias at version v atomically.
func (s *Store) Promote(ctx context.Context, v int) error {
	ops := []*qdrant.AliasOperations{qdrant.NewAliasCreate(s.alias, s.CollectionName(v))}
	active, err := s.Active(ctx)
	if err != nil {
		return err
	}
	if active > 0 {
		ops = append([]*qdrant.AliasOperations{qdrant.NewAliasDelete(s.alias)}, ops...)
	}
	if err := s.client.UpdateAliases(ctx, ops); err != nil {
		return fmt.Errorf("promote %s: %w", s.CollectionName(v), err)
	}
	return nil
}

// Count returns the exact number of points in version v.
func (s *Store) Count(ctx context.Context, v int) (int64, error) {
	exact := true
	n, err := s.client.Count(ctx, &qdrant.CountPoints{CollectionName: s.CollectionName(v), Exact: &exact})
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", s.CollectionName(v), err)
	}
	if n > math.MaxInt64 {
		return 0, fmt.Errorf("count %s: %d overflows int64", s.CollectionName(v), n)
	}
	return int64(n), nil
}

// Drop deletes version v. Dropping a missing version is not an error.
func (s *Store) Drop(ctx context.Context, v int) error {
	exists, err := s.client.CollectionExists(ctx, s.CollectionName(v))
	if err != nil {
		return fmt.Errorf("check %s: %w", s.CollectionName(v), err)
	}
	if !exists {
		return nil
	}
	if err := s.client.DeleteCollection(ctx, s.CollectionName(v)); err != nil {
		return fmt.Errorf("drop %s: %w", s.CollectionName(v), err)
	}
	return nil
}

// Payload is the stored payload of p: every filterable field (canonical and custom) plus the
// display fields search results need without a second lookup.
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

// toValueMap converts typed slices, which the qdrant value conversion does not accept, to []any.
func toValueMap(m map[string]any) (map[string]*qdrant.Value, error) {
	norm := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case []string:
			arr := make([]any, len(x))
			for i, s := range x {
				arr[i] = s
			}
			norm[k] = arr
		case int32:
			norm[k] = int64(x)
		case int:
			norm[k] = int64(x)
		default:
			norm[k] = v
		}
	}
	vm, err := qdrant.TryValueMap(norm)
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	return vm, nil
}
