// Package filter is the structured filter AST (`FilterExpr` in docs/API.md): conditions on
// filterable fields combined with and/or. Compile validates an expression against a tenant's
// field registry and normalizes values to the field types; each store adapter translates the
// compiled form into its native filter, because post-filtering structured constraints is a bug.
package filter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/platform"
)

// Op is a condition operator.
type Op string

// Operators (docs/API.md §1).
const (
	Eq       Op = "eq"
	Ne       Op = "ne"
	In       Op = "in"
	Nin      Op = "nin"
	Gt       Op = "gt"
	Gte      Op = "gte"
	Lt       Op = "lt"
	Lte      Op = "lte"
	Contains Op = "contains"
	Range    Op = "range"
)

// Limits on a single expression.
const (
	MaxDepth      = 3
	MaxConditions = 64
	MaxListValues = 1000
)

// Expr is a filter expression: exactly one of And, Or or Cond is set. The zero Expr matches
// everything.
type Expr struct {
	And  []Expr
	Or   []Expr
	Cond *Condition
}

// Condition constrains one field. After Compile, Value holds normalized values: string, int64,
// float64, bool or time.Time; []any for in/nin; Bounds for range.
type Condition struct {
	Field string
	Op    Op
	Value any
	// Type is filled in by Compile from the registry.
	Type catalog.FieldType
}

// Bounds is the value of a range condition; nil ends are open.
type Bounds struct {
	Gt, Gte, Lt, Lte any
}

// IsZero reports whether e matches everything.
func (e Expr) IsZero() bool { return e.Cond == nil && len(e.And) == 0 && len(e.Or) == 0 }

// All combines expressions with AND, dropping empty ones.
func All(exprs ...Expr) Expr {
	var parts []Expr
	for _, e := range exprs {
		if !e.IsZero() {
			parts = append(parts, e)
		}
	}
	switch len(parts) {
	case 0:
		return Expr{}
	case 1:
		return parts[0]
	}
	return Expr{And: parts}
}

