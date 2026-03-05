package keys

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyPrefixesAreUnique(t *testing.T) {
	var ws [8]byte
	var id [16]byte
	var trigram [3]byte

	// One representative per prefix byte. Keys that share a prefix intentionally
	// (e.g. ProvenanceKey and ProvenanceSuffixKey both use 0x16) are represented
	// by only one entry.
	prefixKeys := []struct {
		name string
		key  []byte
	}{
		{"EngramKey", EngramKey(ws, id)},
		{"MetaKey", MetaKey(ws, id)},
		{"AssocFwdKey", AssocFwdKey(ws, id, 0.5, id)},
		{"AssocRevKey", AssocRevKey(ws, id, 0.5, id)},
		{"FTSPostingKey", FTSPostingKey(ws, "test", id)},
		{"TrigramKey", TrigramKey(ws, trigram, id)},
		{"HNSWNodeKey", HNSWNodeKey(ws, id, 0)},
		{"FTSStatsKey", FTSStatsKey(ws)},
		{"TermStatsKey", TermStatsKey(ws, "test")},
		{"ContradictionKey", ContradictionKey(ws, 0, 0, id)},
		{"StateIndexKey", StateIndexKey(ws, 0, id)},
		{"TagIndexKey", TagIndexKey(ws, 0, id)},
		{"CreatorIndexKey", CreatorIndexKey(ws, 0, id)},
		{"VaultMetaKey", VaultMetaKey(ws)},
		{"VaultNameIndexKey", VaultNameIndexKey("test")},
		{"RelevanceBucketKey", RelevanceBucketKey(ws, 0.5, id)},
		{"DigestFlagsKey", DigestFlagsKey(id)},
		{"CoherenceKey", CoherenceKey(ws)},
		{"VaultWeightsKey", VaultWeightsKey(ws)},
		{"AssocWeightIndexKey", AssocWeightIndexKey(ws, id, id)},
		{"VaultCountKey", VaultCountKey(ws)},
		{"ProvenanceKey", ProvenanceKey(ws, id)},
		{"EpisodeKey", EpisodeKey(ws, id)},
		{"BucketMigrationKey", BucketMigrationKey(ws)},
		{"EmbeddingKey", EmbeddingKey(ws, id)},
		{"TransitionKey", TransitionKey(ws, id, id)},
		{"OrdinalKey", OrdinalKey(ws, id, id)},
		{"EntityKey", EntityKey([8]byte{})},
		{"EntityEngramLinkKey", EntityEngramLinkKey([8]byte{}, [16]byte{}, [8]byte{})},
		{"RelationshipKey", RelationshipKey([8]byte{}, [16]byte{}, [8]byte{}, 0x01, [8]byte{})},
		{"EntityReverseIndexKey", EntityReverseIndexKey([8]byte{}, [8]byte{}, [16]byte{})},
		{"LastAccessIndexKey", LastAccessIndexKey([8]byte{}, 0, [16]byte{})},
	}

	seen := make(map[byte]string)
	for _, pk := range prefixKeys {
		if len(pk.key) == 0 {
			t.Errorf("%s: key is empty", pk.name)
			continue
		}
		prefix := pk.key[0]
		if prev, exists := seen[prefix]; exists {
			t.Errorf("prefix 0x%02X used by both %s and %s", prefix, prev, pk.name)
		}
		seen[prefix] = pk.name
	}
}

func TestTransitionKey(t *testing.T) {
	var ws [8]byte
	var src, dst [16]byte
	ws[0] = 0xAA
	src[0] = 0xBB
	dst[0] = 0xCC

	k := TransitionKey(ws, src, dst)

	if len(k) != 41 {
		t.Fatalf("TransitionKey len = %d, want 41", len(k))
	}
	if k[0] != 0x1C {
		t.Errorf("TransitionKey prefix = 0x%02x, want 0x1C", k[0])
	}
	if k[1] != 0xAA {
		t.Errorf("TransitionKey ws[0] = 0x%02x, want 0xAA", k[1])
	}
	if k[9] != 0xBB {
		t.Errorf("TransitionKey src[0] = 0x%02x, want 0xBB", k[9])
	}
	if k[25] != 0xCC {
		t.Errorf("TransitionKey dst[0] = 0x%02x, want 0xCC", k[25])
	}

	// TransitionPrefixForSrc must be a proper prefix of TransitionKey
	pfx := TransitionPrefixForSrc(ws, src)
	if len(pfx) != 25 {
		t.Fatalf("TransitionPrefixForSrc len = %d, want 25", len(pfx))
	}
	for i := 0; i < len(pfx); i++ {
		if pfx[i] != k[i] {
			t.Errorf("TransitionPrefixForSrc[%d] = 0x%02x, want 0x%02x (from TransitionKey)", i, pfx[i], k[i])
		}
	}
}

