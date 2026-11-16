package provenance

import (
	"context"
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

// The list of creating operations is closed. An operation Origin has never
// heard of must not be taken for an engram's source: before this, any operation
// that was not a known update counted as an origin, so a new kind of update
// would have been reported as where the memory came from.
func TestOrigin_AnUnknownOperationIsNeverTheOrigin(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ws := [8]byte{1}
	t0 := time.Unix(1_700_000_000, 0)

	// Only an operation nobody has classified: the origin is unknown.
	alone := [16]byte{12}
	if err := s.Append(ctx, ws, alone, ProvenanceEntry{Timestamp: t0, Source: SourceInferred, Operation: "update-tags"}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Origin(ctx, ws, alone); err != nil || ok {
		t.Errorf("unclassified operation alone: got %+v ok=%v err=%v, want ok=false", got, ok, err)
	}

	// The same operation sorting before the creating entry: the creating entry
	// is still the origin.
	both := [16]byte{13}
	if err := s.Append(ctx, ws, both, ProvenanceEntry{Timestamp: t0, Source: SourceInferred, Operation: "update-tags"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, ws, both, ProvenanceEntry{Timestamp: t0.Add(time.Minute), Source: SourceDocument, Operation: "create"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Origin(ctx, ws, both)
	if err != nil || !ok {
		t.Fatalf("Origin: ok=%v err=%v", ok, err)
	}
	if got.Operation != "create" || got.Source != SourceDocument {
		t.Errorf("Origin = %+v, want the create entry (document)", got)
	}
}

// If a log ever holds two creating entries, the earliest is the origin.
func TestOrigin_TheEarliestCreatingEntryWins(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	ws := [8]byte{1}
	id := [16]byte{14}
	t0 := time.Unix(1_700_000_000, 0)

	if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0, Source: SourceDocument, Operation: "create"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, ws, id, ProvenanceEntry{Timestamp: t0.Add(time.Hour), Source: SourceExternal, Operation: "evolve"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Origin(ctx, ws, id)
	if err != nil || !ok {
		t.Fatalf("Origin: ok=%v err=%v", ok, err)
	}
	if got.Operation != "create" || got.Source != SourceDocument {
		t.Errorf("Origin = %+v, want the earlier create entry", got)
	}
}

func TestIsOriginOperation_IsAClosedList(t *testing.T) {
	for _, op := range []string{"create", "evolve"} {
		if !IsOriginOperation(op) {
			t.Errorf("%q must be a creating operation", op)
		}
	}
	for _, op := range []string{"update-meta", "update-relevance", "update-trust", "stamp-valid-until", "update-tags", "merge", "promote", ""} {
		if IsOriginOperation(op) {
			t.Errorf("%q must not be a creating operation", op)
		}
	}
}
