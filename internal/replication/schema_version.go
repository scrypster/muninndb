package replication

import (
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/pebble"
)

// CurrentSchemaVersion is the schema version written by this binary.
// Increment whenever a backward-incompatible change is made to Pebble key
// encoding, replication log format, snapshot format, or cognitive state.
//
// Version history:
//
//	1 = initial versioned release (HA beta)
const CurrentSchemaVersion uint64 = 1

func schemaVersionKey() []byte {
	return []byte{0x19, 0x03, 's', 'c', 'h', 'e', 'm', 'a', '_', 'v'}
}

// CheckAndSetSchemaVersion reads the stored schema version from Pebble.
//   - Fresh DB (no key): writes CurrentSchemaVersion, returns nil.
//   - stored == current: returns nil (no-op).
//   - stored > current: returns error (downgrade blocked).
//   - stored < current: updates stored version (migration hook for future use).
func CheckAndSetSchemaVersion(db *pebble.DB) error {
	stored, err := readSchemaVersion(db)
	if err != nil {
		return fmt.Errorf("schema version: read: %w", err)
	}
	if stored == 0 {
		return writeSchemaVersion(db, CurrentSchemaVersion)
	}
	if stored > CurrentSchemaVersion {
		return fmt.Errorf(
			"schema version: database was written by a newer binary (stored=%d, current=%d); "+
				"refusing to start to prevent data corruption — upgrade the binary",
			stored, CurrentSchemaVersion,
		)
	}
	if stored < CurrentSchemaVersion {
		// Future: run migrations here in version order.
		return writeSchemaVersion(db, CurrentSchemaVersion)
	}
	return nil
}

func readSchemaVersion(db *pebble.DB) (uint64, error) {
	val, closer, err := db.Get(schemaVersionKey())
	if err == pebble.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer closer.Close()
	if len(val) < 8 {
		return 0, fmt.Errorf("schema version: corrupt value (len=%d)", len(val))
	}
	return binary.BigEndian.Uint64(val), nil
}

func writeSchemaVersion(db *pebble.DB, v uint64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return db.Set(schemaVersionKey(), buf, pebble.Sync)
}
