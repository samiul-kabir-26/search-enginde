package qdrant

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
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

// Store is a tenant's platform-default vector collection, or a BYO collection. Platform
// writes target the alias `t_{tenant}_products` unless bound to a physical version with
// Version; a BYO store reads and writes the tenant's own collection in place, through its
// declared vector names and payload mapping.
type Store struct {
	client   *qdrant.Client
	alias    string
	target   string
	dims     VectorDims
	registry *catalog.Registry
	spec     vectorstore.Spec
	byo      bool
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

// NewBYO returns a store over a tenant's own collection.
func NewBYO(client *qdrant.Client, spec vectorstore.Spec) *Store {
	return &Store{client: client, alias: spec.Collection, target: spec.Collection, spec: spec, byo: true}
}

// CollectionName is the physical collection for version v (a BYO store has only one).
func (s *Store) CollectionName(v int) string {
	if s.byo {
		return s.target
	}
	return s.alias + "_v" + strconv.Itoa(v)
}

// vector returns the stored name of a pipeline vector.
func (s *Store) vector(name string) string {
	if v, ok := s.spec.Vector(name); ok {
		return v.Name
	}
	return name
}

// Close closes a BYO store's client; the platform client is shared and owned by the caller.
func (s *Store) Close() error {
	if s.byo {
		if err := s.client.Close(); err != nil {
			return fmt.Errorf("close qdrant client: %w", err)
		}
	}
	return nil
}

// ProviderName labels the store in traces and breaker keys.
func (s *Store) ProviderName() string {
	if s.byo {
		return "byo:qdrant"
	}
	return "platform:qdrant"
}

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
			vecs[s.vector(name)] = qdrant.NewVector(v...)
		}
		payload := make(map[string]any, len(p.Payload)+1)
		for k, v := range p.Payload {
			payload[s.spec.Field(k)] = v
		}
		payload[s.spec.Field(vectorstore.ProductIDField)] = p.ProductID
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

// Count returns the exact number of points in version v (a BYO store counts its collection).
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
