//go:build integration

package security

import (
	"context"
	"errors"
	"testing"

	"github.com/zavisoft/search-engine/internal/testinfra"
)

func TestVaultSecretStoreAgainstVault(t *testing.T) {
	addr := testinfra.Vault(t)
	s, err := NewVaultSecretStore(VaultConfig{Addr: addr, Token: "root"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := s.Put(ctx, tenantA, "api_key", "sk-live-123")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, tenantA, ref)
	if err != nil || got.Reveal() != "sk-live-123" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := s.Get(ctx, tenantB, ref); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("another tenant's path: %v", err)
	}
	if _, err := s.Get(ctx, tenantA, "01JTESTSECRET0000000000000"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("unknown ref: %v", err)
	}
}
