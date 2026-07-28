package prefix

import (
	"testing"
)

// TestAll_NoDuplicateBytes asserts no two registry entries share the same
// prefix byte. This is the foundational invariant of the #611 fix — the
// storage/auth/capability keyspaces must be byte-disjoint.
func TestAll_NoDuplicateBytes(t *testing.T) {
	seen := make(map[byte]string, len(All())) // byte -> first Name seen
	for _, e := range All() {
		if prev, ok := seen[e.Byte]; ok {
			t.Errorf("prefix byte 0x%02X duplicated: %q and %q both claim it", e.Byte, prev, e.Name)
		}
		seen[e.Byte] = e.Name
	}
}

// TestAll_OwnerGroupsPairwiseDisjoint asserts the three owner groups
// (storage, auth, capability) hold pairwise-disjoint byte sets. This is
// stronger than NoDuplicateBytes scoped across owners — it catches the
// specific class of bug #611 fixes (e.g. auth 0x11 colliding with storage
// DigestFlags 0x11 pre-relocation).
func TestAll_OwnerGroupsPairwiseDisjoint(t *testing.T) {
	groups := map[string]map[byte]string{} // owner -> byte -> name
	for _, e := range All() {
		if groups[e.Owner] == nil {
			groups[e.Owner] = map[byte]string{}
		}
		groups[e.Owner][e.Byte] = e.Name
	}
	// Pairwise check across every distinct pair of owners.
	owners := []string{"storage", "auth", "capability"}
	for i := 0; i < len(owners); i++ {
		for j := i + 1; j < len(owners); j++ {
			a, b := owners[i], owners[j]
			for byteVal, aName := range groups[a] {
				if bName, ok := groups[b][byteVal]; ok {
					t.Errorf("owner %q and %q share byte 0x%02X (%q vs %q)", a, b, byteVal, aName, bName)
				}
			}
		}
	}
}

// TestCategory_KnownPrefixes verifies Category() resolves each named prefix
// to the Cat value recorded in the registry. Category() is the lookup Task
// 5's per-list partition guards depend on.
func TestCategory_KnownPrefixes(t *testing.T) {
	for _, e := range All() {
		got := Category(e.Byte)
		if got != e.Cat {
			t.Errorf("Category(0x%02X) = %q; want %q (entry %q)", e.Byte, got, e.Cat, e.Name)
		}
	}
	// Unknown prefix must return "" (the zero value), not a stale match.
	if got := Category(0x00); got != "" {
		t.Errorf("Category(0x00) = %q; want empty for unallocated byte", got)
	}
}

// TestAll_ConstSliceComplete is the [RT-FIX RT3] completeness guard: every
// exported prefix-byte const must appear in All(), and every All() entry
// must correspond to a named const. Catches add-const-forget-slice drift
// (and the inverse — a registry entry with no backing const).
func TestAll_ConstSliceComplete(t *testing.T) {
	// Named prefix-byte consts (the source of truth the rest of the codebase
	// references via prefix.X). Update this list when adding a const; the
	// bidirectional check below catches omissions in either direction.
	named := []struct {
		name string
		b    byte
	}{
		// Storage (0x01–0x2A)
		{"Engram", Engram},
		{"Meta", Meta},
		{"AssocFwd", AssocFwd},
		{"AssocRev", AssocRev},
		{"FTSPosting", FTSPosting},
		{"Trigram", Trigram},
		{"HNSWNode", HNSWNode},
		{"FTSStats", FTSStats},
		{"TermStats", TermStats},
		{"Contradiction", Contradiction},
		{"StateIndex", StateIndex},
		{"TagIndex", TagIndex},
		{"CreatorIndex", CreatorIndex},
		{"VaultMeta", VaultMeta},
		{"VaultNameIndex", VaultNameIndex},
		{"RelevanceBucket", RelevanceBucket},
		{"DigestFlags", DigestFlags},
		{"Coherence", Coherence},
		{"VaultWeights", VaultWeights},
		{"AssocWeightIndex", AssocWeightIndex},
		{"VaultCount", VaultCount},
		{"Provenance", Provenance},
		{"BucketMigration", BucketMigration},
		{"Embedding", Embedding},
		{"Idempotency", Idempotency},
		{"Episode", Episode},
		{"FTSVersion", FTSVersion},
		{"Transition", Transition},
		{"EmbedModel", EmbedModel},
		{"Ordinal", Ordinal},
		{"Entity", Entity},
		{"EntityEngramLink", EntityEngramLink},
		{"Relationship", Relationship},
		{"LastAccess", LastAccess},
		{"EntityReverseIndex", EntityReverseIndex},
		{"CoOccurrence", CoOccurrence},
		{"ArchiveAssoc", ArchiveAssoc},
		{"RelEntityIndex", RelEntityIndex},
		{"DreamState", DreamState},
		{"ContentHash", ContentHash},
		{"RecallEvent", RecallEvent},
		{"Lease", Lease},
		{"RawTagRange", RawTagRange},
		// Capability (0x40/0x41)
		{"Capability", Capability},
		{"CapabilityVaultIdx", CapabilityVaultIdx},
		// Auth (RELOCATED 0x42–0x45 by #611)
		{"AdminUser", AdminUser},
		{"APIKey", APIKey},
		{"APIKeyVaultIdx", APIKeyVaultIdx},
		{"VaultConfig", VaultConfig},
	}

	// Build a set of bytes that appear in All() for the forward direction.
	inAll := make(map[byte]bool, len(All()))
	for _, e := range All() {
		inAll[e.Byte] = true
	}

	// Forward: every named const must be in All().
	for _, n := range named {
		if !inAll[n.b] {
			t.Errorf("named const %s (0x%02X) not present in All()", n.name, n.b)
		}
	}
	// Reverse: every All() entry must be one of the named consts.
	namedSet := make(map[byte]bool, len(named))
	for _, n := range named {
		namedSet[n.b] = true
	}
	for _, e := range All() {
		if !namedSet[e.Byte] {
			t.Errorf("All() entry 0x%02X (%q) has no backing named const", e.Byte, e.Name)
		}
	}

	// Count check — catches accidental duplication in either list.
	if got, want := len(All()), len(named); got != want {
		t.Errorf("len(All()) = %d; want %d (named consts)", got, want)
	}
}
