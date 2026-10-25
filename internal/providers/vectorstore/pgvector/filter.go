package pgvector

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// translate renders a compiled filter as a SQL boolean expression over the table's columns,
// appending its parameters to args (placeholders continue from len(args)). Negations treat a
// NULL column as not matching the positive condition, like the lexical index treats a missing
// field. A field without a column is an error: filters are always pushed down.
func translate(e filter.Expr, spec vectorstore.Spec, cols map[string]column, args []any) (string, []any, error) {
	if e.IsZero() {
		return "", args, nil
	}
	t := &translator{spec: spec, cols: cols, args: args}
	sql, err := t.expr(e)
	return sql, t.args, err
}

type translator struct {
	spec vectorstore.Spec
	cols map[string]column
	args []any
}

func (t *translator) param(v any, cast string) string {
	t.args = append(t.args, v)
	return "$" + strconv.Itoa(len(t.args)) + cast
}

func (t *translator) expr(e filter.Expr) (string, error) {
	switch {
	case e.And != nil || e.Or != nil:
		kids, sep := e.And, " AND "
		if e.Or != nil {
			kids, sep = e.Or, " OR "
		}
		parts := make([]string, 0, len(kids))
		for _, k := range kids {
			p, err := t.expr(k)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+p+")")
		}
		return strings.Join(parts, sep), nil
	case e.Cond != nil:
		return t.leaf(*e.Cond)
	}
	return "", fmt.Errorf("empty filter expression")
}

func not(positive string) string { return "NOT COALESCE((" + positive + "), false)" }

func (t *translator) leaf(c filter.Condition) (string, error) {
	name := t.spec.Field(c.Field)
	if _, ok := t.cols[name]; !ok {
		return "", fmt.Errorf("filter on %s: table %s has no column %q, so it cannot be pushed down", c.Field, t.spec.Collection, name)
	}
	col := pgx.Identifier{name}.Sanitize()
	array := c.Type == catalog.TypeKeywordArray
	switch c.Op {
	case filter.Eq, filter.Contains:
		if array {
			return t.param(c.Value, "::text") + " = ANY(" + col + ")", nil
		}
		return col + " = " + t.param(c.Value, ""), nil
	case filter.Ne:
		if array {
			return not(t.param(c.Value, "::text") + " = ANY(" + col + ")"), nil
		}
		return col + " IS DISTINCT FROM " + t.param(c.Value, ""), nil
	case filter.In, filter.Nin:
		pos, err := t.in(col, c, array)
		if err != nil {
			return "", err
		}
		if c.Op == filter.Nin {
			return not(pos), nil
		}
		return pos, nil
	case filter.Gt, filter.Gte, filter.Lt, filter.Lte:
		return t.compare(col, c.Op, c.Value), nil
	case filter.Range:
		b := c.Value.(filter.Bounds)
		var parts []string
		for _, x := range []struct {
			op filter.Op
			v  any
		}{{filter.Gt, b.Gt}, {filter.Gte, b.Gte}, {filter.Lt, b.Lt}, {filter.Lte, b.Lte}} {
			if x.v != nil {
				parts = append(parts, t.compare(col, x.op, x.v))
			}
		}
		if len(parts) == 0 {
			return "TRUE", nil
		}
		return strings.Join(parts, " AND "), nil
	}
	return "", fmt.Errorf("filter op %q is not supported by pgvector", c.Op)
}

var sqlOps = map[filter.Op]string{filter.Gt: ">", filter.Gte: ">=", filter.Lt: "<", filter.Lte: "<="}

func (t *translator) compare(col string, op filter.Op, v any) string {
	return col + " " + sqlOps[op] + " " + t.param(v, "")
}

func (t *translator) in(col string, c filter.Condition, array bool) (string, error) {
	vals := c.Value.([]any)
	switch c.Type {
	case catalog.TypeKeyword, catalog.TypeKeywordArray:
		ss := make([]string, len(vals))
		for i, v := range vals {
			ss[i] = v.(string)
		}
		if array {
			return col + " && " + t.param(ss, "::text[]"), nil
		}
		return col + " = ANY(" + t.param(ss, "::text[]") + ")", nil
	case catalog.TypeInt:
		ns := make([]int64, len(vals))
		for i, v := range vals {
			ns[i] = v.(int64)
		}
		return col + " = ANY(" + t.param(ns, "::bigint[]") + ")", nil
	case catalog.TypeFloat:
		fs := make([]float64, len(vals))
		for i, v := range vals {
			fs[i] = v.(float64)
		}
		return col + " = ANY(" + t.param(fs, "::float8[]") + ")", nil
	}
	return "", fmt.Errorf("field %s: type %s does not support in/nin", c.Field, c.Type)
}
