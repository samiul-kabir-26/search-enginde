package milvus

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// Config is a tenant's Milvus endpoint.
type Config struct {
	// Endpoint is https://host[:19530] (http only for platform-trusted hosts, see Plaintext).
	Endpoint string
	// Username and Password, or APIKey (token auth, as on Zilliz Cloud).
	Username, Password, APIKey string
	Database                   string
	Plaintext                  bool
	// Dial is the egress dialer.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
}

// Store is a tenant's Milvus collection, spoken to through Milvus's gRPC API
// (milvus-proto). The connection is lazy (gRPC connects on the first call, under that call's
// deadline) and the collection schema is described once and cached, so building the provider
// never blocks. Filters become Milvus boolean expressions over the collection's scalar fields,
// or its dynamic field when enabled.
type Store struct {
	conn *grpc.ClientConn
	svc  milvuspb.MilvusServiceClient
	md   metadata.MD
	spec vectorstore.Spec

	mu     sync.Mutex
	schema *schemapb.CollectionSchema
}

var _ pipeline.VectorStore = (*Store)(nil)

// New validates cfg and returns the store without connecting.
func New(cfg Config, spec vectorstore.Spec) (*Store, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("milvus endpoint %q is not a URL", cfg.Endpoint)
	}
	if u.Scheme != "https" && !cfg.Plaintext {
		return nil, fmt.Errorf("milvus endpoint %q must be https", cfg.Endpoint)
	}
	port := u.Port()
	if port == "" {
		port = "19530"
	}
	creds := insecure.NewCredentials()
	if u.Scheme == "https" {
		creds = credentials.NewTLS(&tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(u.Hostname(), port),
		grpc.WithTransportCredentials(creds), grpc.WithContextDialer(cfg.Dial),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		return nil, fmt.Errorf("milvus client: %w", err)
	}
	md := metadata.MD{}
	switch {
	case cfg.APIKey != "":
		md.Set("authorization", base64.StdEncoding.EncodeToString([]byte(cfg.APIKey)))
	case cfg.Username != "" || cfg.Password != "":
		md.Set("authorization", base64.StdEncoding.EncodeToString([]byte(cfg.Username+":"+cfg.Password)))
	}
	if cfg.Database != "" {
		md.Set("dbname", cfg.Database)
	}
	return &Store{conn: conn, svc: milvuspb.NewMilvusServiceClient(conn), md: md, spec: spec}, nil
}

func (s *Store) ctx(ctx context.Context) context.Context {
	if len(s.md) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, s.md)
}

// statusErr turns a Milvus status into an error.
func statusErr(op string, st *commonpb.Status, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if st.GetCode() != 0 || st.GetErrorCode() != commonpb.ErrorCode_Success { //nolint:staticcheck // servers before 2.3 set only error_code
		return fmt.Errorf("%s: milvus code %d: %s", op, st.GetCode(), st.GetReason())
	}
	return nil
}

// errCollectionMissing is returned by describe when the collection does not exist.
var errCollectionMissing = errors.New("collection does not exist")

// collection returns the cached schema, describing the collection on first use.
func (s *Store) collection(ctx context.Context) (*schemapb.CollectionSchema, error) {
	s.mu.Lock()
	sch := s.schema
	s.mu.Unlock()
	if sch != nil {
		return sch, nil
	}
	resp, err := s.svc.DescribeCollection(s.ctx(ctx), &milvuspb.DescribeCollectionRequest{CollectionName: s.spec.Collection})
	if err := statusErr("describe "+s.spec.Collection, resp.GetStatus(), err); err != nil {
		if strings.Contains(strings.ToLower(resp.GetStatus().GetReason()), "not found") || strings.Contains(strings.ToLower(resp.GetStatus().GetReason()), "not exist") {
			return nil, fmt.Errorf("%s: %w", s.spec.Collection, errCollectionMissing)
		}
		return nil, err
	}
	s.mu.Lock()
	s.schema = resp.GetSchema()
	s.mu.Unlock()
	return resp.GetSchema(), nil
}

// Close closes the connection.
func (s *Store) Close() error {
	if err := s.conn.Close(); err != nil {
		return fmt.Errorf("close milvus connection: %w", err)
	}
	return nil
}

