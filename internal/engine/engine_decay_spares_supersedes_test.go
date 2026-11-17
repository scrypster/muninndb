package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// A supersedes edge through the real engine: association decay must never move
// it, archive it or erase it, because the fork guard, Link and every inline
// supersedes read it to know the target is taken. When decay did, the target
// looked free, a rival was linked, and a recall's lazy restore then gave the
// target two successors for good.
//
// Fixtures are invented (an orchard).

type decayEnv struct {
	t     *testing.T
	eng   *Engine
	store *storage.PebbleStore
	as    *auth.Store
	db    *pebble.DB
	ctx   context.Context
	ws    [8]byte
}

func newDecayEnv(t *testing.T) (*decayEnv, func()) {
	t.Helper()
	db := openMemPebble(t)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	as := auth.NewStore(db)
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	delay := time.Millisecond
	eng := NewEngine(EngineConfig{
		Store: store, AuthStore: as, FTSIndex: ftsIdx, Embedder: embedder,
		ActivationEngine:       activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder),
		TriggerSystem:          trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder),
		AssocWeightRepairDelay: &delay,
	})
	// The startup repair ran on the empty store; tests run the pass again
	// themselves once their fixtures are in place.
	<-eng.assocWeightRepairExited
	return &decayEnv{t: t, eng: eng, store: store, as: as, db: db, ctx: context.Background(), ws: store.ResolveVaultPrefix("default")},
		func() {
			eng.Stop()
			store.Close()
		}
}

func (d *decayEnv) write(content string) storage.ULID {
	d.t.Helper()
	r, err := d.eng.Write(d.ctx, &mbp.WriteRequest{Vault: "default", Concept: "orchard rule", Content: content})
	if err != nil {
		d.t.Fatal(err)
	}
	id, _ := storage.ParseULID(r.ID)
	return id
}

// declared writes what a supersedes link leaves (the target's stamp and the
// edge), with the edge's last activation idle in the past: the state an
// ordinary store reaches months after the link.
func (d *decayEnv) declared(src, dst storage.ULID, rel storage.RelType, w float32, idle time.Duration) {
	d.t.Helper()
	last := time.Now().Add(-idle)
	if rel == storage.RelSupersedes {
		if _, err := d.store.StampValidUntil(d.ctx, d.ws, dst, last, true); err != nil {
			d.t.Fatal(err)
		}
	}
	if err := d.store.WriteAssociation(d.ctx, d.ws, src, dst, &storage.Association{
		TargetID: dst, RelType: rel, Weight: w, Confidence: 1, CreatedAt: last, LastActivated: int32(last.Unix()),
	}); err != nil {
		d.t.Fatal(err)
	}
}

func (d *decayEnv) decay() {
	d.t.Helper()
	if !d.eng.assocWeightRepairComplete() {
		d.t.Fatal("the decay gate is shut")
	}
	d.eng.decayAllVaults(d.ctx, []string{"default"})
}

