package storage

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	transitionFlushInterval = 30 * time.Second
	transitionEvictAge      = 5 * time.Minute
)

// transitionKey is the compound cache key: ws(8) + src(16) + dst(16) = 40 bytes.
type transitionKey [40]byte

// sourceKey is the per-source tracking key: ws(8) + src(16) = 24 bytes.
type sourceKey [24]byte

func makeTransitionKey(ws [8]byte, src, dst [16]byte) transitionKey {
	var k transitionKey
	copy(k[0:8], ws[:])
	copy(k[8:24], src[:])
	copy(k[24:40], dst[:])
	return k
}

func makeSourceKey(ws [8]byte, src [16]byte) sourceKey {
	var k sourceKey
	copy(k[0:8], ws[:])
	copy(k[8:24], src[:])
	return k
}

// sourceEntry tracks load state and last-read time for eviction.
type sourceEntry struct {
	lastRead atomic.Int64 // unix nanos
}

// TransitionCache is a read-through cache for PAS transition counts.
// The in-memory layer is the single source of truth during runtime.
// Pebble provides durability via periodic flushes.
type TransitionCache struct {
	counts sync.Map // transitionKey → *atomic.Uint32
	loaded sync.Map // sourceKey → *sourceEntry
	dirty  sync.Map // sourceKey → struct{}

	store  TransitionStore
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewTransitionCache creates a cache backed by the given store and starts
// the periodic flush goroutine.
func NewTransitionCache(store TransitionStore) *TransitionCache {
	tc := &TransitionCache{
		store:  store,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go tc.flushLoop()
	return tc
}

// Incr atomically increments the transition count for (ws, src, dst) by 1.
// O(1) — never touches Pebble.
func (tc *TransitionCache) Incr(ws [8]byte, src, dst [16]byte) {
	tc.IncrBy(ws, src, dst, 1)
}

// IncrBy atomically increments the transition count for (ws, src, dst) by n.
// O(1) — never touches Pebble. If the source hasn't been loaded from Pebble
// yet, the delta is tracked and merged on the next GetTopTransitions call.
func (tc *TransitionCache) IncrBy(ws [8]byte, src, dst [16]byte, n uint32) {
	tk := makeTransitionKey(ws, src, dst)
	val, _ := tc.counts.LoadOrStore(tk, &atomic.Uint32{})
	val.(*atomic.Uint32).Add(n)

	sk := makeSourceKey(ws, src)
	tc.dirty.Store(sk, struct{}{})
}

// GetTopTransitions returns the top-K transition targets for a source engram.
// Uses the read-through cache: warm sources are served entirely from memory;
// cold sources are loaded from Pebble on first access.
func (tc *TransitionCache) GetTopTransitions(ctx context.Context, ws [8]byte, srcID [16]byte, topK int) ([]TransitionTarget, error) {
	if topK <= 0 {
		return nil, nil
	}

	sk := makeSourceKey(ws, srcID)

	// Touch last-read timestamp for eviction tracking.
	if v, ok := tc.loaded.Load(sk); ok {
		v.(*sourceEntry).lastRead.Store(time.Now().UnixNano())
	} else {
		// Cold miss — load from Pebble.
		if err := tc.loadSource(ctx, ws, srcID); err != nil {
			return nil, err
		}
	}

	return tc.scanSource(ws, srcID, topK), nil
}

// loadSource reads all transition targets for a source from Pebble and
// populates the in-memory cache. Any pre-existing in-memory deltas
// (from Incr calls before the source was loaded) are preserved.
func (tc *TransitionCache) loadSource(ctx context.Context, ws [8]byte, srcID [16]byte) error {
	sk := makeSourceKey(ws, srcID)
	se := &sourceEntry{}
	se.lastRead.Store(time.Now().UnixNano())
	if _, alreadyLoaded := tc.loaded.LoadOrStore(sk, se); alreadyLoaded {
		return nil
	}

	targets, err := tc.store.GetTopTransitions(ctx, ws, srcID, math.MaxInt32)
	if err != nil {
		tc.loaded.Delete(sk)
		return err
	}

	for _, t := range targets {
		tk := makeTransitionKey(ws, srcID, t.ID)
		newVal := &atomic.Uint32{}
		newVal.Store(t.Count)
		existing, loaded := tc.counts.LoadOrStore(tk, newVal)
		if loaded {
			existing.(*atomic.Uint32).Add(t.Count)
		}
	}
	return nil
}

// scanSource iterates the in-memory counts for a single source and returns
// the top-K targets sorted by count descending.
func (tc *TransitionCache) scanSource(ws [8]byte, srcID [16]byte, topK int) []TransitionTarget {
	var prefix transitionKey
	copy(prefix[0:8], ws[:])
	copy(prefix[8:24], srcID[:])

	var targets []TransitionTarget
	tc.counts.Range(func(key, val any) bool {
		tk := key.(transitionKey)
		if tk[0] != prefix[0] || tk[1] != prefix[1] || tk[2] != prefix[2] || tk[3] != prefix[3] ||
			tk[4] != prefix[4] || tk[5] != prefix[5] || tk[6] != prefix[6] || tk[7] != prefix[7] {
			return true
		}
		for i := 8; i < 24; i++ {
			if tk[i] != prefix[i] {
				return true
			}
		}
		count := val.(*atomic.Uint32).Load()
		if count == 0 {
			return true
		}
		var dstID [16]byte
		copy(dstID[:], tk[24:40])
		targets = append(targets, TransitionTarget{ID: dstID, Count: count})
		return true
	})

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Count > targets[j].Count
	})

	if len(targets) > topK {
		targets = targets[:topK]
	}
	if len(targets) == 0 {
		return nil
	}
	return targets
}

