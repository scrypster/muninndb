package erf_test

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/erf"
)

// TestValidTime_LegacyRecordDecodePin is the migration-safety PIN for valid-time:
// a record encoded with no validity fields (which is byte-identical to every
// legacy on-disk record — bytes 72-99 zero-filled) MUST decode as
// ValidFrom == CreatedAt and ValidUntil zero ("valid from creation, still true").
// If this test fails, existing vault data would silently change meaning.
func TestValidTime_LegacyRecordDecodePin(t *testing.T) {
	created := time.Unix(0, 1700000000000000000).UTC()
	eng := &erf.Engram{
		Concept:   "legacy pin",
		Content:   "legacy content",
		CreatedBy: "tester",
		CreatedAt: created,
		UpdatedAt: created,
	}
	for name, encode := range map[string]func(*erf.Engram) ([]byte, error){
		"v1": erf.Encode,
		"v2": erf.EncodeV2,
	} {
		raw, err := encode(eng)
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}

		// Pin the on-disk layout: with no validity set, bytes 72-99 are all zero —
		// exactly what every legacy record contains.
		for i := erf.OffsetValidFrom; i < erf.MetadataOffset+erf.MetadataSize; i++ {
			if raw[i] != 0 {
				t.Fatalf("%s: byte %d = 0x%02x, want 0x00 (zero-default idiom broken)", name, i, raw[i])
			}
		}

		decoded, err := erf.Decode(raw)
		if err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if !decoded.ValidFrom.Equal(created) {
			t.Errorf("%s: legacy decode ValidFrom = %v, want CreatedAt %v", name, decoded.ValidFrom, created)
		}
		if !decoded.ValidUntil.IsZero() {
			t.Errorf("%s: legacy decode ValidUntil = %v, want zero (open)", name, decoded.ValidUntil)
		}
		if decoded.Importance != 0 {
			t.Errorf("%s: legacy decode Importance = %v, want 0", name, decoded.Importance)
		}

		meta, err := erf.DecodeMeta(raw)
		if err != nil {
			t.Fatalf("%s DecodeMeta: %v", name, err)
		}
		if !meta.ValidFrom.Equal(created) {
			t.Errorf("%s: legacy DecodeMeta ValidFrom = %v, want CreatedAt %v", name, meta.ValidFrom, created)
		}
		if !meta.ValidUntil.IsZero() {
			t.Errorf("%s: legacy DecodeMeta ValidUntil = %v, want zero (open)", name, meta.ValidUntil)
		}
	}
}

// TestValidTime_EncodeDecodeRoundTrip verifies explicit validity fields and
// Importance survive Encode/Decode (v1 and v2) and DecodeMeta.
func TestValidTime_EncodeDecodeRoundTrip(t *testing.T) {
	created := time.Unix(0, 1700000000000000000).UTC()
	from := created.Add(-24 * time.Hour)
	until := created.Add(48 * time.Hour)
	eng := &erf.Engram{
		Concept:    "validity roundtrip",
		Content:    "content",
		CreatedBy:  "tester",
		CreatedAt:  created,
		UpdatedAt:  created,
		ValidFrom:  from,
		ValidUntil: until,
		Importance: 0.75,
	}
	for name, encode := range map[string]func(*erf.Engram) ([]byte, error){
		"v1": erf.Encode,
		"v2": erf.EncodeV2,
	} {
		raw, err := encode(eng)
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}
		decoded, err := erf.Decode(raw)
		if err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		if !decoded.ValidFrom.Equal(from) {
			t.Errorf("%s: ValidFrom = %v, want %v", name, decoded.ValidFrom, from)
		}
		if !decoded.ValidUntil.Equal(until) {
			t.Errorf("%s: ValidUntil = %v, want %v", name, decoded.ValidUntil, until)
		}
		if decoded.Importance != 0.75 {
			t.Errorf("%s: Importance = %v, want 0.75", name, decoded.Importance)
		}

		meta, err := erf.DecodeMeta(raw)
		if err != nil {
			t.Fatalf("%s DecodeMeta: %v", name, err)
		}
		if !meta.ValidFrom.Equal(from) {
			t.Errorf("%s: meta.ValidFrom = %v, want %v", name, meta.ValidFrom, from)
		}
		if !meta.ValidUntil.Equal(until) {
			t.Errorf("%s: meta.ValidUntil = %v, want %v", name, meta.ValidUntil, until)
		}
		if meta.Importance != 0.75 {
			t.Errorf("%s: meta.Importance = %v, want 0.75", name, meta.Importance)
		}
	}
}