func (d *decayEnv) superseders(id storage.ULID) []storage.ULID {
	d.t.Helper()
	s, err := d.store.SupersedersOf(d.ctx, d.ws, id)
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

func (d *decayEnv) rivalRefused(target storage.ULID) {
	d.t.Helper()
	rival := d.write("A rival note that claims to replace the old orchard rule.")
	_, err := d.eng.Link(d.ctx, &mbp.LinkRequest{Vault: "default", SourceID: rival.String(), TargetID: target.String(), RelType: uint16(storage.RelSupersedes), Weight: 0.8})
	if !errors.Is(err, ErrEvolveTargetSuperseded) {
		d.t.Errorf("a rival supersedes link onto the taken target: err = %v, want ErrEvolveTargetSuperseded", err)
	}
}

// TestDecay_ASupersedesLinkOutlivesDecay (the refute's probe B, end to end): a
// supersedes link declared just above the floor and idle a day was ARCHIVED by
// one decay pass. The target then had no successor, a rival link was admitted,
// and a recall of the first successor restored the archived edge: two
// successors, permanently.
//
// RED on develop: SupersedersOf is empty after the pass, the rival is linked,
// and the restore forks the target.
func TestDecay_ASupersedesLinkOutlivesDecay(t *testing.T) {
	d, cleanup := newDecayEnv(t)
	defer cleanup()
	target := d.write("Apples are picked when the seeds turn brown.")
	succ := d.write("Apples are picked when the starch test reads six.")
	d.declared(succ, target, storage.RelSupersedes, 0.051, 24*time.Hour+time.Minute)

	d.decay()
	if got := d.superseders(target); len(got) != 1 || got[0] != succ {
		t.Fatalf("after one decay pass SupersedersOf(target) = %v, want [%s]", got, succ)
	}
	d.rivalRefused(target)
	if _, err := d.store.RestoreArchivedEdgesTransitive(d.ctx, d.ws, succ, 10, 5); err != nil {
		t.Fatal(err)
	}
	if got := d.superseders(target); len(got) != 1 || got[0] != succ {
		t.Errorf("after a recall's restore SupersedersOf(target) = %v, want only %s", got, succ)
	}
}

// archivedSupersedes writes the 0x25 row an older binary's decay pass left for
// a supersedes edge it archived (layout: relType(2) | confidence(4) |
// createdAt(8) | lastActivated(4) | peakWeight(4) | coActivationCount(4) |
// restoredAt(4)), and the target's stamp the original link made.
func (d *decayEnv) archivedSupersedes(src, dst storage.ULID, peak float32) {
	d.t.Helper()
	last := time.Now().Add(-125 * 24 * time.Hour)
	if _, err := d.store.StampValidUntil(d.ctx, d.ws, dst, last, true); err != nil {
		d.t.Fatal(err)
	}
	var v [30]byte
	binary.BigEndian.PutUint16(v[0:2], uint16(storage.RelSupersedes))
	binary.BigEndian.PutUint32(v[2:6], math.Float32bits(1))
	binary.BigEndian.PutUint64(v[6:14], uint64(last.Add(-time.Hour).UnixNano()))
	binary.BigEndian.PutUint32(v[14:18], uint32(int32(last.Unix())))
	binary.BigEndian.PutUint32(v[18:22], math.Float32bits(peak))
	binary.BigEndian.PutUint32(v[22:26], 13)
	if err := d.db.Set(keys.ArchiveAssocKey(d.ws, [16]byte(src), [16]byte(dst)), v[:], pebble.Sync); err != nil {
		d.t.Fatal(err)
	}
}

func (d *decayEnv) archived(src, dst storage.ULID) bool {
	d.t.Helper()
	v, err := storage.Get(d.db, keys.ArchiveAssocKey(d.ws, [16]byte(src), [16]byte(dst)))
	if err != nil {
		d.t.Fatal(err)
	}
	return v != nil
}

type warnCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *warnCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *warnCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *warnCapture) WithGroup(string) slog.Handler            { return c }
func (c *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	c.mu.Lock()
	c.msgs = append(c.msgs, b.String())
	c.mu.Unlock()
	return nil
}

