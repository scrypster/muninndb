package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// keywordHolders returns the ids the keyword index matches for one word.
func keywordHolders(t *testing.T, eng *Engine, ws [8]byte, word string) map[string]bool {
	t.Helper()
	awaitFTS(t, eng)
	hits, err := eng.fts.Search(context.Background(), ws, word, 50)
	if err != nil {
		t.Fatalf("Search(%q): %v", word, err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[storage.ULID(h.ID).String()] = true
	}
	return got
}

// An evolved memory inherits its predecessor's tags, but the keyword index job
// for the new version left the tags out. A word that lives only in a tag then
// found the retired version and not the current one: the current version of a
// fact was the one the keyword channel could not reach by its tags.
func TestEvolve_NewVersionKeepsItsTagsInTheKeywordIndex(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "evolve-indexes-tags"
	ws := store.ResolveVaultPrefix(vault)

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "berth allocation",
		Content: "The east berth is reserved for the survey boat.",
		Tags:    []string{"quartermaster"}, // appears in no other field
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !keywordHolders(t, eng, ws, "quartermaster")[w.ID] {
		t.Fatal("control: the original memory is not found by its own tag word; the fixture is not exercising the tags field")
	}

	newID, err := eng.Evolve(ctx, vault, w.ID, "The east berth is reserved for the pilot launch.", "reassigned", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	newEng, err := store.GetEngram(ctx, ws, newID)
	if err != nil {
		t.Fatalf("GetEngram(new): %v", err)
	}
	if len(newEng.Tags) != 1 || newEng.Tags[0] != "quartermaster" {
		t.Fatalf("precondition: the new version did not inherit the tag (tags %q)", newEng.Tags)
	}
	if !keywordHolders(t, eng, ws, "quartermaster")[newID.String()] {
		t.Errorf("the new version carries the tag %q but the keyword index does not find it by that word", "quartermaster")
	}
}

// Every path that stores a new memory must index the same fields. This drives
// each one with a tag word found nowhere else in the memory and asks the
// keyword index for it. RememberTree used to submit nothing at all, so its root
// is also asked for by a word from its content. The upsert re-sync inherits the
// tag from the version it replaces (tags sent with a re-sync are not applied,
// which this test does not change).
func TestEveryWritePathIndexesTags(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "every-write-path-indexes-tags"
	ws := store.ResolveVaultPrefix(vault)

	want := map[string]struct{ id, word string }{}

	w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "tide gauge", Content: "The gauge at the quay reads in metres.", Tags: []string{"harbourmaster"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	want["Write"] = struct{ id, word string }{w.ID, "harbourmaster"}

	resps, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{Vault: vault, Concept: "fuel dock hours", Content: "The fuel dock opens at seven.", Tags: []string{"bunkering"}},
	})
	if errs[0] != nil {
		t.Fatalf("WriteBatch: %v", errs[0])
	}
	want["WriteBatch"] = struct{ id, word string }{resps[0].ID, "bunkering"}

	base, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "crane inspection", Content: "The crane was inspected in March.", Tags: []string{"stevedore"}})
	if err != nil {
		t.Fatalf("Write (to evolve): %v", err)
	}
	evolved, err := eng.Evolve(ctx, vault, base.ID, "The crane was inspected again in June.", "re-inspected", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	want["Evolve (the new version)"] = struct{ id, word string }{evolved.String(), "stevedore"}

	first, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "pilot roster", Content: "Two pilots are on call this week.", Tags: []string{"wheelhouse"}, IdempotentID: "roster-doc", UpsertMode: true})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	second, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "pilot roster", Content: "Three pilots are on call this week.", Tags: []string{"wheelhouse"}, IdempotentID: "roster-doc", UpsertMode: true})
	if err != nil {
		t.Fatalf("upsert re-sync: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("precondition: the changed re-sync did not produce a new version")
	}
	want["upsert re-sync with changed content"] = struct{ id, word string }{second.ID, "wheelhouse"}

	tree, err := eng.RememberTree(ctx, &RememberTreeRequest{Vault: vault, Root: TreeNodeInput{
		Concept: "slipway jobs", Content: "Jobs booked on the slipway.", Tags: []string{"shipwright"},
		Children: []TreeNodeInput{{Concept: "antifoul the tender", Content: "Two coats before launch.", Tags: []string{"chandlery"}}},
	}})
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}
	want["RememberTree (root)"] = struct{ id, word string }{tree.RootID, "shipwright"}
	want["RememberTree (child)"] = struct{ id, word string }{tree.NodeMap["antifoul the tender"], "chandlery"}

	added, err := eng.AddChild(ctx, vault, tree.RootID, &AddChildInput{Concept: "replace the winch cable", Content: "The cable is frayed near the drum.", Tags: []string{"rigger"}})
	if err != nil {
		t.Fatalf("AddChild: %v", err)
	}
	want["AddChild"] = struct{ id, word string }{added.ChildID, "rigger"}

	if !keywordHolders(t, eng, ws, "slipway")[tree.RootID] {
		t.Errorf("RememberTree (root): not found by a word from its own content: it was never submitted to the keyword index")
	}

	for path, c := range want {
		if !keywordHolders(t, eng, ws, c.word)[c.id] {
			t.Errorf("%s: the memory carries the tag %q but the keyword index does not find it by that word", path, c.word)
		}
	}
}

// A vault whose only writes were trees was never registered: it was missing
// from the vault list and the keyword index rebuild refused it as not found, so
// the one repair for an unindexed memory could not reach it.
func TestRememberTree_RegistersItsVault(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "tree-only-vault"

	if _, err := eng.RememberTree(ctx, &RememberTreeRequest{Vault: vault, Root: TreeNodeInput{
		Concept: "mooring plan", Content: "Visitors moor on the outer pontoon.",
		Children: []TreeNodeInput{{Concept: "winter berths", Content: "Winter berths are allocated in October."}},
	}}); err != nil {
		t.Fatalf("RememberTree: %v", err)
	}

	names, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatalf("ListVaults: %v", err)
	}
	listed := false
	for _, n := range names {
		if n == vault {
			listed = true
		}
	}
	if !listed {
		t.Errorf("a vault written only by RememberTree is not in the vault list (%q)", names)
	}
	awaitFTS(t, eng)
	if _, err := eng.ReindexFTSVault(ctx, vault); err != nil {
		t.Errorf("the keyword index rebuild cannot reach a tree-only vault: %v", err)
	}
}
