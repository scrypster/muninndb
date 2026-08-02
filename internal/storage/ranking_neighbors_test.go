package storage

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// COG-31: RelType symmetry classification
// ---------------------------------------------------------------------------

// relTypeSymmetryClassification is the census table: every RelType constant
// declared in types.go, and whether it asserts the same fact in both
// directions. Keyed by CONSTANT NAME so the completeness check below can be
// derived from the source rather than from a second hand-maintained list.
//
// A "false" here is not a shrug — it is a claim that reading this relation
// backwards would state something the author never wrote. RelSupersedes is the
// canonical case: backwards it says the OLD version supersedes the NEW one.
var relTypeSymmetryClassification = map[string]struct {
	value     RelType
	symmetric bool
}{
	"RelCoActivated":      {RelCoActivated, true}, // Hebbian: "these fired together"
	"RelSupports":         {RelSupports, false},   // A supports B ≠ B supports A
	"RelContradicts":      {RelContradicts, true}, // mutual by definition
	"RelDependsOn":        {RelDependsOn, false},  //
	"RelSupersedes":       {RelSupersedes, false}, // the whole point is which is newer
	"RelRelatesTo":        {RelRelatesTo, true},   // "related" is mutual
	"RelIsPartOf":         {RelIsPartOf, false},   //
	"RelCauses":           {RelCauses, false},     //
	"RelPrecededBy":       {RelPrecededBy, false}, //
	"RelFollowedBy":       {RelFollowedBy, false}, //
	"RelCreatedByPerson":  {RelCreatedByPerson, false},
	"RelBelongsToProject": {RelBelongsToProject, false},
	"RelReferences":       {RelReferences, false},
	"RelImplements":       {RelImplements, false},
	"RelBlocks":           {RelBlocks, false},
	"RelResolves":         {RelResolves, false},
	"RelRefines":          {RelRefines, false},
	"RelUserDefined":      {RelUserDefined, false}, // DIRECTIONAL by default
}

// TestRelType_SymmetryCensus requires an explicit symmetry classification for
// every RelType constant DECLARED IN types.go — the declared set is parsed out
// of the source with go/ast, not hand-listed, so a new constant that nobody
// classified fails CI here instead of silently inheriting a default.
//
// This is the SEC-15 method-census shape (`TestAppendMode_MethodCensus`) and
// the same reasoning as SEC-6: the way this class of bug actually happens is
// that someone adds a member and never learns there was a table to update.
//
// #800: six increments each locally guessed a direction convention that was
// never written down, and guessed it backwards.
func TestRelType_SymmetryCensus(t *testing.T) {
	declared := declaredRelTypeConstants(t)
	if len(declared) == 0 {
		t.Fatal("parsed zero RelType constants from types.go — the parser, not the code, is broken")
	}

	for _, name := range declared {
		entry, ok := relTypeSymmetryClassification[name]
		if !ok {
			t.Errorf("RelType constant %s is declared in types.go but has NO symmetry "+
				"classification. Add it to relTypeSymmetryClassification and decide, "+
				"explicitly, whether reading it backwards states something its author "+
				"never wrote (COG-31).", name)
			continue
		}
		if got := entry.value.IsSymmetric(); got != entry.symmetric {
			t.Errorf("%s (0x%04X).IsSymmetric() = %v, want %v",
				name, uint16(entry.value), got, entry.symmetric)
		}
	}

	// The converse: a classification for a constant that no longer exists.
	declaredSet := make(map[string]bool, len(declared))
	for _, n := range declared {
		declaredSet[n] = true
	}
	for name := range relTypeSymmetryClassification {
		if !declaredSet[name] {
			t.Errorf("relTypeSymmetryClassification names %s, which is not declared in types.go", name)
		}
	}
}

// declaredRelTypeConstants parses internal/storage/types.go and returns the
// names of every RelType constant it declares. Parsing the source is the
// point: a hand-maintained mirror list is exactly the drift the census exists
// to prevent.
func declaredRelTypeConstants(t *testing.T) []string {
	t.Helper()
	return relTypeConstantNames(t, "types.go", nil)
}

// relTypeConstBlockAnchor is a member of the RelType const block used to
// locate that block in the AST. Any member would do; RelSupports is picked
// because it is the oldest and least likely to be renamed. If it ever is,
// relTypeConstantNames fails loudly rather than censusing an empty set.
const relTypeConstBlockAnchor = "RelSupports"

