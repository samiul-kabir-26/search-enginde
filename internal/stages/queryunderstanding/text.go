package queryunderstanding

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

// Normalize applies Unicode NFKC, lower-casing and whitespace collapsing.
func Normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(s))), " ")
}

// PriceBounds is a price constraint parsed from query text, in minor units.
type PriceBounds struct {
	Min, Max *int64
}

var (
	money        = `\$?\s*(\d+(?:[.,]\d{1,2})?)\s*(?:\$|usd|dollars?|bucks)?`
	reBetween    = regexp.MustCompile(`\b(?:between|from)\s+` + money + `\s*(?:and|to|-)\s*` + money)
	reMax        = regexp.MustCompile(`(?:\b(?:under|below|less than|cheaper than|up to|max(?:imum)?|at most)\s*|<\s*=?\s*)` + money)
	reMin        = regexp.MustCompile(`(?:\b(?:over|above|more than|at least|min(?:imum)?|from)\s*|>\s*=?\s*)` + money)
	reMultiSpace = regexp.MustCompile(`\s+`)
	reUnit       = regexp.MustCompile(`^\s*(?:lbs?|pounds?|kg|g|oz|ounces?|in|inch(?:es)?|ft|feet|foot|cm|mm|m|ml|l|liters?|litres?|w|watts?|v|volts?|mah|gb|tb|pcs|pieces?|pack|count|ct|years?|months?|%)\b`)
)

// unitAfter reports whether the number ending at end is followed by a unit ("under 5 lbs").
func unitAfter(text string, end int) bool { return reUnit.MatchString(text[end:]) }

func minor(s string) (int64, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil || f < 0 || f > 1e9 {
		return 0, false
	}
	return int64(math.Round(f * 100)), true
}

// ExtractPrice finds a price phrase ("under $50", "between 20 and 40 dollars", "over 100"),
// returns the bounds in minor units (two-decimal currencies) and the text without the phrase.
func ExtractPrice(text string) (PriceBounds, string) {
	var b PriceBounds
	if m := reBetween.FindStringSubmatchIndex(text); m != nil && !unitAfter(text, m[1]) {
		lo, ok1 := minor(text[m[2]:m[3]])
		hi, ok2 := minor(text[m[4]:m[5]])
		if ok1 && ok2 {
			if lo > hi {
				lo, hi = hi, lo
			}
			b.Min, b.Max = &lo, &hi
			return b, clean(text[:m[0]] + " " + text[m[1]:])
		}
	}
	if m := reMax.FindStringSubmatchIndex(text); m != nil && !unitAfter(text, m[1]) {
		if v, ok := minor(text[m[2]:m[3]]); ok {
			b.Max = &v
			text = text[:m[0]] + " " + text[m[1]:]
		}
	}
	if m := reMin.FindStringSubmatchIndex(text); m != nil && !unitAfter(text, m[1]) {
		if v, ok := minor(text[m[2]:m[3]]); ok {
			b.Min = &v
			text = text[:m[0]] + " " + text[m[1]:]
		}
	}
	return b, clean(text)
}

func clean(s string) string { return strings.TrimSpace(reMultiSpace.ReplaceAllString(s, " ")) }

// Filter returns the price bounds as a compiled filter on price_minor.
func (b PriceBounds) Filter() filter.Expr {
	if b.Min == nil && b.Max == nil {
		return filter.Expr{}
	}
	bounds := filter.Bounds{}
	if b.Min != nil {
		bounds.Gte = *b.Min
	}
	if b.Max != nil {
		bounds.Lte = *b.Max
	}
	return filter.Expr{Cond: &filter.Condition{Field: "price_minor", Op: filter.Range, Value: bounds, Type: catalog.TypeInt}}
}

// DefaultDictionary is the built-in attribute vocabulary; tenants extend it in `query.attributes`.
func DefaultDictionary() map[string][]string {
	return map[string][]string{
		"color": {
			"black", "white", "red", "blue", "navy", "navy blue", "light blue", "green", "olive", "yellow",
			"orange", "pink", "purple", "brown", "beige", "tan", "grey", "gray", "silver", "gold", "rose gold",
			"cream", "ivory", "maroon", "teal", "turquoise", "multicolor", "clear", "transparent",
		},
		"material": {
			"leather", "faux leather", "suede", "cotton", "linen", "wool", "silk", "polyester", "nylon", "denim",
			"velvet", "steel", "stainless steel", "aluminum", "aluminium", "metal", "iron", "brass", "copper",
			"wood", "wooden", "oak", "walnut", "pine", "bamboo", "glass", "ceramic", "porcelain", "plastic",
			"rubber", "silicone", "marble", "stone", "gold plated", "sterling silver", "platinum", "fabric", "mesh",
		},
		"size": {
			"xs", "small", "medium", "large", "xl", "xxl", "extra large", "king", "queen", "twin", "full",
			"mini", "compact", "oversized",
		},
	}
}

// Dictionary matches attribute terms in normalized text, longest term first.
type Dictionary struct {
	terms []term
}

type term struct {
	attr, value string
	re          *regexp.Regexp
}

// NewDictionary merges the default vocabulary with tenant additions.
func NewDictionary(extra map[string][]string) *Dictionary {
	merged := DefaultDictionary()
	for attr, vals := range extra {
		merged[attr] = append(merged[attr], vals...)
	}
	d := &Dictionary{}
	seen := map[string]bool{}
	for attr, vals := range merged {
		for _, v := range vals {
			v = Normalize(v)
			if v == "" || seen[attr+"\x00"+v] {
				continue
			}
			seen[attr+"\x00"+v] = true
			d.terms = append(d.terms, term{attr: attr, value: v, re: regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\b`)})
		}
	}
	sort.Slice(d.terms, func(i, j int) bool {
		if len(d.terms[i].value) != len(d.terms[j].value) {
			return len(d.terms[i].value) > len(d.terms[j].value)
		}
		return d.terms[i].attr+d.terms[i].value < d.terms[j].attr+d.terms[j].value
	})
	return d
}

// Extract returns attribute values found in text. A longer term masks the shorter terms it
// contains ("stainless steel" does not also yield "steel").
func (d *Dictionary) Extract(text string) map[string][]string {
	out := map[string][]string{}
	masked := []byte(text)
	for _, t := range d.terms {
		locs := t.re.FindAllIndex(masked, -1)
		if len(locs) == 0 {
			continue
		}
		out[t.attr] = append(out[t.attr], t.value)
		for _, l := range locs {
			for i := l[0]; i < l[1]; i++ {
				masked[i] = '\x00'
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
