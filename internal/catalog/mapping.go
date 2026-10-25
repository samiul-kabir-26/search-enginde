package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/theory/jsonpath"

	"github.com/zavisoft/search-engine/internal/platform"
)

// FieldMapping maps canonical field names to RFC 9535 JSONPath expressions over the tenant's
// own product JSON (`catalog.field_mapping`).
type FieldMapping map[string]string

// derivedFields are computed during normalization and cannot be mapped.
var derivedFields = []string{"tenant_id", "category_ids", "listing_completeness"}

func compilePath(expr string) (*jsonpath.Path, error) {
	if !strings.HasPrefix(expr, "$") {
		return nil, fmt.Errorf("JSONPath must start with $")
	}
	p, err := jsonpath.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("parse JSONPath: %w", err)
	}
	return p, nil
}

// ValidateMapping checks that every key is a mappable canonical field, every mandatory field is
// mapped, and every expression parses. Paths are relative to `/catalog/field_mapping`.
func ValidateMapping(m FieldMapping) []platform.FieldError {
	var errs []platform.FieldError
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		path := "/catalog/field_mapping/" + k
		switch f, ok := lookupCanonical(k); {
		case !ok:
			errs = append(errs, platform.FieldError{Path: path, Code: "unknown_field", Message: "not a canonical field; use catalog.custom_fields for tenant fields"})
			continue
		case slices.Contains(derivedFields, f.Name):
			errs = append(errs, platform.FieldError{Path: path, Code: "derived_field", Message: k + " is computed and cannot be mapped"})
			continue
		}
		if _, err := compilePath(m[k]); err != nil {
			errs = append(errs, platform.FieldError{Path: path, Code: "invalid_jsonpath", Message: err.Error()})
		}
	}
	for _, k := range MandatoryFields {
		if _, ok := m[k]; !ok {
			errs = append(errs, platform.FieldError{Path: "/catalog/field_mapping/" + k, Code: "required", Message: k + " must be mapped"})
		}
	}
	return errs
}

func lookupCanonical(name string) (Field, bool) {
	for _, f := range canonicalFields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

type compiled struct {
	field    Field
	path     *jsonpath.Path
	source   string
	required bool
}

// Mapper turns tenant product JSON into canonical products for one config version.
type Mapper struct {
	fields []compiled
}

// NewMapper compiles the field mapping and custom fields.
func NewMapper(m FieldMapping, custom []CustomField) (*Mapper, error) {
	errs := append(ValidateMapping(m), ValidateCustomFields(custom, 0)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("catalog mapping: %s: %s", errs[0].Path, errs[0].Message)
	}
	mp := &Mapper{}
	for _, f := range canonicalFields {
		expr, ok := m[f.Name]
		if !ok {
			continue
		}
		p, _ := compilePath(expr)
		mp.fields = append(mp.fields, compiled{field: f, path: p, source: expr, required: slices.Contains(MandatoryFields, f.Name)})
	}
	for _, c := range custom {
		p, _ := compilePath(c.Source)
		mp.fields = append(mp.fields, compiled{
			field: Field{Name: CustomPrefix + c.Name, Type: c.Type, Custom: true}, path: p, source: c.Source, required: c.Required,
		})
	}
	return mp, nil
}

// Map decodes one tenant product document and maps it. The product is returned even when
// there are errors so previews can show partial results; callers must not index it then.
func (m *Mapper) Map(tenantID string, doc []byte) (Product, []platform.FieldError) {
	var input any
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&input); err != nil {
		return Product{}, []platform.FieldError{{Path: "", Code: "invalid_json", Message: err.Error()}}
	}
	p := Product{TenantID: tenantID}
	var errs []platform.FieldError
	for _, c := range m.fields {
		nodes := c.path.Select(input)
		ptr := "/" + strings.ReplaceAll(c.field.Name, ".", "/")
		if len(nodes) == 0 || (len(nodes) == 1 && nodes[0] == nil) {
			if c.required {
				errs = append(errs, platform.FieldError{Path: ptr, Code: "missing", Message: fmt.Sprintf("%s selected nothing", c.source)})
			}
			continue
		}
		v, err := coerce(c.field.Type, nodes)
		if err != nil {
			errs = append(errs, platform.FieldError{Path: ptr, Code: "type", Message: fmt.Sprintf("%s: %v", c.source, err)})
			continue
		}
		if c.field.Custom {
			if p.Custom == nil {
				p.Custom = map[string]any{}
			}
			p.Custom[strings.TrimPrefix(c.field.Name, CustomPrefix)] = v
			continue
		}
		if err := assign(&p, c.field.Name, v); err != nil {
			errs = append(errs, platform.FieldError{Path: ptr, Code: "type", Message: fmt.Sprintf("%s: %v", c.source, err)})
		}
	}
	p.Normalize()
	for _, e := range p.Validate() {
		if !slices.ContainsFunc(errs, func(x platform.FieldError) bool { return x.Path == e.Path }) {
			errs = append(errs, e)
		}
	}
	return p, errs
}

