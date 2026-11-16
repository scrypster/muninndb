package provenance

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"
)

func TestOrigin_IsTheCreatingEntryHoweverManyUpdatesFollow(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ws := [8]byte{1}
	id := [16]byte{7}
	t0 := time.Unix(1_700_000_000, 0)

	if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0, Source: SourceDocument, Operation: "create", AgentID: "loader"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 50; i++ {
		if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0.Add(time.Duration(i) * time.Minute), Source: SourceInferred, Operation: "update-relevance"}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Origin(ctx, ws, id)
	if err != nil || !ok {
		t.Fatalf("Origin: ok=%v err=%v", ok, err)
	}
	if got.Operation != "create" || got.Source != SourceDocument || got.AgentID != "loader" {
		t.Errorf("Origin = %+v, want the create entry (document, loader)", got)
	}
}

// The creating entry is stamped with the engram's CreatedAt, which a caller can
// set in the future, so an update can sort before it. Origin must still find
// the creating entry, not the first key.
func TestOrigin_SkipsUpdatesThatSortBeforeTheCreatingEntry(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ws := [8]byte{1}
	id := [16]byte{8}
	t0 := time.Unix(1_700_000_000, 0)

	if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0, Source: SourceInferred, Operation: "update-meta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0.Add(time.Hour), Source: SourceExternal, Operation: "evolve"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Origin(ctx, ws, id)
	if err != nil || !ok {
		t.Fatalf("Origin: ok=%v err=%v", ok, err)
	}
	if got.Operation != "evolve" || got.Source != SourceExternal {
		t.Errorf("Origin = %+v, want the evolve entry", got)
	}
}

// With no creating entry nothing is known about where the engram came from;
// Origin must say so rather than hand back whoever touched it.
func TestOrigin_UnknownWhenOnlyUpdatesOrNothing(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ws := [8]byte{1}

	if _, ok, err := s.Origin(ctx, ws, [16]byte{9}); err != nil || ok {
		t.Errorf("no entries: ok=%v err=%v, want ok=false", ok, err)
	}
	id := [16]byte{10}
	for _, op := range []string{"update-relevance", "update-trust", "stamp-valid-until", "update-meta"} {
		if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: time.Unix(1_700_000_000, 0), Source: SourceInferred, Operation: op}); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, err := s.Origin(ctx, ws, id); err != nil || ok {
		t.Errorf("only updates: got %+v ok=%v err=%v, want ok=false", got, ok, err)
	}
	// Another engram's entries must not leak in.
	other := [16]byte{11}
	if err := s.Append(ctx, ws, other, ProvenanceEntry{Timestamp: time.Unix(1_700_000_000, 0), Source: SourceHuman, Operation: "create"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Origin(ctx, ws, id); ok {
		t.Error("Origin read a neighbouring engram's creating entry")
	}
}

// Census: every operation the storage layer writes when it CHANGES an existing
// engram must be classified as an update here, or Origin would mistake a later
// touch for the engram's source. internal/storage/engram.go holds the update
// writers; a new one added there with an unclassified operation fails this.
func TestIsUpdateOperation_CoversEveryUpdateTheStorageLayerWrites(t *testing.T) {
	src, err := os.ReadFile("../storage/engram.go")
	if err != nil {
		t.Fatalf("read storage/engram.go: %v", err)
	}
	found := regexp.MustCompile(`Operation:\s+"([a-z-]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) < 4 {
		t.Fatalf("found %d operation literals in storage/engram.go, expected at least 4: the census lost its input", len(found))
	}
	for _, m := range found {
		if !IsUpdateOperation(m[1]) {
			t.Errorf("storage/engram.go writes operation %q, which is not classified as an update: Origin would treat it as an engram's source", m[1])
		}
	}
	for _, op := range []string{"create", "evolve", "merge", "promote", ""} {
		if IsUpdateOperation(op) {
			t.Errorf("%q must not be classified as an update", op)
		}
	}
}
