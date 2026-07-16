package auth

import (
	"testing"
)

// TestCapabilityPrefixesNoCollision (RedTeam NOTABLE #5 regression guard):
// the 0x40/0x41 capability prefixes were relocated off storage's 0x15/0x16
// keyspace (commit 0553a07). This test asserts the prefixes don't collide
// with the storage layer's range (0x01–0x2A) or auth's own range
// (0x11–0x14). Reverting 0x40→0x15 (or any value in either range) would
// fail this test, catching the regression before it ships.
//
// The storage prefix range is sourced from the keys package; we hard-code
// the known bounds here so this test stays in the auth package without an
// import cycle. If storage extends past 0x2A, update storageMaxPrefix.
func TestCapabilityPrefixesNoCollision(t *testing.T) {
	// Known storage prefix bounds (internal/storage/keys/keys.go).
	// As of this writing, the highest storage prefix is 0x2A (LeaseKey).
	const storageMinPrefix = 0x01
	const storageMaxPrefix = 0x2A

	// Auth's own non-capability prefixes (admin user, API key, API key vIdx,
	// vault config).
	const authOwnMin = 0x11
	const authOwnMax = 0x14

	capPrefixes := []struct {
		name string
		val  byte
	}{
		{"prefixCapability", prefixCapability},         // 0x40
		{"prefixCapabilityVIdx", prefixCapabilityVIdx}, // 0x41
	}

	for _, p := range capPrefixes {
		// Must be ABOVE both ranges — not inside storage's, not inside auth's own.
		if p.val >= storageMinPrefix && p.val <= storageMaxPrefix {
			t.Errorf("%s = 0x%02X collides with storage prefix range [0x%02X, 0x%02X] — capabilities would corrupt storage keys",
				p.name, p.val, storageMinPrefix, storageMaxPrefix)
		}
		if p.val >= authOwnMin && p.val <= authOwnMax {
			t.Errorf("%s = 0x%02X collides with auth's own prefix range [0x%02X, 0x%02X] — capabilities would corrupt API key or vault config records",
				p.name, p.val, authOwnMin, authOwnMax)
		}
	}

	// The capability prefixes must also be distinct from each other.
	if prefixCapability == prefixCapabilityVIdx {
		t.Errorf("prefixCapability == prefixCapabilityVIdx (both 0x%02X) — storage and vault index keys would collide", prefixCapability)
	}
}