// relTypeConstantNames is declaredRelTypeConstants with the source injected so
// the parser itself can be tested against forms types.go does not currently
// use. src is anything go/parser accepts (nil reads filename from disk).
//
// # What it recognises, and what it does not
//
// It collects EVERY name declared in the const block containing
// relTypeConstBlockAnchor, regardless of how that name is typed, plus any
// constant elsewhere in the FILE whose type annotation is the identifier
// `RelType` (parenthesised annotations are unwrapped). So all of these are
// censused:
//
//	RelX RelType = 0x0012   // annotated (the house style), anywhere in the file
//	RelX (RelType) = 0x0012 // parenthesised annotation, anywhere in the file
//	RelX = RelType(0x0012)  // nil Type, CallExpr value — ANCHOR BLOCK ONLY
//	RelX = 0x0012           // untyped, inheriting the block — ANCHOR BLOCK ONLY
//	RelX                    // iota continuation — ANCHOR BLOCK ONLY
//
// Keying off the BLOCK rather than off `vs.Type` is deliberate. The census
// exists to catch someone who adds a member without learning there was a table
// to update (SEC-6/SEC-15) — and that person is exactly the one who will not
// match the house style. A parser that only saw the annotated form let the
// other three through with a green suite. Pinned by
// TestRelTypeConstantNames_CatchesUnannotatedDeclarations.
//
// The residual, stated so the boundary is visible — these forms escape, each
// confirmed against this parser rather than reasoned about:
//
//	// A SECOND const block: only the `RelType`-annotated form is swept there,
//	// so everything the anchor block gets for free is invisible.
//	const ( RelStray = RelType(0x0031); RelIota; RelBare = 0x0032 )
//
//	// ALIAS-annotated. It IS annotated, and the annotation is not the
//	// identifier `RelType`. go/ast alone cannot resolve the alias; only a
//	// type-checked pass (go/types) could.
//	type RelKind = RelType
//	const RelAliased RelKind = 0x0032
//
//	// ANOTHER FILE. This parses types.go and nothing else.
//	// internal/storage/relations_extra.go: const RelSideways RelType = 0x0030
//
// Note that the second and third are ANNOTATED members that still escape, which
// is not what "unannotated" would lead a reader to expect. All are off the path
// today — every member lives in one block in types.go, and the anchor check
// t.Fatals if that block stops existing — but if that changes, move the anchor,
// sweep the package rather than the file, or type-check.
func relTypeConstantNames(t *testing.T, filename string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	seen := make(map[string]bool)
	add := func(name string) {
		if name != "_" && !seen[name] {
			seen[name] = true
		}
	}

	anchored := false
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}

		// Is this THE RelType block?
		isAnchorBlock := false
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == relTypeConstBlockAnchor {
					isAnchorBlock = true
				}
			}
		}
		if isAnchorBlock {
			anchored = true
		}

		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Unwrap `(RelType)` — a legal annotation that parses as a
			// ParenExpr, not an Ident.
			texpr := vs.Type
			for {
				p, ok := texpr.(*ast.ParenExpr)
				if !ok {
					break
				}
				texpr = p.X
			}
			typed := false
			if ident, ok := texpr.(*ast.Ident); ok && ident.Name == "RelType" {
				typed = true
			}
			if !isAnchorBlock && !typed {
				continue
			}
			for _, n := range vs.Names {
				add(n.Name)
			}
		}
	}
	if !anchored {
		t.Fatalf("%s declares no const block containing %s — the census anchor moved or was "+
			"renamed, so the parser, not the code, is what needs fixing",
			filename, relTypeConstBlockAnchor)
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestRelType_BidirectionalForRanking pins the two deliberate divergences
// between the strict predicate and the ranking one.
func TestRelType_BidirectionalForRanking(t *testing.T) {
	// Symmetric types are bidirectional for ranking.
	for _, rt := range []RelType{RelCoActivated, RelRelatesTo, RelContradicts} {
		if !rt.BidirectionalForRanking() {
			t.Errorf("RelType(0x%04X).BidirectionalForRanking() = false, want true", uint16(rt))
		}
	}
	// Directional types are NOT — RelSupersedes above all: reading it backwards
	// is literally "the OLD version supersedes the NEW one".
	for _, rt := range []RelType{RelSupersedes, RelSupports, RelDependsOn, RelCauses, RelRefines} {
		if rt.BidirectionalForRanking() {
			t.Errorf("RelType(0x%04X).BidirectionalForRanking() = true, want false", uint16(rt))
		}
	}
	// The user-defined range is admitted for RANKING ONLY (principle #4:
	// fail open on presentation) but is NOT symmetric for any writer.
	if !RelUserDefined.BidirectionalForRanking() {
		t.Error("RelUserDefined.BidirectionalForRanking() = false, want true")
	}
	if RelUserDefined.IsSymmetric() {
		t.Error("RelUserDefined.IsSymmetric() = true, want false — a writer must never assume it")
	}
	if !RelType(0x9001).BidirectionalForRanking() {
		t.Error("RelType(0x9001) in the user-defined range should be bidirectional for ranking")
	}
	// An unknown value BELOW the user-defined range is directional.
	if RelType(0x0500).BidirectionalForRanking() {
		t.Error("unknown RelType(0x0500) must not be bidirectional for ranking")
	}
}

// ---------------------------------------------------------------------------
// GetRankingNeighbors
// ---------------------------------------------------------------------------

// #803/STO-12: both endpoints are seeded first. These COG-31 fixtures were
// written against a WriteAssociation that accepted an edge between two ULIDs
// with no engram record at all — exactly the state STO-12 now refuses — so
// without this they fail at the write, before reaching the ranking behaviour
// they are actually about. Seeding is also the more faithful fixture: nothing
// in production ranks an edge whose endpoints do not exist.
func mustWriteAssoc(t *testing.T, store *PebbleStore, ws [8]byte, src, dst ULID, w float32, rt RelType) {
	t.Helper()
	seedEndpoints(t, store, ws, src, dst)
	if err := store.WriteAssociation(context.Background(), ws, src, dst, &Association{
		TargetID: dst,
		Weight:   w,
		RelType:  rt,
	}); err != nil {
		t.Fatalf("WriteAssociation %v->%v: %v", src, dst, err)
	}
}

func targetsOf(assocs []Association) []ULID {
	out := make([]ULID, 0, len(assocs))
	for _, a := range assocs {
		out = append(out, a.TargetID)
	}
	return out
}

func containsTarget(assocs []Association, id ULID) bool {
	for _, a := range assocs {
		if a.TargetID == id {
			return true
		}
	}
	return false
}

// TestGetAssociations_StaysForwardOnly is the non-interference pin (COG-31,
// design P2). GetAssociations is consumed by a WRITER (dream transitive
// inference persists what it infers) and by direction-presenting surfaces
// (Engine.Traverse, REST /associations). Unioning 0x04 into it made dream
// persist manufactured facts and made REST report the OLD version superseding
// the NEW one. This test fails the moment a future increment does that.
func TestGetAssociations_StaysForwardOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("fwd-only-pin")

	a, b := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, a, b, 0.8, RelRelatesTo)

	got, err := store.GetAssociations(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(got[b]) != 0 {
		t.Fatalf("GetAssociations(B) must stay FORWARD-ONLY, got %d edge(s): %v",
			len(got[b]), targetsOf(got[b]))
	}

	// ...and the union lives in the sibling method instead.
	rn, err := store.GetRankingNeighbors(ctx, ws, []ULID{a, b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if !containsTarget(rn[b], a) {
		t.Fatalf("GetRankingNeighbors(B) must reach A over the reverse index, got %v", targetsOf(rn[b]))
	}
	if rn[b][0].Weight != 0.8 {
		t.Errorf("reverse edge weight = %v, want 0.8", rn[b][0].Weight)
	}
	if rn[b][0].RelType != RelRelatesTo {
		t.Errorf("reverse edge relType = %v, want RelRelatesTo", rn[b][0].RelType)
	}

	// And the forward endpoint is unchanged.
	if !containsTarget(rn[a], b) {
		t.Fatalf("GetRankingNeighbors(A) lost the forward edge, got %v", targetsOf(rn[a]))
	}
}

// TestGetRankingNeighbors_DirectionalExcluded is design P3. A directional
// relation must NOT become readable from its destination, or supersession
// inverts.
func TestGetRankingNeighbors_DirectionalExcluded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("directional-excluded")

	newer, older := NewULID(), NewULID()
	// evolve writes successor -RelSupersedes-> predecessor.
	mustWriteAssoc(t, store, ws, newer, older, 0.9, RelSupersedes)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{older, newer}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if containsTarget(got[older], newer) {
		t.Fatalf("RelSupersedes leaked backwards: GetRankingNeighbors(predecessor) returned the successor %v", targetsOf(got[older]))
	}
	// The forward direction is untouched.
	if !containsTarget(got[newer], older) {
		t.Fatalf("forward RelSupersedes edge lost, got %v", targetsOf(got[newer]))
	}

	// Every other directional type is excluded too.
	for _, rt := range []RelType{RelSupports, RelDependsOn, RelCauses, RelRefines, RelBlocks} {
		src, dst := NewULID(), NewULID()
		mustWriteAssoc(t, store, ws, src, dst, 0.9, rt)
		m, err := store.GetRankingNeighbors(ctx, ws, []ULID{dst}, 10)
		if err != nil {
			t.Fatalf("GetRankingNeighbors: %v", err)
		}
		if containsTarget(m[dst], src) {
			t.Errorf("RelType(0x%04X) leaked backwards into ranking neighbours", uint16(rt))
		}
	}
}

// TestGetRankingNeighbors_Merge checks the two-pointer merge produces a single
// weight-DESCENDING stream across both halves.
func TestGetRankingNeighbors_Merge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("merge-order")

	center := NewULID()
	f1, f2 := NewULID(), NewULID()
	r1, r2 := NewULID(), NewULID()

	mustWriteAssoc(t, store, ws, center, f1, 0.90, RelRelatesTo)
	mustWriteAssoc(t, store, ws, center, f2, 0.30, RelRelatesTo)
	mustWriteAssoc(t, store, ws, r1, center, 0.60, RelRelatesTo)
	mustWriteAssoc(t, store, ws, r2, center, 0.10, RelCoActivated)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{center}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if len(got[center]) != 4 {
		t.Fatalf("want 4 merged edges, got %d: %v", len(got[center]), got[center])
	}
	wantOrder := []ULID{f1, r1, f2, r2}
	if !reflect.DeepEqual(targetsOf(got[center]), wantOrder) {
		t.Errorf("merge is not weight-descending across both halves.\n got: %v\nwant: %v",
			targetsOf(got[center]), wantOrder)
	}
	for i := 1; i < len(got[center]); i++ {
		if got[center][i].Weight > got[center][i-1].Weight {
			t.Fatalf("merged stream not weight-descending at %d: %v > %v",
				i, got[center][i].Weight, got[center][i-1].Weight)
		}
	}
}

