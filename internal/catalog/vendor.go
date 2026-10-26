package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/theory/jsonpath"

	"github.com/zavisoft/search-engine/internal/platform"
)

// Vendor field names, shared by PUT /v1/vendors, catalog.vendor.v1 and catalog.vendor_mapping.
const (
	VendorScore         = "score"
	VendorOnTime        = "on_time_dispatch_rate"
	VendorDisputeRate   = "dispute_rate"
	VendorResponseHours = "response_hours"
	VendorVerified      = "verified"
)

// VendorFields lists the vendor fields in a stable order.
var VendorFields = []string{VendorScore, VendorOnTime, VendorDisputeRate, VendorResponseHours, VendorVerified}

// unitVendorFields are within [0, 1] and may be rescaled by a mapping's {min, max}.
var unitVendorFields = []string{VendorScore, VendorOnTime, VendorDisputeRate}

// Vendor is a partial update of a vendor's online features (PLAN.md R9); nil fields keep their
// stored value. The tenant computes these values; the platform only stores and reads them.
type Vendor struct {
	ID                 string   `json:"vendor_id"`
	Score              *float64 `json:"score,omitempty"`
	OnTimeDispatchRate *float64 `json:"on_time_dispatch_rate,omitempty"`
	DisputeRate        *float64 `json:"dispute_rate,omitempty"`
	ResponseHours      *float64 `json:"response_hours,omitempty"`
	Verified           *bool    `json:"verified,omitempty"`
}

// Floats returns the numeric fields that are set, by field name.
func (v Vendor) Floats() map[string]float64 {
	out := map[string]float64{}
	for name, x := range map[string]*float64{VendorScore: v.Score, VendorOnTime: v.OnTimeDispatchRate,
		VendorDisputeRate: v.DisputeRate, VendorResponseHours: v.ResponseHours} {
		if x != nil {
			out[name] = *x
		}
	}
	return out
}

// Empty reports whether the update sets no field.
func (v Vendor) Empty() bool { return len(v.Floats()) == 0 && v.Verified == nil }

// Validate checks the ID and ranges: score and rates within [0, 1], response hours >= 0. ptr
// prefixes error paths.
func (v Vendor) Validate(ptr string) []platform.FieldError {
	var errs []platform.FieldError
	switch {
	case v.ID == "":
		errs = append(errs, platform.FieldError{Path: ptr + "/vendor_id", Code: "required", Message: "vendor_id is required"})
	case len(v.ID) > 256:
		errs = append(errs, platform.FieldError{Path: ptr + "/vendor_id", Code: "too_long", Message: "vendor_id is longer than 256 bytes"})
	}
	floats := v.Floats()
	for _, name := range VendorFields {
		x, ok := floats[name]
		switch {
		case !ok:
		case math.IsNaN(x) || math.IsInf(x, 0):
			errs = append(errs, platform.FieldError{Path: ptr + "/" + name, Code: "invalid", Message: name + " must be a finite number"})
		case name == VendorResponseHours && x < 0:
			errs = append(errs, platform.FieldError{Path: ptr + "/" + name, Code: "out_of_range", Message: name + " must be >= 0"})
		case name != VendorResponseHours && (x < 0 || x > 1):
			errs = append(errs, platform.FieldError{Path: ptr + "/" + name, Code: "out_of_range", Message: name + " must be within [0, 1]"})
		}
	}
	if v.Empty() {
		errs = append(errs, platform.FieldError{Path: ptr, Code: "required", Message: "set at least one vendor field"})
	}
	return errs
}

// VendorEvent is the `catalog.vendor.v1` envelope, for vendor data that changes independently of
// products (PLAN.md R9). Key `{tenant_id}:{vendor_id}`; the topic is compacted, so it keeps the
// latest update per vendor. Updates are partial, so producers should send every field.
type VendorEvent struct {
	EventID  string    `json:"event_id"`
	TenantID string    `json:"tenant_id"`
	TS       time.Time `json:"ts"`
	Vendor   Vendor    `json:"vendor"`
}

// NewVendorEvent wraps v for tenantID.
func NewVendorEvent(tenantID string, v Vendor, ts time.Time) VendorEvent {
	return VendorEvent{EventID: uuid.NewString(), TenantID: tenantID, TS: ts.UTC(), Vendor: v}
}

// Key is the Kafka record key.
func (e VendorEvent) Key() []byte { return []byte(e.TenantID + ":" + e.Vendor.ID) }

// Headers returns the Kafka headers for e.
func (e VendorEvent) Headers() map[string]string {
	return map[string]string{HeaderTenantID: e.TenantID, HeaderEventID: e.EventID, HeaderSchemaVersion: strconv.Itoa(SchemaVersion)}
}

