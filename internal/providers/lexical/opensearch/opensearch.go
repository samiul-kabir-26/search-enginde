package opensearch

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	opensearchgo "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/tenant"
)

//go:embed mapping.json
var baseMapping []byte

// NewClient connects to OpenSearch at url.
func NewClient(url string) (*opensearchapi.Client, error) {
	c, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearchgo.Config{Addresses: []string{url}}})
	if err != nil {
		return nil, fmt.Errorf("opensearch client: %w", err)
	}
	return c, nil
}

// Store is a tenant's platform-default lexical index. Writes target the alias
// `t_{tenant}_products` unless the Store was bound to a physical version with Version.
type Store struct {
	client   *opensearchapi.Client
	alias    string
	target   string
	registry *catalog.Registry
}

var (
	_ pipeline.LexicalIndex  = (*Store)(nil)
	_ pipeline.IndexVersions = (*Store)(nil)
)

// New returns the tenant's store. The registry supplies custom fields for new index versions.
func New(client *opensearchapi.Client, t tenant.ID, registry *catalog.Registry) *Store {
	alias := t.ResourcePrefix() + "_products"
	return &Store{client: client, alias: alias, target: alias, registry: registry}
}

// Alias is the tenant's read/write alias.
func (s *Store) Alias() string { return s.alias }

// IndexName is the physical index for version v.
func (s *Store) IndexName(v int) string { return s.alias + "_v" + strconv.Itoa(v) }

// Version returns a copy of the store that writes to physical version v (used by backfills).
func (s *Store) Version(v int) *Store {
	c := *s
	c.target = s.IndexName(v)
	return &c
}

// Upsert indexes documents with external_gte versioning: a write older than the indexed
// version fails with 409 and is reported stale; an equal version re-applies, so replays are
// idempotent. SourceVersion 0 writes unconditionally.
func (s *Store) Upsert(ctx context.Context, docs []pipeline.IndexDoc) ([]string, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, d := range docs {
		meta := map[string]any{"_index": s.target, "_id": d.Product.ProductID}
		if d.SourceVersion > 0 {
			meta["version"], meta["version_type"] = d.SourceVersion, "external_gte"
		}
		if err := enc.Encode(map[string]any{"index": meta}); err != nil {
			return nil, fmt.Errorf("encode bulk action: %w", err)
		}
		if err := enc.Encode(document(d.Product)); err != nil {
			return nil, fmt.Errorf("encode %s: %w", d.Product.ProductID, err)
		}
	}
	return s.bulk(ctx, &body, len(docs))
}

// Delete removes documents, reporting stale IDs when a newer version is indexed. Deleting a
// missing document is not an error.
func (s *Store) Delete(ctx context.Context, dels []pipeline.IndexDelete) ([]string, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, d := range dels {
		meta := map[string]any{"_index": s.target, "_id": d.ProductID}
		if d.SourceVersion > 0 {
			meta["version"], meta["version_type"] = d.SourceVersion, "external_gte"
		}
		if err := enc.Encode(map[string]any{"delete": meta}); err != nil {
			return nil, fmt.Errorf("encode bulk action: %w", err)
		}
	}
	return s.bulk(ctx, &body, len(dels))
}

func (s *Store) bulk(ctx context.Context, body *bytes.Buffer, n int) ([]string, error) {
	if n == 0 {
		return nil, nil
	}
	resp, err := s.client.Bulk(ctx, opensearchapi.BulkReq{Body: body})
	if err != nil {
		return nil, fmt.Errorf("bulk %s: %w", s.target, err)
	}
	var stale []string
	var failures []string
	for _, item := range resp.Items {
		for _, r := range item {
			switch {
			case r.Status == http.StatusConflict:
				stale = append(stale, r.ID)
			case r.Status == http.StatusNotFound && r.Result == "not_found":
			case r.Status >= 300:
				reason := ""
				if r.Error != nil {
					reason = r.Error.Type + ": " + r.Error.Reason
				}
				failures = append(failures, fmt.Sprintf("%s (%d %s)", r.ID, r.Status, reason))
			}
		}
	}
	if len(failures) > 0 {
		return stale, fmt.Errorf("bulk %s: %d of %d items failed, first: %s", s.target, len(failures), n, failures[0])
	}
	return stale, nil
}

// Health checks that the cluster answers.
func (s *Store) Health(ctx context.Context) error {
	if _, err := s.client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("opensearch ping: %w", err)
	}
	return nil
}

type indexInfo struct {
	Aliases map[string]json.RawMessage `json:"aliases"`
}

func (s *Store) versions(ctx context.Context) (map[int]bool, error) {
	var out map[string]indexInfo
	req := opensearchapi.IndicesGetReq{Indices: []string{s.alias + "_v*"}}
	if _, err := opensearchgo.Do(ctx, s.client.Client, http.MethodGet, req, &out); err != nil {
		return nil, fmt.Errorf("list %s versions: %w", s.alias, err)
	}
	vs := map[int]bool{}
	for name, info := range out {
		n, err := strconv.Atoi(strings.TrimPrefix(name, s.alias+"_v"))
		if err != nil {
			continue
		}
		_, active := info.Aliases[s.alias]
		vs[n] = active
	}
	return vs, nil
}