func TestOrdinalKey(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	parent := [16]byte{0x10}
	child := [16]byte{0x20}
	key := OrdinalKey(ws, parent, child)
	if len(key) != 41 {
		t.Fatalf("OrdinalKey length: got %d, want 41", len(key))
	}
	if key[0] != 0x1E {
		t.Fatalf("OrdinalKey prefix: got 0x%02X, want 0x1E", key[0])
	}
	for i, b := range ws {
		if key[1+i] != b {
			t.Fatalf("OrdinalKey wsPrefix byte %d: got 0x%02X, want 0x%02X", i, key[1+i], b)
		}
	}
	for i, b := range parent {
		if key[9+i] != b {
			t.Fatalf("OrdinalKey parentID byte %d: got 0x%02X, want 0x%02X", i, key[9+i], b)
		}
	}
	for i, b := range child {
		if key[25+i] != b {
			t.Fatalf("OrdinalKey childID byte %d: got 0x%02X, want 0x%02X", i, key[25+i], b)
		}
	}
}

func TestOrdinalPrefixForParent(t *testing.T) {
	ws := [8]byte{0x01}
	parent := [16]byte{0x10}
	prefix := OrdinalPrefixForParent(ws, parent)
	if len(prefix) != 25 {
		t.Fatalf("OrdinalPrefixForParent length: got %d, want 25", len(prefix))
	}
	if prefix[0] != 0x1E {
		t.Fatalf("prefix byte: got 0x%02X, want 0x1E", prefix[0])
	}
	for i, b := range ws {
		if prefix[1+i] != b {
			t.Fatalf("wsPrefix byte %d: got 0x%02X, want 0x%02X", i, prefix[1+i], b)
		}
	}
	for i, b := range parent {
		if prefix[9+i] != b {
			t.Fatalf("parentID byte %d: got 0x%02X, want 0x%02X", i, prefix[9+i], b)
		}
	}

	// OrdinalPrefixForParent must be a byte-for-byte prefix of OrdinalKey with same inputs.
	child := [16]byte{0x20}
	full := OrdinalKey(ws, parent, child)
	for i := 0; i < len(prefix); i++ {
		if prefix[i] != full[i] {
			t.Errorf("OrdinalPrefixForParent[%d] = 0x%02X, want 0x%02X (from OrdinalKey)", i, prefix[i], full[i])
		}
	}
}

func TestOrdinalWorkspacePrefix(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	wsPfx := OrdinalWorkspacePrefix(ws)

	// Must be exactly 9 bytes: 0x1E | ws(8).
	if len(wsPfx) != 9 {
		t.Fatalf("OrdinalWorkspacePrefix length: got %d, want 9", len(wsPfx))
	}
	if wsPfx[0] != 0x1E {
		t.Fatalf("OrdinalWorkspacePrefix prefix byte: got 0x%02X, want 0x1E", wsPfx[0])
	}
	for i, b := range ws {
		if wsPfx[1+i] != b {
			t.Fatalf("OrdinalWorkspacePrefix ws byte %d: got 0x%02X, want 0x%02X", i, wsPfx[1+i], b)
		}
	}

	// OrdinalWorkspacePrefix must be a proper byte-for-byte prefix of OrdinalKey.
	parent := [16]byte{0x10}
	child := [16]byte{0x20}
	full := OrdinalKey(ws, parent, child)
	for i := 0; i < len(wsPfx); i++ {
		if wsPfx[i] != full[i] {
			t.Errorf("OrdinalWorkspacePrefix[%d] = 0x%02X, want 0x%02X (from OrdinalKey)", i, wsPfx[i], full[i])
		}
	}
}

func TestEmbeddingKey(t *testing.T) {
	var ws [8]byte
	var id [16]byte
	ws[0] = 0x01
	id[0] = 0x02
	k := EmbeddingKey(ws, id)
	if len(k) != 25 {
		t.Fatalf("EmbeddingKey len = %d, want 25", len(k))
	}
	if k[0] != 0x18 {
		t.Errorf("EmbeddingKey prefix = 0x%02x, want 0x18", k[0])
	}
	if k[1] != 0x01 {
		t.Errorf("EmbeddingKey ws[0] = 0x%02x, want 0x01", k[1])
	}
	if k[9] != 0x02 {
		t.Errorf("EmbeddingKey id[0] = 0x%02x, want 0x02", k[9])
	}
}

