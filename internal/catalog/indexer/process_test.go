package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/tenant"
)

type fakeLexical struct {
	mu       sync.Mutex
	versions map[string]int64
	fail     error
}

func (f *fakeLexical) Upsert(_ context.Context, docs []pipeline.IndexDoc) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	var stale []string
	for _, d := range docs {
		id := d.Product.ProductID
		if cur, ok := f.versions[id]; ok && d.SourceVersion > 0 && d.SourceVersion < cur {
			stale = append(stale, id)
			continue
		}
		f.versions[id] = d.SourceVersion
	}
	return stale, nil
}

func (f *fakeLexical) Delete(_ context.Context, dels []pipeline.IndexDelete) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var stale []string
	for _, d := range dels {
		if cur, ok := f.versions[d.ProductID]; ok && d.SourceVersion > 0 && d.SourceVersion < cur {
			stale = append(stale, d.ProductID)
			continue
		}
		delete(f.versions, d.ProductID)
	}
	return stale, nil
}

func (f *fakeLexical) Health(context.Context) error { return nil }

type fakeVectors struct {
	mu     sync.Mutex
	points map[string]pipeline.VectorPoint
}

func (f *fakeVectors) Upsert(_ context.Context, ps []pipeline.VectorPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range ps {
		f.points[p.ProductID] = p
	}
	return nil
}

func (f *fakeVectors) Delete(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.points, id)
	}
	return nil
}

func (f *fakeVectors) Health(context.Context) error { return nil }

// fakeEmbedder rejects any image whose bytes are "corrupt", mimicking Triton's INVALID_ARG for
// the whole batch.
type fakeEmbedder struct {
	dim        int
	imageCalls atomic.Int32
}

func (f *fakeEmbedder) EmbedText(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = make([]float32, f.dim)
	}
	return out, nil
}

func (f *fakeEmbedder) EmbedImage(_ context.Context, images [][]byte) ([][]float32, error) {
	f.imageCalls.Add(1)
	out := make([][]float32, len(images))
	for i, b := range images {
		if string(b) == "corrupt" {
			return nil, fmt.Errorf("decode: %w", pipeline.ErrInvalidInput)
		}
		out[i] = make([]float32, f.dim)
	}
	return out, nil
}

func (f *fakeEmbedder) Dim() int { return f.dim }

type fakeFetcher map[string]string

func (f fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	b, ok := f[url]
	if !ok {
		return nil, errors.New("404")
	}
	return []byte(b), nil
}

const tid = tenant.ID("01JTENANT0000000000000000A")

