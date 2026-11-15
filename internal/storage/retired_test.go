package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func memStore(t testing.TB) *PebbleStore {
	t.Helper()
	db, err := pebble.Open("retired", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	ps := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 1000, NoSyncEngrams: true})
	t.Cleanup(func() { ps.Close() })
	return ps
}

// A stale 0x0B soft-deleted entry for a LIVE engram must not put it in the
// set: a live memory the set calls retired is invisible to default recall.
// The build verifies every scanned id against its 0x02 record.
func TestRetiredSet_BuildVerifiesAgainstTheRecord(t *testing.T) {
	ps := memStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail")
	live, err := ps.WriteEngram(ctx, ws, &Engram{Concept: "bridge", Content: "footbridge decking is sound"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := ps.WriteEngram(ctx, ws, &Engram{Concept: "stile", Content: "stile replaced"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SoftDelete(ctx, ws, gone); err != nil {
		t.Fatal(err)
	}
	// Plant the stale index entry a buggy writer could leave behind.
	if err := ps.db.Set(keys.StateIndexKey(ws, uint8(StateSoftDeleted), [16]byte(live)), []byte{}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	view, err := ps.RetiredIDs(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if view.Contains([16]byte(live)) {
		t.Errorf("a live engram with a stale soft-deleted index entry was admitted to the retired set")
	}
	if !view.Contains([16]byte(gone)) || view.Len() != 1 {
		t.Errorf("retired set = %d entries (contains forgotten: %v), want exactly the forgotten engram", view.Len(), view.Contains([16]byte(gone)))
	}
}

// Bulk raw-KV paths reset the target vault's set, so retirements they copy in
// are seen by the next reader.
func TestRetiredSet_BulkCopyPathsReset(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"clone", "merge", "import"} {
		t.Run(path, func(t *testing.T) {
			ps := memStore(t)
			src := ps.VaultPrefix("src-" + path)
			dst := ps.VaultPrefix("dst-" + path)
			if _, err := ps.WriteEngram(ctx, dst, &Engram{Concept: "kept", Content: "already here"}); err != nil {
				t.Fatal(err)
			}
			id, err := ps.WriteEngram(ctx, src, &Engram{Concept: "copied", Content: "retired in the source"})
			if err != nil {
				t.Fatal(err)
			}
			if err := ps.SoftDelete(ctx, src, id); err != nil {
				t.Fatal(err)
			}
			// Build the target's set first, so a missing reset would leave it stale.
			if v, err := ps.RetiredIDs(ctx, dst); err != nil || v.Len() != 0 {
				t.Fatalf("target set before copy: len=%d err=%v", v.Len(), err)
			}
			switch path {
			case "clone":
				if _, err := ps.CloneVaultData(ctx, src, dst, nil); err != nil {
					t.Fatal(err)
				}
			case "merge":
				if _, err := ps.MergeVaultData(ctx, src, dst, nil); err != nil {
					t.Fatal(err)
				}
			case "import":
				var buf bytes.Buffer
				if _, err := ps.ExportVaultData(ctx, src, "src-"+path, ExportOpts{}, &buf); err != nil {
					t.Fatal(err)
				}
				if _, err := gzip.NewReader(bytes.NewReader(buf.Bytes())); err != nil {
					t.Fatalf("export is not a gzip stream: %v", err)
				}
				if _, err := ps.ImportVaultData(ctx, dst, "dst-"+path, ImportOpts{}, &buf); err != nil {
					t.Fatal(err)
				}
			}
			v, err := ps.RetiredIDs(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			if !v.Contains([16]byte(id)) {
				t.Errorf("%s: the copied retired engram is not in the target's retired set", path)
			}
		})
	}
}

// TestRetiredSet_BuildCost measures the startup-side cost of the set on the
// shape the design names: 20,000 memories, 5,000 retired, one of them a
// 500-version chain. The set is built lazily on a vault's first recall, so
// this is the one-time cost that first recall pays, not a server-start cost.
// Measurement only: set MUNINN_RETIRED_MEASURE=1.
func TestRetiredSet_BuildCost(t *testing.T) {
	if os.Getenv("MUNINN_RETIRED_MEASURE") == "" {
		t.Skip("measurement; set MUNINN_RETIRED_MEASURE=1")
	}
	ps := memStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("measure")
	const total, retired, chain = 20000, 5000, 500
	ids := make([]ULID, 0, total)
	for off := 0; off < total; off += 500 {
		items := make([]EngramBatchItem, 0, 500)
		for i := off; i < off+500; i++ {
			items = append(items, EngramBatchItem{WSPrefix: ws, Engram: &Engram{Concept: fmt.Sprintf("post %d", i), Content: fmt.Sprintf("trail post %d inspected", i)}})
		}
		got, errs := ps.WriteEngramBatch(ctx, items)
		for _, e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		ids = append(ids, got...)
	}
	// One 500-version chain (supersede + edge, the evolve shape) plus 4,501
	// plain soft-deletes.
	now := time.Now()
	for i := 0; i < chain-1; i++ {
		b := ps.NewBatch()
		if err := b.SupersedeEngram(ctx, ws, ids[i], now); err != nil {
			t.Fatal(err)
		}
		if err := b.WriteAssociation(ctx, ws, ids[i+1], ids[i], &Association{TargetID: ids[i], RelType: RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for i := chain; i < chain+(retired-(chain-1)); i++ {
		if err := ps.SoftDelete(ctx, ws, ids[i]); err != nil {
			t.Fatal(err)
		}
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	view, err := ps.RetiredIDs(ctx, ws)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if view.Len() != retired {
		t.Fatalf("retired set = %d, want %d", view.Len(), retired)
	}
	t.Logf("build: %d memories, %d retired → %s, retained heap ≈ %d bytes (%.1f B/entry)",
		total, view.Len(), elapsed, int64(after.HeapAlloc)-int64(before.HeapAlloc),
		float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(view.Len()))

	start = time.Now()
	n := 0
	for i := 0; i < 100000; i++ {
		if view.Contains([16]byte(ids[i%total])) {
			n++
		}
	}
	t.Logf("Contains: %s per lookup", time.Since(start)/100000)
}