// TestGetRankingNeighbors_CapIsTopNOverMerge is design kill-direction K5.
//
// The concatenate-then-truncate mistake would fill the cap with floor-weight
// forward edges and discard a far heavier reverse edge. It also asserts the
// converse: a forward edge may only be displaced by a STRICTLY heavier reverse
// edge, never by an equal-or-weaker one.
func TestGetRankingNeighbors_CapIsTopNOverMerge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cap-top-n")

	center := NewULID()
	const maxPerNode = 3

	// Four floor-weight forward edges — enough to fill the cap on their own.
	weak := make([]ULID, 4)
	for i := range weak {
		weak[i] = NewULID()
		mustWriteAssoc(t, store, ws, center, weak[i], 0.0005, RelCoActivated)
	}
	// One heavy reverse edge, 2000x heavier.
	heavy := NewULID()
	mustWriteAssoc(t, store, ws, heavy, center, 1.0, RelRelatesTo)
	// One reverse edge exactly equal to the forward floor weight: must NOT
	// displace a forward edge (ties go to forward).
	tie := NewULID()
	mustWriteAssoc(t, store, ws, tie, center, 0.0005, RelRelatesTo)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{center}, maxPerNode)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	res := got[center]
	if len(res) != maxPerNode {
		t.Fatalf("cap not honoured: got %d edges, want %d", len(res), maxPerNode)
	}
	if res[0].TargetID != heavy {
		t.Fatalf("the 1.0-weight reverse edge was discarded by the cap; head is %v at weight %v",
			res[0].TargetID, res[0].Weight)
	}
	if containsTarget(res, tie) {
		t.Errorf("an EQUAL-weight reverse edge displaced a forward edge from the cap: %v", targetsOf(res))
	}
	// The remaining two slots must be forward floor edges.
	for _, a := range res[1:] {
		found := false
		for _, w := range weak {
			if a.TargetID == w {
				found = true
			}
		}
		if !found {
			t.Errorf("unexpected edge %v (weight %v) in the capped result", a.TargetID, a.Weight)
		}
	}
}

