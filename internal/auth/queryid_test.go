package auth

import (
	"strings"
	"testing"
)

// TestQueryID_MintVerifyClassify pins the ro2. wire form: shape, length,
// verification, vault binding, key separation and tamper rejection.
func TestQueryID_MintVerifyClassify(t *testing.T) {
	k := NewQueryIDKey([]byte("persisted-secret-a"))
	id := [16]byte{0x01, 0x93, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	const ulidStr = "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"
	rc, rs := k.CredHash("tok"), k.SessionHash("sess")
	rows := [][16]byte{{1}, {2}, {3}}
	s := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, rows, nil)
	if !strings.HasPrefix(s, "ro2."+ulidStr+".") {
		t.Fatalf("minted %q", s)
	}
	kind, p := ClassifyQueryID(s)
	if kind != QueryIDReadOnly || p.ULID != ulidStr || p.RC != rc || p.RS != rs {
		t.Fatalf("classify: kind %v, parsed %+v", kind, p)
	}
	if !k.VerifyReadOnly("trail-vault", id, p) {
		t.Fatal("a freshly minted id must verify")
	}
	if p.Rows() != 3 {
		t.Fatalf("rows = %d, want 3", p.Rows())
	}
	for i, r := range rows {
		if rank, ok := k.ReadOnlyRank("trail-vault", id, p, r); !ok || rank != i+1 {
			t.Fatalf("row %d: rank %d ok %v", i, rank, ok)
		}
	}
	if _, ok := k.ReadOnlyRank("trail-vault", id, p, [16]byte{9}); ok {
		t.Fatal("a memory outside the minted set has no tag")
	}
	if _, ok := k.ReadOnlyRank("other-vault", id, p, rows[0]); ok {
		t.Fatal("row tags bind the vault")
	}
	// Tags are MAC-covered: dropping or swapping a tag breaks the MAC.
	cut := p
	cut.tags = p.tags[ROTagLen:]
	if k.VerifyReadOnly("trail-vault", id, cut) {
		t.Fatal("altered row tags must fail the MAC")
	}
	if NewQueryIDKey([]byte("persisted-secret-a")).VerifyReadOnly("trail-vault", id, p) != true {
		t.Fatal("the same persisted secret (a restart) must verify")
	}
	if k.VerifyReadOnly("other-vault", id, p) {
		t.Fatal("the MAC binds the vault")
	}
	if NewQueryIDKey([]byte("rotated-secret")).VerifyReadOnly("trail-vault", id, p) {
		t.Fatal("a rotated secret must not verify")
	}
	tampered := p
	tampered.RC[0] ^= 1
	if k.VerifyReadOnly("trail-vault", id, tampered) {
		t.Fatal("the recall credential hash is authenticated")
	}
	// Key separation: k_q is not the raw secret.
	if string(k.k) == "persisted-secret-a" {
		t.Fatal("k_q must be derived, not the auth secret itself")
	}
	if got := k.SessionHash(""); got != ([8]byte{}) {
		t.Fatal("no session → zero hash")
	}
	if k.CredHash("a") == k.CredHash("b") {
		t.Fatal("distinct credentials hash distinctly")
	}

	// Sizes, pinned: an id for 10 and for 50 returned rows.
	ten := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, make([][16]byte, 10), nil)
	tenPlus3 := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, make([][16]byte, 10), make([][16]byte, 3))
	fifty := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, make([][16]byte, 50), nil)
	if len(ten) != 143 || len(tenPlus3) != 159 || len(fifty) != 356 {
		t.Fatalf("id lengths: 10 rows %d, 10 rows + 3 associated %d, 50 rows %d (want 143, 159, 356)", len(ten), len(tenPlus3), len(fifty))
	}
	if kind, _ := ClassifyQueryID(k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, make([][16]byte, ROMaxRows), make([][16]byte, 3))); kind != QueryIDReadOnly {
		t.Fatal("a full-size ro2 id (100 rows + 3 associated) classifies")
	}
	if kind, _ := ClassifyQueryID(fifty); kind != QueryIDReadOnly {
		t.Fatal("a 50-row id classifies")
	}
	empty := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs, nil, nil)
	if kind, pe := ClassifyQueryID(empty); kind != QueryIDReadOnly || pe.Rows() != 0 || !k.VerifyReadOnly("trail-vault", id, pe) {
		t.Fatal("a zero-row read-only id is well-formed and verifies")
	}

	for _, bad := range []string{"", "q-0000000000000001", "ro1.short", "ro2.short", "RO1." + ulidStr,
		"ro2." + ulidStr + ".0011223344556677.8899aabbccddeeff..AAAAAAAAAAAAAAAAAAAAAA", // ro2 needs the associated section
		ulidStr + "x", "ro1." + ulidStr + ".zz11223344556677.8899aabbccddeeff..AAAAAAAAAAAAAAAAAAAAAA", "ro1." + ulidStr + ".0011223344556677.8899aabbccddeeff.AAA.AAAAAAAAAAAAAAAAAAAAAA"} {
		if kind, _ := ClassifyQueryID(bad); kind != QueryIDInvalid {
			t.Errorf("ClassifyQueryID(%q) = %v, want invalid", bad, kind)
		}
	}
	if kind, _ := ClassifyQueryID(ulidStr); kind != QueryIDEvent {
		t.Error("a bare ULID is an event id")
	}
	var nilKey *QueryIDKey
	ro0 := nilKey.MintReadOnlyQueryID("v", id, ulidStr, rc, rs, rows, nil)
	if ro0 != "ro0."+ulidStr {
		t.Errorf("no key → %q, want ro0.<ULID>", ro0)
	}
	if kind, _ := ClassifyQueryID(ro0); kind != QueryIDNoKey {
		t.Error("ro0. classifies as no-key")
	}
	if nilKey.VerifyReadOnly("v", id, p) {
		t.Error("no key verifies nothing")
	}
}

