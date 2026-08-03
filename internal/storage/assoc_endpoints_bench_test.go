package storage

import (
	"context"
	"os"
	"testing"
)

// The evidence behind STO-12's guard-placement decision: guarding at the
// primitive costs one Pebble point read per endpoint. Run with:
//
//	go test -tags localassets -run XXX -bench 'STO12' ./internal/storage/
//
// Measured on an M-series laptop, 30k iterations, several runs:
//
//	BenchmarkSTO12_EngramExists            0.55–1.1 µs/op (16 KB engram: same)
//	BenchmarkSTO12_WriteAssociationGuarded 3.8–4.8 µs/op
//	BenchmarkSTO12_WriteAssociationRaw     2.8–3.0 µs/op
//
// i.e. +1.0–1.6 µs on a call that is already off the request path for every
// amplifier (autoassoc, neighbor and goal-link workers are async); the one
// synchronous caller is muninn_link, at one edge per user request.
//
// READ THOSE AS ONE MACHINE'S NUMBERS, NOT AS A CONSTANT. Re-measured on a
// later M-series part they did not reproduce, in both directions:
//
//   - At the documented -benchtime=30000x the two write arms are INDISTINGUISH-
//     ABLE — 8.9–14.5 µs guarded vs 9.9–12.5 µs raw over 5 runs each, with the
//     ordering INVERTED on some runs. At that iteration count the store setup
//     and WAL behaviour dominate and the guard is buried in noise. An earlier
//     revision of this comment claimed "the ORDERING is stable"; at 30k on this
//     hardware it is not, so do not treat an inversion here as a regression.
//   - At -benchtime=3s (where the noise averages out) the ordering is stable
//     and the gap is LARGER than the headline: 6.7–7.4 µs guarded vs 1.3–1.4 µs
//     raw, while engramExists alone measures 245 ns. Two point reads cannot
//     account for +5.3 µs; the rest is this fixture's own artefact, since that
//     arm commits millions of association writes into one store and the 0x01
//     point read then has to descend past all of them. It is a property of the
//     benchmark's write volume, not of the guard on a real vault.
//
// What survives re-measurement is the SHAPE of the decision, which is what it
// was made on: the guard is one bounded point read per endpoint, it does not
// scale with engram size (see the 16 KB arm), and every amplifying caller is
// async. Quote the ratio you measured on the machine you measured it on.
func benchSTO12Store(b *testing.B) (*PebbleStore, func()) {
	dir, err := os.MkdirTemp("", "sto12-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	ps := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 1000})
	return ps, func() { ps.Close(); os.RemoveAll(dir) }
}

func BenchmarkSTO12_EngramExists(b *testing.B) {
	ps, cleanup := benchSTO12Store(b)
	defer cleanup()
	ws := ps.VaultPrefix("bench")
	id := NewULID()
	seedEndpoints(b, ps, ws, id)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ps.engramExists(ws, [16]byte(id)) {
			b.Fatal("missing")
		}
	}
}

// BenchmarkSTO12_EngramExistsLargeEngram shows the check does not scale with
// engram size: Pebble hands back a reference into the cached block, so a 16 KB
// engram is no slower than a 1-byte one. (A bounded-iterator alternative
// measured 1.14–1.36 µs, consistently slower.)
func BenchmarkSTO12_EngramExistsLargeEngram(b *testing.B) {
	ps, cleanup := benchSTO12Store(b)
	defer cleanup()
	ws := ps.VaultPrefix("bench")
	id := NewULID()
	big := make([]byte, 16000)
	for i := range big {
		big[i] = 'a' + byte(i%26)
	}
	if _, err := ps.WriteEngram(context.Background(), ws, &Engram{
		ID: id, Concept: "large fixture", Content: string(big),
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ps.engramExists(ws, [16]byte(id)) {
			b.Fatal("missing")
		}
	}
}

func benchSTO12Write(b *testing.B, guarded bool) {
	ps, cleanup := benchSTO12Store(b)
	defer cleanup()
	ctx := context.Background()
	ws := ps.VaultPrefix("bench")
	src, dst := NewULID(), NewULID()
	seedEndpoints(b, ps, ws, src, dst)
	a := &Association{TargetID: dst, Weight: 0.3, Confidence: 1}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		if guarded {
			err = ps.WriteAssociation(ctx, ws, src, dst, a)
		} else {
			err = ps.writeAssociationUnguarded(ctx, ws, src, dst, a)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSTO12_WriteAssociationGuarded(b *testing.B) { benchSTO12Write(b, true) }
func BenchmarkSTO12_WriteAssociationRaw(b *testing.B)     { benchSTO12Write(b, false) }