// TestGetRankingNeighbors_Dedup: the same partner reachable both ways consumes
// ONE slot and appears ONCE, at the larger weight. Without this,
// phase4HebbianBoost sums the pair twice.
func TestGetRankingNeighbors_Dedup(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("dedup")

	a, b := NewULID(), NewULID()
	// A caller's link(A,B) plus the neighbour worker's independent B->A.
	mustWriteAssoc(t, store, ws, a, b, 0.20, RelRelatesTo)
	mustWriteAssoc(t, store, ws, b, a, 0.70, RelRelatesTo)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if len(got[a]) != 1 {
		t.Fatalf("pair reachable both ways must appear ONCE, got %d: %v", len(got[a]), got[a])
	}
	if got[a][0].TargetID != b {
		t.Fatalf("wrong target %v, want %v", got[a][0].TargetID, b)
	}
	if got[a][0].Weight != 0.70 {
		t.Errorf("dedup must keep the LARGER weight: got %v, want 0.70", got[a][0].Weight)
	}
}

// TestGetRankingNeighbors_SelfEdge: a self-loop is written to BOTH 0x03 and
// 0x04, so it reaches the merge twice. It must be emitted once.
//
// This is covered by the TargetID dedup, not by a special case — see the note
// in rankingReverseEdges. An earlier draft carried an explicit `srcID == id`
// skip; it was removed after this test passed identically with the skip
// deleted, which proved the skip was dead code.
func TestGetRankingNeighbors_SelfEdge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("self-edge")

	a := NewULID()
	mustWriteAssoc(t, store, ws, a, a, 0.5, RelRelatesTo)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if len(got[a]) != 1 {
		t.Fatalf("self-edge must appear exactly once, got %d: %v", len(got[a]), targetsOf(got[a]))
	}
}

