package pgvector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// Connect returns a lazy connection pool for a tenant's PostgreSQL endpoint
// (postgres://user@host:5432/db?sslmode=verify-full). TLS is required unless plaintext is set
// (platform-trusted hosts only); dial is the egress dialer, and host names are passed through
// to it unresolved so it can check the allowlist and every resolved address.
func Connect(ctx context.Context, endpoint, password string, plaintext bool, dial func(context.Context, string, string) (net.Conn, error)) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		return nil, fmt.Errorf("pgvector endpoint: %w", err)
	}
	cc := cfg.ConnConfig
	if !plaintext {
		if cc.TLSConfig == nil {
			return nil, errors.New("pgvector endpoint must use TLS (sslmode=require, verify-ca or verify-full)")
		}
		var tlsOnly []*pgconn.FallbackConfig
		for _, fb := range cc.Fallbacks {
			if fb.TLSConfig != nil {
				tlsOnly = append(tlsOnly, fb)
			}
		}
		cc.Fallbacks = tlsOnly
	}
	if password != "" {
		cc.Password = password
	}
	cc.DialFunc = dial
	cc.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgvector pool: %w", err)
	}
	return pool, nil
}

// Store is a tenant's pgvector table: a text primary-key column for the product ID, one
// `vector(d)` column per declared vector and one column per stored payload field. Filters are
// translated to a SQL WHERE clause on those columns; a filter on a field without a column
// cannot be pushed down and fails the query.
type Store struct {
	pool  *pgxpool.Pool
	spec  vectorstore.Spec
	table pgx.Identifier

	mu   sync.Mutex
	cols map[string]column
}

type column struct {
	dataType string
	udt      string
}

var _ pipeline.VectorStore = (*Store)(nil)

// New returns the store; spec.Collection is the table, optionally schema-qualified.
func New(pool *pgxpool.Pool, spec vectorstore.Spec) *Store {
	return &Store{pool: pool, spec: spec, table: pgx.Identifier(strings.Split(spec.Collection, "."))}
}

// Close closes the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// ProviderName labels the store in traces and breaker keys.
func (s *Store) ProviderName() string { return "byo:pgvector" }

// Metrics are the pgvector distance operators the adapter uses.
func (s *Store) Metrics() []string {
	return []string{vectorstore.MetricCosine, vectorstore.MetricDot, vectorstore.MetricL2}
}

func (s *Store) tableRef() (schema, name string) {
	if len(s.table) == 2 {
		return s.table[0], s.table[1]
	}
	return "", s.table[0]
}

// columns introspects the table once; an error is not cached, so a later call retries.
func (s *Store) columns(ctx context.Context) (map[string]column, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cols != nil {
		return s.cols, nil
	}
	schema, name := s.tableRef()
	rows, err := s.pool.Query(ctx, `SELECT column_name, data_type, udt_name FROM information_schema.columns
		WHERE table_name = $1 AND table_schema = COALESCE(NULLIF($2, ''), current_schema())`, name, schema)
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", s.spec.Collection, err)
	}
	cols := map[string]column{}
	for rows.Next() {
		var n string
		var c column
		if err := rows.Scan(&n, &c.dataType, &c.udt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("introspect %s: %w", s.spec.Collection, err)
		}
		cols[n] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("introspect %s: %w", s.spec.Collection, err)
	}
	if len(cols) > 0 {
		s.cols = cols
	}
	return cols, nil
}

func (s *Store) vectorColumn(name string) (vectorstore.VectorSpec, error) {
	v, ok := s.spec.Vector(name)
	if !ok {
		return v, fmt.Errorf("vector %q is not declared for %s: %w", name, s.spec.Collection, pipeline.ErrUnsupported)
	}
	return v, nil
}

// operator returns the pgvector distance operator and the score expression (higher is
// better: cosine similarity, inner product, or negated L2 distance).
func operator(metric, col, param string) (string, string) {
	switch metric {
	case vectorstore.MetricDot:
		return "<#>", "-(" + col + " <#> " + param + ")"
	case vectorstore.MetricL2:
		return "<->", "-(" + col + " <-> " + param + ")"
	}
	return "<=>", "1 - (" + col + " <=> " + param + ")"
}

