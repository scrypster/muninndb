package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/prefix"
)

// ---------------------------------------------------------------------------
// #808 — a forward-association scan that FAILS PARTWAY THROUGH must not be
// served, or cached, as a short-but-complete edge list.
//
// The fault seam is the iterator sibling of readFault: readFault mediates point
// reads only, and no arrangement of a real Pebble DB makes a scan stop early
// AND report an error without corrupting an .sst. See scan_iter.go.
// ---------------------------------------------------------------------------

var errInjectedScan = errors.New("injected pebble scan failure")

// truncatingIter wraps a real scan and makes it stop after `remaining` keys,
// reporting errInjectedScan from Error() from that point on — the shape a
// corrupt block partway through a node's edge list produces.
type truncatingIter struct {
	scanIterator
	remaining int
	failed    bool
}

func (t *truncatingIter) gate(ok bool) bool {
	if t.failed || !ok {
		return false
	}
	if t.remaining <= 0 {
		t.failed = true
		return false
	}
	t.remaining--
	return true
}

func (t *truncatingIter) First() bool          { return t.gate(t.scanIterator.First()) }
func (t *truncatingIter) SeekGE(k []byte) bool { return t.gate(t.scanIterator.SeekGE(k)) }
func (t *truncatingIter) Next() bool           { return t.gate(t.scanIterator.Next()) }
func (t *truncatingIter) Valid() bool          { return !t.failed && t.scanIterator.Valid() }
func (t *truncatingIter) Error() error {
	if t.failed {
		return errInjectedScan
	}
	return t.scanIterator.Error()
}

// truncateForwardScansAfter arms iterFault for the 0x03 namespace only, so the
// engram/endpoint reads the fixture needs are untouched.
func truncateForwardScansAfter(n int) func([]byte, scanIterator) scanIterator {
	return func(scanPrefix []byte, it scanIterator) scanIterator {
		if len(scanPrefix) == 0 || scanPrefix[0] != prefix.AssocFwd {
			return nil
		}
		return &truncatingIter{scanIterator: it, remaining: n}
	}
}

// threeEdgeFixture writes src -> three distinct targets and returns src.
func threeEdgeFixture(t *testing.T, store *PebbleStore, ws [8]byte) ULID {
	t.Helper()
	src := NewULID()
	for _, w := range []float32{0.9, 0.6, 0.3} {
		mustWriteAssoc(t, store, ws, src, NewULID(), w, RelCoActivated)
	}
	return src
}

// TestGetAssociations_TruncatedScanIsAnErrorNotAShortList is the live path:
// GetAssociations is what recall traversal and currencyInDeclaredChain read
// through. A scan that dies after one edge previously returned (1 edge, nil)
// and wrote that truncated list into assocCache, where every OTHER reader
// picked it up for the 2s TTL — a fault in one call contaminating callers that
// never saw one.
func TestGetAssociations_TruncatedScanIsAnErrorNotAShortList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-iter-fault-fwd")

	src := threeEdgeFixture(t, store, ws)

	// Cold cache, so the call must go to Pebble.
	fresh := newFreshStore(t, store.db)
	fresh.iterFault = truncateForwardScansAfter(1)

	got, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	fresh.iterFault = nil

	if err == nil {
		t.Fatalf("GetAssociations returned nil error under a mid-scan fault; got %d edges (want an error)", len(got[src]))
	}
	if !errors.Is(err, errInjectedScan) && !strings.Contains(err.Error(), errInjectedScan.Error()) {
		t.Errorf("error does not carry the scan failure: %v", err)
	}
	if _, cached := fresh.assocCache.Get(assocCacheKey(ws, src)); cached {
		t.Error("a truncated edge list was written into assocCache and will be served to every reader for the TTL")
	}
}

// TestAssociationsForOne_TruncatedScanIsAnErrorNotAShortList pins the same
// property on the single-source helper that carried the `// KNOWN GAP (#808)`
// marker.
func TestAssociationsForOne_TruncatedScanIsAnErrorNotAShortList(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("assoc-iter-fault-one")

	src := threeEdgeFixture(t, store, ws)

	fresh := newFreshStore(t, store.db)
	fresh.iterFault = truncateForwardScansAfter(1)

	got, err := fresh.associationsForOne(ws, src, 10)
	fresh.iterFault = nil

	if err == nil {
		t.Fatalf("associationsForOne returned nil error under a mid-scan fault; got %d edges (want an error)", len(got))
	}
	if !errors.Is(err, errInjectedScan) && !strings.Contains(err.Error(), errInjectedScan.Error()) {
		t.Errorf("error does not carry the scan failure: %v", err)
	}
	if _, cached := fresh.assocCache.Get(assocCacheKey(ws, src)); cached {
		t.Error("a truncated edge list was written into assocCache")
	}
}

// TestScanIterSeamIsNilInProductionConstructors is the iterFault twin of
// TestPointGetSeamIsNilInProductionConstructors: with the seam nil, scanIter is
// the identity function, so nothing in these tests can change production
// behaviour.
func TestScanIterSeamIsNilInProductionConstructors(t *testing.T) {
	store := newTestStore(t)
	if store.iterFault != nil {
		t.Fatal("iterFault must be nil on a store built by the production constructor")
	}
	var sentinel scanIterator = &truncatingIter{}
	if got := store.scanIter([]byte{prefix.AssocFwd}, sentinel); got != sentinel {
		t.Error("scanIter must be the identity function when iterFault is nil")
	}
}
