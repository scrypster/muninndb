package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// stateIndexStates returns every lifecycle state that has a 0x0B state-index
// entry for id (a consistent store has exactly one, equal to the record's).
func stateIndexStates(t *testing.T, s *PebbleStore, ws [8]byte, id ULID) []LifecycleState {
	t.Helper()
	var out []LifecycleState
	for st := 0; st < 256; st++ {
		v, err := Get(s.db, keys.StateIndexKey(ws, uint8(st), [16]byte(id)))
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			out = append(out, LifecycleState(st))
		}
	}
	return out
}

// restoreLikeEngine is Engine.Restore's storage write: UpdateMetadata to
// active, which takes its oldState (and so whether to move the 0x0B entry)
// from GetMetadata — i.e. from metaCache when it is warm.
func restoreLikeEngine(t *testing.T, s *PebbleStore, ws [8]byte, id ULID) {
	t.Helper()
	ctx := context.Background()
	e, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != StateSoftDeleted {
		t.Fatalf("restore precondition: engram reads %v, want soft_deleted", e.State)
	}
	if err := s.UpdateMetadata(ctx, ws, id, &EngramMeta{
		State: StateActive, Confidence: e.Confidence, Relevance: e.Relevance, Stability: e.Stability,
		AccessCount: e.AccessCount, UpdatedAt: e.UpdatedAt, LastAccess: e.LastAccess,
	}); err != nil {
		t.Fatal(err)
	}
}

// Reproduction 4 (metadata cache): a recall pinned before a forget reads the
// metadata through its snapshot after the forget committed and the engram's
// L1 entry was evicted. The stale "active" it cached became UpdateMetadata's
// oldState on the next restore, which then skipped the 0x0B state-index move:
// the record said active while the index still said soft_deleted.
func TestCacheStaleFill_ForgetSnapshotMetadataRestore_StateIndexAgrees(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "stale-meta-restore", 0.5)

	snapCtx, snap := s.NewSnapshotContext(ctx) // recall in flight
	defer snap.Close()

	if err := s.SoftDelete(ctx, ws, id); err != nil { // forget commits
		t.Fatal(err)
	}
	s.cache.Delete(ws, id) // the engram's L1 entry is evicted (small cache)

	if _, err := s.GetMetadata(snapCtx, ws, []ULID{id}); err != nil { // the recall's metadata read
		t.Fatal(err)
	}

	restoreLikeEngine(t, s, ws, id)

	metas, err := s.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || metas[0] == nil {
		t.Fatal(err)
	}
	if metas[0].State != StateActive {
		t.Fatalf("after restore GetMetadata reads %v, want active", metas[0].State)
	}
	if got := stateIndexStates(t, s, ws, id); len(got) != 1 || got[0] != StateActive {
		t.Fatalf("state index diverged from the record after restore: index holds %v, record says active", got)
	}
}

// The association caches are filled from snapshot-aware scans too. A recall
// pinned before an edge-weight write must not leave the old weight cached.
func TestCacheStaleFill_SnapshotAssociationReadDoesNotOverwriteNewerCommit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := s.VaultPrefix("stale-assoc")
	src, dst := NewULID(), NewULID()
	seedEndpoints(t, s, ws, src, dst)
	if err := s.WriteAssociation(ctx, ws, src, dst, &Association{TargetID: dst, Weight: 0.2, RelType: RelRelatesTo, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	s.assocCache.Purge()
	s.revAssocCache.Purge()

	snapCtx, snap := s.NewSnapshotContext(ctx)
	defer snap.Close()
	if err := s.UpdateAssocWeight(ctx, ws, src, dst, 0.9, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAssociations(snapCtx, ws, []ULID{src}, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAssociations(ctx, ws, []ULID{src}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[src]) != 1 || got[src][0].Weight < 0.89 {
		t.Fatalf("stale association cache fill: live GetAssociations returned %+v after a committed weight of 0.9", got[src])
	}
}

// TestNoRawBatchCommitsInStorage pins the structural half of the guard:
// commitBatch is the only place a raw Pebble batch is committed in this
// package, so no write path can commit an engram or association key without
// bumping the fill guard and invalidating the caches. Direct single-key
// writes on the DB must never target a cache-backing key.
func TestNoRawBatchCommitsInStorage(t *testing.T) {
	allow := map[string]string{
		"cache_commit.go":    "the wrapper itself",
		"write_batcher.go":   "not constructed by PebbleStore (tests only); writes caller-supplied keys",
		"evolve_repair.go":   "commits a StoreBatch (pebbleStoreBatch.Commit routes through commitBatch)",
		"cache_fill_seam.go": "",
	}
	rawCommit := regexp.MustCompile(`\.Commit\(`)
	directCacheKeyWrite := regexp.MustCompile(`db\.(Set|Delete|SingleDelete|Merge|DeleteRange)\(.*\bkeys\.(EngramKey|MetaKey|AssocFwdKey|AssocRevKey)\(`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if _, ok := allow[f]; !ok && rawCommit.MatchString(code) && !strings.Contains(code, "commitBatch(") {
				t.Errorf("%s:%d commits a batch without commitBatch (the cache fill guard would be skipped): %s", f, i+1, strings.TrimSpace(line))
			}
			if directCacheKeyWrite.MatchString(code) {
				t.Errorf("%s:%d writes a cache-backing key directly on the DB: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// A commit that APPLIED but reported an error must still invalidate: the
// guard fails safe. Without it the cache keeps serving the pre-write value
// that Pebble no longer holds.
func TestCommitBatch_InvalidatesEvenWhenCommitReportsError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "commit-error", 0.20)
	if e, err := s.GetEngram(ctx, ws, id); err != nil || e.Relevance != 0.20 {
		t.Fatalf("precondition: %v %v", e, err) // now cached
	}

	inject := func(err error) error {
		if err != nil {
			return err
		}
		return errors.New("injected: commit applied, then reported failure")
	}
	commitResultForTest.Store(s, inject)
	// The StoreBatch path (evolve's supersede) does no invalidation of its own
	// before the commit, so commitBatch is the only thing that can drop the
	// cached pre-write copy.
	b := s.NewBatch()
	if err := b.SupersedeEngram(ctx, ws, id, time.Now()); err != nil {
		commitResultForTest.Delete(s)
		t.Fatal(err)
	}
	err := b.Commit()
	commitResultForTest.Delete(s)
	if err == nil {
		t.Fatal("injection did not reach the commit")
	}

	e, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != StateSoftDeleted {
		t.Fatalf("cache served state %v after a commit that applied soft_deleted but returned an error (invalidation skipped on the error path)", e.State)
	}
}