// TestGetRankingNeighbors_BatchedIsPerIDExact pins the batched reverse scan
// against an EXACT expected adjacency, not against a per-id call.
//
// Batch-vs-per-id equivalence is the tempting assertion and it is worthless
// here: both paths run the same scan, so a broken per-id prefix boundary — the
// characteristic way a shared-iterator batch scan breaks, bleeding one id's
// reverse edges into the next — breaks both identically and the comparison
// stays green. Verified: deleting the `bytes.Equal(k[:25], idPrefix)` boundary
// left a batch-vs-per-id test passing.
func TestGetRankingNeighbors_BatchedIsPerIDExact(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("batched-exact")

	const n = 20
	ids := make([]ULID, n)
	for i := range ids {
		ids[i] = NewULID()
	}
	// A ring of symmetric edges (distinct weights, so ordering is total) plus
	// directional noise that must never come back over the reverse index.
	ringWeight := func(i int) float32 { return float32(0.10 + 0.01*float64(i)) }
	for i := 0; i < n; i++ {
		mustWriteAssoc(t, store, ws, ids[i], ids[(i+1)%n], ringWeight(i), RelRelatesTo)
		mustWriteAssoc(t, store, ws, ids[i], ids[(i+7)%n], 0.05, RelSupersedes)
	}

	// Expected ranking neighbours of ids[i], weight-descending:
	//   forward relates_to  -> ids[i+1]  at ringWeight(i)
	//   forward supersedes  -> ids[i+7]  at 0.05
	//   reverse relates_to  <- ids[i-1]  at ringWeight(i-1)
	// The reverse supersedes edge from ids[i-7] is EXCLUDED (directional).
	expected := func(i int) []ULID {
		prev := (i - 1 + n) % n
		type e struct {
			id ULID
			w  float32
		}
		es := []e{
			{ids[(i+1)%n], ringWeight(i)},
			{ids[(i+7)%n], 0.05},
			{ids[prev], ringWeight(prev)},
		}
		sort.SliceStable(es, func(a, b int) bool { return es[a].w > es[b].w })
		out := make([]ULID, 0, len(es))
		for _, x := range es {
			out = append(out, x.id)
		}
		return out
	}

	check := func(label string, got map[ULID][]Association) {
		t.Helper()
		for i, id := range ids {
			if !reflect.DeepEqual(targetsOf(got[id]), expected(i)) {
				t.Fatalf("%s: wrong neighbours for ids[%d]:\n got: %v\nwant: %v",
					label, i, targetsOf(got[id]), expected(i))
			}
		}
	}

	batch, err := store.GetRankingNeighbors(ctx, ws, ids, 10)
	if err != nil {
		t.Fatalf("batched GetRankingNeighbors: %v", err)
	}
	check("batched", batch)

	// Ids passed in DESCENDING order still each get their own edges: the
	// internal ascending sort must not scramble the id->result mapping.
	shuffled := append([]ULID(nil), ids...)
	sort.Slice(shuffled, func(a, b int) bool { return shuffled[a].String() > shuffled[b].String() })
	rev, err := store.GetRankingNeighbors(ctx, ws, shuffled, 10)
	if err != nil {
		t.Fatalf("shuffled GetRankingNeighbors: %v", err)
	}
	check("descending-id-order", rev)

	// And a single-id call agrees with the batch.
	one, err := store.GetRankingNeighbors(ctx, ws, []ULID{ids[3]}, 10)
	if err != nil {
		t.Fatalf("single GetRankingNeighbors: %v", err)
	}
	if !reflect.DeepEqual(one[ids[3]], batch[ids[3]]) {
		t.Fatalf("single-id result differs from batched for ids[3]")
	}
}

// TestGetRankingNeighbors_VaultPrefixEndingIn0xFF is the STO-11 guard for the
// new 0x04 range bound. A vault prefix whose last byte is 0xFF breaks a naive
// last-byte increment (upper bound below lower bound → silently zero results,
// forever, for that vault only).
func TestGetRankingNeighbors_VaultPrefixEndingIn0xFF(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Derive a 0xFF-terminated vault prefix deterministically rather than
	// hoping one turns up (~1 name in 256).
	var vault string
	var ws [8]byte
	found := false
	for i := 0; i < 100000; i++ {
		name := fmt.Sprintf("sto11-probe-%d", i)
		p := store.VaultPrefix(name)
		if p[7] == 0xFF {
			vault, ws, found = name, p, true
			break
		}
	}
	if !found {
		t.Skip("no 0xFF-terminated vault prefix found in the probe space")
	}
	t.Logf("vault %q has prefix ending in 0xFF", vault)

	a, b := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, a, b, 0.8, RelRelatesTo)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if !containsTarget(got[b], a) {
		t.Fatalf("STO-11: reverse scan returned nothing for a 0xFF-terminated vault prefix (got %v)", targetsOf(got[b]))
	}
}

// TestGetRankingNeighbors_ReverseCacheInvalidated: the reverse adjacency is
// cached (revAssocCache, 2s TTL) because an uncached reverse scan cost more
// than the pre-committed latency budget allowed. A cache with a 2s TTL and no
// eviction would make a brand-new edge invisible from its destination for two
// seconds — the same defect #800 fixes, only intermittent. Every mutation site
// that evicts the forward cache for the SOURCE must evict the reverse cache
// for the DESTINATION.
func TestGetRankingNeighbors_ReverseCacheInvalidated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("rev-cache-invalidation")

	a, b, c := NewULID(), NewULID(), NewULID()

	// Warm the reverse cache for b with a single inbound edge.
	mustWriteAssoc(t, store, ws, a, b, 0.3, RelRelatesTo)
	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (warm): %v", err)
	}
	if len(got[b]) != 1 {
		t.Fatalf("warm-up: want 1 inbound edge, got %d", len(got[b]))
	}

	// WriteAssociation: a second inbound edge must be visible immediately.
	mustWriteAssoc(t, store, ws, c, b, 0.9, RelRelatesTo)
	got, err = store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (after write): %v", err)
	}
	if !containsTarget(got[b], c) {
		t.Fatalf("WriteAssociation did not evict the destination's reverse cache: %v", targetsOf(got[b]))
	}

	// UpdateAssocWeight: the new weight must be visible immediately, and it
	// must reorder the merged stream.
	if err := store.UpdateAssocWeight(ctx, ws, a, b, 0.99, 1); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}
	got, err = store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (after update): %v", err)
	}
	if got[b][0].TargetID != a || got[b][0].Weight != 0.99 {
		t.Fatalf("UpdateAssocWeight did not evict the destination's reverse cache: head is %v at %v, want %v at 0.99",
			got[b][0].TargetID, got[b][0].Weight, a)
	}

	// UpdateAssocWeightBatch: same obligation on the batch path.
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: ws, Src: c, Dst: b, Weight: 0.999},
	}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}
	got, err = store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (after batch): %v", err)
	}
	if got[b][0].TargetID != c {
		t.Fatalf("UpdateAssocWeightBatch did not evict the destination's reverse cache: %v",
			targetsOf(got[b]))
	}
}

