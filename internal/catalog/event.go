package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Topic carries catalog changes for all tenants (PLAN.md R1).
const Topic = "catalog.product.v1"

// SchemaVersion is the envelope version sent in the schema-version header.
const SchemaVersion = 1

// Kafka header names on catalog events.
const (
	HeaderTenantID      = "tenant-id"
	HeaderEventID       = "event-id"
	HeaderOp            = "op"
	HeaderSchemaVersion = "schema-version"
)

// Op is the change type of a catalog event.
type Op string

// Catalog operations. A Kafka tombstone (null value) is decoded as OpDelete.
const (
	OpUpsert Op = "upsert"
	OpDelete Op = "delete"
)

// ProductEvent is the `catalog.product.v1` envelope. Product holds the tenant's own JSON, mapped
// through `catalog.field_mapping` by the indexer. SourceVersion orders changes to one product:
// the indexer ignores an event older than what is already indexed, so replays are safe. Zero
// means unversioned (tombstones), which always applies.
type ProductEvent struct {
	EventID       string          `json:"event_id"`
	TenantID      string          `json:"tenant_id"`
	ProductID     string          `json:"product_id"`
	Op            Op              `json:"op"`
	TS            time.Time       `json:"ts"`
	SourceVersion int64           `json:"source_version"`
	Product       json.RawMessage `json:"product,omitempty"`
}

// Key is the Kafka record key, `{tenant_id}:{product_id}`, so one product's changes share a
// partition and stay ordered.
func (e ProductEvent) Key() []byte { return []byte(e.TenantID + ":" + e.ProductID) }

// Headers returns the Kafka headers for e.
func (e ProductEvent) Headers() map[string]string {
	return map[string]string{
		HeaderTenantID:      e.TenantID,
		HeaderEventID:       e.EventID,
		HeaderOp:            string(e.Op),
		HeaderSchemaVersion: strconv.Itoa(SchemaVersion),
	}
}

// Validate checks the envelope's own fields; the product body is validated by mapping.
func (e ProductEvent) Validate() error {
	switch {
	case e.TenantID == "":
		return errors.New("tenant_id is required")
	case e.ProductID == "":
		return errors.New("product_id is required")
	case e.Op != OpUpsert && e.Op != OpDelete:
		return fmt.Errorf("op %q must be upsert or delete", e.Op)
	case e.Op == OpUpsert && len(bytes.TrimSpace(e.Product)) == 0:
		return errors.New("product is required for upsert")
	case e.SourceVersion < 0:
		return errors.New("source_version must be >= 0")
	}
	return nil
}

// DecodeProductEvent parses a Kafka record. A nil value is a tombstone: the tenant and product
// come from the key. Headers, when present, must agree with the body.
func DecodeProductEvent(key, value []byte, headers map[string]string) (ProductEvent, error) {
	if value == nil {
		tenantID, productID, ok := bytes.Cut(key, []byte(":"))
		if !ok || len(tenantID) == 0 || len(productID) == 0 {
			return ProductEvent{}, fmt.Errorf("tombstone key %q is not tenant:product", key)
		}
		return ProductEvent{TenantID: string(tenantID), ProductID: string(productID), Op: OpDelete}, nil
	}
	var e ProductEvent
	if err := json.Unmarshal(value, &e); err != nil {
		return ProductEvent{}, fmt.Errorf("decode catalog event: %w", err)
	}
	if err := e.Validate(); err != nil {
		return ProductEvent{}, err
	}
	if h, ok := headers[HeaderTenantID]; ok && h != e.TenantID {
		return ProductEvent{}, fmt.Errorf("tenant-id header %q does not match body %q", h, e.TenantID)
	}
	if h, ok := headers[HeaderOp]; ok && h != string(e.Op) {
		return ProductEvent{}, fmt.Errorf("op header %q does not match body %q", h, e.Op)
	}
	if h, ok := headers[HeaderSchemaVersion]; ok && h != strconv.Itoa(SchemaVersion) {
		return ProductEvent{}, fmt.Errorf("unsupported schema-version %q", h)
	}
	return e, nil
}
