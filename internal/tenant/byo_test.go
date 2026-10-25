package tenant

import (
	"database/sql"
	"testing"

	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

func TestModelAndConnectionFromRow(t *testing.T) {
	m, err := ModelFromRow(tenantdb.TenantModel{ID: "m", Kind: "embedder", Type: "openai_embeddings", Name: "emb", Status: StatusValidated,
		SecretRef: sql.NullString{String: "s", Valid: true}, InputSchema: []byte(`{"modality":"text"}`), OutputSchema: []byte(`{"dim":8}`)})
	if err != nil || m.SecretRef != "s" || m.Output.Dim != 8 || m.Settings != (ModelSettings{}) {
		t.Fatalf("ModelFromRow = %+v, %v", m, err)
	}
	if info := m.EmbedderInfo(); !info.TextEmbedder || info.Image || info.Dim != 8 {
		t.Errorf("text model info = %+v", info)
	}
	for _, bad := range []tenantdb.TenantModel{{InputSchema: []byte(`[`)}, {OutputSchema: []byte(`{"dim":"x"}`)}, {Settings: []byte(`{"max_batch_size":"x"}`)}} {
		if _, err := ModelFromRow(bad); err == nil {
			t.Errorf("ModelFromRow(%s %s %s) must fail", bad.InputSchema, bad.OutputSchema, bad.Settings)
		}
	}
	c, err := ConnectionFromRow(tenantdb.TenantConnection{ID: "c", Kind: "vector_store", Type: StorePgvector, Settings: []byte("null")})
	if err != nil || c.Type != StorePgvector || c.Settings != (ConnectionSettings{}) {
		t.Fatalf("ConnectionFromRow = %+v, %v", c, err)
	}
	if _, err := ConnectionFromRow(tenantdb.TenantConnection{Settings: []byte(`{"username":1}`)}); err == nil {
		t.Error("bad settings must fail")
	}
}