// TestPatchValidUntil verifies the in-place stamp patch: sets/clears the
// ValidUntil field, keeps CRC32 valid, and leaves all other fields untouched.
func TestPatchValidUntil(t *testing.T) {
	created := time.Unix(0, 1700000000000000000).UTC()
	eng := &erf.Engram{
		Concept:   "patch validuntil",
		Content:   "content",
		CreatedBy: "tester",
		CreatedAt: created,
		Trust:     0x02,
	}
	raw, err := erf.EncodeV2(eng)
	if err != nil {
		t.Fatalf("EncodeV2: %v", err)
	}

	if got := erf.GetValidUntil(raw); !got.IsZero() {
		t.Fatalf("initial GetValidUntil = %v, want zero", got)
	}

	until := created.Add(72 * time.Hour)
	if err := erf.PatchValidUntil(raw, until); err != nil {
		t.Fatalf("PatchValidUntil: %v", err)
	}
	if !erf.VerifyCRC32(raw) {
		t.Fatal("CRC32 invalid after PatchValidUntil")
	}
	if got := erf.GetValidUntil(raw); !got.Equal(until) {
		t.Errorf("GetValidUntil = %v, want %v", got, until)
	}
	decoded, err := erf.Decode(raw)
	if err != nil {
		t.Fatalf("Decode after patch: %v", err)
	}
	if !decoded.ValidUntil.Equal(until) {
		t.Errorf("decoded.ValidUntil = %v, want %v", decoded.ValidUntil, until)
	}
	if decoded.Trust != 0x02 {
		t.Errorf("decoded.Trust = %d, want 2 (patch must not clobber neighbors)", decoded.Trust)
	}
	if !decoded.ValidFrom.Equal(created) {
		t.Errorf("decoded.ValidFrom = %v, want CreatedAt %v", decoded.ValidFrom, created)
	}

	// Clearing: patching the zero time re-opens the record (restore path).
	if err := erf.PatchValidUntil(raw, time.Time{}); err != nil {
		t.Fatalf("PatchValidUntil(zero): %v", err)
	}
	decoded, err = erf.Decode(raw)
	if err != nil {
		t.Fatalf("Decode after clear: %v", err)
	}
	if !decoded.ValidUntil.IsZero() {
		t.Errorf("decoded.ValidUntil after clear = %v, want zero", decoded.ValidUntil)
	}
}

// TestPatchAllMeta_PreservesValidity pins that the in-place metadata patch used
// by UpdateMetadata (decay, CAS, restore-meta) never clobbers a validity stamp —
// it patches only its listed fields, so bytes 72-91 must survive.
func TestPatchAllMeta_PreservesValidity(t *testing.T) {
	created := time.Unix(0, 1700000000000000000).UTC()
	until := created.Add(time.Hour)
	eng := &erf.Engram{
		Concept:    "patchallmeta validity",
		Content:    "content",
		CreatedBy:  "tester",
		CreatedAt:  created,
		ValidFrom:  created.Add(-time.Hour),
		ValidUntil: until,
		Importance: 0.5,
	}
	raw, err := erf.EncodeV2(eng)
	if err != nil {
		t.Fatalf("EncodeV2: %v", err)
	}
	if err := erf.PatchAllMeta(raw, created.Add(time.Minute), created.Add(time.Minute), 0.9, 0.8, 30, 5, 0x01); err != nil {
		t.Fatalf("PatchAllMeta: %v", err)
	}
	decoded, err := erf.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !decoded.ValidFrom.Equal(created.Add(-time.Hour)) {
		t.Errorf("ValidFrom clobbered by PatchAllMeta: got %v", decoded.ValidFrom)
	}
	if !decoded.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil clobbered by PatchAllMeta: got %v", decoded.ValidUntil)
	}
	if decoded.Importance != 0.5 {
		t.Errorf("Importance clobbered by PatchAllMeta: got %v", decoded.Importance)
	}
}
