package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The L1 cache's per-engram access time is a recency SCORING input for later
// recalls (EngramLastAccessNs). A read that is declared side-effect free —
// muninn_read with read_only, any read under an observe credential,
// muninn_where_left_off, muninn_explain — must not stamp it, or the read
// itself re-ranks the next recall. Fixtures are invented (trail maintenance).

// coldEngram writes an aged engram at the store level, so nothing has loaded
// it into the L1 cache yet (EngramLastAccessNs == 0).
func coldEngram(t *testing.T, eng *Engine, vault, concept, content string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	ws := eng.store.ResolveVaultPrefix(vault)
	old := time.Now().Add(-400 * 24 * time.Hour)
	id, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: concept, Content: content, Confidence: 1,
		CreatedAt: old, UpdatedAt: old, LastAccess: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.fts.IndexEngram(ws, [16]byte(id), concept, "", content, nil); err != nil {
		t.Fatal(err)
	}
	if got := eng.store.EngramLastAccessNs(ws, id); got != 0 {
		t.Fatalf("fixture: cold engram already stamped (%d)", got)
	}
	return id
}

func TestReadOnlyReadPaths_DoNotStampCacheRecency(t *testing.T) {
	ctx := context.Background()
	observe := context.WithValue(ctx, auth.ContextMode, "observe")
	for name, read := range map[string]func(eng *Engine, vault string, id storage.ULID) error{
		"read read_only=true": func(eng *Engine, vault string, id storage.ULID) error {
			_, err := eng.Read(ctx, &mbp.ReadRequest{Vault: vault, ID: id.String(), ReadOnly: true})
			return err
		},
		"read under observe credential": func(eng *Engine, vault string, id storage.ULID) error {
			_, err := eng.Read(observe, &mbp.ReadRequest{Vault: vault, ID: id.String()})
			return err
		},
		"where_left_off": func(eng *Engine, vault string, _ storage.ULID) error {
			_, err := eng.WhereLeftOff(ctx, vault, 10, nil)
			return err
		},
		"explain": func(eng *Engine, vault string, id storage.ULID) error {
			_, err := eng.Explain(ctx, vault, id.String(), []string{"waterbar drainage"}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			eng, cleanup := testEnv(t)
			defer cleanup()
			const vault = "trail-readonly"
			id := coldEngram(t, eng, vault, "waterbar drainage", "the waterbar on switchback four drains to the left")
			awaitFTS(t, eng)
			if err := read(eng, vault, id); err != nil {
				t.Fatal(err)
			}
			eng.WaitWriteTimeIdle()
			if got := eng.store.EngramLastAccessNs(eng.store.ResolveVaultPrefix(vault), id); got != 0 {
				t.Fatalf("%s stamped the cache access time (%d): a side-effect-free read changed a recency scoring input", name, got)
			}
		})
	}
}

// TestReadOnlyRead_DoesNotChangeTheNextReadOnlyRecall is the user-visible
// form: an identical read-only recall must return exactly what it returned
// before a read-only read of one of its candidates.
func TestReadOnlyRead_DoesNotChangeTheNextReadOnlyRecall(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "trail-rank"
	for i := 0; i < 6; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault,
			Concept: fmt.Sprintf("blowdown cleared on segment %d", i),
			Content: fmt.Sprintf("crew cleared blowdown on segment %d with a crosscut saw", i)}); err != nil {
			t.Fatal(err)
		}
	}
	target := coldEngram(t, eng, vault, "crosscut saw sharpening", "the crosscut saw was sharpened before the blowdown season")
	awaitFTS(t, eng)
	eng.WaitWriteTimeIdle()

	recall := func() string {
		t.Helper()
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"crosscut saw blowdown"}, MaxResults: 10, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, a := range resp.Activations {
			out += fmt.Sprintf("%s:%.6f ", a.ID, a.Score)
		}
		return out
	}
	before := recall()
	if again := recall(); again != before {
		t.Skipf("control failed: two read-only recalls differ with no read between them (a separate read-only-recall defect, not this one):\n%s\n%s", before, again)
	}
	if _, err := eng.Read(ctx, &mbp.ReadRequest{Vault: vault, ID: target.String(), ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	eng.WaitWriteTimeIdle()
	if after := recall(); after != before {
		t.Fatalf("a read-only read changed the next read-only recall:\nbefore %s\nafter  %s", before, after)
	}
}
