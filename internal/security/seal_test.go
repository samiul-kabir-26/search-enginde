package security

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func TestSealer(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	s, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal([]byte("secret"), []byte("kid-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Open(sealed, []byte("kid-1")); err != nil || string(got) != "secret" {
		t.Fatalf("open = %q %v", got, err)
	}
	if _, err := s.Open(sealed, []byte("kid-2")); !errors.Is(err, ErrUnseal) {
		t.Error("ciphertext bound to other aad must not open")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed, []byte("kid-1")); !errors.Is(err, ErrUnseal) {
		t.Error("tampered ciphertext must not open")
	}
	if _, err := s.Open([]byte{1}, nil); !errors.Is(err, ErrUnseal) {
		t.Error("short input must not open")
	}
	other, _ := NewSealer(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	fresh, _ := s.Seal([]byte("x"), nil)
	if _, err := other.Open(fresh, nil); err == nil {
		t.Error("another key must not open")
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewSealer(bad); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}