// TestDecay_RestoresSupersedesEdgesAnOlderDecayArchived: stores written by an
// older binary can hold supersedes edges its decay archived. The leader's first
// decay pass over a vault the startup association repair has marked puts each
// one back, under the target's evolve lock, when nothing else has taken the
// target since. When a rival has (the store holds two declarations for one
// memory), it picks neither: the row stays archived, the rival stays live, and
// a WARN names all three memories. Then the vault is marked (0x31, its own
// key, not a value of 0x2E), and it is done: a later pass does not run it
// again. A follower decides nothing: it receives the leader's batches. A vault
// the startup pass has not marked yet waits for it (its legacy repair comes
// first).
//
// RED on develop: nothing reads 0x25 for supersedes rows, so the free target
// keeps no successor and a rival link is accepted.
func TestDecay_RestoresSupersedesEdgesAnOlderDecayArchived(t *testing.T) {
	for _, c := range []struct {
		name        string
		mark        uint8
		follower    bool
		decayOff    bool
		wantRestore bool
		wantMark    uint8
	}{
		{"the leader, after the startup pass", assocWeightRepairVersion, false, false, true, supersedesRestoreVersion},
		{"the leader, in a vault whose decay is switched off", assocWeightRepairVersion, false, true, true, supersedesRestoreVersion},
		{"a follower", assocWeightRepairVersion, true, false, false, 0},
		{"a vault the startup pass has not marked", 0, false, false, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, cleanup := newDecayEnv(t)
			defer cleanup()
			freeT, freeS := d.write("Pruning starts in late February."), d.write("Pruning starts once the buds swell.")
			heldT, heldS := d.write("Mulch is spread in October."), d.write("Mulch is spread in November.")
			rival := d.write("Mulch is spread in March, after the thaw.")
			d.archivedSupersedes(freeS, freeT, 0.8)
			d.archivedSupersedes(heldS, heldT, 0.8)
			if err := d.store.WriteAssociation(d.ctx, d.ws, rival, heldT, &storage.Association{
				TargetID: heldT, RelType: storage.RelSupersedes, Weight: 0.8, Confidence: 1, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := d.store.SetAssocWeightRepairMark(d.ctx, d.ws, c.mark); err != nil {
				t.Fatal(err)
			}
			if c.decayOff {
				off := false
				if err := d.as.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true,
					Plasticity: &auth.PlasticityConfig{HebbianEnabled: &off}}); err != nil {
					t.Fatal(err)
				}
			}
			if c.follower {
				d.eng.SetWriteGate(func() error { return errors.New("this node is a follower") })
			}
			logs := &warnCapture{}
			prev := slog.Default()
			slog.SetDefault(slog.New(logs))
			d.decay()
			slog.SetDefault(prev)

			got := d.superseders(freeT)
			if c.wantRestore {
				if len(got) != 1 || got[0] != freeS {
					t.Errorf("a free target: SupersedersOf = %v, want the archived successor %s back", got, freeS)
				}
				if d.archived(freeS, freeT) {
					t.Errorf("a free target: the archive row is still there after its edge was restored")
				}
				d.eng.SetWriteGate(nil)
				d.rivalRefused(freeT)
			} else {
				if len(got) != 0 || !d.archived(freeS, freeT) {
					t.Errorf("SupersedersOf = %v, archived %v: want nothing restored here", got, d.archived(freeS, freeT))
				}
			}

			if got := d.superseders(heldT); len(got) != 1 || got[0] != rival {
				t.Errorf("a rival-held target: SupersedersOf = %v, want only the rival %s (no fork made by the restore)", got, rival)
			}
			if !d.archived(heldS, heldT) {
				t.Errorf("a rival-held target: the archived declaration was removed, but it must be kept and reported")
			}
			warned := false
			for _, m := range logs.msgs {
				if strings.Contains(m, heldS.String()) && strings.Contains(m, heldT.String()) && strings.Contains(m, rival.String()) {
					warned = true
				}
			}
			if warned != c.wantRestore {
				t.Errorf("a WARN names the archived successor, the target and the rival: %v, want %v (%v)", warned, c.wantRestore, logs.msgs)
			}
			if mark, _ := d.store.GetSupersedesRestoreMark(d.ctx, d.ws); mark != c.wantMark {
				t.Errorf("restore watermark (0x31) = %d, want %d", mark, c.wantMark)
			}
			if mark, _ := d.store.GetAssocWeightRepairMark(d.ctx, d.ws); mark != c.mark {
				t.Errorf("the startup repair's watermark (0x2E) = %d, want it untouched at %d", mark, c.mark)
			}
			if !c.wantRestore {
				return
			}
			// Done means done: a row that reaches the vault after the restore
			// ran (an older leader's batch, a downgrade) is not restored by a
			// later pass. That is the named residual, pinned here so a change
			// to it is a decision.
			lateT, lateS := d.write("Grafting is done in April."), d.write("Grafting is done in early May.")
			d.archivedSupersedes(lateS, lateT, 0.8)
			d.decay()
			if got := d.superseders(lateT); len(got) != 0 || !d.archived(lateS, lateT) {
				t.Errorf("a second pass ran the restore again: SupersedersOf = %v, archived %v", got, d.archived(lateS, lateT))
			}
		})
	}
}