// TestQueryID_AssociatedSection — an ro2 id carries the COG-35 associated
// block's ids in their own MAC-covered section: a matched row and an
// associated item are told apart, and a tag moved between the sections fails
// the MAC (the length of the matched section is authenticated).
func TestQueryID_AssociatedSection(t *testing.T) {
	k := NewQueryIDKey([]byte("persisted-secret-a"))
	id := [16]byte{0x01, 0x93, 9}
	const ulidStr = "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"
	rows := [][16]byte{{1}, {2}}
	assoc := [][16]byte{{7}, {8}}
	s := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, [8]byte{}, [8]byte{}, rows, assoc)
	kind, p := ClassifyQueryID(s)
	if kind != QueryIDReadOnly || p.Legacy() || p.Rows() != 2 || !k.VerifyReadOnly("trail-vault", id, p) {
		t.Fatalf("ro2 id: kind %v legacy %v rows %d", kind, p.Legacy(), p.Rows())
	}
	if r, ok := k.ReadOnlyRank("trail-vault", id, p, rows[1]); !ok || r != 2 {
		t.Errorf("matched row 2: rank %d ok %v", r, ok)
	}
	if _, ok := k.ReadOnlyRank("trail-vault", id, p, assoc[0]); ok {
		t.Error("an associated item is not in the matched section")
	}
	if pos, ok := k.ReadOnlyAssociated("trail-vault", id, p, assoc[1]); !ok || pos != 2 {
		t.Errorf("associated item 2: pos %d ok %v", pos, ok)
	}
	if _, ok := k.ReadOnlyAssociated("trail-vault", id, p, rows[0]); ok {
		t.Error("a matched row is not in the associated section")
	}
	moved := p
	moved.tags = append(append([]byte{}, p.tags...), p.atags[:ROTagLen]...)
	moved.atags = p.atags[ROTagLen:]
	if k.VerifyReadOnly("trail-vault", id, moved) {
		t.Fatal("a tag moved from the associated to the matched section must fail the MAC")
	}
}

// TestQueryID_LegacyRO1StillVerifies — ro1 ids issued before the associated
// section existed classify, verify and rank exactly as they did; Legacy()
// says the decoder cannot tell matched rows from associated items in them.
func TestQueryID_LegacyRO1StillVerifies(t *testing.T) {
	k := NewQueryIDKey([]byte("persisted-secret-a"))
	id := [16]byte{0x01, 0x93, 4}
	const ulidStr = "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"
	rows := [][16]byte{{1}, {2}, {3}}
	s := k.MintReadOnlyQueryIDV1("trail-vault", id, ulidStr, [8]byte{}, [8]byte{}, rows)
	if !strings.HasPrefix(s, "ro1."+ulidStr+".") || len(k.MintReadOnlyQueryIDV1("trail-vault", id, ulidStr, [8]byte{}, [8]byte{}, make([][16]byte, 10))) != 142 {
		t.Fatalf("legacy layout changed: %q", s)
	}
	kind, p := ClassifyQueryID(s)
	if kind != QueryIDReadOnly || !p.Legacy() || p.Rows() != 3 || !k.VerifyReadOnly("trail-vault", id, p) {
		t.Fatalf("legacy id: kind %v legacy %v rows %d", kind, p.Legacy(), p.Rows())
	}
	if r, ok := k.ReadOnlyRank("trail-vault", id, p, rows[2]); !ok || r != 3 {
		t.Errorf("legacy row 3: rank %d ok %v", r, ok)
	}
	if _, ok := k.ReadOnlyAssociated("trail-vault", id, p, rows[2]); ok {
		t.Error("a legacy id has no associated section")
	}
}
