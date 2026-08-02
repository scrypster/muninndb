package cognitive

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// errReadFailed is the synthetic association-read failure these tests inject.
// It stands for the shape that actually occurs: a corrupt 0x14 weight-index
// record, which GetAssocWeight reports as an error rather than as absence.
var errReadFailed = errors.New("injected assoc weight read failure")

// readFaultStore fails GetAssocWeight for a chosen set of pair sources and
// applies every batch it is handed. It isolates the branch under test: the
// batch path is healthy, so anything unobservable here was dropped by
// processBatch itself and not by the store.
//
// canonicalPair sorts a pair, so keying the fault on Src selects a pair
// deterministically as long as the chosen ID is the lower of the two.
type readFaultStore struct {
	mu      sync.Mutex
	failSrc map[[16]byte]bool
	reads   int
	applied map[[16]byte]float32
}

func newReadFaultStore() *readFaultStore {
	return &readFaultStore{
		failSrc: map[[16]byte]bool{},
		applied: map[[16]byte]float32{},
	}
}

func (s *readFaultStore) GetAssocWeight(_ context.Context, _ [8]byte, src, _ [16]byte) (float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failSrc[src] {
		return 0, errReadFailed
	}
	return 0, nil
}

func (s *readFaultStore) UpdateAssocWeight(context.Context, [8]byte, [16]byte, [16]byte, float32) error {
	return nil
}

func (s *readFaultStore) DecayAssocWeights(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
	return 0, nil
}

func (s *readFaultStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range updates {
		s.applied[u.Src] = u.Weight
	}
	return nil
}

func (s *readFaultStore) appliedWeight(src [16]byte) float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied[src]
}

// syncBuffer is a race-free sink for the default slog handler. The worker's own
// goroutine is running while these tests call processBatch directly, so an
// unguarded bytes.Buffer would be a data race under -race rather than a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects the default logger for the duration of the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestProcessBatch_ReadFaultIsReportedNotDropped pins that a pair whose current
// weight cannot be read is OBSERVABLE, not silently skipped.
//
// The skip itself is correct and is not what this test challenges: seeding a
// live edge back to the 0.01 cold-start weight on a read fault is the defect
// this branch exists to avoid. What the skip must not do is vanish. A corrupt
// Pebble block is PERMANENT and KEY-SCOPED, so the same pair fails in every
// flush window — a bare `continue` means that pair stops learning forever with
// nothing in the logs and nothing in Stats().Errors, which is the only place a
// failure in this worker becomes observable. That is principle #2 failing on
// this change's own terms: the fault never reaches UpdateAssocWeightBatch, so
// the AssocBatchSkipError machinery one layer down never sees it.
//
// Before the fix this branch was `if err != nil { continue }` and processBatch
// returned nil.
func TestProcessBatch_ReadFaultIsReportedNotDropped(t *testing.T) {
	store := newReadFaultStore()
	logs := captureLogs(t)

	ws := [8]byte{4, 4, 4, 4, 4, 4, 4, 4}
	idA := [16]byte{1}
	idB := [16]byte{2}
	idC := [16]byte{3}
	idD := [16]byte{4}

	// canonicalPair orders each pair, so the unreadable pair is (idC,idD).
	store.failSrc[idC] = true

	var mu sync.Mutex
	fired := map[[16]byte]int{}
	hw := NewHebbianWorkerWithDB(store, nil, func(_ [8]byte, id [16]byte, _ string, _, _ float64) {
		mu.Lock()
		defer mu.Unlock()
		fired[id]++
	})
	defer hw.Stop()

	err := hw.processBatch(context.Background(), twoPairBatch(ws, idA, idB, idC, idD))

	if err == nil {
		t.Fatal("processBatch returned nil after a pair's weight read failed — the pair was skipped " +
			"silently: no Stats().Errors increment, and a permanently corrupt record means that pair " +
			"never learns again")
	}
	if !errors.Is(err, errReadFailed) {
		t.Errorf("processBatch error does not wrap the injected read failure: %v", err)
	}

	// The healthy pair must be unaffected: reporting the fault must not cost the
	// rest of the batch, which is the same trade UpdateAssocWeightBatch makes.
	if store.appliedWeight(idA) == 0 {
		t.Error("the readable pair's update was not applied — reporting the read fault must not " +
			"abort the rest of the batch")
	}
	mu.Lock()
	firedA, firedC := fired[idA], fired[idC]
	mu.Unlock()
	if firedA != 1 {
		t.Errorf("OnWeightUpdate fired %d time(s) for the readable pair, want 1", firedA)
	}
	if firedC != 0 {
		t.Errorf("OnWeightUpdate fired %d time(s) for the pair that was never read, want 0", firedC)
	}

	// A log line naming the pair is the operator-facing half. Stats().Errors says
	// "something failed"; only this says which edge stopped learning.
	out := logs.String()
	if !strings.Contains(out, "association weight read failed") {
		t.Errorf("no log line for the unreadable pair; captured logs:\n%s", out)
	}
	if !strings.Contains(out, "03000000000000000000000000000000") {
		t.Errorf("the log line does not name the unreadable pair's source id; captured logs:\n%s", out)
	}
	if !strings.Contains(out, "0404040404040404") {
		t.Errorf("the log line does not name the vault prefix; captured logs:\n%s", out)
	}
}

// TestProcessBatch_ReadFaultLogIsRateLimitedPerPair pins the other half of the
// same requirement. Because the fault is permanent and the worker flushes every
// HebbianPassInterval, an unguarded log line at this site trades a silent bug
// for a log flood: one damaged edge would emit a line a minute, forever. The
// pair is logged once; every later window still REPORTS through the returned
// error, which is counted rather than printed.
func TestProcessBatch_ReadFaultLogIsRateLimitedPerPair(t *testing.T) {
	store := newReadFaultStore()
	logs := captureLogs(t)

	ws := [8]byte{5, 5, 5, 5, 5, 5, 5, 5}
	idA := [16]byte{1}
	idB := [16]byte{2}
	idC := [16]byte{3}
	idD := [16]byte{4}
	store.failSrc[idC] = true

	hw := NewHebbianWorkerWithDB(store, nil, nil)
	defer hw.Stop()

	const windows = 4
	for i := 0; i < windows; i++ {
		err := hw.processBatch(context.Background(), twoPairBatch(ws, idA, idB, idC, idD))
		if err == nil {
			t.Fatalf("window %d: processBatch returned nil despite the permanent read fault — "+
				"suppressing the LOG must not suppress the REPORT", i)
		}
		if !errors.Is(err, errReadFailed) {
			t.Fatalf("window %d: error does not wrap the injected read failure: %v", i, err)
		}
	}

	got := strings.Count(logs.String(), "association weight read failed")
	if got != 1 {
		t.Errorf("the same permanently unreadable pair logged %d time(s) across %d flush windows, want 1 — "+
			"a corrupt block never heals, so an unguarded log at this site is a flood", got, windows)
	}
}