// TestGetRankingNeighbors_CacheDoesNotUnderServeALargerCap pins the trap the
// FORWARD cache still has: GetAssociations caches the list it built under the
// caller's maxPerNode, so a later caller asking for more is silently served
// the shorter list. The reverse cache records `truncated` and re-scans instead.
func TestGetRankingNeighbors_CacheDoesNotUnderServeALargerCap(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("rev-cache-cap")

	center := NewULID()
	const inbound = revAssocScanCap + 10
	for i := 0; i < inbound; i++ {
		src := NewULID()
		mustWriteAssoc(t, store, ws, src, center, float32(1.0-0.001*float64(i)), RelRelatesTo)
	}

	// A small-cap call first, which is what populates the cache.
	small, err := store.GetRankingNeighbors(ctx, ws, []ULID{center}, 5)
	if err != nil {
		t.Fatalf("GetRankingNeighbors(5): %v", err)
	}
	if len(small[center]) != 5 {
		t.Fatalf("want 5, got %d", len(small[center]))
	}

	// A larger-cap call must NOT be served the 5-entry cache entry.
	big, err := store.GetRankingNeighbors(ctx, ws, []ULID{center}, 40)
	if err != nil {
		t.Fatalf("GetRankingNeighbors(40): %v", err)
	}
	if len(big[center]) != 40 {
		t.Fatalf("cache under-served a larger cap: got %d edges, want 40", len(big[center]))
	}

	// A request ABOVE the scan cap gets everything, not a truncated cache hit.
	all, err := store.GetRankingNeighbors(ctx, ws, []ULID{center}, inbound)
	if err != nil {
		t.Fatalf("GetRankingNeighbors(all): %v", err)
	}
	if len(all[center]) != inbound {
		t.Fatalf("want all %d inbound edges, got %d", inbound, len(all[center]))
	}
}

// TestRankingReverseEdges_DirectionalEdgeDoesNotConsumeCapSlot pins what
// revAssocScanCap actually bounds: ACCEPTED edges, not keys scanned.
//
// The fixture is the hazard the doc argues about. A hub carries
// revAssocScanCap*2 inbound DIRECTIONAL edges at a HIGH weight — the shape an
// explicit relation is written in, once, at a fixed confidence — plus one
// inbound RelCoActivated edge at a LOW weight, the shape a Hebbian edge starts
// in. Reverse keys arrive weight-descending, so the co-activation edge sorts
// BELOW every directional one.
//
// Because the cap counts accepted edges, the scan walks past all of them and
// returns the co-activation edge. Turn the cap into a scanned-key budget and
// this test goes red with the reason attached: the budget would be spent
// entirely on directional keys and the only real neighbour would vanish.
func TestRankingReverseEdges_DirectionalEdgeDoesNotConsumeCapSlot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("directional-hub")

	hub := NewULID()
	for i := 0; i < revAssocScanCap*2; i++ {
		mustWriteAssoc(t, store, ws, NewULID(), hub, 0.9, RelBelongsToProject)
	}
	hebbian := NewULID()
	mustWriteAssoc(t, store, ws, hebbian, hub, 0.1, RelCoActivated)

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{hub}, 20)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if !containsTarget(got[hub], hebbian) {
		t.Fatalf("the one symmetric inbound edge was lost behind %d high-weight directional "+
			"edges: GetRankingNeighbors(hub) = %v. revAssocScanCap must bound ACCEPTED edges, "+
			"not keys scanned — a scanned-key budget fills with directional edges and hides "+
			"exactly the Hebbian edges this union exists to surface.",
			revAssocScanCap*2, targetsOf(got[hub]))
	}
	// ...and none of the directional edges leaked in alongside it.
	if len(got[hub]) != 1 {
		t.Errorf("GetRankingNeighbors(hub) returned %d edges, want exactly 1 (the symmetric one): %v",
			len(got[hub]), targetsOf(got[hub]))
	}
}

