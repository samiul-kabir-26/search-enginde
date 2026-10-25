// Package centroids builds, stores and queries per-category centroids of product text vectors
// (PLAN.md R6). S1 predicts a query's categories by nearest centroids, in memory.
package centroids

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// MinMembers is the fewest products a category needs to get a centroid.
const MinMembers = 3

// Index is an in-memory set of L2-normalized category centroids.
type Index struct {
	Dim    int
	IDs    []string
	Counts []int
	matrix []float32
}

// Builder accumulates vectors per category.
type Builder struct {
	dim    int
	sums   map[string][]float64
	counts map[string]int
}

// NewBuilder returns an empty builder.
func NewBuilder() *Builder {
	return &Builder{sums: map[string][]float64{}, counts: map[string]int{}}
}

// Add records one product's vector under each of its categories (leaf and ancestors).
func (b *Builder) Add(vec []float32, categories []string) error {
	if b.dim == 0 {
		b.dim = len(vec)
	}
	if len(vec) != b.dim {
		return fmt.Errorf("vector has %d dims, want %d", len(vec), b.dim)
	}
	for _, c := range categories {
		s, ok := b.sums[c]
		if !ok {
			s = make([]float64, b.dim)
			b.sums[c] = s
		}
		for i, x := range vec {
			s[i] += float64(x)
		}
		b.counts[c]++
	}
	return nil
}

// Build returns the normalized centroids of categories with at least MinMembers products,
// sorted by category ID.
func (b *Builder) Build() *Index {
	ids := make([]string, 0, len(b.sums))
	for id := range b.sums {
		if b.counts[id] >= MinMembers {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	idx := &Index{Dim: b.dim, IDs: ids, Counts: make([]int, len(ids)), matrix: make([]float32, len(ids)*b.dim)}
	for i, id := range ids {
		idx.Counts[i] = b.counts[id]
		var norm float64
		for _, x := range b.sums[id] {
			norm += x * x
		}
		norm = math.Sqrt(norm)
		for j, x := range b.sums[id] {
			if norm > 0 {
				idx.matrix[i*b.dim+j] = float32(x / norm)
			}
		}
	}
	return idx
}

// Len is the number of centroids.
func (idx *Index) Len() int { return len(idx.IDs) }

// Temperature sharpens the softmax that turns centroid similarities into confidences.
const Temperature = 0.05

// Nearest returns up to k categories whose centroids are closest to vec (cosine; vec is
// expected L2-normalized), with confidences from a softmax over the k similarities. Guesses
// below minConfidence are dropped.
func (idx *Index) Nearest(vec []float32, k int, minConfidence float64) []pipeline.CategoryGuess {
	if idx == nil || len(vec) != idx.Dim || idx.Len() == 0 || k <= 0 {
		return nil
	}
	type scored struct {
		i   int
		sim float64
	}
	top := make([]scored, 0, k+1)
	for i := range idx.IDs {
		row := idx.matrix[i*idx.Dim : (i+1)*idx.Dim]
		var dot float64
		for j, x := range row {
			dot += float64(x) * float64(vec[j])
		}
		top = append(top, scored{i, dot})
		sort.Slice(top, func(a, b int) bool { return top[a].sim > top[b].sim })
		if len(top) > k {
			top = top[:k]
		}
	}
	var z float64
	for _, s := range top {
		z += math.Exp((s.sim - top[0].sim) / Temperature)
	}
	var out []pipeline.CategoryGuess
	for _, s := range top {
		conf := math.Exp((s.sim-top[0].sim)/Temperature) / z
		if conf >= minConfidence {
			out = append(out, pipeline.CategoryGuess{ID: idx.IDs[s.i], Confidence: math.Round(conf*1e4) / 1e4})
		}
	}
	return out
}

var magic = []byte("SECENT1\n")

type header struct {
	Dim    int      `json:"dim"`
	IDs    []string `json:"ids"`
	Counts []int    `json:"counts"`
}

// Encode writes the index: magic, a length-prefixed JSON header, then the float32 matrix
// (little endian).
func (idx *Index) Encode() ([]byte, error) {
	h, err := json.Marshal(header{Dim: idx.Dim, IDs: idx.IDs, Counts: idx.Counts})
	if err != nil {
		return nil, fmt.Errorf("encode centroids: %w", err)
	}
	if len(h) > math.MaxUint32 {
		return nil, errors.New("encode centroids: header too large")
	}
	var buf bytes.Buffer
	buf.Write(magic)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(h))) //nolint:gosec // bounded above
	buf.Write(h)
	_ = binary.Write(&buf, binary.LittleEndian, idx.matrix)
	return buf.Bytes(), nil
}

