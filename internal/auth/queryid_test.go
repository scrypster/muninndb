package auth

import (
	"strings"
	"testing"
)

// TestQueryID_MintVerifyClassify pins the ro1. wire form: shape, length,
// verification, vault binding, key separation and tamper rejection.
func TestQueryID_MintVerifyClassify(t *testing.T) {
	k := NewQueryIDKey([]byte("persisted-secret-a"))
	id := [16]byte{0x01, 0x93, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	const ulidStr = "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"
	rc, rs := k.CredHash("tok"), k.SessionHash("sess")
	s := k.MintReadOnlyQueryID("trail-vault", id, ulidStr, rc, rs)
	if !strings.HasPrefix(s, "ro1."+ulidStr+".") || len(s) != ROQueryIDLenV1 {
		t.Fatalf("minted %q (len %d), want ro1.<ULID>… of length %d", s, len(s), ROQueryIDLenV1)
	}
	kind, p := ClassifyQueryID(s)
	if kind != QueryIDReadOnly || p.ULID != ulidStr || p.RC != rc || p.RS != rs {
		t.Fatalf("classify: kind %v, parsed %+v", kind, p)
	}
	if !k.VerifyReadOnly("trail-vault", id, p) {
		t.Fatal("a freshly minted id must verify")
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

	for _, bad := range []string{"", "q-0000000000000001", "ro1.short", "RO1." + ulidStr, ulidStr + "x", "ro1." + ulidStr + ".zz11223344556677.8899aabbccddeeff.AAAAAAAAAAAAAAAAAAAAAA"} {
		if kind, _ := ClassifyQueryID(bad); kind != QueryIDInvalid {
			t.Errorf("ClassifyQueryID(%q) = %v, want invalid", bad, kind)
		}
	}
	if kind, _ := ClassifyQueryID(ulidStr); kind != QueryIDEvent {
		t.Error("a bare ULID is an event id")
	}
	var nilKey *QueryIDKey
	ro0 := nilKey.MintReadOnlyQueryID("v", id, ulidStr, rc, rs)
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