// TestRankingReverseEdges_MissPathDoesNotAliasTheCache pins that BOTH cache
// paths hand the caller a private slice, not the cache's backing array.
//
// The hit path copies (`append([]Association(nil), entry.assocs[:n]...)`); the
// miss path used to cache `assocs` and then return that same slice, so the
// caller held a live view of a cache entry for the rest of the 2s TTL. Nothing
// in-tree mutated it, which is exactly what makes it a trap: adding the
// symmetric shortcut `if len(fwd) == 0 { return rev }` to the merge is a
// five-second edit that looks like an unambiguous win and silently publishes
// the cache's array to phase4HebbianBoost and phase5Traverse.
//
// TestGetRankingNeighbors_NoReverseEdgesDoesNotAliasTheCache is the same rule
// on the forward half, at the public entry point.
//
// The invariant is defended at the source rather than by a comment on the
// merge, because a caller cannot see the aliasing from where it stands.
func TestRankingReverseEdges_MissPathDoesNotAliasTheCache(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("rev-cache-aliasing")

	dst := NewULID()
	srcA, srcB := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, srcA, dst, 0.8, RelRelatesTo)
	mustWriteAssoc(t, store, ws, srcB, dst, 0.4, RelRelatesTo)

	// Cold: the MISS path scans, populates revAssocCache, and returns.
	first, err := store.rankingReverseEdges(ctx, ws, []ULID{dst}, 10)
	if err != nil {
		t.Fatalf("rankingReverseEdges (cold): %v", err)
	}
	if len(first[dst]) != 2 {
		t.Fatalf("cold read: want 2 inbound edges, got %d", len(first[dst]))
	}

	// A caller mutates what it was handed. That is legal: the returned slice
	// is documented as the caller's.
	first[dst][0].Weight = 42
	first[dst][0].TargetID = ULID{}

	// Warm: the HIT path must serve the edge as stored.
	second, err := store.rankingReverseEdges(ctx, ws, []ULID{dst}, 10)
	if err != nil {
		t.Fatalf("rankingReverseEdges (warm): %v", err)
	}
	if len(second[dst]) != 2 {
		t.Fatalf("warm read: want 2 inbound edges, got %d", len(second[dst]))
	}
	if second[dst][0].TargetID != srcA || second[dst][0].Weight != 0.8 {
		t.Fatalf("the cache-miss path returned a slice aliasing the cached backing array: "+
			"after a caller mutated its own copy, the cached head became %v at weight %v, "+
			"want %v at 0.8. Copy on the miss path, not just the hit path.",
			second[dst][0].TargetID, second[dst][0].Weight, srcA)
	}
}

// TestGetRankingNeighbors_NoReverseEdgesDoesNotAliasTheCache pins the same rule
// at the PUBLIC entry point, on the half that reaches it through the merge.
//
// rankingReverseEdges copies on both of its paths, so the reverse half is clean.
// mergeRankingNeighbors' `if len(rev) == 0 { return fwd }` shortcut then handed
// GetAssociations' own miss-path slice — which DOES alias assocCache (#820) —
// straight back to the caller. GetRankingNeighbors' return was therefore
// copy-safe for a node with inbound symmetric edges and aliased for a node
// without, decided by data, with nothing at the call site to show which. A
// non-uniform ownership contract is worse than a uniformly unsafe one: the
// caller that gets away with mutating it fifty times is the one that breaks on
// the fifty-first, in another vault.
//
// The copy is taken in the merge because that is where the union's contract is
// stated. #820 still owns GetAssociations' own contract for its other callers.
func TestGetRankingNeighbors_NoReverseEdgesDoesNotAliasTheCache(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("union-fwd-aliasing")

	src := NewULID()
	dstA, dstB := NewULID(), NewULID()
	// Forward edges only: nothing points AT src, so the merge takes its
	// len(rev) == 0 shortcut.
	mustWriteAssoc(t, store, ws, src, dstA, 0.7, RelRelatesTo)
	mustWriteAssoc(t, store, ws, src, dstB, 0.3, RelRelatesTo)

	first, err := store.GetRankingNeighbors(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (cold): %v", err)
	}
	if len(first[src]) != 2 {
		t.Fatalf("cold read: want 2 forward edges, got %d", len(first[src]))
	}
	if len(first[src]) > 0 && first[src][0].Weight != 0.7 {
		t.Fatalf("UNDERPOWERED: the fixture's heaviest edge is %v, want 0.7", first[src][0].Weight)
	}

	// The caller mutates what it was handed — legal, per the documented contract.
	first[src][0].Weight = -42

	second, err := store.GetRankingNeighbors(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors (warm): %v", err)
	}
	if len(second[src]) != 2 {
		t.Fatalf("warm read: want 2 forward edges, got %d", len(second[src]))
	}
	if second[src][0].Weight != 0.7 {
		t.Fatalf("the union published a cache entry's backing array: after a caller "+
			"mutated its own copy, the next read returned weight %v, want 0.7. "+
			"mergeRankingNeighbors' len(rev) == 0 shortcut must copy too — "+
			"GetRankingNeighbors' ownership contract may not depend on whether a "+
			"node happens to have inbound edges.", second[src][0].Weight)
	}
}