// Active returns the version behind the alias, or 0.
func (s *Store) Active(ctx context.Context) (int, error) {
	vs, err := s.versions(ctx)
	for v, active := range vs {
		if active {
			return v, err
		}
	}
	return 0, err
}

// Latest returns the highest existing version, or 0.
func (s *Store) Latest(ctx context.Context) (int, error) {
	vs, err := s.versions(ctx)
	latest := 0
	for v := range vs {
		latest = max(latest, v)
	}
	return latest, err
}

// Create creates physical version v with the base mapping plus the registry's custom fields.
func (s *Store) Create(ctx context.Context, v int) error {
	body, err := IndexBody(s.registry)
	if err != nil {
		return err
	}
	_, err = s.client.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: s.IndexName(v), Body: bytes.NewReader(body)})
	if err != nil {
		return fmt.Errorf("create %s: %w", s.IndexName(v), err)
	}
	return nil
}

// Promote points the alias at version v in one atomic alias update.
func (s *Store) Promote(ctx context.Context, v int) error {
	active, err := s.Active(ctx)
	if err != nil {
		return err
	}
	actions := []map[string]any{{"add": map[string]any{"index": s.IndexName(v), "alias": s.alias}}}
	if active > 0 && active != v {
		actions = append(actions, map[string]any{"remove": map[string]any{"index": s.IndexName(active), "alias": s.alias}})
	}
	body, _ := json.Marshal(map[string]any{"actions": actions})
	if _, err := s.client.Aliases(ctx, opensearchapi.AliasesReq{Body: bytes.NewReader(body)}); err != nil {
		return fmt.Errorf("promote %s: %w", s.IndexName(v), err)
	}
	return nil
}

// Count refreshes version v and returns its document count.
func (s *Store) Count(ctx context.Context, v int) (int64, error) {
	idx := s.IndexName(v)
	if _, err := s.client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Index: []string{idx}}); err != nil {
		return 0, fmt.Errorf("refresh %s: %w", idx, err)
	}
	resp, err := s.client.Indices.Count(ctx, &opensearchapi.IndicesCountReq{Indices: []string{idx}})
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", idx, err)
	}
	return int64(resp.Count), nil
}

// Drop deletes version v. Dropping a missing version is not an error.
func (s *Store) Drop(ctx context.Context, v int) error {
	_, err := s.client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{s.IndexName(v)}})
	var se *opensearchgo.StructError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("drop %s: %w", s.IndexName(v), err)
	}
	return nil
}

// IndexBody is the create-index body: the embedded base mapping with one property per custom
// field under `custom`.
func IndexBody(r *catalog.Registry) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(baseMapping, &body); err != nil {
		return nil, fmt.Errorf("base mapping: %w", err)
	}
	custom := body["mappings"].(map[string]any)["properties"].(map[string]any)["custom"].(map[string]any)["properties"].(map[string]any)
	for _, f := range r.Fields() {
		if !f.Custom {
			continue
		}
		custom[strings.TrimPrefix(f.Name, catalog.CustomPrefix)] = customMapping(f)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("index body: %w", err)
	}
	return b, nil
}

func customMapping(f catalog.Field) map[string]any {
	switch f.Type {
	case catalog.TypeKeyword, catalog.TypeKeywordArray:
		m := map[string]any{"type": "keyword"}
		if f.Searchable {
			m["fields"] = map[string]any{"text": map[string]any{"type": "text", "analyzer": "english"}}
		}
		return m
	case catalog.TypeText:
		return map[string]any{"type": "text", "analyzer": "english"}
	case catalog.TypeInt:
		return map[string]any{"type": "long"}
	case catalog.TypeFloat:
		return map[string]any{"type": "double"}
	case catalog.TypeBool:
		return map[string]any{"type": "boolean"}
	case catalog.TypeDate:
		return map[string]any{"type": "date"}
	}
	return map[string]any{"type": "keyword"}
}

// document is the indexed form of p. Attribute keys become sub-fields of `attributes`.
func document(p catalog.Product) map[string]any {
	d := map[string]any{
		"product_id":           p.ProductID,
		"tenant_id":            p.TenantID,
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
	}
	setIf := func(k string, ok bool, v any) {
		if ok {
			d[k] = v
		}
	}
	setIf("description", p.Description != "", p.Description)
	setIf("brand", p.Brand != "", p.Brand)
	setIf("product_type", p.ProductType != "", p.ProductType)
	setIf("ship_regions", len(p.ShipRegions) > 0, p.ShipRegions)
	setIf("image_urls", len(p.ImageURLs) > 0, p.ImageURLs)
	setIf("stock", p.Stock != nil, p.Stock)
	setIf("rating", p.Rating != nil, p.Rating)
	setIf("created_at", p.CreatedAt != nil, p.CreatedAt)
	if len(p.Attributes) > 0 {
		attrs := make(map[string]any, len(p.Attributes))
		for k, v := range p.Attributes {
			attrs[attributeKey(k)] = v
		}
		d["attributes"] = attrs
	}
	setIf("custom", len(p.Custom) > 0, p.Custom)
	return d
}

// attributeKey keeps tenant attribute names from creating nested objects or invalid fields.
func attributeKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	return strings.NewReplacer(".", "_", " ", "_").Replace(k)
}