// TestRestoreArchivedSupersedes_HoldsTheTargetsEvolveLock: the one-off restore
// decides from the target's successors whether the archived edge may go back,
// and must hold the target's evolve lock (the lock every supersedes link of
// that target takes) until it has written it. Otherwise a rival link runs its
// own check between the restore's check and its write, sees the target free
// (the restore has not committed), and links: two successors. The refute's
// race probe forked 29 of 60 runs without the lock.
//
// The rival is landed at the storage seam inside the restore's window: inline
// when the evolve lock is free (the window is open), on a goroutine when it is
// held (the rival waits and is then refused).
//
// RED without the evolve lock in restoreArchivedSupersedes: two successors.
func TestRestoreArchivedSupersedes_HoldsTheTargetsEvolveLock(t *testing.T) {
	d, cleanup := newDecayEnv(t)
	defer cleanup()
	target := d.write("Pears are picked hard and ripened indoors.")
	succ := d.write("Pears are picked when the stalk lifts off the spur.")
	rival := d.write("Pears are picked when they yield at the neck, a rival note.")
	d.archivedSupersedes(succ, target, 0.8)

	var (
		once    sync.Once
		wg      sync.WaitGroup
		linkErr error
		ran     bool
	)
	link := func() error {
		_, err := d.eng.Link(d.ctx, &mbp.LinkRequest{Vault: "default", SourceID: rival.String(), TargetID: target.String(), RelType: uint16(storage.RelSupersedes), Weight: 0.8})
		return err
	}
	defer storage.SetAssocGuardHookForTesting(func(op string, ws [8]byte, src, dst storage.ULID) {
		if op != "supersedes-restore" || src != succ || dst != target {
			return
		}
		once.Do(func() {
			ran = true
			mu := d.eng.evolveLock(ws, target)
			if mu.TryLock() {
				mu.Unlock()
				linkErr = link()
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); linkErr = link() }()
		})
	})()

	if err := d.eng.restoreArchivedSupersedes(d.ctx, "default", d.ws); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !ran {
		t.Fatal("the seam never fired: the test is not exercising the window")
	}
	if got := d.superseders(target); len(got) != 1 || got[0] != succ {
		t.Errorf("SupersedersOf(target) = %v, want only the restored successor %s", got, succ)
	}
	if !errors.Is(linkErr, ErrEvolveTargetSuperseded) {
		t.Errorf("the rival link: err = %v, want ErrEvolveTargetSuperseded", linkErr)
	}
}

// TestHebbian_NeverErasesASupersedesEdgeOnItsPair (the refute's probe I, end to
// end through engine Link, real recall and the real Hebbian worker): an agent
// declares that an OLDER memory supersedes a NEWER one (the original was right
// after all) at weight 1.0, and links the pair relates_to at 0.95 too. Recalls
// that show history (include_invalid) return both memories, so the Hebbian
// worker co-activates the pair. It moves the edge the pair's weight index
// points at (relates_to) up to 1.0, the key the supersedes edge holds. The move
// used to Set over it: the supersedes edge was gone and a rival was linked.
// With supersedes edges exempt from decay the declaration stays at 1.0 for
// good, so this would have been permanent.
//
// RED without the destination check in UpdateAssocWeightBatch: SupersedersOf
// is empty after the recalls and the rival is admitted.
func TestHebbian_NeverErasesASupersedesEdgeOnItsPair(t *testing.T) {
	eng, as, store, hw, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()
	ctx := withMode(auth.ModeFull)
	const vault = "studio"
	if err := as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatal(err)
	}
	write := func(c string) storage.ULID {
		r, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "glaze recipe", Content: c})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := storage.ParseULID(r.ID)
		return id
	}
	older := write("glaze recipe celadon: two percent iron oxide in the base")
	newer := write("glaze recipe celadon: one percent iron oxide in the base")
	if string(older[:]) >= string(newer[:]) {
		t.Fatal("fixture: ULIDs are not ordered older < newer")
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: older.String(), TargetID: newer.String(), RelType: uint16(storage.RelSupersedes), Weight: 1.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: older.String(), TargetID: newer.String(), RelType: uint16(storage.RelRelatesTo), Weight: 0.95}); err != nil {
		t.Fatal(err)
	}
	both := 0
	for i := 0; i < 80; i++ {
		eng.waitWriteTimeIdle()
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"glaze recipe celadon iron oxide"}, MaxResults: 10, IncludeInvalid: true})
		if err != nil {
			t.Fatal(err)
		}
		var o, n bool
		for _, a := range resp.Activations {
			o = o || a.ID == older.String()
			n = n || a.ID == newer.String()
		}
		if o && n {
			both++
		}
	}
	hw.Stop() // drains: every co-activation the recalls produced is applied
	if both == 0 {
		t.Fatal("no recall returned both memories: the Hebbian worker never saw the pair, so this test proves nothing")
	}
	ws := store.ResolveVaultPrefix(vault)
	if s, _ := store.SupersedersOf(context.Background(), ws, newer); len(s) != 1 || s[0] != older {
		t.Errorf("after %d co-activating recalls SupersedersOf(newer) = %v, want [older]", both, s)
	}
	rival := write("glaze recipe celadon: a rival note")
	_, err := eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: rival.String(), TargetID: newer.String(), RelType: uint16(storage.RelSupersedes), Weight: 0.8})
	if !errors.Is(err, ErrEvolveTargetSuperseded) {
		t.Errorf("a rival supersedes link onto newer: err = %v, want ErrEvolveTargetSuperseded", err)
	}
}