func TestEntityReverseIndexKey(t *testing.T) {
	nameHash := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	ws := [8]byte{9, 10, 11, 12, 13, 14, 15, 16}
	engramID := [16]byte{17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

	k := EntityReverseIndexKey(nameHash, ws, engramID)
	assert.Equal(t, byte(0x23), k[0])
	assert.Equal(t, 33, len(k))
	assert.Equal(t, nameHash[:], k[1:9])
	assert.Equal(t, ws[:], k[9:17])
	assert.Equal(t, engramID[:], k[17:33])

	prefix := EntityReverseIndexPrefix(nameHash)
	assert.Equal(t, byte(0x23), prefix[0])
	assert.Equal(t, 9, len(prefix))
	assert.True(t, bytes.HasPrefix(k, prefix))
}

func TestLastAccessIndexKey_DescendingOrder(t *testing.T) {
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	id1 := [16]byte{1}
	id2 := [16]byte{2}

	newerKey := LastAccessIndexKey(ws, 2000, id1)
	olderKey := LastAccessIndexKey(ws, 1000, id2)

	// In ascending byte order, newer key should sort first (smaller inverted millis).
	assert.True(t, bytes.Compare(newerKey, olderKey) < 0, "newer access should sort first in ascending scan")
	assert.Equal(t, 33, len(newerKey))
	assert.Equal(t, byte(0x22), newerKey[0])

	prefix := LastAccessIndexPrefix(ws)
	assert.Equal(t, 9, len(prefix))
	assert.Equal(t, byte(0x22), prefix[0])
	assert.True(t, bytes.HasPrefix(newerKey, prefix))
}

func TestEntityNameHash_Normalizes(t *testing.T) {
	h1 := EntityNameHash("MJ")
	h2 := EntityNameHash("mj")
	h3 := EntityNameHash("  MJ  ")
	require.Equal(t, h1, h2, "case should be normalized")
	require.Equal(t, h1, h3, "whitespace should be trimmed")

	hA := EntityNameHash("Alice")
	hB := EntityNameHash("Bob")
	require.NotEqual(t, hA, hB)
}

func TestEntityKeyLayout(t *testing.T) {
	k := EntityKey([8]byte{1, 2, 3, 4, 5, 6, 7, 8})
	require.Equal(t, byte(0x1F), k[0], "EntityKey must start with 0x1F")
	require.Len(t, k, 9, "EntityKey must be 9 bytes")

	ws := [8]byte{0xAA}
	engramID := [16]byte{0xBB}
	nameHash := [8]byte{0xCC}
	lk := EntityEngramLinkKey(ws, engramID, nameHash)
	require.Equal(t, byte(0x20), lk[0], "EntityEngramLinkKey must start with 0x20")
	require.Len(t, lk, 33, "EntityEngramLinkKey must be 33 bytes")

	rk := RelationshipKey(ws, engramID, nameHash, 0x01, [8]byte{0xDD})
	require.Equal(t, byte(0x21), rk[0], "RelationshipKey must start with 0x21")
	require.Len(t, rk, 42, "RelationshipKey must be 42 bytes")
}

func TestArchiveAssocKey_Layout(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	src := [16]byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F}
	dst := [16]byte{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F}

	key := ArchiveAssocKey(ws, src, dst)

	// Total length: 1 (prefix) + 8 (ws) + 16 (src) + 16 (dst) = 41 bytes
	if len(key) != 41 {
		t.Fatalf("expected 41 bytes, got %d", len(key))
	}
	if key[0] != 0x25 {
		t.Errorf("prefix byte: got 0x%02X, want 0x25", key[0])
	}
	if !bytes.Equal(key[1:9], ws[:]) {
		t.Error("ws mismatch")
	}
	if !bytes.Equal(key[9:25], src[:]) {
		t.Error("src mismatch")
	}
	if !bytes.Equal(key[25:41], dst[:]) {
		t.Error("dst mismatch")
	}
}

func TestArchiveAssocPrefixForID_Length(t *testing.T) {
	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	src := [16]byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F}

	prefix := ArchiveAssocPrefixForID(ws, src)
	if len(prefix) != 25 {
		t.Fatalf("expected 25 bytes, got %d", len(prefix))
	}
	if prefix[0] != 0x25 {
		t.Errorf("prefix byte: got 0x%02X, want 0x25", prefix[0])
	}
}