// ProviderName labels the store in traces and breaker keys.
func (s *Store) ProviderName() string { return "byo:milvus" }

// Metrics are the Milvus metric types the adapter maps.
func (s *Store) Metrics() []string {
	return []string{vectorstore.MetricCosine, vectorstore.MetricDot, vectorstore.MetricL2}
}

func fieldOf(sch *schemapb.CollectionSchema, name string) *schemapb.FieldSchema {
	for _, f := range sch.GetFields() {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

// stored reports whether a field can be written and read: a schema field, or any field when
// the dynamic field is enabled.
func stored(sch *schemapb.CollectionSchema, name string) bool {
	return fieldOf(sch, name) != nil || sch.GetEnableDynamicField()
}

func param(kvs []*commonpb.KeyValuePair, key string) string {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return ""
}

func (s *Store) vectorField(name string) (vectorstore.VectorSpec, error) {
	v, ok := s.spec.Vector(name)
	if !ok {
		return v, fmt.Errorf("vector %q is not declared for %s: %w", name, s.spec.Collection, pipeline.ErrUnsupported)
	}
	return v, nil
}

// outputFields are the stored names of the ranking fields, with their canonical names.
func (s *Store) outputFields(sch *schemapb.CollectionSchema) (canonical, names []string) {
	for _, f := range vectorstore.RankingFields {
		if n := s.spec.Field(f); stored(sch, n) {
			canonical, names = append(canonical, f), append(names, n)
		}
	}
	return canonical, names
}

// Search runs a filtered kNN query on one vector field. Scores are higher-is-better: cosine
// similarity, inner product, or negated L2 distance.
func (s *Store) Search(ctx context.Context, req pipeline.VectorSearch) ([]pipeline.Hit, error) {
	if len(req.Query) == 0 || req.K <= 0 {
		return nil, nil
	}
	v, err := s.vectorField(req.Vector)
	if err != nil {
		return nil, err
	}
	sch, err := s.collection(ctx)
	if err != nil {
		return nil, err
	}
	expr, err := translate(req.Filter, s.spec, sch)
	if err != nil {
		return nil, err
	}
	pg, err := proto.Marshal(&commonpb.PlaceholderGroup{Placeholders: []*commonpb.PlaceholderValue{{
		Tag: "$0", Type: commonpb.PlaceholderType_FloatVector, Values: [][]byte{floatBytes(req.Query)},
	}}})
	if err != nil {
		return nil, fmt.Errorf("encode query vector: %w", err)
	}
	canonical, names := s.outputFields(sch)
	resp, err := s.svc.Search(s.ctx(ctx), &milvuspb.SearchRequest{
		CollectionName: s.spec.Collection, Dsl: expr, DslType: commonpb.DslType_BoolExprV1,
		SearchInput: &milvuspb.SearchRequest_PlaceholderGroup{PlaceholderGroup: pg}, OutputFields: names, Nq: 1,
		SearchParams: []*commonpb.KeyValuePair{
			{Key: "anns_field", Value: v.Name}, {Key: "topk", Value: strconv.Itoa(req.K)},
			{Key: "params", Value: "{}"}, {Key: "round_decimal", Value: "-1"},
		},
		ConsistencyLevel: commonpb.ConsistencyLevel_Bounded,
	})
	if err := statusErr("search "+s.spec.Collection+"."+v.Name, resp.GetStatus(), err); err != nil {
		return nil, err
	}
	r := resp.GetResults()
	ids := idStrings(r.GetIds())
	rows, err := columns(r.GetFieldsData(), len(ids))
	if err != nil {
		return nil, err
	}
	hits := make([]pipeline.Hit, 0, len(ids))
	for i := range ids {
		m := s.canonicalRow(rows[i], canonical, names)
		if _, ok := m[vectorstore.ProductIDField]; !ok {
			m[vectorstore.ProductIDField] = ids[i]
		}
		id, doc := vectorstore.DocFromMap(m)
		score := float64(r.GetScores()[i])
		if v.Metric == vectorstore.MetricL2 {
			score = -score
		}
		hits = append(hits, pipeline.Hit{ProductID: id, Score: score, Doc: doc})
	}
	return hits, nil
}

func (s *Store) canonicalRow(row map[string]any, canonical, names []string) map[string]any {
	m := make(map[string]any, len(canonical))
	for j, n := range names {
		if v, ok := row[n]; ok {
			m[canonical[j]] = v
		}
	}
	return m
}

func floatBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func idStrings(ids *schemapb.IDs) []string {
	if s := ids.GetStrId(); s != nil {
		return s.GetData()
	}
	n := ids.GetIntId().GetData()
	out := make([]string, len(n))
	for i, x := range n {
		out[i] = strconv.FormatInt(x, 10)
	}
	return out
}

// columns decodes column-based field data into one map per row; dynamic-field ($meta) JSON is
// flattened into the row.
func columns(fields []*schemapb.FieldData, n int) ([]map[string]any, error) {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{}
	}
	for _, fd := range fields {
		vals, err := fieldValues(fd)
		if err != nil {
			return nil, err
		}
		for i, v := range vals {
			if i >= n {
				break
			}
			if fd.GetIsDynamic() {
				if b, ok := v.([]byte); ok {
					var dyn map[string]any
					if json.Unmarshal(b, &dyn) == nil {
						for k, x := range dyn {
							rows[i][k] = x
						}
					}
				}
				continue
			}
			if v != nil {
				rows[i][fd.GetFieldName()] = v
			}
		}
	}
	return rows, nil
}

// fieldValues decodes one column; null entries (valid_data false) are nil.
func fieldValues(fd *schemapb.FieldData) ([]any, error) {
	var out []any
	if vf := fd.GetVectors(); vf != nil {
		data, dim := vf.GetFloatVector().GetData(), int(vf.GetDim())
		if dim > 0 {
			for i := 0; i+dim <= len(data); i += dim {
				out = append(out, data[i:i+dim:i+dim])
			}
		}
		return out, nil
	}
	sf := fd.GetScalars()
	switch {
	case sf.GetStringData() != nil:
		for _, x := range sf.GetStringData().GetData() {
			out = append(out, x)
		}
	case sf.GetLongData() != nil:
		for _, x := range sf.GetLongData().GetData() {
			out = append(out, x)
		}
	case sf.GetIntData() != nil:
		for _, x := range sf.GetIntData().GetData() {
			out = append(out, int64(x))
		}
	case sf.GetDoubleData() != nil:
		for _, x := range sf.GetDoubleData().GetData() {
			out = append(out, x)
		}
	case sf.GetFloatData() != nil:
		for _, x := range sf.GetFloatData().GetData() {
			out = append(out, float64(x))
		}
	case sf.GetBoolData() != nil:
		for _, x := range sf.GetBoolData().GetData() {
			out = append(out, x)
		}
	case sf.GetJsonData() != nil:
		for _, x := range sf.GetJsonData().GetData() {
			out = append(out, x)
		}
	case sf.GetArrayData() != nil:
		for _, x := range sf.GetArrayData().GetData() {
			out = append(out, x.GetStringData().GetData())
		}
	}
	if valid := fd.GetValidData(); len(valid) > 0 && len(out) < len(valid) {
		// Nullable columns may omit null entries; re-expand them in row order.
		full, j := make([]any, len(valid)), 0
		for i, ok := range valid {
			if ok && j < len(out) {
				full[i], j = out[j], j+1
			}
		}
		out = full
	} else if len(valid) > 0 {
		for i, ok := range valid {
			if !ok && i < len(out) {
				out[i] = nil
			}
		}
	}
	return out, nil
}

func (s *Store) idExpr(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = literal(id)
	}
	return s.spec.Field(vectorstore.ProductIDField) + " in [" + strings.Join(quoted, ", ") + "]"
}

