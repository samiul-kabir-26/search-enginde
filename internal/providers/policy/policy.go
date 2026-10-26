package policy

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"
)

// Rule kinds.
const (
	KindFilter = "filter"
	KindBoost  = "boost"
)

// Limits (docs/TENANT_CONFIG.md §5 rule 7).
const (
	MaxRules     = 50
	MaxExprBytes = 2048
	// MaxCost bounds a rule's estimated worst-case cost (lists assumed up to MaxListSize long)
	// at compile time and its actual cost per evaluation at run time.
	MaxCost     = 2000
	MaxListSize = 64
)

// Rule is a CEL filter or boost rule.
type Rule struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Expr string `json:"expr"`
}

// Item fields a rule may read (`item.<field>`), with `item.vendor.<field>` and
// `item.custom.<name>` for vendor features and the tenant's custom fields.
var (
	ItemFields   = []string{"product_id", "vendor_id", "brand", "price_minor", "category_ids", "rating", "rating_count", "listing_completeness", "image_count", "created_at", "stock", "status", "ship_regions", "vendor", "custom"}
	VendorFields = []string{"score", "on_time_dispatch_rate", "dispute_rate", "response_hours", "verified"}
	QueryFields  = []string{"text", "type", "categories", "attributes"}
)

// Error is a compile error of one rule; Field is the rule field it concerns (expr, kind).
type Error struct {
	Index   int
	Field   string
	Code    string
	Message string
}

func (e Error) Error() string { return fmt.Sprintf("rule %d %s: %s", e.Index, e.Field, e.Message) }

type program struct {
	id   string
	kind string
	prg  cel.Program
}

// Set is a tenant's compiled rules, safe for concurrent use.
type Set struct {
	filters []program
	boosts  []program
}

// Len is the number of rules.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.filters) + len(s.boosts)
}

func newEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("item", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("query", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	return env, nil
}

// Compile type-checks every rule in the restricted environment (variables `item` and `query`,
// the standard library only), rejects unknown fields, wrong result types and rules whose
// estimated cost exceeds MaxCost, and returns all errors. customFields are the tenant's custom
// field names (without the `custom.` prefix).
func Compile(rules []Rule, customFields []string) (*Set, []Error) {
	var errs []Error
	if len(rules) > MaxRules {
		errs = append(errs, Error{Index: -1, Code: "too_many", Message: fmt.Sprintf("at most %d rules", MaxRules)})
	}
	env, err := newEnv()
	if err != nil {
		return nil, []Error{{Index: -1, Code: "internal", Message: err.Error()}}
	}
	set := &Set{}
	ids := map[string]bool{}
	for i, r := range rules {
		fail := func(field, code, format string, args ...any) {
			errs = append(errs, Error{Index: i, Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
		}
		if r.ID == "" || ids[r.ID] {
			fail("id", "invalid", "rule ids must be non-empty and unique")
		}
		ids[r.ID] = true
		if r.Kind != KindFilter && r.Kind != KindBoost {
			fail("kind", "invalid", "kind must be filter or boost")
			continue
		}
		if len(r.Expr) == 0 || len(r.Expr) > MaxExprBytes {
			fail("expr", "invalid", "expr must be 1 to %d bytes", MaxExprBytes)
			continue
		}
		checked, iss := env.Compile(r.Expr)
		if iss.Err() != nil {
			fail("expr", "cel_compile", "%s", strings.TrimSpace(iss.Err().Error()))
			continue
		}
		if bad := unknownFields(checked, customFields); len(bad) > 0 {
			fail("expr", "unknown_field", "unknown field %s", strings.Join(bad, ", "))
			continue
		}
		out := checked.OutputType()
		switch {
		case r.Kind == KindFilter && !out.IsExactType(types.BoolType) && !out.IsExactType(types.DynType):
			fail("expr", "cel_type", "filter rules must return bool, not %s", out)
			continue
		case r.Kind == KindBoost && !out.IsExactType(types.DoubleType) && !out.IsExactType(types.IntType) && !out.IsExactType(types.DynType):
			fail("expr", "cel_type", "boost rules must return a number, not %s", out)
			continue
		}
		est, err := env.EstimateCost(checked, sizes{})
		if err != nil {
			fail("expr", "cel_cost", "cost estimate: %v", err)
			continue
		}
		if est.Max > MaxCost {
			fail("expr", "cel_cost", "estimated cost %d exceeds the limit %d", est.Max, MaxCost)
			continue
		}
		prg, err := env.Program(checked, cel.CostLimit(MaxCost))
		if err != nil {
			fail("expr", "cel_compile", "%v", err)
			continue
		}
		p := program{id: r.ID, kind: r.Kind, prg: prg}
		if r.Kind == KindFilter {
			set.filters = append(set.filters, p)
		} else {
			set.boosts = append(set.boosts, p)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return set, nil
}

// unknownFields lists `item.x`, `item.vendor.x`, `item.custom.x` and `query.x` selections that
// name no known field, so typos fail at publish instead of erroring on every candidate.
func unknownFields(checked *cel.Ast, custom []string) []string {
	var bad []string
	ast.PreOrderVisit(checked.NativeRep().Expr(), ast.NewExprVisitor(func(e ast.Expr) {
		if e.Kind() != ast.SelectKind {
			return
		}
		sel := e.AsSelect()
		field, operand := sel.FieldName(), sel.Operand()
		var known []string
		var path string
		switch {
		case isIdent(operand, "item"):
			known, path = ItemFields, "item."
		case isIdent(operand, "query"):
			known, path = QueryFields, "query."
		case isSelectOf(operand, "item", "vendor"):
			known, path = VendorFields, "item.vendor."
		case isSelectOf(operand, "item", "custom"):
			known, path = custom, "item.custom."
		default:
			return
		}
		if !slices.Contains(known, field) {
			bad = append(bad, path+field)
		}
	}))
	sort.Strings(bad)
	return slices.Compact(bad)
}

func isIdent(e ast.Expr, name string) bool { return e.Kind() == ast.IdentKind && e.AsIdent() == name }

func isSelectOf(e ast.Expr, ident, field string) bool {
	return e.Kind() == ast.SelectKind && e.AsSelect().FieldName() == field && isIdent(e.AsSelect().Operand(), ident)
}

// sizes bounds list and string sizes for cost estimation.
type sizes struct{}

func (sizes) EstimateSize(checker.AstNode) *checker.SizeEstimate {
	return &checker.SizeEstimate{Min: 0, Max: MaxListSize}
}

func (sizes) EstimateCallCost(string, string, *checker.AstNode, []checker.AstNode) *checker.CallEstimate {
	return nil
}

// vars are one evaluation's activation: `item` and the request's `query`.
type vars struct {
	item  map[string]any
	query map[string]any
}

// ResolveName implements interpreter.Activation.
func (v *vars) ResolveName(name string) (any, bool) {
	switch name {
	case "item":
		return v.item, v.item != nil
	case "query":
		return v.query, v.query != nil
	}
	return nil, false
}

// Parent implements interpreter.Activation.
func (*vars) Parent() interpreter.Activation { return nil }

// Eval runs every rule against one item: keep is false when a filter rule returned false;
// boost is the sum of boost rules clamped to [-1, 1]; errs counts rules that failed to evaluate
// (missing field, cost limit, wrong type), where a failing filter keeps the item and a failing
// boost adds nothing, so a broken rule never empties results.
func (s *Set) Eval(item, query map[string]any) (keep bool, boost float64, errs int) {
	if s == nil {
		return true, 0, 0
	}
	v := &vars{item: item, query: query}
	for _, p := range s.filters {
		val, _, err := p.prg.Eval(v)
		if err != nil {
			errs++
			continue
		}
		if b, ok := val.(types.Bool); !ok {
			errs++
		} else if !bool(b) {
			return false, 0, errs
		}
	}
	for _, p := range s.boosts {
		val, _, err := p.prg.Eval(v)
		if err != nil {
			errs++
			continue
		}
		x, ok := number(val)
		if !ok {
			errs++
			continue
		}
		boost += x
	}
	return true, math.Max(-1, math.Min(1, boost)), errs
}

func number(v ref.Val) (float64, bool) {
	switch x := v.(type) {
	case types.Double:
		f := float64(x)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case types.Int:
		return float64(x), true
	case types.Uint:
		return float64(x), true
	}
	return 0, false
}