// Decode reads an index written by Encode.
func Decode(b []byte) (*Index, error) {
	if !bytes.HasPrefix(b, magic) || len(b) < len(magic)+4 {
		return nil, errors.New("decode centroids: bad magic")
	}
	r := bytes.NewReader(b[len(magic):])
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("decode centroids: %w", err)
	}
	hb := make([]byte, n)
	if _, err := io.ReadFull(r, hb); err != nil {
		return nil, fmt.Errorf("decode centroids: %w", err)
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, fmt.Errorf("decode centroids: %w", err)
	}
	if len(h.Counts) != len(h.IDs) {
		return nil, errors.New("decode centroids: counts and ids differ in length")
	}
	m := make([]float32, len(h.IDs)*h.Dim)
	if err := binary.Read(r, binary.LittleEndian, m); err != nil {
		return nil, fmt.Errorf("decode centroids: %w", err)
	}
	return &Index{Dim: h.Dim, IDs: h.IDs, Counts: h.Counts, matrix: m}, nil
}

// Store keeps centroid files in S3 at `centroids/{tenant}/v{n}.bin` with a JSON manifest next
// to it.
type Store struct {
	Client *s3.Client
	Bucket string
}

// Manifest describes a stored centroid file.
type Manifest struct {
	Tenant     string    `json:"tenant"`
	Version    int       `json:"index_version"`
	Categories int       `json:"categories"`
	Dim        int       `json:"dim"`
	SHA256     string    `json:"sha256"`
	BuiltAt    time.Time `json:"built_at"`
}

func key(tenant string, version int, ext string) string {
	return fmt.Sprintf("centroids/%s/v%d.%s", tenant, version, ext)
}

// Save writes the index and its manifest for a tenant's index version.
func (s *Store) Save(ctx context.Context, tenant string, version int, idx *Index) (Manifest, error) {
	b, err := idx.Encode()
	if err != nil {
		return Manifest{}, err
	}
	sum := sha256.Sum256(b)
	m := Manifest{Tenant: tenant, Version: version, Categories: idx.Len(), Dim: idx.Dim, SHA256: hex.EncodeToString(sum[:]), BuiltAt: time.Now().UTC()}
	mb, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, fmt.Errorf("centroid manifest: %w", err)
	}
	for _, obj := range []struct {
		key, ctype string
		body       []byte
	}{{key(tenant, version, "bin"), "application/octet-stream", b}, {key(tenant, version, "json"), "application/json", mb}} {
		if _, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &s.Bucket, Key: aws.String(obj.key), Body: bytes.NewReader(obj.body), ContentType: aws.String(obj.ctype),
		}); err != nil {
			return Manifest{}, fmt.Errorf("put %s: %w", obj.key, err)
		}
	}
	return m, nil
}

// ErrNotFound means no centroids are stored for that tenant and version.
var ErrNotFound = errors.New("centroids not found")

// Load reads and verifies the index for a tenant's index version.
func (s *Store) Load(ctx context.Context, tenant string, version int) (*Index, error) {
	read := func(k string) ([]byte, error) {
		out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: aws.String(k)})
		var nk *s3types.NoSuchKey
		if errors.As(err, &nk) {
			return nil, fmt.Errorf("%s: %w", k, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", k, err)
		}
		defer out.Body.Close()
		b, err := io.ReadAll(out.Body)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", k, err)
		}
		return b, nil
	}
	mb, err := read(key(tenant, version, "json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("centroid manifest: %w", err)
	}
	b, err := read(key(tenant, version, "bin"))
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, fmt.Errorf("centroids %s v%d: sha256 mismatch", tenant, version)
	}
	return Decode(b)
}

// VectorSource streams product vectors and categories (implemented by the Qdrant store).
type VectorSource interface {
	ScrollVectors(ctx context.Context, v int, vector string, fn func(productID string, vec []float32, categories []string) error) error
}

// BuildFrom builds the centroids of index version v's `text` vectors.
func BuildFrom(ctx context.Context, src VectorSource, v int) (*Index, error) {
	b := NewBuilder()
	err := src.ScrollVectors(ctx, v, pipeline.VectorText, func(_ string, vec []float32, cats []string) error {
		return b.Add(vec, cats)
	})
	if err != nil {
		return nil, err
	}
	return b.Build(), nil
}