// encodeVector renders a vector in pgvector's text format.
func encodeVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 10)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func decodeVector(s string) ([]float32, error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("vector value %q: %w", p, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

// rankingColumns returns the stored ranking fields that exist, with their canonical names.
func (s *Store) rankingColumns(cols map[string]column) (canonical, stored []string) {
	for _, f := range vectorstore.RankingFields {
		if c := s.spec.Field(f); hasColumn(cols, c) {
			canonical, stored = append(canonical, f), append(stored, c)
		}
	}
	return canonical, stored
}

func hasColumn(cols map[string]column, name string) bool {
	_, ok := cols[name]
	return ok
}

func quoted(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = pgx.Identifier{n}.Sanitize()
	}
	return strings.Join(q, ", ")
}

// Search runs a filtered kNN query ordered by the vector's distance operator.
func (s *Store) Search(ctx context.Context, req pipeline.VectorSearch) ([]pipeline.Hit, error) {
	if len(req.Query) == 0 || req.K <= 0 {
		return nil, nil
	}
	v, err := s.vectorColumn(req.Vector)
	if err != nil {
		return nil, err
	}
	cols, err := s.columns(ctx)
	if err != nil {
		return nil, err
	}
	canonical, stored := s.rankingColumns(cols)
	vec := pgx.Identifier{v.Name}.Sanitize()
	op, score := operator(v.Metric, vec, "$1::vector")
	args := []any{encodeVector(req.Query)}
	where, args, err := translate(req.Filter, s.spec, cols, args)
	if err != nil {
		return nil, err
	}
	if where != "" {
		where = " AND (" + where + ")"
	}
	args = append(args, req.K)
	q := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IS NOT NULL%s ORDER BY %s %s $1::vector LIMIT $%d",
		quoted(stored), score, s.table.Sanitize(), vec, where, vec, op, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search %s.%s: %w", s.spec.Collection, v.Name, err)
	}
	defer rows.Close()
	var hits []pipeline.Hit
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", s.spec.Collection, err)
		}
		m := make(map[string]any, len(canonical))
		for i, f := range canonical {
			m[f] = vals[i]
		}
		id, doc := vectorstore.DocFromMap(m)
		hits = append(hits, pipeline.Hit{ProductID: id, Score: vectorstore.AsFloat(vals[len(vals)-1]), Doc: doc})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search %s.%s: %w", s.spec.Collection, v.Name, err)
	}
	return hits, nil
}

// Get returns a product's stored vectors and ranking doc.
func (s *Store) Get(ctx context.Context, productID string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	cols, err := s.columns(ctx)
	if err != nil {
		return nil, nil, err
	}
	canonical, stored := s.rankingColumns(cols)
	var vecNames []string
	var vecExprs []string
	for _, name := range []string{pipeline.VectorText, pipeline.VectorImage} {
		if v, ok := s.spec.Vector(name); ok && hasColumn(cols, v.Name) {
			vecNames = append(vecNames, name)
			vecExprs = append(vecExprs, pgx.Identifier{v.Name}.Sanitize()+"::text")
		}
	}
	sel := quoted(stored)
	if len(vecExprs) > 0 {
		sel += ", " + strings.Join(vecExprs, ", ")
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1", sel, s.table.Sanitize(), pgx.Identifier{s.spec.Field(vectorstore.ProductIDField)}.Sanitize())
	rows, err := s.pool.Query(ctx, q, productID)
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", productID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("get %s: %w", productID, err)
		}
		return nil, nil, fmt.Errorf("product %s: %w", productID, pipeline.ErrNotFound)
	}
	vals, err := rows.Values()
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", productID, err)
	}
	m := map[string]any{}
	for i, f := range canonical {
		m[f] = vals[i]
	}
	_, doc := vectorstore.DocFromMap(m)
	p := &pipeline.VectorPoint{ProductID: productID, Vectors: map[string][]float32{}}
	for i, name := range vecNames {
		if str, ok := vals[len(canonical)+i].(string); ok {
			vec, err := decodeVector(str)
			if err != nil {
				return nil, nil, err
			}
			p.Vectors[name] = vec
		}
	}
	return p, doc, nil
}

