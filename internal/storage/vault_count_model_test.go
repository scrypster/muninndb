package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Pins for the vault count model (vault_count.go). Each test names the
// finding it closes. Invented data only.

func vmOpen(t *testing.T, dir string) *PebbleStore {
	t.Helper()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
}

func vmDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-vcount-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func vmWrite(t *testing.T, ps *PebbleStore, ws [8]byte, tag string, i int) ULID {
	t.Helper()
	id, err := ps.WriteEngram(context.Background(), ws, &Engram{
		Concept: fmt.Sprintf("hedge row %s %d", tag, i), Content: fmt.Sprintf("hedge row %s %d was laid in early spring", tag, i),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func vmStored(t *testing.T, ps *PebbleStore, ws [8]byte) int64 {
	t.Helper()
	n, err := ps.countEngramsForVault(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// F1. A write landing between a hard delete's commit and its count change.
// The delete used to write its own (older) count to the key and hand it to the
// coalescer after the write had handed over the newer one, so the saved count
// ended one low (30 of 60 unforced races on the branch that did it).
func TestVaultCountModel_WriteInsideDeleteSurvivesRestart(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	ws := vcWS("vm-f1")
	_ = ps.GetVaultCount(ctx, ws)
	var ids []ULID
	for i := 0; i < 5; i++ {
		ids = append(ids, vmWrite(t, ps, ws, "a", i))
	}
	ps.FlushCounters()
	ps.deleteBeforeCountForTest = func() {
		ps.deleteBeforeCountForTest = nil
		vmWrite(t, ps, ws, "b", 0)
	}
	if err := ps.DeleteEngram(ctx, ws, ids[0]); err != nil {
		t.Fatal(err)
	}
	ps.Close()
	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if got, want := ps2.GetVaultCount(ctx, ws), vmStored(t, ps2, ws); got != want {
		t.Fatalf("after a clean restart the count is %d, the vault holds %d", got, want)
	}
}

// F2. A write racing ClearVault. The count was dropped BEFORE the range delete,
// so a write landing in between re-seeded from the records about to be deleted
// and the empty vault kept that count across restarts (40 of 40 on the branch
// before this model). Both sides of the range delete are pinned.
func TestVaultCountModel_WriteRacingClearVault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool
		want   int64
	}{
		{"write lands before the range delete", true, 0},
		{"write lands after the range delete", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := vmDir(t)
			ctx := context.Background()
			ps := vmOpen(t, dir)
			ws := vcWS("vm-f2")
			for i := 0; i < 50; i++ {
				vmWrite(t, ps, ws, "old", i)
			}
			ps.FlushCounters()
			inject := func() { vmWrite(t, ps, ws, "new", 0) }
			if tc.before {
				ps.clearBeforeRangeDeleteForTest = func() { ps.clearBeforeRangeDeleteForTest = nil; inject() }
			} else {
				ps.clearAfterRangeDeleteForTest = func() { ps.clearAfterRangeDeleteForTest = nil; inject() }
			}
			if _, err := ps.ClearVault(ctx, ws); err != nil {
				t.Fatal(err)
			}
			if got := vmStored(t, ps, ws); got != tc.want {
				t.Fatalf("fixture: %d records after the clear, want %d", got, tc.want)
			}
			if got := ps.GetVaultCount(ctx, ws); got != tc.want {
				t.Fatalf("count after the clear is %d, the vault holds %d", got, tc.want)
			}
			ps.Close()
			ps2 := vmOpen(t, dir)
			defer ps2.Close()
			if got := ps2.GetVaultCount(ctx, ws); got != tc.want {
				t.Fatalf("after a clean restart the count is %d, the vault holds %d", got, tc.want)
			}
		})
	}
}

// F3. Import used to write the archive's saved count key verbatim and set the
// count to the number of imported engrams. An archive with an inflated or
// lagging key, or a target that already held memories, came out wrong.
func TestVaultCountModel_ImportCountsWhatIsStored(t *testing.T) {
	ctx := context.Background()
	src := openTestStore(t)
	wsSrc := src.VaultPrefix("vm-import-src")
	for i := 0; i < 3; i++ {
		vmWrite(t, src, wsSrc, "src", i)
	}
	src.FlushCounters()
	var inflated [8]byte
	binary.BigEndian.PutUint64(inflated[:], 9999)
	if err := src.db.Set(keys.VaultCountKey(wsSrc), inflated[:], nil); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, err := src.ExportVaultData(ctx, wsSrc, "vm-import-src", ExportOpts{}, &archive); err != nil {
		t.Fatal(err)
	}

	for _, existing := range []int{0, 2} {
		t.Run(fmt.Sprintf("target holding %d", existing), func(t *testing.T) {
			dir := vmDir(t)
			dst := vmOpen(t, dir)
			ws := dst.VaultPrefix("vm-import-dst")
			for i := 0; i < existing; i++ {
				vmWrite(t, dst, ws, "dst", i)
			}
			if _, err := dst.ImportVaultData(ctx, ws, "vm-import-dst", ImportOpts{SkipCompatCheck: true}, bytes.NewReader(archive.Bytes())); err != nil {
				t.Fatal(err)
			}
			want := vmStored(t, dst, ws)
			if want != int64(existing+3) {
				t.Fatalf("fixture: %d records after import, want %d", want, existing+3)
			}
			if got := dst.GetVaultCount(ctx, ws); got != want {
				t.Fatalf("count after import is %d, the vault holds %d", got, want)
			}
			dst.Close()
			dst2 := vmOpen(t, dir)
			defer dst2.Close()
			if got := dst2.GetVaultCount(ctx, ws); got != want {
				t.Fatalf("after a clean restart the count is %d, the vault holds %d", got, want)
			}
		})
	}
}

// An unclean start (no marker: a crash, or the first start of this binary)
// recounts every vault from its records before any count is read.
func TestVaultCountModel_UncleanStartRecounts(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	wsA, wsB, wsGone := vcWS("vm-unc-a"), vcWS("vm-unc-b"), vcWS("vm-unc-c")
	for i := 0; i < 4; i++ {
		vmWrite(t, ps, wsA, "a", i)
	}
	vmWrite(t, ps, wsB, "b", 0)
	ps.FlushCounters()
	// Wrong saved counts of every shape: high, low, and one for a vault with
	// no records at all.
	for ws, n := range map[[8]byte]uint64{wsA: 8, wsB: 0, wsGone: 7} {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], n)
		if err := ps.db.Set(keys.VaultCountKey(ws), buf[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	// A crash: the database closes without the store's Close.
	ps.counterFlush.Close()
	if err := ps.db.Close(); err != nil {
		t.Fatal(err)
	}

	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if !ps2.recountedAtOpen {
		t.Fatal("an unclean start did not recount")
	}
	for ws, want := range map[[8]byte]int64{wsA: 4, wsB: 1, wsGone: 0} {
		if got := ps2.GetVaultCount(ctx, ws); got != want {
			t.Errorf("%s: count %d after an unclean start, want %d", ws[:], got, want)
		}
	}
	if v, err := Get(ps2.db, keys.VaultCountKey(wsGone)); err == nil && v != nil {
		t.Errorf("a vault with no records kept a saved count")
	}
}

// A clean start trusts the saved counts and does not recount, and deletes the
// marker so a crash before the next clean Close recounts.
func TestVaultCountModel_CleanStartDoesNotRecount(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	ws := vcWS("vm-clean")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, ws, "c", i)
	}
	ps.Close()

	ps2 := vmOpen(t, dir)
	if ps2.recountedAtOpen {
		t.Fatal("a clean start recounted")
	}
	if got := ps2.GetVaultCount(ctx, ws); got != 3 {
		t.Fatalf("count %d after a clean start, want 3", got)
	}
	if v, err := Get(ps2.db, cleanShutdownKey); err == nil && v != nil {
		t.Fatal("the clean-shutdown marker survived the open")
	}
	ps2.Close()
}

// A marker left by our last clean Close, followed by a run of an older binary
// (which never deletes the marker) that moved a count: the fingerprint no
// longer matches, so the next start recounts.
func TestVaultCountModel_StaleMarkerAfterForeignRunRecounts(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	ws := vcWS("vm-stale")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, ws, "s", i)
	}
	ps.Close()

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], 6) // the older binary's drifted count
	if err := db.Set(keys.VaultCountKey(ws), buf[:], nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if !ps2.recountedAtOpen {
		t.Fatal("a marker whose counts changed since it was written was trusted")
	}
	if got := ps2.GetVaultCount(ctx, ws); got != 3 {
		t.Fatalf("count %d, want 3", got)
	}
}