// DecodeVendorEvent parses and validates a catalog.vendor.v1 record.
func DecodeVendorEvent(value []byte, headers map[string]string) (VendorEvent, error) {
	var e VendorEvent
	if err := json.Unmarshal(value, &e); err != nil {
		return VendorEvent{}, fmt.Errorf("decode vendor event: %w", err)
	}
	if e.TenantID == "" {
		return VendorEvent{}, errors.New("tenant_id is required")
	}
	if errs := e.Vendor.Validate("/vendor"); len(errs) > 0 {
		return VendorEvent{}, fmt.Errorf("%s: %s", errs[0].Path, errs[0].Message)
	}
	if h, ok := headers[HeaderTenantID]; ok && h != e.TenantID {
		return VendorEvent{}, fmt.Errorf("tenant-id header %q does not match body %q", h, e.TenantID)
	}
	if h, ok := headers[HeaderSchemaVersion]; ok && h != strconv.Itoa(SchemaVersion) {
		return VendorEvent{}, fmt.Errorf("unsupported schema-version %q", h)
	}
	return e, nil
}

// VendorField maps one vendor field from the product payload: a JSONPath, either alone or as
// {path, min, max}. min and max rescale the tenant's own range to [0, 1] (unit fields only).
type VendorField struct {
	Path string   `json:"path"`
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
}

// UnmarshalJSON accepts "$.vendor.score" or {"path": ..., "min": ..., "max": ...}.
func (f *VendorField) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &f.Path); err == nil {
		return nil
	}
	type plain VendorField
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("vendor field: %w", err)
	}
	*f = VendorField(p)
	return nil
}

// MarshalJSON writes the short form when no range is set.
func (f VendorField) MarshalJSON() ([]byte, error) {
	type plain VendorField
	var v any = plain(f)
	if f.Min == nil && f.Max == nil {
		v = f.Path
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("vendor field: %w", err)
	}
	return b, nil
}

// VendorMapping is `catalog.vendor_mapping`: vendor field → where to read it in the product
// payload. The vendor is the product's mapped vendor_id.
type VendorMapping map[string]VendorField

// ValidateVendorMapping checks field names, JSONPaths and ranges.
func ValidateVendorMapping(m VendorMapping) []platform.FieldError {
	var errs []platform.FieldError
	for _, name := range slices.Sorted(maps.Keys(m)) {
		f, ptr := m[name], "/catalog/vendor_mapping/"+name
		if !slices.Contains(VendorFields, name) {
			errs = append(errs, platform.FieldError{Path: ptr, Code: "unknown_field", Message: "vendor fields are " + strings.Join(VendorFields, ", ")})
			continue
		}
		if _, err := compilePath(f.Path); err != nil {
			errs = append(errs, platform.FieldError{Path: ptr + "/path", Code: "invalid_jsonpath", Message: err.Error()})
		}
		hasRange := f.Min != nil || f.Max != nil
		switch {
		case hasRange && !slices.Contains(unitVendorFields, name):
			errs = append(errs, platform.FieldError{Path: ptr, Code: "invalid", Message: "min and max apply to score and rates only"})
		case hasRange && (f.Min == nil || f.Max == nil || *f.Max <= *f.Min):
			errs = append(errs, platform.FieldError{Path: ptr, Code: "invalid", Message: "set both min and max, with max > min"})
		}
	}
	return errs
}

type vendorRule struct {
	name     string
	path     *jsonpath.Path
	min, max *float64
}

// VendorMapper extracts vendor fields from product payloads.
type VendorMapper struct{ rules []vendorRule }

// NewVendorMapper compiles a validated mapping; nil when the mapping is empty.
func NewVendorMapper(m VendorMapping) (*VendorMapper, error) {
	if len(m) == 0 {
		return nil, nil //nolint:nilnil // no mapping means no extraction
	}
	if errs := ValidateVendorMapping(m); len(errs) > 0 {
		return nil, fmt.Errorf("vendor mapping %s: %s", errs[0].Path, errs[0].Message)
	}
	vm := &VendorMapper{}
	for _, name := range VendorFields {
		if f, ok := m[name]; ok {
			p, _ := compilePath(f.Path)
			vm.rules = append(vm.rules, vendorRule{name: name, path: p, min: f.Min, max: f.Max})
		}
	}
	return vm, nil
}

// Extract reads the mapped vendor fields of one product payload. ok is false when none is
// present. Ranged fields are rescaled and clamped to [0, 1]; other values are returned as is
// for the caller to validate.
func (vm *VendorMapper) Extract(doc []byte) (Vendor, bool) {
	var root any
	if vm == nil || json.Unmarshal(doc, &root) != nil {
		return Vendor{}, false
	}
	var v Vendor
	found := false
	for _, r := range vm.rules {
		nodes := r.path.Select(root)
		if len(nodes) == 0 || nodes[0] == nil {
			continue
		}
		if r.name == VendorVerified {
			if b, ok := nodes[0].(bool); ok {
				v.Verified, found = &b, true
			}
			continue
		}
		x, err := asFloat(nodes[0])
		if err != nil {
			continue
		}
		if r.min != nil && r.max != nil {
			x = math.Min(math.Max((x-*r.min)/(*r.max-*r.min), 0), 1)
		}
		found = true
		switch r.name {
		case VendorScore:
			v.Score = &x
		case VendorOnTime:
			v.OnTimeDispatchRate = &x
		case VendorDisputeRate:
			v.DisputeRate = &x
		case VendorResponseHours:
			v.ResponseHours = &x
		}
	}
	return v, found
}
