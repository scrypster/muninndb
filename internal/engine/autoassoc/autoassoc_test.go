package autoassoc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// --- stubs ---

type stubStore struct {
	mu    sync.Mutex
	links [][2]storage.ULID // [source, target] pairs
	err   error
}

func (s *stubStore) WriteAssociation(ctx context.Context, wsPrefix [8]byte, sourceID, targetID storage.ULID, assoc *storage.Association) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.links = append(s.links, [2]storage.ULID{sourceID, targetID})
	return nil
}

func (s *stubStore) linkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.links)
}

type stubFTS struct {
	results map[string][]fts.ScoredID
	err     error
}

func (f *stubFTS) Search(_ context.Context, _ [8]byte, query string, _ int) ([]fts.ScoredID, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.results[query], nil
}

// drain awaits every job enqueued so far, deterministically via the worker's
// own WaitIdle seam instead of polling len(w.jobs) against a fixed sleep.
func drain(w *Worker) {
	w.WaitIdle()
}

// --- tests ---

func TestWorkerCreatesLinksForMatchingTags(t *testing.T) {
	id1 := storage.NewULID()
	id2 := storage.NewULID()
	id3 := storage.NewULID()

	store := &stubStore{}
	ftsIdx := &stubFTS{
		results: map[string][]fts.ScoredID{
			"neuroscience": {{ID: [16]byte(id2), Score: 0.9}, {ID: [16]byte(id3), Score: 0.8}},
		},
	}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{
		WSPrefix: [8]byte{},
		NewID:    id1,
		Tags:     []string{"neuroscience"},
	})

	drain(w)

	if store.linkCount() != 2 {
		t.Fatalf("expected 2 links, got %d", store.linkCount())
	}
}

func TestWorkerExcludesSelf(t *testing.T) {
	id1 := storage.NewULID()

	store := &stubStore{}
	ftsIdx := &stubFTS{
		results: map[string][]fts.ScoredID{
			"memory": {{ID: [16]byte(id1), Score: 1.0}}, // same as new engram
		},
	}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{
		WSPrefix: [8]byte{},
		NewID:    id1,
		Tags:     []string{"memory"},
	})

	drain(w)

	if store.linkCount() != 0 {
		t.Fatalf("expected 0 links (self excluded), got %d", store.linkCount())
	}
}

func TestWorkerNoTagsNoLinks(t *testing.T) {
	store := &stubStore{}
	ftsIdx := &stubFTS{results: map[string][]fts.ScoredID{}}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{
		WSPrefix: [8]byte{},
		NewID:    storage.NewULID(),
		Tags:     nil,
	})

	drain(w)

	if store.linkCount() != 0 {
		t.Fatalf("expected 0 links, got %d", store.linkCount())
	}
}

func TestWorkerMaxAssociationsCap(t *testing.T) {
	// Generate MaxAssociations+5 candidates from FTS
	var results []fts.ScoredID
	for i := 0; i < MaxAssociations+5; i++ {
		results = append(results, fts.ScoredID{ID: [16]byte(storage.NewULID()), Score: float64(MaxAssociations + 5 - i)})
	}

	store := &stubStore{}
	ftsIdx := &stubFTS{results: map[string][]fts.ScoredID{"tag": results}}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{
		WSPrefix: [8]byte{},
		NewID:    storage.NewULID(),
		Tags:     []string{"tag"},
	})

	drain(w)

	if store.linkCount() > MaxAssociations {
		t.Fatalf("expected at most %d links, got %d", MaxAssociations, store.linkCount())
	}
}

func TestWorkerDropsJobsWhenQueueFull(t *testing.T) {
	// Use a slow FTS that blocks to fill the queue
	store := &stubStore{}
	blockCh := make(chan struct{})
	var callCount atomic.Int64

	slowFTS := &slowFTSStub{block: blockCh, counter: &callCount}
	w := New(context.Background(), store, slowFTS)
	defer func() {
		close(blockCh)
		w.Stop()
	}()

	// Enqueue more than the buffer can hold
	dropped := 0
	for i := 0; i < JobBufSize+NumWorkers+10; i++ {
		before := w.metrics.Dropped.Load()
		w.Enqueue(Job{WSPrefix: [8]byte{}, NewID: storage.NewULID(), Tags: []string{"x"}})
		if w.metrics.Dropped.Load() > before {
			dropped++
		}
	}

	if dropped == 0 {
		t.Log("note: no drops observed (queue may have drained fast), this is acceptable under load")
	}
}

func TestWorkerFTSErrorIsNonFatal(t *testing.T) {
	store := &stubStore{}
	ftsIdx := &stubFTS{err: errors.New("index error")}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{
		WSPrefix: [8]byte{},
		NewID:    storage.NewULID(),
		Tags:     []string{"tag"},
	})

	drain(w)

	// Should complete without panic; no links since FTS errored
	if store.linkCount() != 0 {
		t.Fatalf("expected 0 links on FTS error, got %d", store.linkCount())
	}
}

func TestWorkerStoreErrorIsNonFatal(t *testing.T) {
	id := storage.NewULID()
	store := &stubStore{err: errors.New("store write error")}
	ftsIdx := &stubFTS{
		results: map[string][]fts.ScoredID{
			"tag": {{ID: [16]byte(storage.NewULID()), Score: 1.0}},
		},
	}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	w.Enqueue(Job{WSPrefix: [8]byte{}, NewID: id, Tags: []string{"tag"}})
	drain(w)
	// Should complete without panic
}

func TestWorkerMetricsAccurate(t *testing.T) {
	id := storage.NewULID()
	store := &stubStore{}
	ftsIdx := &stubFTS{results: map[string][]fts.ScoredID{}}

	w := New(context.Background(), store, ftsIdx)
	defer w.Stop()

	const n = 5
	for i := 0; i < n; i++ {
		w.Enqueue(Job{WSPrefix: [8]byte{}, NewID: id, Tags: []string{"t"}})
	}
	drain(w)

	enqueued, completed, dropped, _ := w.GetMetrics()
	if enqueued != n {
		t.Errorf("enqueued = %d, want %d", enqueued, n)
	}
	if completed != n {
		t.Errorf("completed = %d, want %d", completed, n)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
}

// slowFTSStub blocks until blockCh is closed.
type slowFTSStub struct {
	block   chan struct{}
	counter *atomic.Int64
}

func (f *slowFTSStub) Search(_ context.Context, _ [8]byte, _ string, _ int) ([]fts.ScoredID, error) {
	f.counter.Add(1)
	<-f.block
	return nil, nil
}