// The count lock. A writer has seeded its counter and not yet committed when a
// ClearVault completes and a reader seeds a NEW counter from a scan that
// cannot see the writer's record. Without the lock the writer's Add lands on
// the dropped counter and the new one never counts its record. With it the
// clear's reset waits for the writer, which is what the bounded wait below
// observes (it only stops the wait, either way the test then proceeds).
func TestVaultCountModel_ClearInsideAWriteKeepsTheWriteCounted(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := vcWS("vm-lock")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, ws, "old", i)
	}
	cleared := make(chan struct{})
	ps.writeBeforeCommitForTest = func() {
		ps.writeBeforeCommitForTest = nil
		go func() {
			if _, err := ps.ClearVault(ctx, ws); err != nil {
				t.Error(err)
			}
			close(cleared)
		}()
		select {
		case <-cleared:
		case <-time.After(300 * time.Millisecond):
		}
		_ = ps.GetVaultCount(ctx, ws) // a reader seeds whatever counter is live now
	}
	vmWrite(t, ps, ws, "new", 0)
	<-cleared
	want := vmStored(t, ps, ws)
	if want != 1 {
		t.Fatalf("fixture: %d records, want the one written during the clear", want)
	}
	if got := ps.GetVaultCount(ctx, ws); got != want {
		t.Fatalf("count %d, the vault holds %d: a write inside a clear was not counted", got, want)
	}
}

