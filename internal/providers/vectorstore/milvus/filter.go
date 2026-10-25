package milvus

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// translate renders a compiled filter as a Milvus boolean expression. Field names come from
// the payload mapping; a field that is neither in the schema nor storable in the dynamic field
// is an error, because filters are always pushed down. Dates compare as Unix seconds on Int64
// fields and as RFC 3339 strings otherwise.
func translate(e filter.Expr, spec vectorstore.Spec, sch *schemapb.CollectionSchema) (string, error) {
	if e.IsZero() {
		return "", nil
	}
	return expr(e, spec, sch)
}

func expr(e filter.Expr, spec vectorstore.Spec, sch *schemapb.CollectionSchema) (string, error) {
	switch {
	case e.And != nil || e.Or != nil:
		kids, sep := e.And, " and "
		if e.Or != nil {
			kids, sep = e.Or, " or "
		}
		parts := make([]string, 0, len(kids))
		for _, k := range kids {
			p, err := expr(k, spec, sch)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+p+")")
		}
		return strings.Join(parts, sep), nil
	case e.Cond != nil:
		return leaf(*e.Cond, spec, sch)
	}
	return "", fmt.Errorf("empty filter expression")
}

// literal quotes a string for a Milvus expression.
func literal(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func value(v any, f *schemapb.FieldSchema) (string, error) {
	switch x := v.(type) {
	case string:
		return literal(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	case time.Time:
		if f.GetDataType() == schemapb.DataType_Int64 {
			return strconv.FormatInt(x.Unix(), 10), nil
		}
		return literal(x.UTC().Format(time.RFC3339)), nil
	}
	return "", fmt.Errorf("unsupported filter value %T", v)
}

func list(vs []any, f *schemapb.FieldSchema) (string, error) {
	out := make([]string, len(vs))
	for i, v := range vs {
		s, err := value(v, f)
		if err != nil {
			return "", err
		}
		out[i] = s
	}
	return "[" + strings.Join(out, ", ") + "]", nil
}

var cmpOps = map[filter.Op]string{filter.Gt: ">", filter.Gte: ">=", filter.Lt: "<", filter.Lte: "<="}

func leaf(c filter.Condition, spec vectorstore.Spec, sch *schemapb.CollectionSchema) (string, error) {
	name := spec.Field(c.Field)
	if !stored(sch, name) {
		return "", fmt.Errorf("filter on %s: collection %s has no field %q, so it cannot be pushed down", c.Field, spec.Collection, name)
	}
	f := fieldOf(sch, name)
	array := c.Type == catalog.TypeKeywordArray
	switch c.Op {
	case filter.Eq, filter.Contains, filter.Ne:
		v, err := value(c.Value, f)
		if err != nil {
			return "", err
		}
		pos := name + " == " + v
		if array {
			pos = "array_contains(" + name + ", " + v + ")"
		}
		if c.Op == filter.Ne {
			return "not (" + pos + ")", nil
		}
		return pos, nil
	case filter.In, filter.Nin:
		l, err := list(c.Value.([]any), f)
		if err != nil {
			return "", err
		}
		pos := name + " in " + l
		if array {
			pos = "array_contains_any(" + name + ", " + l + ")"
		}
		if c.Op == filter.Nin {
			return "not (" + pos + ")", nil
		}
		return pos, nil
	case filter.Gt, filter.Gte, filter.Lt, filter.Lte:
		v, err := value(c.Value, f)
		if err != nil {
			return "", err
		}
		return name + " " + cmpOps[c.Op] + " " + v, nil
	case filter.Range:
		b := c.Value.(filter.Bounds)
		var parts []string
		for _, x := range []struct {
			op filter.Op
			v  any
		}{{filter.Gt, b.Gt}, {filter.Gte, b.Gte}, {filter.Lt, b.Lt}, {filter.Lte, b.Lte}} {
			if x.v == nil {
				continue
			}
			v, err := value(x.v, f)
			if err != nil {
				return "", err
			}
			parts = append(parts, name+" "+cmpOps[x.op]+" "+v)
		}
		if len(parts) == 0 {
			return "true", nil
		}
		return strings.Join(parts, " and "), nil
	}
	return "", fmt.Errorf("filter op %q is not supported by milvus", c.Op)
}
