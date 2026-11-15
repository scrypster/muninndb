package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strings"
	"sync"
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

// deriveRetiredFromRecords is the reference: every engram whose RECORD (the
// 0x01 value, read by scanning Pebble, not the 0x0B index the set is built
// from) is soft-deleted or archived.
func deriveRetiredFromRecords(t testing.TB, ps *PebbleStore, ws [8]byte) map[[16]byte]bool {
	t.Helper()
	out := map[[16]byte]bool{}
	if err := ps.ScanEngrams(context.Background(), ws, func(e *Engram) error {
		if isRetiredState(e.State) {
			out[[16]byte(e.ID)] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSetMatchesRecords(t testing.TB, ps *PebbleStore, ws [8]byte, step string) {
	t.Helper()
	want := deriveRetiredFromRecords(t, ps, ws)
	view, err := ps.RetiredIDs(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	extra := 0
	view.s.mu.RLock()
	for id := range view.s.ids {
		if !want[id] {
			extra++
		}
	}
	view.s.mu.RUnlock()
	missing := 0
	for id := range want {
		if !view.Contains(id) {
			missing++
		}
	}
	if extra != 0 || missing != 0 {
		t.Errorf("%s: set=%d records=%d — %d in the set the record calls live (hidden from recall), %d retired records missing",
			step, view.Len(), len(want), extra, missing)
	}
}

// The enrichment digest rewrite (UpdateDigest) re-encodes the whole record
// and commits it without going through any lifecycle method. Racing it
// against forget must still leave the set equal to the records: the set is
// driven from committed batches, not from an enumerated list of write paths.
func TestRetiredSet_DigestRewriteRacingForget(t *testing.T) {
	ps := memStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("digest-race")
	const n = 3000
	ids := make([]ULID, n)
	for i := range ids {
		id, err := ps.WriteEngram(ctx, ws, &Engram{Concept: fmt.Sprintf("hive %d", i), Content: fmt.Sprintf("brood frame note %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	if _, err := ps.RetiredIDs(ctx, ws); err != nil {
		t.Fatal(err)
	}
	// Four enrichment rewriters sweep every id while one goroutine forgets
	// them all. UpdateDigest takes no stripe lock, so a rewrite that read the
	// record before a forget can commit after it — whatever the RECORD ends up
	// saying, the set must say the same.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(off int) {
			defer wg.Done()
			for i := range ids {
				_ = ps.UpdateDigest(ctx, ids[(i+off*n/4)%n], "summary", []string{"point"}, "fact", "")
			}
		}(g)
	}
	for i := range ids {
		if err := ps.SoftDelete(ctx, ws, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	assertSetMatchesRecords(t, ps, ws, "after forget racing digest rewrites")
}

// Every function in this package that writes an engram RECORD (builds a 0x01
// or 0x02 key) and commits must commit through commitBatch, because that is
// where the retired set learns about it — on success and on error. A raw
// batch.Commit would leave the set stale. Raw bulk-copy paths (which reset the
// set) are the named exceptions.
func TestRetiredSet_EveryRecordCommitGoesThroughCommitBatch(t *testing.T) {
	exempt := map[string]bool{
		"CloneVaultData": true, "MergeVaultData": true, "ImportVaultData": true, // reset the target set
		"ClearVault":  true, // resets every set
		"commitBatch": true,
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for fname, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil || exempt[fn.Name.Name] {
					continue
				}
				var writesRecord, commits, rawCommit bool
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "MetaKey", "EngramKey":
						writesRecord = true
					case "Commit":
						commits, rawCommit = true, true
					case "commitBatch":
						commits = true
					}
					return true
				})
				if writesRecord && commits {
					checked++
					if rawCommit {
						t.Errorf("%s: %s builds an engram record key and commits a raw batch — commit through ps.commitBatch or the retired set will not see it",
							fname, fn.Name.Name)
					}
				}
			}
		}
	}
	if checked < 12 {
		t.Fatalf("found only %d record-writing committers — the scan is not finding the write paths", checked)
	}
	t.Logf("%d record-writing committers, none commits a raw batch", checked)
}

// A commit that FAILS still reaches the retired set, and the set re-derives
// from the record rather than trusting the batch. The set is deliberately
// poisoned (a live engram marked retired); a SoftDelete of that engram whose
// commit fails (read-only DB) must heal the entry to what the record says —
// live — not leave it, and not adopt the batch's "soft-deleted".
func TestRetiredSet_FailedCommitStillRederivesFromRecord(t *testing.T) {
	fs := vfs.NewMem()
	ctx := context.Background()
	db, err := pebble.Open("ro-store", &pebble.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	ps := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100, NoSyncEngrams: true})
	ws := ps.VaultPrefix("ro")
	live, err := ps.WriteEngram(ctx, ws, &Engram{Concept: "cairn", Content: "cairn rebuilt at the col"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := ps.WriteEngram(ctx, ws, &Engram{Concept: "blaze", Content: "blaze repainted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SoftDelete(ctx, ws, gone); err != nil {
		t.Fatal(err)
	}
	ps.Close()

	rodb, err := pebble.Open("ro-store", &pebble.Options{FS: fs, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ro := NewPebbleStore(rodb, PebbleStoreConfig{CacheSize: 100, NoSyncEngrams: true})
	defer ro.Close()
	view, err := ro.RetiredIDs(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	view.s.mu.Lock()
	view.s.ids[[16]byte(live)] = struct{}{} // poison: a live engram the set calls retired
	view.s.mu.Unlock()

	if err := ro.SoftDelete(ctx, ws, live); err == nil {
		t.Fatal("expected the soft delete's commit to fail on a read-only DB")
	}
	if view.Contains([16]byte(live)) {
		t.Errorf("after a failed commit touching it, the live engram is still in the set — the hook did not run on the error path")
	}
	if !view.Contains([16]byte(gone)) || view.Len() != 1 {
		t.Errorf("set = %d entries, want exactly the forgotten engram", view.Len())
	}
	assertSetMatchesRecords(t, ro, ws, "after a failed commit")
}
