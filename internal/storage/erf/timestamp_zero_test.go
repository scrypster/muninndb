package erf

import (
	"encoding/binary"
	"testing"
	"time"
)

// TestTimestampRoundTrip_ZeroTimeStaysZero is the encoder round-trip boundary
// property for the three fixed-metadata timestamps that have NO documented
// zero-default sentinel (CreatedAt, UpdatedAt, LastAccess).
//
// ERF stores them as uint64(t.UnixNano()) big-endian. time.Time{} is outside
// UnixNano's defined range, so the bit pattern that comes back out
// (-6795364578871345152 ns => 1754-08-30T22:43:41.128654848Z) is NOT the zero
// time: IsZero() returns false and every downstream IsZero() guard waves the
// garbage through. Decode must map that one bit pattern back to time.Time{}.
//
// This also repairs data at rest: a vault cloned before the fix already has the
// sentinel on disk, and only the decode side can make IsZero() true for it.
//
// ValidFrom/ValidUntil are deliberately NOT asserted here — they carry
// documented raw-0 sentinels on both the encode and decode side (see
// decodeValidity) and are covered by valid_time_test.go.
func TestTimestampRoundTrip_ZeroTimeStaysZero(t *testing.T) {
	for _, encName := range []string{"Encode", "EncodeV2"} {
		t.Run(encName, func(t *testing.T) {
			// A fully zeroed Engram: every time.Time field is time.Time{}.
			eng := &Engram{
				Concept: "zero timestamps",
				Content: "body",
			}

			var raw []byte
			var err error
			if encName == "Encode" {
				raw, err = Encode(eng)
			} else {
				raw, err = EncodeV2(eng)
			}
			if err != nil {
				t.Fatalf("%s: %v", encName, err)
			}

			got, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}

			for _, f := range []struct {
				name string
				val  time.Time
			}{
				{"CreatedAt", got.CreatedAt},
				{"UpdatedAt", got.UpdatedAt},
				{"LastAccess", got.LastAccess},
			} {
				if !f.val.IsZero() {
					t.Errorf("Decode: %s.IsZero() = false, got %v (want the zero time)", f.name, f.val.UTC())
				}
			}

			meta, err := DecodeMeta(raw)
			if err != nil {
				t.Fatalf("DecodeMeta: %v", err)
			}
			for _, f := range []struct {
				name string
				val  time.Time
			}{
				{"CreatedAt", meta.CreatedAt},
				{"UpdatedAt", meta.UpdatedAt},
				{"LastAccess", meta.LastAccess},
			} {
				if !f.val.IsZero() {
					t.Errorf("DecodeMeta: %s.IsZero() = false, got %v (want the zero time)", f.name, f.val.UTC())
				}
			}
		})
	}
}

// TestTimestampRoundTrip_RealTimesUnaffected pins that the sentinel mapping is
// surgical: every timestamp a real writer can produce still round-trips to the
// same nanosecond, including both ends of UnixNano's defined range.
//
// It does NOT establish collision-freedom, and an earlier version of this
// comment wrongly claimed it did. The sentinel IS inside UnixNano's range, so
// exactly one in-range instant collides with it — see
// TestDecodeTimestamp_SentinelInstantAliases, which pins that case, and
// TestCreatedAtFloor_IsAboveERFZeroTimeSentinel in internal/engine, which pins
// the write-path floor that keeps it unreachable.
func TestTimestampRoundTrip_RealTimesUnaffected(t *testing.T) {
	cases := []struct {
		name string
		ts   time.Time
	}{
		{"now", time.Now()},
		{"unix epoch", time.Unix(0, 0)},
		{"one ns after epoch", time.Unix(0, 1)},
		{"one ns before epoch", time.Unix(0, -1)},
		{"unixnano lower bound 1678", time.Unix(0, -9223372036854775808+1)},
		{"unixnano upper bound 2262", time.Unix(0, 9223372036854775807)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &Engram{
				Concept:    "real timestamps",
				Content:    "body",
				CreatedAt:  tc.ts,
				UpdatedAt:  tc.ts,
				LastAccess: tc.ts,
			}
			raw, err := EncodeV2(eng)
			if err != nil {
				t.Fatalf("EncodeV2: %v", err)
			}
			got, err := Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.CreatedAt.UnixNano() != tc.ts.UnixNano() {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt.UTC(), tc.ts.UTC())
			}
			if got.UpdatedAt.UnixNano() != tc.ts.UnixNano() {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt.UTC(), tc.ts.UTC())
			}
			if got.LastAccess.UnixNano() != tc.ts.UnixNano() {
				t.Errorf("LastAccess = %v, want %v", got.LastAccess.UTC(), tc.ts.UTC())
			}
			if got.CreatedAt.IsZero() {
				t.Errorf("CreatedAt.IsZero() = true for a real timestamp %v", tc.ts.UTC())
			}
		})
	}
}