func (s *Store) query(ctx context.Context, expr string, fields []string, params ...*commonpb.KeyValuePair) ([]map[string]any, error) {
	resp, err := s.svc.Query(s.ctx(ctx), &milvuspb.QueryRequest{
		CollectionName: s.spec.Collection, Expr: expr, OutputFields: fields, QueryParams: params,
		ConsistencyLevel: commonpb.ConsistencyLevel_Strong,
	})
	if err := statusErr("query "+s.spec.Collection, resp.GetStatus(), err); err != nil {
		return nil, err
	}
	n := 0
	for _, fd := range resp.GetFieldsData() {
		vals, err := fieldValues(fd)
		if err != nil {
			return nil, err
		}
		n = max(n, len(vals))
	}
	return columns(resp.GetFieldsData(), n)
}

// Get returns a product's stored vectors and ranking doc.
func (s *Store) Get(ctx context.Context, productID string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	sch, err := s.collection(ctx)
	if err != nil {
		return nil, nil, err
	}
	canonical, names := s.outputFields(sch)
	fields := append([]string(nil), names...)
	for _, name := range []string{pipeline.VectorText, pipeline.VectorImage} {
		if v, ok := s.spec.Vector(name); ok && fieldOf(sch, v.Name) != nil {
			fields = append(fields, v.Name)
		}
	}
	rows, err := s.query(ctx, s.idExpr([]string{productID}), fields)
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", productID, err)
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("product %s: %w", productID, pipeline.ErrNotFound)
	}
	_, doc := vectorstore.DocFromMap(s.canonicalRow(rows[0], canonical, names))
	p := &pipeline.VectorPoint{ProductID: productID, Vectors: map[string][]float32{}}
	for _, name := range []string{pipeline.VectorText, pipeline.VectorImage} {
		if v, ok := s.spec.Vector(name); ok {
			if vec, ok := rows[0][v.Name].([]float32); ok {
				p.Vectors[name] = vec
			}
		}
	}
	return p, doc, nil
}

