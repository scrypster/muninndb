package auth

import (
	"testing"

	"github.com/scrypster/muninndb/internal/prefix"
)

// TestCapabilityPrefixesNoCollision (RedTeam NOTABLE #5 regression guard):
// the 0x40/0x41 capability prefixes were relocated off storage's 0x15/0x16
// keyspace (commit 0553a07). This test asserts the prefixes don't collide
// with any storage or auth-own prefix registered in prefix.All(). Reverting
// 0x40→0x15 (or any value already claimed by another owner) would fail this
// test, catching the regression before it ships.
func TestCapabilityPrefixesNoCollision(t *testing.T) {
	// Build the set of every registered prefix byte grouped by owner, sourced
	// from the registry (prefix.All()). Task 5 dropped the hardcoded bounds
	// (0x01–0x2A storage / 0x42–0x45 auth) in favour of the registry so that
	// adding a new prefix anywhere in the range automatically tightens this
	// guard.
	type owner struct {
		name string
		cat  string
	}
	owners := make(map[byte]owner, len(prefix.All()))
	for _, e := range prefix.All() {
		owners[e.Byte] = owner{name: e.Owner + "/" + e.Name, cat: e.Cat}
	}

	capPrefixes := []struct {
		name string
		val  byte
	}{
		{"prefixCapability", prefixCapability},         // 0x40
		{"prefixCapabilityVIdx", prefixCapabilityVIdx}, // 0x41
	}

	for _, p := range capPrefixes {
		// Must not collide with any NON-capability registered prefix. The
		// capability entries themselves (capability/Capability,
		// capability/CapabilityVaultIdx) are expected to match their own
		// byte — that's the registry recording the allocation, not a
		// collision.
		if existing, ok := owners[p.val]; ok && existing.name != "capability/Capability" && existing.name != "capability/CapabilityVaultIdx" {
			t.Errorf("%s = 0x%02X collides with already-registered prefix %s — "+
				"two keyspaces would overlap",
				p.name, p.val, existing.name)
		}
	}

	// The capability prefixes must also be distinct from each other.
	if prefixCapability == prefixCapabilityVIdx {
		t.Errorf("prefixCapability == prefixCapabilityVIdx (both 0x%02X) — storage and vault index keys would collide", prefixCapability)
	}
}