// TestTimestampRoundTrip_ExistingSentinelOnDiskDecodesZero simulates a record
// written BEFORE the fix — clone wrote uint64(time.Time{}.UnixNano()) straight
// into the fixed metadata section. Part 1 of the fix (clone no longer writes it)
// cannot repair those bytes; only decode can.
func TestTimestampRoundTrip_ExistingSentinelOnDiskDecodesZero(t *testing.T) {
	now := time.Now()
	eng := &Engram{
		Concept:    "already cloned before the fix",
		Content:    "body",
		CreatedAt:  now,
		UpdatedAt:  now,
		LastAccess: now,
	}
	raw, err := EncodeV2(eng)
	if err != nil {
		t.Fatalf("EncodeV2: %v", err)
	}

	// Stamp the legacy sentinel bytes in place, exactly as the pre-fix clone did.
	sentinel := uint64(time.Time{}.UnixNano())
	binary.BigEndian.PutUint64(raw[OffsetLastAccess:OffsetLastAccess+8], sentinel)
	crc32val := ComputeCRC32(raw[:len(raw)-TrailerSize])
	binary.BigEndian.PutUint32(raw[len(raw)-TrailerSize:], crc32val)

	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.LastAccess.IsZero() {
		t.Errorf("legacy on-disk sentinel: LastAccess.IsZero() = false, got %v", got.LastAccess.UTC())
	}
	meta, err := DecodeMeta(raw)
	if err != nil {
		t.Fatalf("DecodeMeta: %v", err)
	}
	if !meta.LastAccess.IsZero() {
		t.Errorf("legacy on-disk sentinel: meta.LastAccess.IsZero() = false, got %v", meta.LastAccess.UTC())
	}
	// CreatedAt is untouched and must survive intact.
	if got.CreatedAt.UnixNano() != now.UnixNano() {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt.UTC(), now.UTC())
	}
}

// TestDecodeTimestamp_SentinelInstantAliases pins the ONE case where
// decodeTimestamp's mapping is lossy, so it is documented behaviour instead of a
// future surprise.
//
// ZeroTimeSentinelNanos is itself a legal int64 nanosecond offset inside
// UnixNano's defined range: the instant 1754-08-30T22:43:41.128654848Z encodes
// to exactly those bits, and decode maps them back to the zero time. That
// instant is therefore unrepresentable in ERF and is DESTROYED on read — the
// mapping is aliased, not collision-free.
//
// It is unreachable, not impossible, and the only thing making it unreachable
// lives in another package: engine.createdAtFloor (2000-01-01) rejects every
// pre-2000 caller-supplied CreatedAt before the encoder is reached.
// TestCreatedAtFloor_IsAboveERFZeroTimeSentinel pins that coupling; this test
// pins what it is protecting against. If someone lowers that floor for a
// historical or journal-import vault, this is the data loss they will get.
func TestDecodeTimestamp_SentinelInstantAliases(t *testing.T) {
	collision := time.Unix(0, ZeroTimeSentinelNanos)
	if collision.IsZero() {
		t.Fatalf("fixture: the collision instant must be a real time.Time, got the zero time")
	}
	if got := collision.UnixNano(); got != ZeroTimeSentinelNanos {
		t.Fatalf("fixture: collision instant UnixNano = %d, want %d", got, ZeroTimeSentinelNanos)
	}
	t.Logf("collision instant = %s (UnixNano=%d)", collision.UTC().Format(time.RFC3339Nano), ZeroTimeSentinelNanos)

	// The write path cannot produce this, which is the whole defence — assert
	// that the instant is below the year floor the engine enforces.
	if collision.Year() >= 2000 {
		t.Fatalf("collision instant year = %d; the engine's pre-2000 CreatedAt floor no longer protects it", collision.Year())
	}

	eng := &Engram{
		Concept:    "aliased instant",
		Content:    "body",
		CreatedAt:  collision,
		UpdatedAt:  collision,
		LastAccess: collision,
	}
	raw, err := EncodeV2(eng)
	if err != nil {
		t.Fatalf("EncodeV2: %v", err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v; the aliasing this test documents is gone — if decodeTimestamp "+
			"became genuinely collision-free, delete this test and the createdAtFloor coupling note",
			got.CreatedAt.UTC())
	}
	if !got.LastAccess.IsZero() {
		t.Errorf("LastAccess = %v, want the zero time (aliased)", got.LastAccess.UTC())
	}
	meta, err := DecodeMeta(raw)
	if err != nil {
		t.Fatalf("DecodeMeta: %v", err)
	}
	if !meta.LastAccess.IsZero() {
		t.Errorf("DecodeMeta LastAccess = %v, want the zero time (aliased)", meta.LastAccess.UTC())
	}
}