// TestForwardOnlyFanFixture_TakesTheMergeCopyShortcut asserts the SHAPE that
// BenchmarkPhase4Read_ForwardOnlyFan claims to measure, rather than trusting it.
//
// The benchmark's number is quoted in COG-31 and in #800's commit body as the
// cost of the copy in mergeRankingNeighbors' len(rev) == 0 branch. That number
// is only about the copy if the fixture actually reaches that branch with a
// non-empty forward list — i.e. every candidate must have forward edges and NO
// inbound ones. BenchmarkPhase4Read's ring fixture fails exactly this check on
// its edges > 0 arms (every node has an inbound edge, so len(rev) > 0) and
// returns an empty list on its edges == 0 arm, which is how a ~1us noise
// reading was recorded as the copy's cost for a whole review round.
//
// So the precondition is checked here, in the gate, where a later edit to the
// fixture that reintroduces inbound edges fails loudly instead of quietly
// re-measuring the wrong arm.
func TestForwardOnlyFanFixture_TakesTheMergeCopyShortcut(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, degree := range []int{2, 10, 20} {
		ws, cand := buildForwardOnlyFan(t, store, fmt.Sprintf("fan-shape-%d", degree), 50, degree)

		fwd, err := store.GetAssociations(ctx, ws, cand, 20)
		if err != nil {
			t.Fatalf("degree %d: GetAssociations: %v", degree, err)
		}
		rev, err := store.rankingReverseEdges(ctx, ws, cand, 20)
		if err != nil {
			t.Fatalf("degree %d: rankingReverseEdges: %v", degree, err)
		}

		want := degree
		if want > 20 {
			want = 20
		}
		for _, id := range cand {
			if len(fwd[id]) != want {
				t.Fatalf("degree %d: candidate %v has %d forward edges, want %d — "+
					"the fan fixture is not producing the forward degree the benchmark "+
					"claims to measure", degree, id, len(fwd[id]), want)
			}
			if len(rev[id]) != 0 {
				t.Fatalf("degree %d: candidate %v has %d INBOUND edges, want 0. The fan "+
					"fixture's sinks must never be candidates: with inbound edges, "+
					"mergeRankingNeighbors takes its two-pointer path and the benchmark "+
					"measures the merge, not the len(rev) == 0 copy it is quoted for",
					degree, id, len(rev[id]))
			}
		}

		// Both halves of the shortcut's precondition hold, so the union must
		// return exactly the forward list. (That it returns a COPY of it is
		// pinned by TestGetRankingNeighbors_NoReverseEdgesDoesNotAliasTheCache;
		// what this test owns is that the branch is reached at all.)
		union, err := store.GetRankingNeighbors(ctx, ws, cand, 20)
		if err != nil {
			t.Fatalf("degree %d: GetRankingNeighbors: %v", degree, err)
		}
		for _, id := range cand {
			if len(union[id]) != want {
				t.Fatalf("degree %d: union returned %d edges for %v, want %d",
					degree, len(union[id]), id, want)
			}
		}
	}
}

// TestRelTypeConstantNames_CatchesUnannotatedDeclarations guards the census's
// own parser.
//
// TestRelType_SymmetryCensus is only as complete as the set of names it is
// handed. The parser used to require `vs.Type` to be the identifier `RelType`,
// so a member written in any other legal form — `RelMentions =
// RelType(0x0012)`, a ValueSpec with a nil Type and a CallExpr value — was
// invisible, and the census stayed green while an unclassified relation
// shipped. Every constant in types.go today uses the annotated form, so that
// gap was latent; but the census's whole premise (SEC-6/SEC-15) is catching
// the person who adds a member without learning there was a table to update,
// and that person is precisely the one who will not match the house style.
//
// The fixture is synthetic source, not types.go, because the point is to
// exercise forms types.go does not contain.
func TestRelTypeConstantNames_CatchesUnannotatedDeclarations(t *testing.T) {
	const src = `package storage

type RelType uint16

const (
	RelCoActivated RelType = 0x0000
	RelSupports    RelType = 0x0001
	// Not the house style. Nil Type, CallExpr value — invisible to a parser
	// that keys off vs.Type.
	RelMentions = RelType(0x0012)
	// Untyped, inheriting the block. Also invisible to that parser.
	RelWhispers = 0x0013
	_           = 0x0014
)

// A second block, annotated: found by the type sweep, not the anchor.
const RelStrayTyped RelType = 0x0015

// Annotated inside parentheses. Legal Go, an *ast.ParenExpr rather than an
// *ast.Ident, and outside the anchor block — so the type sweep must unwrap it.
const RelParenTyped (RelType) = 0x0016

// Not a RelType at all, and in its own block: must NOT be collected.
const someUnrelatedConst = 7
`

	got := relTypeConstantNames(t, "synthetic_types.go", src)
	want := []string{
		"RelCoActivated",
		"RelMentions",
		"RelParenTyped",
		"RelStrayTyped",
		"RelSupports",
		"RelWhispers",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("relTypeConstantNames() = %v, want %v\n"+
			"The census is only as complete as this list. A RelType member the parser "+
			"cannot see is a member nobody is forced to classify (COG-31).", got, want)
	}
}
