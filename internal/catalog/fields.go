package catalog

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/zavisoft/search-engine/internal/platform"
)

// FieldType is the storage type of a canonical or custom field.
type FieldType string

// Field types. Arrays of keywords are the only multi-valued type.
const (
	TypeKeyword      FieldType = "keyword"
	TypeKeywordArray FieldType = "keyword[]"
	TypeText         FieldType = "text"
	TypeInt          FieldType = "int"
	TypeFloat        FieldType = "float"
	TypeBool         FieldType = "bool"
	TypeDate         FieldType = "date"
	TypeObject       FieldType = "object"
)

func (t FieldType) valid() bool {
	switch t {
	case TypeKeyword, TypeKeywordArray, TypeText, TypeInt, TypeFloat, TypeBool, TypeDate:
		return true
	}
	return false
}

// Field describes one searchable/filterable field. Custom fields are addressed as `custom.{name}`.
type Field struct {
	Name       string
	Type       FieldType
	Filterable bool
	Facetable  bool
	Searchable bool
	Sortable   bool
	Custom     bool
}

// CustomPrefix is how filters, facets and CEL address tenant-defined fields.
const CustomPrefix = "custom."

var canonicalFields = []Field{
	{Name: "product_id", Type: TypeKeyword, Filterable: true},
	{Name: "vendor_id", Type: TypeKeyword, Filterable: true, Facetable: true},
	{Name: "title", Type: TypeText, Searchable: true},
	{Name: "description", Type: TypeText, Searchable: true},
	{Name: "brand", Type: TypeKeyword, Filterable: true, Facetable: true, Searchable: true},
	{Name: "category_path", Type: TypeKeywordArray, Filterable: true},
	{Name: "category_ids", Type: TypeKeywordArray, Filterable: true, Facetable: true},
	{Name: "product_type", Type: TypeKeyword, Filterable: true, Facetable: true},
	{Name: "attributes", Type: TypeObject, Searchable: true},
	{Name: "price_minor", Type: TypeInt, Filterable: true, Facetable: true, Sortable: true},
	{Name: "currency", Type: TypeKeyword, Filterable: true},
	{Name: "moq", Type: TypeInt, Filterable: true},
	{Name: "stock", Type: TypeInt, Filterable: true},
	{Name: "ship_regions", Type: TypeKeywordArray, Filterable: true, Facetable: true},
	{Name: "status", Type: TypeKeyword, Filterable: true},
	{Name: "rating", Type: TypeFloat, Filterable: true, Sortable: true},
	{Name: "rating_count", Type: TypeInt, Sortable: true},
	{Name: "image_urls", Type: TypeKeywordArray},
	{Name: "listing_completeness", Type: TypeFloat},
	{Name: "created_at", Type: TypeDate, Filterable: true, Sortable: true},
}

// CustomField is a tenant-defined field in `catalog.custom_fields` (PLAN.md R1).
type CustomField struct {
	Name       string    `json:"name"`
	Type       FieldType `json:"type"`
	Source     string    `json:"source"`
	Filterable bool      `json:"filterable,omitempty"`
	Facetable  bool      `json:"facetable,omitempty"`
	Searchable bool      `json:"searchable,omitempty"`
	Sortable   bool      `json:"sortable,omitempty"`
	Required   bool      `json:"required,omitempty"`
}

var customNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ValidateCustomFields checks names, types and capability combinations, and the plan's cap
// (max <= 0 means unlimited). Paths are relative to `/catalog/custom_fields`.
func ValidateCustomFields(fields []CustomField, max int) []platform.FieldError {
	var errs []platform.FieldError
	add := func(i int, field, code, msg string) {
		errs = append(errs, platform.FieldError{
			Path: fmt.Sprintf("/catalog/custom_fields/%d/%s", i, field), Code: code, Message: msg,
		})
	}
	if max > 0 && len(fields) > max {
		errs = append(errs, platform.FieldError{
			Path: "/catalog/custom_fields", Code: "too_many",
			Message: fmt.Sprintf("at most %d custom fields are allowed on this plan", max),
		})
	}
	seen := map[string]bool{}
	for i, f := range fields {
		switch {
		case !customNameRe.MatchString(f.Name):
			add(i, "name", "invalid", "must match ^[a-z][a-z0-9_]{0,62}$")
		case seen[f.Name]:
			add(i, "name", "duplicate", "custom field names must be unique")
		}
		seen[f.Name] = true
		if !f.Type.valid() {
			add(i, "type", "invalid", "must be one of keyword, keyword[], text, int, float, bool, date")
			continue
		}
		if f.Source == "" {
			add(i, "source", "required", "a JSONPath source is required")
		} else if _, err := compilePath(f.Source); err != nil {
			add(i, "source", "invalid_jsonpath", err.Error())
		}
		if f.Type == TypeText && (f.Filterable || f.Facetable || f.Sortable) {
			add(i, "type", "unsupported", "text fields can only be searchable")
		}
		if f.Searchable && f.Type != TypeText && f.Type != TypeKeyword && f.Type != TypeKeywordArray {
			add(i, "searchable", "unsupported", "only text and keyword fields can be searchable")
		}
		if f.Facetable && f.Type == TypeDate {
			add(i, "facetable", "unsupported", "date fields cannot be facets")
		}
		if f.Sortable && f.Type == TypeKeywordArray {
			add(i, "sortable", "unsupported", "multi-valued fields cannot be sorted")
		}
	}
	return errs
}

// Registry is the merged set of canonical and custom fields for one tenant config version. It
// drives filter validation, pushdown, facets and the index mappings.
type Registry struct {
	fields []Field
	byName map[string]Field
}

// NewRegistry merges the canonical fields with validated custom fields.
func NewRegistry(custom []CustomField) (*Registry, error) {
	if errs := ValidateCustomFields(custom, 0); len(errs) > 0 {
		return nil, fmt.Errorf("custom fields: %s: %s", errs[0].Path, errs[0].Message)
	}
	r := &Registry{byName: make(map[string]Field, len(canonicalFields)+len(custom))}
	for _, f := range canonicalFields {
		r.add(f)
	}
	for _, c := range custom {
		r.add(Field{
			Name: CustomPrefix + c.Name, Type: c.Type, Custom: true,
			Filterable: c.Filterable, Facetable: c.Facetable, Searchable: c.Searchable, Sortable: c.Sortable,
		})
	}
	return r, nil
}

func (r *Registry) add(f Field) {
	r.fields = append(r.fields, f)
	r.byName[f.Name] = f
}

// Field looks up a field by its canonical name or `custom.{name}`.
func (r *Registry) Field(name string) (Field, bool) {
	f, ok := r.byName[name]
	return f, ok
}

// Fields returns all fields, canonical first, in declaration order.
func (r *Registry) Fields() []Field { return slices.Clone(r.fields) }

// Filterable returns the fields that may appear in filters and must be indexed for pushdown.
func (r *Registry) Filterable() []Field {
	var out []Field
	for _, f := range r.fields {
		if f.Filterable {
			out = append(out, f)
		}
	}
	return out
}