func newTarget(t *testing.T) (*Target, *fakeLexical, *fakeVectors, *fakeEmbedder) {
	t.Helper()
	cfg, err := tenant.ParseConfig(tenant.DefaultDocument())
	if err != nil {
		t.Fatal(err)
	}
	m, err := catalog.NewMapper(cfg.Catalog.FieldMapping, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := catalog.NewRegistry(nil)
	lex := &fakeLexical{versions: map[string]int64{}}
	vec := &fakeVectors{points: map[string]pipeline.VectorPoint{}}
	img := &fakeEmbedder{dim: 2}
	return &Target{Tenant: tid, Mapper: m, Registry: reg, TextEmbedder: &fakeEmbedder{dim: 3}, ImageEmbedder: img, Lexical: lex, Vectors: vec}, lex, vec, img
}

func upsert(id string, version int64, image string) catalog.ProductEvent {
	doc := map[string]any{
		"id": id, "name": "Product " + id, "category": map[string]any{"path": []string{"Bags"}},
		"price": map[string]any{"amount_minor": 100, "currency": "USD"},
	}
	if image != "" {
		doc["images"] = []map[string]string{{"url": image}}
	}
	b, _ := json.Marshal(doc)
	return catalog.ProductEvent{EventID: id, TenantID: string(tid), ProductID: id, Op: catalog.OpUpsert, SourceVersion: version, Product: b}
}

func del(id string, version int64) catalog.ProductEvent {
	return catalog.ProductEvent{TenantID: string(tid), ProductID: id, Op: catalog.OpDelete, SourceVersion: version}
}

func newProc() *Processor {
	return NewProcessor(fakeFetcher{"img/ok": "jpeg", "img/bad": "corrupt"}, slog.New(slog.NewTextHandler(io.Discard, nil)), 4, 3)
}

func TestProcessUpsertDeleteAndStale(t *testing.T) {
	ctx := context.Background()
	tg, lex, vec, _ := newTarget(t)
	p := newProc()

	res, err := p.Process(ctx, tg, []catalog.ProductEvent{
		upsert("a", 2, "img/ok"),
		upsert("a", 1, "img/ok"), // older duplicate in the same batch: dropped
		upsert("b", 1, "img/missing"),
		upsert("c", 1, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.UpsertedIDs, []string{"a", "b", "c"}) || res.MissingImage != 2 {
		t.Fatalf("res = %+v", res)
	}
	if lex.versions["a"] != 2 {
		t.Errorf("a indexed at v%d, want 2", lex.versions["a"])
	}
	if _, ok := vec.points["a"].Vectors[pipeline.VectorImage]; !ok {
		t.Error("a should have an image vector")
	}
	if _, ok := vec.points["b"].Vectors[pipeline.VectorImage]; ok {
		t.Error("b's image failed to download; it must be indexed without an image vector")
	}
	if vec.points["a"].Payload["title"] != "Product a" {
		t.Errorf("payload = %v", vec.points["a"].Payload)
	}

	res, err = p.Process(ctx, tg, []catalog.ProductEvent{upsert("a", 1, "img/ok"), del("b", 0), del("c", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stale != 1 || !slices.Equal(res.DeletedIDs, []string{"b", "c"}) || len(res.UpsertedIDs) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if _, ok := vec.points["b"]; ok {
		t.Error("b must be deleted from the vector store")
	}
	if len(vec.points) != 1 {
		t.Errorf("vector store has %d points, want 1", len(vec.points))
	}
}

func TestProcessInvalidEvents(t *testing.T) {
	tg, _, vec, _ := newTarget(t)
	bad := upsert("x", 1, "")
	bad.Product = []byte(`{"id":"x","name":"no category or price"}`)
	other := upsert("y", 1, "")
	other.TenantID = "01JOTHER00000000000000000B"
	mismatch := upsert("z", 1, "")
	mismatch.ProductID = "zz"

	res, err := newProc().Process(context.Background(), tg, []catalog.ProductEvent{bad, other, mismatch, upsert("ok", 1, "")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Invalid != 3 || !slices.Equal(res.UpsertedIDs, []string{"ok"}) || len(vec.points) != 1 {
		t.Fatalf("res = %+v, points = %d", res, len(vec.points))
	}
}

func TestProcessIsolatesCorruptImages(t *testing.T) {
	tg, _, vec, img := newTarget(t)
	res, err := newProc().Process(context.Background(), tg, []catalog.ProductEvent{
		upsert("a", 1, "img/ok"), upsert("b", 1, "img/bad"), upsert("c", 1, "img/ok"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.MissingImage != 1 || len(res.UpsertedIDs) != 3 {
		t.Fatalf("res = %+v", res)
	}
	if _, ok := vec.points["b"].Vectors[pipeline.VectorImage]; ok {
		t.Error("corrupt image must not produce a vector")
	}
	if _, ok := vec.points["c"].Vectors[pipeline.VectorImage]; !ok {
		t.Error("valid images in a rejected batch must still be embedded")
	}
	if n := img.imageCalls.Load(); n != 4 {
		t.Errorf("image calls = %d, want 1 batch + 3 singles", n)
	}
}

func TestProcessPropagatesStoreErrors(t *testing.T) {
	tg, lex, _, _ := newTarget(t)
	lex.fail = errors.New("opensearch down")
	if _, err := newProc().Process(context.Background(), tg, []catalog.ProductEvent{upsert("a", 1, "")}); err == nil {
		t.Fatal("store failure must be returned for retry")
	}
}

func TestLatestPerProduct(t *testing.T) {
	got := latestPerProduct([]catalog.ProductEvent{
		upsert("a", 3, ""), upsert("b", 1, ""), upsert("a", 2, ""), del("b", 0), upsert("b", 5, ""), del("a", 0),
	})
	if len(got) != 2 || got[0].Op != catalog.OpDelete || got[1].SourceVersion != 5 {
		t.Fatalf("got %+v", got)
	}
}

func TestProcessAllChunksConcurrently(t *testing.T) {
	tg, lex, vec, _ := newTarget(t)
	p := newProc()
	p.chunkSize = 2
	var events []catalog.ProductEvent
	for i := range 9 {
		events = append(events, upsert(fmt.Sprintf("p%d", i), 1, ""), upsert(fmt.Sprintf("p%d", i), 2, ""))
	}
	res, err := p.ProcessAll(context.Background(), tg, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UpsertedIDs) != 9 || len(vec.points) != 9 {
		t.Fatalf("upserted %d, points %d; want 9", len(res.UpsertedIDs), len(vec.points))
	}
	for id, v := range lex.versions {
		if v != 2 {
			t.Errorf("%s indexed at v%d, want the latest (2)", id, v)
		}
	}
}