// flushLoop runs the periodic flush ticker.
func (tc *TransitionCache) flushLoop() {
	defer close(tc.doneCh)
	ticker := time.NewTicker(transitionFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-tc.stopCh:
			tc.flushAndEvict()
			return
		case <-ticker.C:
			tc.flushAndEvict()
		}
	}
}

// flushAndEvict writes dirty sources to Pebble and evicts cold entries.
func (tc *TransitionCache) flushAndEvict() {
	defer func() {
		if r := recover(); r != nil {
			// Swallow closed-DB panics from Pebble during engine shutdown.
			if IsClosedPanic(r) {
				return
			}
			slog.Error("transition cache: flush panicked", "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tc.Flush(ctx); err != nil {
		slog.Warn("transition cache flush failed, skipping eviction", "error", err)
		return
	}
	tc.evictCold()
}

// Flush writes all dirty in-memory transition counts to Pebble.
// Uses SetTransitionBatch (overwrite) since the cache holds merged totals.
func (tc *TransitionCache) Flush(ctx context.Context) error {
	var dirtySources []sourceKey
	tc.dirty.Range(func(key, _ any) bool {
		dirtySources = append(dirtySources, key.(sourceKey))
		return true
	})

	if len(dirtySources) == 0 {
		return nil
	}

	var sets []TransitionSet
	for _, sk := range dirtySources {
		var ws [8]byte
		var srcID [16]byte
		copy(ws[:], sk[0:8])
		copy(srcID[:], sk[8:24])

		tc.counts.Range(func(key, val any) bool {
			tk := key.(transitionKey)
			for i := 0; i < 24; i++ {
				if tk[i] != sk[i] {
					return true
				}
			}
			count := val.(*atomic.Uint32).Load()
			if count == 0 {
				return true
			}
			var dst [16]byte
			copy(dst[:], tk[24:40])
			sets = append(sets, TransitionSet{
				WS:    ws,
				Src:   srcID,
				Dst:   dst,
				Count: count,
			})
			return true
		})
	}

	if len(sets) == 0 {
		return nil
	}
	if err := tc.store.SetTransitionBatch(ctx, sets); err != nil {
		return err
	}
	for _, sk := range dirtySources {
		tc.dirty.Delete(sk)
	}
	return nil
}

// evictCold removes entries for sources that haven't been read recently
// and have no unflushed dirty data.
func (tc *TransitionCache) evictCold() {
	cutoff := time.Now().Add(-transitionEvictAge).UnixNano()

	tc.loaded.Range(func(key, val any) bool {
		sk := key.(sourceKey)
		se := val.(*sourceEntry)

		if se.lastRead.Load() > cutoff {
			return true
		}
		if _, isDirty := tc.dirty.Load(sk); isDirty {
			return true
		}

		tc.counts.Range(func(ck, _ any) bool {
			tk := ck.(transitionKey)
			for i := 0; i < 24; i++ {
				if tk[i] != sk[i] {
					return true
				}
			}
			tc.counts.Delete(ck)
			return true
		})

		// A concurrent Incr may have dirtied this source while we were
		// deleting counts. Re-mark as loaded so the dirty data isn't orphaned.
		if _, becameDirty := tc.dirty.Load(sk); becameDirty {
			tc.loaded.Store(key, &sourceEntry{})
			return true
		}
		tc.loaded.Delete(key)
		return true
	})
}

// Close signals the flush goroutine to stop, performs a final flush,
// and blocks until the goroutine exits.
func (tc *TransitionCache) Close() {
	select {
	case <-tc.stopCh:
		return
	default:
		close(tc.stopCh)
	}
	<-tc.doneCh
}