// Finding A. A reset drops the counter LAST. Readers take no count lock, and
// one that finds no counter seeds it from the saved key. Dropping the counter
// before deleting the key let a reader rebuild it from the key being deleted,
// and the next sweep saved that stale count (44 for 45 after a merge, 51 for 1
// after a clear). The test holds flushMu, as an in-flight sweep does, so the
// reset waits inside its window while a reader spins.
func TestVaultCountModel_ResetNeverExposesTheStaleKeyToAReader(t *testing.T) {
	ctx := context.Background()
	ps := openTestStore(t)
	src, dst := vcWS("pin-src"), vcWS("pin-dst")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, src, "s", i)
	}
	for i := 0; i < 5; i++ {
		vmWrite(t, ps, dst, "d", i)
	}
	ps.FlushCounters() // dst's saved key is 5: the ordinary steady state

	ps.counterFlush.flushMu.Lock()
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = ps.GetVaultCount(ctx, dst) // recall's vault size read, metrics, stats
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := ps.MergeVaultData(ctx, src, dst, nil); done <- err }()
	time.Sleep(100 * time.Millisecond) // widens the window. The fixed order holds for any duration.
	ps.counterFlush.flushMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stop.Store(true)
	wg.Wait()
	if got, want := ps.GetVaultCount(ctx, dst), vmStored(t, ps, dst); got != want {
		t.Fatalf("count %d after the merge, the vault holds %d: a reader seeded from the key the reset was deleting", got, want)
	}
}