// Upsert writes points column by column. Every schema field gets a value for every row (its
// zero value when the product has none, or null when the field is nullable); payload fields
// without a schema field go to the dynamic field when it is enabled and are dropped otherwise.
func (s *Store) Upsert(ctx context.Context, points []pipeline.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	sch, err := s.collection(ctx)
	if err != nil {
		return err
	}
	rows := make([]map[string]any, len(points))
	vectors := make([]map[string][]float32, len(points))
	for i, p := range points {
		rows[i] = make(map[string]any, len(p.Payload)+1)
		for k, v := range p.Payload {
			rows[i][s.spec.Field(k)] = v
		}
		rows[i][s.spec.Field(vectorstore.ProductIDField)] = p.ProductID
		vectors[i] = map[string][]float32{}
		for name, vec := range p.Vectors {
			if v, ok := s.spec.Vector(name); ok {
				vectors[i][v.Name] = vec
			}
		}
	}
	var data []*schemapb.FieldData
	for _, f := range sch.GetFields() {
		if f.GetIsDynamic() || (f.GetIsPrimaryKey() && f.GetAutoID()) || f.GetIsFunctionOutput() {
			continue
		}
		fd, err := column(f, rows, vectors)
		if err != nil {
			return fmt.Errorf("upsert into %s: field %s: %w", s.spec.Collection, f.GetName(), err)
		}
		data = append(data, fd)
		for _, r := range rows {
			delete(r, f.GetName())
		}
	}
	if sch.GetEnableDynamicField() {
		dyn := make([][]byte, len(rows))
		for i, r := range rows {
			b, err := json.Marshal(r)
			if err != nil {
				return fmt.Errorf("encode dynamic fields: %w", err)
			}
			dyn[i] = b
		}
		data = append(data, &schemapb.FieldData{Type: schemapb.DataType_JSON, FieldName: "$meta", IsDynamic: true,
			Field: &schemapb.FieldData_Scalars{Scalars: &schemapb.ScalarField{Data: &schemapb.ScalarField_JsonData{JsonData: &schemapb.JSONArray{Data: dyn}}}}})
	}
	resp, err := s.svc.Upsert(s.ctx(ctx), &milvuspb.UpsertRequest{CollectionName: s.spec.Collection, FieldsData: data, NumRows: uint32(len(points))}) //nolint:gosec // batch sizes are small
	return statusErr("upsert into "+s.spec.Collection, resp.GetStatus(), err)
}

