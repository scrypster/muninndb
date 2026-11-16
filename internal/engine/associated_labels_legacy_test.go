package engine

import (
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
)

// mintLegacyReadOnlyID mints a read-only id in the ro1 layout this build
// issued before the associated section existed.
func mintLegacyReadOnlyID(q *auth.QueryIDKey, vault string, ev storage.ULID, rows [][16]byte) string {
	return q.MintReadOnlyQueryIDV1(vault, [16]byte(ev), ev.String(), [8]byte{}, [8]byte{}, rows)
}
