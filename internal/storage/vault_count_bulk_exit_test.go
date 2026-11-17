package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// Every bulk operation (clone, merge, import, clear) commits records without
// moving the count, so it resets the target's count on EVERY exit, not only on
// success. Merge is pinned by TestVaultCountModel_CancelledMergeIsRecountedAfterRestart.
// These pin the other three: each stops part way after records have landed,
// and the count must match what is stored, in this process and after a clean
// restart. A target with a live, saved count is what makes a missed reset
// visible: the stale counter answers in-process and the stale key would be
// trusted after the restart.

// vmSaved gives ws a live counter and a saved count of n.
func vmSaved(t *testing.T, ps *PebbleStore, ws [8]byte, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		vmWrite(t, ps, ws, "kept", i)
	}
	ps.FlushCounters()
	if got := ps.GetVaultCount(context.Background(), ws); got != int64(n) {
		t.Fatalf("fixture: count %d, want %d", got, n)
	}
}

func vmBulkFill(t *testing.T, ps *PebbleStore, ws [8]byte, n int) {
	t.Helper()
	items := make([]EngramBatchItem, n)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: &Engram{Concept: fmt.Sprintf("bobbin %d", i), Content: "a bobbin from the lace school"}}
	}
	if _, errs := ps.WriteEngramBatch(context.Background(), items); errs != nil {
		for _, e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
	}
}

// vmCountMatches checks the count against the records, then again after a
// clean restart.
func vmCountMatches(t *testing.T, dir string, ps *PebbleStore, ws [8]byte, minStored int64) {
	t.Helper()
	ctx := context.Background()
	stored := vmStored(t, ps, ws)
	if stored < minStored {
		t.Fatalf("fixture: %d records after the interrupted operation, want at least %d", stored, minStored)
	}
	if got := ps.GetVaultCount(ctx, ws); got != stored {
		t.Errorf("count %d after the interrupted operation, the vault holds %d", got, stored)
	}
	if err := ps.Close(); err != nil {
		t.Fatal(err)
	}
	ps2 := vmOpen(t, dir)
	defer ps2.Close()
	if got, want := ps2.GetVaultCount(ctx, ws), vmStored(t, ps2, ws); got != want {
		t.Errorf("after a clean restart the count is %d, the vault holds %d", got, want)
	}
}

func TestVaultCountModel_CancelledCloneResetsTheCount(t *testing.T) {
	dir := vmDir(t)
	ps := vmOpen(t, dir)
	src, dst := vcWS("vm-csrc"), vcWS("vm-cdst")
	vmBulkFill(t, ps, src, cloneBatchSize+7)
	vmSaved(t, ps, dst, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := ps.CloneVaultData(ctx, src, dst, func(int64) { cancel() }); err == nil {
		t.Fatal("fixture: the clone was not cancelled part way")
	}
	vmCountMatches(t, dir, ps, dst, 3)
}

// cancelAfter cancels its context once more than n bytes have been read.
type cancelAfter struct {
	r      io.Reader
	n      int
	read   int
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if len(p) > 256 {
		p = p[:256]
	}
	k, err := c.r.Read(p)
	c.read += k
	if c.read > c.n {
		c.cancel()
	}
	return k, err
}

func TestVaultCountModel_CancelledImportResetsTheCount(t *testing.T) {
	src := openTestStore(t)
	wsSrc := src.VaultPrefix("vm-isrc")
	vmBulkFill(t, src, wsSrc, 2*exportBatchSize)
	var archive bytes.Buffer
	if _, err := src.ExportVaultData(context.Background(), wsSrc, "vm-isrc", ExportOpts{}, &archive); err != nil {
		t.Fatal(err)
	}

	dir := vmDir(t)
	ps := vmOpen(t, dir)
	ws := ps.VaultPrefix("vm-idst")
	vmSaved(t, ps, ws, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &cancelAfter{r: bytes.NewReader(archive.Bytes()), n: archive.Len() * 3 / 4, cancel: cancel}
	if _, err := ps.ImportVaultData(ctx, ws, "vm-idst", ImportOpts{SkipCompatCheck: true}, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("fixture: import returned %v, want it cancelled part way", err)
	}
	vmCountMatches(t, dir, ps, ws, 3)
}

// A clear whose commit reports an error after the range delete has landed.
func TestVaultCountModel_FailedClearResetsTheCount(t *testing.T) {
	dir := vmDir(t)
	ps := vmOpen(t, dir)
	ws := vcWS("vm-clrfail")
	vmSaved(t, ps, ws, 4)
	ps.clearBeforeRangeDeleteForTest = func() {
		commitResultForTest.Store(ps, func(error) error {
			commitResultForTest.Delete(ps)
			return errors.New("invented commit error after the batch applied")
		})
	}
	if _, err := ps.ClearVault(context.Background(), ws); err == nil {
		t.Fatal("fixture: the clear reported no error")
	}
	ps.clearBeforeRangeDeleteForTest = nil
	vmCountMatches(t, dir, ps, ws, 0)
}