// column builds one field's column for rows, coercing canonical values to the field type.
func column(f *schemapb.FieldSchema, rows []map[string]any, vectors []map[string][]float32) (*schemapb.FieldData, error) {
	fd := &schemapb.FieldData{Type: f.GetDataType(), FieldName: f.GetName()}
	var valid []bool
	isNull := func(v any) bool {
		null := v == nil && f.GetNullable()
		if f.GetNullable() {
			valid = append(valid, !null)
		}
		return null
	}
	scalars := func(sf *schemapb.ScalarField) { fd.Field = &schemapb.FieldData_Scalars{Scalars: sf} }
	switch f.GetDataType() {
	case schemapb.DataType_FloatVector:
		dim, _ := strconv.Atoi(param(f.GetTypeParams(), "dim"))
		var flat []float32
		for _, vs := range vectors {
			vec, ok := vs[f.GetName()]
			if !ok || len(vec) != dim {
				// A product without this vector (an image that failed to embed) gets a zero
				// vector, which has no direction and so never ranks on similarity.
				vec = make([]float32, dim)
			}
			flat = append(flat, vec...)
		}
		fd.Field = &schemapb.FieldData_Vectors{Vectors: &schemapb.VectorField{Dim: int64(dim), Data: &schemapb.VectorField_FloatVector{FloatVector: &schemapb.FloatArray{Data: flat}}}}
		return fd, nil
	case schemapb.DataType_VarChar, schemapb.DataType_String:
		var out []string
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				if t, ok := v.(time.Time); ok {
					v = t.UTC().Format(time.RFC3339)
				}
				out = append(out, vectorstore.AsString(v))
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_StringData{StringData: &schemapb.StringArray{Data: out}}})
	case schemapb.DataType_Int64:
		var out []int64
		for _, r := range rows {
			v := r[f.GetName()]
			if isNull(v) {
				continue
			}
			if str, ok := v.(string); ok {
				if t, err := time.Parse(time.RFC3339, str); err == nil {
					out = append(out, t.Unix())
					continue
				}
			}
			out = append(out, vectorstore.AsInt(v))
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_LongData{LongData: &schemapb.LongArray{Data: out}}})
	case schemapb.DataType_Int32, schemapb.DataType_Int16, schemapb.DataType_Int8:
		var out []int32
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				out = append(out, int32(vectorstore.AsInt(v))) //nolint:gosec // narrowing to the declared field type
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_IntData{IntData: &schemapb.IntArray{Data: out}}})
	case schemapb.DataType_Double:
		var out []float64
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				out = append(out, vectorstore.AsFloat(v))
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_DoubleData{DoubleData: &schemapb.DoubleArray{Data: out}}})
	case schemapb.DataType_Float:
		var out []float32
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				out = append(out, float32(vectorstore.AsFloat(v)))
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_FloatData{FloatData: &schemapb.FloatArray{Data: out}}})
	case schemapb.DataType_Bool:
		var out []bool
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				b, _ := v.(bool)
				out = append(out, b)
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_BoolData{BoolData: &schemapb.BoolArray{Data: out}}})
	case schemapb.DataType_Array:
		if f.GetElementType() != schemapb.DataType_VarChar {
			return nil, fmt.Errorf("arrays of %v are not supported", f.GetElementType())
		}
		var out []*schemapb.ScalarField
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				out = append(out, &schemapb.ScalarField{Data: &schemapb.ScalarField_StringData{StringData: &schemapb.StringArray{Data: vectorstore.AsStrings(v)}}})
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_ArrayData{ArrayData: &schemapb.ArrayArray{Data: out, ElementType: schemapb.DataType_VarChar}}})
	case schemapb.DataType_JSON:
		var out [][]byte
		for _, r := range rows {
			if v := r[f.GetName()]; !isNull(v) {
				b, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("encode JSON field: %w", err)
				}
				out = append(out, b)
			}
		}
		scalars(&schemapb.ScalarField{Data: &schemapb.ScalarField_JsonData{JsonData: &schemapb.JSONArray{Data: out}}})
	default:
		return nil, fmt.Errorf("field type %v is not supported", f.GetDataType())
	}
	fd.ValidData = valid
	return fd, nil
}

// Delete removes entities by product ID.
func (s *Store) Delete(ctx context.Context, productIDs []string) error {
	if len(productIDs) == 0 {
		return nil
	}
	resp, err := s.svc.Delete(s.ctx(ctx), &milvuspb.DeleteRequest{CollectionName: s.spec.Collection, Expr: s.idExpr(productIDs)})
	return statusErr("delete from "+s.spec.Collection, resp.GetStatus(), err)
}