// Finding B. A merge that stops part way (engine Stop cancels its context) has
// still committed batches. It returned before its reset, the raw batches never
// mark the vault changed, Close wrote the marker, and the next start trusted
// the pre-merge count (5 for 517). The reset is deferred now.
func TestVaultCountModel_CancelledMergeIsRecountedAfterRestart(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	wsSrc, wsDst := vcWS("vm-msrc"), vcWS("vm-mdst")
	items := make([]EngramBatchItem, cloneBatchSize+7)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: wsSrc, Engram: &Engram{Concept: fmt.Sprintf("pebble %d", i), Content: "a pebble from the east beck"}}
	}
	if _, errs := ps.WriteEngramBatch(ctx, items); errs != nil {
		for _, e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	for i := 0; i < 5; i++ {
		vmWrite(t, ps, wsDst, "dst", i)
	}
	ps.FlushCounters()
	mctx, cancel := context.WithCancel(ctx)
	if _, err := ps.MergeVaultData(mctx, wsSrc, wsDst, func(int64) { cancel() }); err == nil {
		t.Fatal("fixture: the merge was not cancelled part way")
	}
	ps.Close()
	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if got, want := ps2.GetVaultCount(ctx, wsDst), vmStored(t, ps2, wsDst); got != want {
		t.Fatalf("a clean restart reports %d for a vault holding %d", got, want)
	}
}

// Guard: Close writes the marker only when the final sweep saved everything.
// A write that changes a count after the final sweep must leave no marker,
// so the next start recounts instead of trusting the older saved count.
func TestVaultCountModel_ChangeAfterFinalSweepLeavesNoMarker(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	ws := vcWS("vm-late")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, ws, "a", i)
	}
	ps.closeAfterFinalSweepForTest = func() { vmWrite(t, ps, ws, "late", 0) }
	ps.Close()
	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if got, want := ps2.GetVaultCount(ctx, ws), vmStored(t, ps2, ws); got != want {
		t.Fatalf("count %d after a restart, the vault holds %d: a marker was written over an unsaved change", got, want)
	}
	if !ps2.recountedAtOpen {
		t.Fatal("no recount after a change that the final sweep did not save")
	}
}

// Guard: a merge into a vault that is in use (its counter live and dirty,
// readers active) leaves the exact count, before and after a clean restart.
func TestVaultCountModel_MergeIntoAVaultInUse(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	src, dst := vcWS("vm-use-s"), vcWS("vm-use-d")
	for i := 0; i < 4; i++ {
		vmWrite(t, ps, src, "s", i)
	}
	for i := 0; i < 6; i++ {
		vmWrite(t, ps, dst, "d", i)
	}
	ps.FlushCounters()
	vmWrite(t, ps, dst, "d", 6) // live and dirty when the merge starts
	if got := ps.GetVaultCount(ctx, dst); got != 7 {
		t.Fatalf("fixture: in-use count %d, want 7", got)
	}
	if _, err := ps.MergeVaultData(ctx, src, dst, nil); err != nil {
		t.Fatal(err)
	}
	if got := ps.GetVaultCount(ctx, dst); got != 11 {
		t.Fatalf("count %d after merging 4 into a vault in use holding 7, want 11", got)
	}
	vmWrite(t, ps, dst, "after", 0)
	ps.Close()
	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if got, want := ps2.GetVaultCount(ctx, dst), vmStored(t, ps2, dst); got != want || want != 12 {
		t.Fatalf("after a clean restart the count is %d, the vault holds %d (want 12)", got, want)
	}
}

// A cluster follower applies the leader's batches as raw records and never
// moves its own counts (measured: 8 for 5 after the leader forgot 3).
// Promotion resets every count, so the new leader prunes from real counts.
func TestVaultCountModel_PromotionResetsAFollowersCounts(t *testing.T) {
	ctx := context.Background()
	ps := openTestStore(t)
	ws := vcWS("vm-follow")
	var ids []ULID
	for i := 0; i < 8; i++ {
		ids = append(ids, vmWrite(t, ps, ws, "seed", i))
	}
	ps.FlushCounters()
	// The leader forgets 3: the follower receives raw deletes, not DeleteEngram.
	b := ps.db.NewBatch()
	for _, id := range ids[:3] {
		if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if got := ps.GetVaultCount(ctx, ws); got != 8 {
		t.Fatalf("fixture: follower count %d before promotion, want the stale 8", got)
	}
	if err := ps.ResetAllVaultCounts(); err != nil {
		t.Fatal(err)
	}
	if got := ps.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("count %d after promotion, the vault holds 5", got)
	}
}