// Upsert writes points in one statement, keyed by the product ID column. Payload fields
// without a column in the table are not stored.
func (s *Store) Upsert(ctx context.Context, points []pipeline.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	cols, err := s.columns(ctx)
	if err != nil {
		return err
	}
	pid := s.spec.Field(vectorstore.ProductIDField)
	if !hasColumn(cols, pid) {
		return fmt.Errorf("table %s has no product ID column %q", s.spec.Collection, pid)
	}
	type target struct{ canonical, column string }
	var vecTargets, fieldTargets []target
	for _, name := range []string{pipeline.VectorText, pipeline.VectorImage} {
		if v, ok := s.spec.Vector(name); ok && hasColumn(cols, v.Name) {
			vecTargets = append(vecTargets, target{name, v.Name})
		}
	}
	seen := map[string]bool{pid: true}
	for _, p := range points {
		for k := range p.Payload {
			if c := s.spec.Field(k); !seen[c] && hasColumn(cols, c) {
				seen[c] = true
				fieldTargets = append(fieldTargets, target{k, c})
			}
		}
	}
	slices.SortFunc(fieldTargets, func(a, b target) int { return strings.Compare(a.column, b.column) })
	names := []string{pid}
	for _, t := range vecTargets {
		names = append(names, t.column)
	}
	for _, t := range fieldTargets {
		names = append(names, t.column)
	}
	args := make([]any, 0, len(points)*len(names))
	tuples := make([]string, 0, len(points))
	for _, p := range points {
		ph := make([]string, 0, len(names))
		add := func(v any, cast string) {
			args = append(args, v)
			ph = append(ph, "$"+strconv.Itoa(len(args))+cast)
		}
		add(p.ProductID, "")
		for _, t := range vecTargets {
			if v, ok := p.Vectors[t.canonical]; ok && len(v) > 0 {
				add(encodeVector(v), "::vector")
			} else {
				add(nil, "::vector")
			}
		}
		for _, t := range fieldTargets {
			add(columnValue(p.Payload[t.canonical], cols[t.column]), "")
		}
		tuples = append(tuples, "("+strings.Join(ph, ", ")+")")
	}
	updates := make([]string, 0, len(names)-1)
	for _, n := range names[1:] {
		id := pgx.Identifier{n}.Sanitize()
		updates = append(updates, id+" = EXCLUDED."+id)
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s ON CONFLICT (%s) DO UPDATE SET %s",
		s.table.Sanitize(), quoted(names), strings.Join(tuples, ", "), pgx.Identifier{pid}.Sanitize(), strings.Join(updates, ", "))
	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("upsert into %s: %w", s.spec.Collection, err)
	}
	return nil
}

// columnValue converts a canonical payload value for its column: RFC 3339 strings to times
// for timestamp and date columns, and numbers to the column's numeric kind.
func columnValue(v any, c column) any {
	switch c.dataType {
	case "timestamp with time zone", "timestamp without time zone", "date":
		if s, ok := v.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	case "bigint", "integer", "smallint":
		switch v.(type) {
		case float64, float32, int32, int:
			return vectorstore.AsInt(v)
		}
	case "double precision", "real", "numeric":
		switch v.(type) {
		case int64, int32, int, float32:
			return vectorstore.AsFloat(v)
		}
	}
	return v
}

// Delete removes rows by product ID.
func (s *Store) Delete(ctx context.Context, productIDs []string) error {
	if len(productIDs) == 0 {
		return nil
	}
	q := fmt.Sprintf("DELETE FROM %s WHERE %s = ANY($1)", s.table.Sanitize(), pgx.Identifier{s.spec.Field(vectorstore.ProductIDField)}.Sanitize())
	if _, err := s.pool.Exec(ctx, q, productIDs); err != nil {
		return fmt.Errorf("delete from %s: %w", s.spec.Collection, err)
	}
	return nil
}

// Health pings the database.
func (s *Store) Health(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgvector health: %w", err)
	}
	return nil
}

// Count returns the number of rows (the version is ignored: BYO tables are written in place).
func (s *Store) Count(ctx context.Context, _ int) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+s.table.Sanitize()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", s.spec.Collection, err)
	}
	return n, nil
}

