package catalog

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/zavisoft/search-engine/internal/platform"
)

// Product is the canonical product (docs/DATA.md §4). Optional numeric fields are pointers so
// that "absent" stays distinguishable from zero: a filter on `stock` never matches a product
// whose catalog does not track stock.
type Product struct {
	ProductID           string              `json:"product_id"`
	TenantID            string              `json:"tenant_id"`
	VendorID            string              `json:"vendor_id"`
	Title               string              `json:"title"`
	Description         string              `json:"description,omitempty"`
	Brand               string              `json:"brand,omitempty"`
	CategoryPath        []string            `json:"category_path"`
	CategoryIDs         []string            `json:"category_ids"`
	ProductType         string              `json:"product_type,omitempty"`
	Attributes          map[string][]string `json:"attributes,omitempty"`
	PriceMinor          int64               `json:"price_minor"`
	Currency            string              `json:"currency"`
	MOQ                 int32               `json:"moq"`
	Stock               *int32              `json:"stock,omitempty"`
	ShipRegions         []string            `json:"ship_regions,omitempty"`
	Status              string              `json:"status"`
	Rating              *float64            `json:"rating,omitempty"`
	RatingCount         int32               `json:"rating_count"`
	ImageURLs           []string            `json:"image_urls,omitempty"`
	ListingCompleteness float64             `json:"listing_completeness"`
	CreatedAt           *time.Time          `json:"created_at,omitempty"`
	Custom              map[string]any      `json:"custom,omitempty"`
}

// Product statuses.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

// MandatoryFields must be mapped by every tenant and present on every product (PLAN.md R1).
var MandatoryFields = []string{"product_id", "title", "category_path", "price_minor", "currency"}

// pointNamespace is the UUIDv5 namespace for vector point IDs. Changing it re-keys every
// stored vector, so it is derived from a fixed name rather than generated.
var pointNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/zavisoft/search-engine/catalog/point"))

// PointID is the vector store point ID for a product: UUIDv5 of the product ID.
func PointID(productID string) string {
	return uuid.NewSHA1(pointNamespace, []byte(productID)).String()
}

// Normalize fills defaults and derived fields: whitespace, currency case, category IDs, status,
// MOQ and listing completeness. Mapping calls it; tests and the seed may call it directly.
func (p *Product) Normalize() {
	p.Title = collapseSpace(p.Title)
	p.Brand = collapseSpace(p.Brand)
	p.Description = strings.TrimSpace(p.Description)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	p.CategoryPath = compact(p.CategoryPath)
	p.CategoryIDs = p.CategoryIDs[:0]
	for _, c := range p.CategoryPath {
		if s := Slug(c); s != "" && !slices.Contains(p.CategoryIDs, s) {
			p.CategoryIDs = append(p.CategoryIDs, s)
		}
	}
	p.ShipRegions = compact(p.ShipRegions)
	p.ImageURLs = compact(p.ImageURLs)
	if p.Status == "" {
		p.Status = StatusActive
	}
	if p.VendorID == "" {
		p.VendorID = p.TenantID
	}
	if p.MOQ == 0 {
		p.MOQ = 1
	}
	p.ListingCompleteness = p.completeness()
}

func (p *Product) completeness() float64 {
	present := []bool{
		p.Description != "", p.Brand != "", p.ProductType != "", len(p.Attributes) > 0,
		len(p.ImageURLs) > 0, len(p.ShipRegions) > 0, p.Rating != nil,
	}
	n := 0
	for _, ok := range present {
		if ok {
			n++
		}
	}
	return float64(n) / float64(len(present))
}

// Validate checks mandatory fields and value ranges. Paths are JSON Pointers into the product.
func (p *Product) Validate() []platform.FieldError {
	var errs []platform.FieldError
	add := func(path, code, msg string) {
		errs = append(errs, platform.FieldError{Path: path, Code: code, Message: msg})
	}
	if p.ProductID == "" {
		add("/product_id", "required", "product_id is required")
	} else if len(p.ProductID) > 128 {
		add("/product_id", "too_long", "product_id is at most 128 characters")
	}
	if p.Title == "" {
		add("/title", "required", "title is required")
	}
	if len(p.CategoryPath) == 0 {
		add("/category_path", "required", "category_path needs at least one segment")
	}
	if p.PriceMinor < 0 {
		add("/price_minor", "out_of_range", "price_minor must be >= 0")
	}
	if !isCurrency(p.Currency) {
		add("/currency", "invalid", "currency must be a 3-letter ISO 4217 code")
	}
	if p.MOQ < 1 {
		add("/moq", "out_of_range", "moq must be >= 1")
	}
	if p.Stock != nil && *p.Stock < 0 {
		add("/stock", "out_of_range", "stock must be >= 0")
	}
	if p.Status != StatusActive && p.Status != StatusInactive {
		add("/status", "invalid", "status must be active or inactive")
	}
	if p.Rating != nil && (*p.Rating < 0 || *p.Rating > 5) {
		add("/rating", "out_of_range", "rating must be within [0, 5]")
	}
	if p.RatingCount < 0 {
		add("/rating_count", "out_of_range", "rating_count must be >= 0")
	}
	return errs
}

// EmbeddingText is the text-embedder input: "{title}. {brand}. {category leaf}. {attributes}"
// with attributes as sorted `key:v1,v2` pairs (docs/DATA.md §6).
func (p *Product) EmbeddingText() string {
	parts := []string{p.Title}
	if p.Brand != "" {
		parts = append(parts, p.Brand)
	}
	if n := len(p.CategoryPath); n > 0 {
		parts = append(parts, p.CategoryPath[n-1])
	}
	if len(p.Attributes) > 0 {
		keys := make([]string, 0, len(p.Attributes))
		for k := range p.Attributes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		kv := make([]string, 0, len(keys))
		for _, k := range keys {
			if vs := p.Attributes[k]; len(vs) > 0 {
				kv = append(kv, k+":"+strings.Join(vs, ","))
			}
		}
		if len(kv) > 0 {
			parts = append(parts, strings.Join(kv, " "))
		}
	}
	return strings.Join(parts, ". ")
}

// Slug lower-cases s, strips accents and replaces every run of non-alphanumerics with "-".
func Slug(s string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	folded, _, err := transform.String(t, s)
	if err != nil {
		folded = s
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(folded) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func isCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func compact(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s = collapseSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