// A read-only open (twin-replay over a backup with no marker) writes nothing,
// logs no error, and takes counts from a scan rather than untrusted keys.
func TestVaultCountModel_ReadOnlyOpenScansAndWritesNothing(t *testing.T) {
	dir := vmDir(t)
	ctx := context.Background()
	ps := vmOpen(t, dir)
	ws := vcWS("vm-ro")
	for i := 0; i < 3; i++ {
		vmWrite(t, ps, ws, "r", i)
	}
	ps.FlushCounters()
	var bad [8]byte
	binary.BigEndian.PutUint64(bad[:], 9)
	if err := ps.db.Set(keys.VaultCountKey(ws), bad[:], nil); err != nil {
		t.Fatal(err)
	}
	ck := dir + "-ck"
	if err := ps.db.Checkpoint(ck); err != nil { // a live checkpoint: no marker
		t.Fatal(err)
	}
	defer os.RemoveAll(ck)
	ps.Close()
	db, err := pebble.Open(ck, &pebble.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ro := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 10})
	defer ro.Close()
	if !ro.readOnly || ro.recountedAtOpen {
		t.Fatalf("read-only open: readOnly=%v recounted=%v", ro.readOnly, ro.recountedAtOpen)
	}
	if got := ro.GetVaultCount(ctx, ws); got != 3 {
		t.Fatalf("read-only count %d, the vault holds 3", got)
	}
}

// A store that has had a batch committed past its write path (a cluster
// follower's applier, which reports each batch through the commit hooks) never
// moved its counts for those records. Its clean Close must not certify them:
// a former follower restarted standalone would trust 8 for a vault holding 5,
// and prune real memories under max_engrams.
func TestVaultCountModel_ForeignCommitLeavesNoMarker(t *testing.T) {
	for _, hook := range []string{"InvalidateCommittedRepr", "NoteCommittedRepr", "InvalidateAllCaches"} {
		t.Run(hook, func(t *testing.T) {
			dir := vmDir(t)
			ctx := context.Background()
			ps := vmOpen(t, dir)
			ws := vcWS("vm-foreign")
			var ids []ULID
			for i := 0; i < 8; i++ {
				ids = append(ids, vmWrite(t, ps, ws, "seed", i))
			}
			ps.FlushCounters() // saved counts, as after the follower's own recount at open
			// The leader forgets 3: the applier commits its batch straight to
			// Pebble, then hands it to the store's hooks.
			b := ps.db.NewBatch()
			for _, id := range ids[:3] {
				if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Commit(pebble.Sync); err != nil {
				t.Fatal(err)
			}
			switch hook {
			case "InvalidateCommittedRepr":
				ps.InvalidateCommittedRepr(b.Repr())
			case "NoteCommittedRepr":
				ps.NoteCommittedRepr(b.Repr())
			case "InvalidateAllCaches":
				ps.InvalidateAllCaches()
			}
			b.Close()
			if err := ps.Close(); err != nil {
				t.Fatal(err)
			}
			ps2 := vmOpen(t, dir) // standalone: no promotion resets anything
			defer ps2.Close()
			if got, want := ps2.GetVaultCount(ctx, ws), vmStored(t, ps2, ws); got != want || want != 5 {
				t.Fatalf("after a clean restart the count is %d, the vault holds %d (want 5): "+
					"Close certified counts that applied records never moved", got, want)
			}
			if !ps2.recountedAtOpen {
				t.Fatal("no recount after a process that applied foreign commits")
			}
		})
	}
}