// ScrollVectors streams every row's vector and categories in product ID order (centroids).
func (s *Store) ScrollVectors(ctx context.Context, _ int, vector string, fn func(string, []float32, []string) error) error {
	v, err := s.vectorColumn(vector)
	if err != nil {
		return err
	}
	cols, err := s.columns(ctx)
	if err != nil {
		return err
	}
	pid := pgx.Identifier{s.spec.Field(vectorstore.ProductIDField)}.Sanitize()
	catExpr := "NULL::text[]"
	if c := s.spec.Field("category_ids"); hasColumn(cols, c) {
		catExpr = pgx.Identifier{c}.Sanitize()
	}
	vec := pgx.Identifier{v.Name}.Sanitize()
	after := ""
	for {
		q := fmt.Sprintf("SELECT %s, %s::text, %s FROM %s WHERE %s IS NOT NULL AND %s > $1 ORDER BY %s LIMIT 1000",
			pid, vec, catExpr, s.table.Sanitize(), vec, pid, pid)
		rows, err := s.pool.Query(ctx, q, after)
		if err != nil {
			return fmt.Errorf("scroll %s: %w", s.spec.Collection, err)
		}
		n := 0
		for rows.Next() {
			var id, raw string
			var cats []string
			if err := rows.Scan(&id, &raw, &cats); err != nil {
				rows.Close()
				return fmt.Errorf("scroll %s: %w", s.spec.Collection, err)
			}
			vec, err := decodeVector(raw)
			if err != nil {
				rows.Close()
				return err
			}
			if err := fn(id, vec, cats); err != nil {
				rows.Close()
				return err
			}
			after, n = id, n+1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("scroll %s: %w", s.spec.Collection, err)
		}
		if n < 1000 {
			return nil
		}
	}
}

// Describe reports the table's vector columns (dimension from the type modifier, metric from
// an ANN index's operator class) and its columns.
func (s *Store) Describe(ctx context.Context) (vectorstore.Info, error) {
	s.mu.Lock()
	s.cols = nil
	s.mu.Unlock()
	cols, err := s.columns(ctx)
	if err != nil {
		return vectorstore.Info{}, err
	}
	if len(cols) == 0 {
		return vectorstore.Info{}, nil
	}
	info := vectorstore.Info{Exists: true, Fields: map[string]bool{}, Vectors: map[string]vectorstore.VectorInfo{}}
	for n, c := range cols {
		if c.udt == "vector" {
			info.Vectors[n] = vectorstore.VectorInfo{}
		} else {
			info.Fields[n] = true
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT a.attname, a.atttypmod FROM pg_attribute a
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped AND format_type(a.atttypid, NULL) = 'vector'`,
		s.table.Sanitize())
	if err != nil {
		return vectorstore.Info{}, fmt.Errorf("describe %s: %w", s.spec.Collection, err)
	}
	for rows.Next() {
		var n string
		var dim int32
		if err := rows.Scan(&n, &dim); err != nil {
			rows.Close()
			return vectorstore.Info{}, fmt.Errorf("describe %s: %w", s.spec.Collection, err)
		}
		vi := info.Vectors[n]
		vi.Dim = int(dim)
		info.Vectors[n] = vi
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT a.attname, oc.opcname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		JOIN pg_opclass oc ON oc.oid = ANY(i.indclass)
		WHERE i.indrelid = $1::regclass AND oc.opcname LIKE 'vector_%_ops'`, s.table.Sanitize())
	if err != nil {
		return vectorstore.Info{}, fmt.Errorf("describe indexes of %s: %w", s.spec.Collection, err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, op string
		if err := rows.Scan(&n, &op); err != nil {
			return vectorstore.Info{}, fmt.Errorf("describe indexes of %s: %w", s.spec.Collection, err)
		}
		vi, ok := info.Vectors[n]
		if !ok {
			continue
		}
		switch op {
		case "vector_cosine_ops":
			vi.Metric = vectorstore.MetricCosine
		case "vector_ip_ops":
			vi.Metric = vectorstore.MetricDot
		case "vector_l2_ops":
			vi.Metric = vectorstore.MetricL2
		}
		info.Vectors[n] = vi
	}
	if err := rows.Err(); err != nil {
		return vectorstore.Info{}, fmt.Errorf("describe indexes of %s: %w", s.spec.Collection, err)
	}
	return info, nil
}
