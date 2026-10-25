package tenant

import (
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// ID identifies a tenant (a ULID). Tenant-scoped code takes an ID, never a raw string from a
// request body; on the data plane it comes from the JWT `tid` claim.
type ID string

// ParseID validates s as a ULID and returns it as an ID in canonical upper case.
func ParseID(s string) (ID, error) {
	u, err := ulid.ParseStrict(s)
	if err != nil {
		return "", fmt.Errorf("tenant id %q: %w", s, err)
	}
	return ID(u.String()), nil
}

// NewID returns a fresh tenant ID.
func NewID() ID { return ID(ulid.Make().String()) }

func (id ID) String() string { return string(id) }

// ResourcePrefix is the lower-case `t_{tenant}` prefix of the tenant's platform-default
// OpenSearch indexes and Qdrant collections (docs/ARCHITECTURE.md §7).
func (id ID) ResourcePrefix() string { return "t_" + strings.ToLower(string(id)) }