// Health checks that the server answers.
func (s *Store) Health(ctx context.Context) error {
	resp, err := s.svc.GetVersion(s.ctx(ctx), &milvuspb.GetVersionRequest{})
	return statusErr("milvus health", resp.GetStatus(), err)
}

// Count returns the number of entities (BYO collections are written in place).
func (s *Store) Count(ctx context.Context, _ int) (int64, error) {
	rows, err := s.query(ctx, "", []string{"count(*)"})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("count %s: empty result", s.spec.Collection)
	}
	return vectorstore.AsInt(rows[0]["count(*)"]), nil
}

// ScrollVectors streams every entity's vector and categories in primary-key order
// (centroids), paging with a primary-key range like the Milvus query iterator.
func (s *Store) ScrollVectors(ctx context.Context, _ int, vector string, fn func(string, []float32, []string) error) error {
	v, err := s.vectorField(vector)
	if err != nil {
		return err
	}
	sch, err := s.collection(ctx)
	if err != nil {
		return err
	}
	pid, cats := s.spec.Field(vectorstore.ProductIDField), s.spec.Field("category_ids")
	if f := fieldOf(sch, pid); f == nil || !f.GetIsPrimaryKey() {
		return fmt.Errorf("scroll %s: %s must be the primary key", s.spec.Collection, pid)
	}
	fields := []string{pid, v.Name}
	if stored(sch, cats) {
		fields = append(fields, cats)
	}
	const batch = 1000
	last := ""
	for {
		expr := ""
		if last != "" {
			expr = pid + " > " + literal(last)
		}
		rows, err := s.query(ctx, expr, fields, &commonpb.KeyValuePair{Key: "limit", Value: strconv.Itoa(batch)},
			&commonpb.KeyValuePair{Key: "iterator", Value: "true"}, &commonpb.KeyValuePair{Key: "reduce_stop_for_best", Value: "true"})
		if err != nil {
			return err
		}
		for _, r := range rows {
			id := vectorstore.AsString(r[pid])
			vec, _ := r[v.Name].([]float32)
			if err := fn(id, vec, vectorstore.AsStrings(r[cats])); err != nil {
				return err
			}
			if id > last {
				last = id
			}
		}
		if len(rows) < batch {
			return nil
		}
	}
}

var metricNames = map[string]string{"COSINE": vectorstore.MetricCosine, "IP": vectorstore.MetricDot, "L2": vectorstore.MetricL2}

// Describe reports the collection's float vector fields (dimension and the metric of their
// index) and its scalar fields.
func (s *Store) Describe(ctx context.Context) (vectorstore.Info, error) {
	s.mu.Lock()
	s.schema = nil
	s.mu.Unlock()
	sch, err := s.collection(ctx)
	if errors.Is(err, errCollectionMissing) {
		return vectorstore.Info{}, nil
	}
	if err != nil {
		return vectorstore.Info{}, err
	}
	info := vectorstore.Info{Exists: true, AnyField: sch.GetEnableDynamicField(),
		Fields: map[string]bool{}, Vectors: map[string]vectorstore.VectorInfo{}}
	for _, f := range sch.GetFields() {
		if f.GetDataType() != schemapb.DataType_FloatVector {
			info.Fields[f.GetName()] = true
			continue
		}
		dim, _ := strconv.Atoi(param(f.GetTypeParams(), "dim"))
		vi := vectorstore.VectorInfo{Dim: dim}
		resp, err := s.svc.DescribeIndex(s.ctx(ctx), &milvuspb.DescribeIndexRequest{CollectionName: s.spec.Collection, FieldName: f.GetName()})
		if statusErr("describe index", resp.GetStatus(), err) == nil {
			for _, d := range resp.GetIndexDescriptions() {
				if d.GetFieldName() == f.GetName() {
					m := param(d.GetParams(), "metric_type")
					if mapped, ok := metricNames[strings.ToUpper(m)]; ok {
						m = mapped
					}
					vi.Metric = m
				}
			}
		}
		info.Vectors[f.GetName()] = vi
	}
	return info, nil
}
