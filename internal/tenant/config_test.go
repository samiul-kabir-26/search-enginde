package tenant

import (
	"strings"
	"testing"

	"github.com/zavisoft/search-engine/internal/catalog"
)

func TestDefaultDocumentParsesAndMaps(t *testing.T) {
	c, err := ParseConfig(DefaultDocument())
	if err != nil {
		t.Fatal(err)
	}
	if c.Embedders.Text.Model != "bge-small-en-v1.5" || c.Embedders.Image.Model != "clip-vit-b32" {
		t.Errorf("embedders = %+v", c.Embedders)
	}
	if errs := catalog.ValidateMapping(c.Catalog.FieldMapping); len(errs) > 0 {
		t.Errorf("default mapping invalid: %+v", errs)
	}
}

func TestParseConfigStrict(t *testing.T) {
	doc := strings.Replace(string(DefaultDocument()), `"locale": "en",`, `"locale": "en", "surprise": 1,`, 1)
	if _, err := ParseConfig([]byte(doc)); err == nil {
		t.Error("unknown top-level field must be rejected")
	}
	doc = strings.Replace(string(DefaultDocument()), `"schema_version": 1`, `"schema_version": 2`, 1)
	if _, err := ParseConfig([]byte(doc)); err == nil {
		t.Error("future schema_version must be rejected")
	}
}

func TestID(t *testing.T) {
	id := NewID()
	parsed, err := ParseID(strings.ToLower(id.String()))
	if err != nil || parsed != id {
		t.Fatalf("ParseID(lower) = %v, %v; want %v", parsed, err, id)
	}
	if _, err := ParseID("demo"); err == nil {
		t.Error("non-ULID must be rejected")
	}
	if p := id.ResourcePrefix(); p != "t_"+strings.ToLower(id.String()) {
		t.Errorf("ResourcePrefix = %s", p)
	}
}

func TestSchemaMigrationHook(t *testing.T) {
	saved := migrations
	defer func() { migrations = saved }()
	migrations = map[int]func(map[string]any) error{}
	doc := strings.Replace(string(DefaultDocument()), `"schema_version": 1`, `"schema_version": 0`, 1)
	if _, err := ParseConfig([]byte(doc)); err == nil {
		t.Error("schema_version 0 is below the supported range")
	}
	// A current document passes through untouched; a future one is rejected.
	if _, err := migrate(DefaultDocument()); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate([]byte(`{"schema_version": 99}`)); err == nil {
		t.Error("a future schema_version must be rejected")
	}
	if _, err := migrate([]byte(`not json`)); err == nil {
		t.Error("invalid JSON must be rejected")
	}
}