// PreviewResult is one mapped sample document for the onboarding mapping preview.
type PreviewResult struct {
	Product Product               `json:"product"`
	Errors  []platform.FieldError `json:"errors,omitempty"`
}

// Preview maps sample documents without side effects.
func (m *Mapper) Preview(tenantID string, docs []json.RawMessage) []PreviewResult {
	out := make([]PreviewResult, len(docs))
	for i, d := range docs {
		p, errs := m.Map(tenantID, d)
		out[i] = PreviewResult{Product: p, Errors: errs}
	}
	return out
}

func coerce(t FieldType, nodes jsonpath.NodeList) (any, error) {
	switch t {
	case TypeKeywordArray:
		var out []string
		for _, n := range flatten(nodes) {
			s, err := asString(n)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case TypeText:
		var parts []string
		for _, n := range flatten(nodes) {
			s, err := asString(n)
			if err != nil {
				return nil, err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n"), nil
	case TypeObject:
		if len(nodes) != 1 {
			return nil, fmt.Errorf("selects %d values, want one object", len(nodes))
		}
		obj, ok := nodes[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("want an object, got %T", nodes[0])
		}
		out := make(map[string][]string, len(obj))
		for k, v := range obj {
			vals, err := coerce(TypeKeywordArray, jsonpath.NodeList{v})
			if err != nil {
				return nil, fmt.Errorf("attribute %q: %w", k, err)
			}
			if vs := vals.([]string); len(vs) > 0 {
				out[k] = vs
			}
		}
		return out, nil
	}
	if len(nodes) != 1 {
		return nil, fmt.Errorf("selects %d values, want one", len(nodes))
	}
	n := nodes[0]
	switch t {
	case TypeKeyword:
		return asString(n)
	case TypeInt:
		return asInt(n)
	case TypeFloat:
		return asFloat(n)
	case TypeBool:
		switch v := n.(type) {
		case bool:
			return v, nil
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("want a boolean: %w", err)
			}
			return b, nil
		}
		return nil, fmt.Errorf("want a boolean, got %T", n)
	case TypeDate:
		switch v := n.(type) {
		case string:
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("want an RFC 3339 date: %w", err)
			}
			return t, nil
		case float64:
			return time.Unix(int64(v), 0).UTC(), nil
		}
		return nil, fmt.Errorf("want an RFC 3339 string or unix seconds, got %T", n)
	}
	return nil, fmt.Errorf("unsupported field type %q", t)
}

func flatten(nodes jsonpath.NodeList) []any {
	var out []any
	for _, n := range nodes {
		if arr, ok := n.([]any); ok {
			out = append(out, arr...)
		} else if n != nil {
			out = append(out, n)
		}
	}
	return out
}

func asString(n any) (string, error) {
	switch v := n.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", fmt.Errorf("want a string, got %T", n)
}

func asFloat(n any) (float64, error) {
	switch v := n.(type) {
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("want a number: %w", err)
		}
		return f, nil
	}
	return 0, fmt.Errorf("want a number, got %T", n)
}

func asInt(n any) (int64, error) {
	f, err := asFloat(n)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("want an integer, got %v", f)
	}
	return int64(f), nil
}

func asInt32(v any) (int32, error) {
	i := v.(int64)
	if i < math.MinInt32 || i > math.MaxInt32 {
		return 0, fmt.Errorf("%d overflows int32", i)
	}
	return int32(i), nil
}

func assign(p *Product, name string, v any) error {
	switch name {
	case "product_id":
		p.ProductID = v.(string)
	case "vendor_id":
		p.VendorID = v.(string)
	case "title":
		p.Title = v.(string)
	case "description":
		p.Description = v.(string)
	case "brand":
		p.Brand = v.(string)
	case "category_path":
		p.CategoryPath = v.([]string)
	case "product_type":
		p.ProductType = v.(string)
	case "attributes":
		p.Attributes = v.(map[string][]string)
	case "price_minor":
		p.PriceMinor = v.(int64)
	case "currency":
		p.Currency = v.(string)
	case "moq":
		i, err := asInt32(v)
		p.MOQ = i
		return err
	case "stock":
		i, err := asInt32(v)
		p.Stock = &i
		return err
	case "ship_regions":
		p.ShipRegions = v.([]string)
	case "status":
		p.Status = strings.ToLower(v.(string))
	case "rating":
		f := v.(float64)
		p.Rating = &f
	case "rating_count":
		i, err := asInt32(v)
		p.RatingCount = i
		return err
	case "image_urls":
		p.ImageURLs = v.([]string)
	case "created_at":
		t := v.(time.Time)
		p.CreatedAt = &t
	default:
		return fmt.Errorf("field %q is not mappable", name)
	}
	return nil
}
