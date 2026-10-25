package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashing(t *testing.T) {
	p := PasswordParams{MemoryKiB: 8 * 1024, Time: 1, Threads: 1}
	h, err := HashPassword("correct horse battery staple", p)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash = %q %v", h, err)
	}
	h2, _ := HashPassword("correct horse battery staple", p)
	if h == h2 {
		t.Error("salts must differ")
	}
	ok, rehash, err := VerifyPassword("correct horse battery staple", h, p)
	if !ok || rehash || err != nil {
		t.Errorf("verify = %v %v %v", ok, rehash, err)
	}
	if ok, _, _ := VerifyPassword("wrong", h, p); ok {
		t.Error("wrong password accepted")
	}
	if _, rehash, _ := VerifyPassword("correct horse battery staple", h, DefaultPasswordParams()); !rehash {
		t.Error("changed parameters must ask for a rehash")
	}
	for _, bad := range []string{"", "$bcrypt$x", "$argon2id$v=19$m=x$a$b", "$argon2id$v=18$m=1,t=1,p=1$YQ$YQ"} {
		if _, _, err := VerifyPassword("x", bad, p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := CheckPasswordPolicy("short", 12); err == nil {
		t.Error("short password accepted")
	}
	if err := CheckPasswordPolicy(strings.Repeat("a", 300), 12); err == nil {
		t.Error("oversized password accepted")
	}
	if RoleScopes("viewer")[0] != ScopeTenantRead || len(RoleScopes("owner")) != 3 || RoleScopes("x") != nil {
		t.Error("role scopes")
	}
}