type wire struct {
	And   []Expr          `json:"and,omitempty"`
	Or    []Expr          `json:"or,omitempty"`
	Field string          `json:"field,omitempty"`
	Op    Op              `json:"op,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// UnmarshalJSON accepts {field, op, value}, {and: [...]} or {or: [...]}.
func (e *Expr) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*e = Expr{}
		return nil
	}
	var w wire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	set := 0
	if w.And != nil {
		set++
	}
	if w.Or != nil {
		set++
	}
	if w.Field != "" || w.Op != "" {
		set++
	}
	if set != 1 {
		return errors.New("filter: an expression needs exactly one of and, or, or field/op/value")
	}
	switch {
	case w.And != nil:
		*e = Expr{And: w.And}
	case w.Or != nil:
		*e = Expr{Or: w.Or}
	default:
		var v any
		if len(w.Value) > 0 {
			d := json.NewDecoder(bytes.NewReader(w.Value))
			d.UseNumber()
			if err := d.Decode(&v); err != nil {
				return fmt.Errorf("filter value: %w", err)
			}
		}
		*e = Expr{Cond: &Condition{Field: w.Field, Op: w.Op, Value: v}}
	}
	return nil
}

// MarshalJSON writes the wire form; compiled values marshal naturally.
func (e Expr) MarshalJSON() ([]byte, error) {
	var out any
	switch {
	case e.Cond != nil:
		v := e.Cond.Value
		if b, ok := v.(Bounds); ok {
			m := map[string]any{}
			for k, x := range map[string]any{"gt": b.Gt, "gte": b.Gte, "lt": b.Lt, "lte": b.Lte} {
				if x != nil {
					m[k] = x
				}
			}
			v = m
		}
		out = map[string]any{"field": e.Cond.Field, "op": e.Cond.Op, "value": v}
	case e.And != nil:
		out = map[string]any{"and": e.And}
	case e.Or != nil:
		out = map[string]any{"or": e.Or}
	default:
		return []byte("null"), nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("filter: %w", err)
	}
	return b, nil
}

// Compile validates e against the registry and returns it with normalized values. Every error
// is reported, with JSON Pointer paths under base.
func Compile(e Expr, r *catalog.Registry, base string) (Expr, []platform.FieldError) {
	c := compiler{r: r}
	out := c.expr(e, base, 0)
	if c.conds > MaxConditions {
		c.errs = append(c.errs, platform.FieldError{Path: base, Code: "too_many_conditions", Message: fmt.Sprintf("at most %d conditions", MaxConditions)})
	}
	return out, c.errs
}

type compiler struct {
	r     *catalog.Registry
	errs  []platform.FieldError
	conds int
}

func (c *compiler) fail(path, code, msg string) {
	c.errs = append(c.errs, platform.FieldError{Path: path, Code: code, Message: msg})
}

func (c *compiler) expr(e Expr, path string, depth int) Expr {
	switch {
	case e.Cond != nil:
		c.conds++
		return Expr{Cond: c.cond(*e.Cond, path)}
	case e.And != nil || e.Or != nil:
		if depth >= MaxDepth {
			c.fail(path, "too_deep", fmt.Sprintf("and/or nesting is limited to %d levels", MaxDepth))
			return Expr{}
		}
		kids, key := e.And, "and"
		if e.Or != nil {
			kids, key = e.Or, "or"
		}
		if len(kids) == 0 {
			c.fail(path+"/"+key, "empty", key+" needs at least one expression")
		}
		out := make([]Expr, len(kids))
		for i, k := range kids {
			out[i] = c.expr(k, fmt.Sprintf("%s/%s/%d", path, key, i), depth+1)
		}
		if key == "and" {
			return Expr{And: out}
		}
		return Expr{Or: out}
	}
	return Expr{}
}

func (c *compiler) cond(in Condition, path string) *Condition {
	f, ok := c.r.Field(in.Field)
	if !ok {
		c.fail(path+"/field", "unknown_field", fmt.Sprintf("%q is not a field", in.Field))
		return &in
	}
	if !f.Filterable {
		c.fail(path+"/field", "not_filterable", fmt.Sprintf("%q is not filterable", in.Field))
		return &in
	}
	out := Condition{Field: f.Name, Op: in.Op, Type: f.Type}
	if !opAllowed(f.Type, in.Op) {
		c.fail(path+"/op", "unsupported_op", fmt.Sprintf("%q is not supported on %s field %q", in.Op, f.Type, f.Name))
		return &out
	}
	vpath := path + "/value"
	switch in.Op {
	case In, Nin:
		list, ok := in.Value.([]any)
		if !ok || len(list) == 0 {
			c.fail(vpath, "invalid_value", string(in.Op)+" needs a non-empty array")
			return &out
		}
		if len(list) > MaxListValues {
			c.fail(vpath, "too_many_values", fmt.Sprintf("at most %d values", MaxListValues))
			return &out
		}
		vals := make([]any, len(list))
		for i, x := range list {
			v, err := scalar(f.Type, x)
			if err != nil {
				c.fail(fmt.Sprintf("%s/%d", vpath, i), "invalid_value", err.Error())
				continue
			}
			vals[i] = v
		}
		out.Value = vals
	case Range:
		m, ok := in.Value.(map[string]any)
		if !ok || len(m) == 0 {
			c.fail(vpath, "invalid_value", "range needs an object with gt, gte, lt and/or lte")
			return &out
		}
		var b Bounds
		for k, x := range m {
			v, err := scalar(f.Type, x)
			if err != nil {
				c.fail(vpath+"/"+k, "invalid_value", err.Error())
				continue
			}
			switch k {
			case "gt":
				b.Gt = v
			case "gte":
				b.Gte = v
			case "lt":
				b.Lt = v
			case "lte":
				b.Lte = v
			default:
				c.fail(vpath+"/"+k, "invalid_value", "range keys are gt, gte, lt and lte")
			}
		}
		out.Value = b
	default:
		v, err := scalar(f.Type, in.Value)
		if err != nil {
			c.fail(vpath, "invalid_value", err.Error())
			return &out
		}
		out.Value = v
	}
	return &out
}

func opAllowed(t catalog.FieldType, op Op) bool {
	switch t {
	case catalog.TypeKeyword:
		return op == Eq || op == Ne || op == In || op == Nin
	case catalog.TypeKeywordArray:
		return op == Eq || op == Ne || op == In || op == Nin || op == Contains
	case catalog.TypeInt, catalog.TypeFloat, catalog.TypeDate:
		return op != Contains
	case catalog.TypeBool:
		return op == Eq || op == Ne
	}
	return false
}

// scalar converts one JSON value (decoded with UseNumber) to the field's Go type.
func scalar(t catalog.FieldType, x any) (any, error) {
	switch t {
	case catalog.TypeKeyword, catalog.TypeKeywordArray:
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("want a string, got %s", kind(x))
		}
		return s, nil
	case catalog.TypeInt:
		n, err := number(x)
		if err != nil {
			return nil, err
		}
		if n != math.Trunc(n) || math.Abs(n) > 1<<53 {
			return nil, fmt.Errorf("want an integer, got %v", n)
		}
		return int64(n), nil
	case catalog.TypeFloat:
		return number(x)
	case catalog.TypeBool:
		b, ok := x.(bool)
		if !ok {
			return nil, fmt.Errorf("want a boolean, got %s", kind(x))
		}
		return b, nil
	case catalog.TypeDate:
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("want an RFC 3339 date string, got %s", kind(x))
		}
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("want an RFC 3339 date: %w", err)
		}
		return ts.UTC(), nil
	}
	return nil, fmt.Errorf("unsupported field type %s", t)
}

func number(x any) (float64, error) {
	switch v := x.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return 0, fmt.Errorf("want a number: %w", err)
		}
		return f, nil
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	}
	return 0, fmt.Errorf("want a number, got %s", kind(x))
}

func kind(x any) string {
	switch x.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64, int, int64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", x)
}

// SplitByField separates the top-level AND conditions of e that constrain a single field from
// everything else. Disjunctive facets drop the conditions on their own field.
func SplitByField(e Expr) (byField map[string][]Expr, rest []Expr) {
	byField = map[string][]Expr{}
	var parts []Expr
	switch {
	case e.Cond != nil:
		parts = []Expr{e}
	case e.And != nil:
		parts = e.And
	case e.Or != nil:
		return byField, []Expr{e}
	}
	for _, p := range parts {
		if p.Cond != nil {
			byField[p.Cond.Field] = append(byField[p.Cond.Field], p)
		} else if !p.IsZero() {
			rest = append(rest, p)
		}
	}
	return byField, rest
}
