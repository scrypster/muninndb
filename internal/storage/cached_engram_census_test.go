package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCachedEngramMutationCensus is the class guard for #858 (STO-20).
//
// # Why a mechanism and not a doc comment
//
// #492 fixed exactly this bug in dream consolidation — dedup did
// `representative.AccessCount += n` on a pointer from GetEngrams — and its
// remedy for the CLASS was to write the immutability contract into
// L1Cache.Get's doc comment. That comment is still there, it is accurate, and
// in the two months since it was written the tree accumulated SEVEN more
// in-place mutators of a cache-returned pointer, one of which (#858's
// mutateEngram) was reported as a live data race from an ordinary two-goroutine
// evolve. A doc comment is a request. This is a check.
//
// The seven were found by walking the AST, not by grepping method names, and
// the enumeration is the point: four of them (SoftDelete, UpdateTagsLocked,
// UpdateConfidence, UpdateConfidenceWithContradiction) already carried long
// comments explaining that their per-engram stripe lock closed this race. It
// did not. The lock serialises those methods against each other; recall's
// readers do not take casLocks (engram.go says so at UpdateTagsLocked's
// re-cache), so an unlocked GetEngram concurrent with any of them raced the
// shared struct. Demonstrated under -race before the fix, on both
// UpdateConfidence and SoftDelete, with a reader goroutine doing nothing but
// GetEngram in a loop.
//
// # What it asserts
//
// In every function in the module: no value obtained from GetEngram,
// GetEngrams or an L1Cache Get may have a field assigned through it, unless the
// binding was first RELEASED by an `x = x.Clone()` lexically earlier in the
// same function, or the site appears in cachedEngramExemptions with a reason.
//
// # What it does NOT catch — the honest boundary
//
//   - INTER-FUNCTION taint. `mutate(eng)` handing a cache pointer to a callback
//     or helper that assigns through it is invisible here; the census would need
//     a call graph. That shape is exactly what mutateEngram had (its `mutate`
//     func literal), and it is caught only because the literal is lexically
//     inside the same FuncDecl, so ast.Inspect descends into it with the
//     enclosing taint. A helper in another function is not seen.
//   - A BARE STRUCT COPY (`cp := *eng`) is not treated as propagating taint, so
//     `cp.State = x` is not reported. That is correct for scalar, string and
//     slice-HEADER writes and WRONG for slice-ELEMENT writes (`cp.Tags[0] = x`
//     aliases the cached engram's array). No site in the tree has that shape,
//     and the sanctioned idiom is Clone, which deep-copies. Stated rather than
//     modelled, because tainting through a deref would report every safe struct
//     copy and exemptions are how coverage rots.
//   - A LEXICALLY earlier release that does not DOMINATE the sink — a Clone
//     inside an unrelated `if` — satisfies this check. Same residual, and the
//     same reason, as TestLastAccessElapsedCensus's guard positions.
//   - Taint that leaves the function by being STORED (into a struct field, a
//     package-level var, a channel) and mutated elsewhere.
//   - A new SLICE-VALUED FIELD on Engram that Clone forgets to copy. Clone's
//     doc says so; nothing mechanical checks it.
//
// # The input set is checked too
//
// After the fix the expected sink count is ZERO, so "found nothing" and "went
// blind" are the same observation — the precise failure
// TestLastAccessElapsedCensus shipped with and had to grow a named floor to
// close. The floor here is therefore on the TAINT SOURCES:
// cachedEngramKnownSources names the seven functions #858's census found, each
// of which must still be seen to BIND a cache-returned engram. Narrow the walk,
// break the source matcher, rename GetEngram — any of those drops sources, and
// the floor names which one.
func TestCachedEngramMutationCensus(t *testing.T) {
	root := moduleRoot(t)

	fset := token.NewFileSet()
	type unitRef struct {
		rel  string
		unit censusUnit
	}
	var units []unitRef
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, u := range censusUnits(f) {
			units = append(units, unitRef{rel: rel, unit: u})
		}
		scanned++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("census scanned no non-test Go files under %s — wrong module root?", root)
	}

	type site struct{ key, pos, expr string }
	var offenders []site
	sourceKeys := map[string]bool{}
	seenExempt := map[string]bool{}

	for _, ur := range units {
		tainted := cachedEngramIdents(ur.unit.body)
		if len(tainted) > 0 {
			sourceKeys[ur.rel+":"+ur.unit.label] = true
		}
		releases := cloneReleasePositions(ur.unit.body, tainted)
		for _, s := range cachedEngramFieldWrites(ur.unit.body, tainted) {
			if guardedBefore(releases[s.root], s.node.Pos()) {
				continue
			}
			key := ur.rel + ":" + ur.unit.label
			if _, ok := cachedEngramExemptions[key]; ok {
				seenExempt[key] = true
				continue
			}
			offenders = append(offenders, site{
				key:  key,
				pos:  fset.Position(s.node.Pos()).String(),
				expr: exprString(fset, s.lhs),
			})
		}
	}

	// The FLOOR on the input set. See cachedEngramKnownSources.
	var missing []string
	for key, why := range cachedEngramKnownSources {
		if !sourceKeys[key] {
			missing = append(missing, "  "+key+"\n      "+why)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("cachedEngramKnownSources: %d of %d known cache-read site(s) were NOT found by this walk:\n%s\n\n"+
			"Exactly one of these is true, and they need opposite responses:\n"+
			"  (a) the site legitimately stopped reading a cache-returned engram — update "+
			"cachedEngramKnownSources in the same commit, saying what replaced it;\n"+
			"  (b) the walk stopped reaching it, or cachedEngramIdents stopped seeing the read — "+
			"this census is now blind there and reports PASS on an unguarded mutation.\n"+
			"After the #858 fix the expected offender count is ZERO, so (b) is INVISIBLE without "+
			"this floor: 'no files scanned' and 'no offenders' produce the identical green.",
			len(missing), len(cachedEngramKnownSources), strings.Join(missing, "\n"))
	}

	if len(offenders) > 0 {
		sort.Slice(offenders, func(i, j int) bool { return offenders[i].pos < offenders[j].pos })
		var b strings.Builder
		for _, s := range offenders {
			b.WriteString("\n  " + s.pos + "  in " + s.key + ":  " + s.expr + " = ...")
		}
		t.Errorf("IN-PLACE MUTATION of a cache-returned *Engram:%s\n\n"+
			"GetEngram / GetEngrams / L1Cache.Get all return the SHARED L1-cache pointer, "+
			"documented read-only at L1Cache.Get. Assigning through it is a data race against "+
			"every concurrent recall (#492 in dream dedup, #858 in mutateEngram — both reproduced "+
			"under -race with two goroutines), and it publishes UNCOMMITTED state to readers the "+
			"instant the assignment runs.\n\n"+
			"Fix it:\n"+
			"    eng, err := ps.GetEngram(ctx, ws, id)\n"+
			"    ...\n"+
			"    eng = eng.Clone()   // private from here on\n"+
			"    eng.State = StateSoftDeleted\n\n"+
			"A per-engram stripe lock is NOT a fix: recall's readers do not take casLocks.\n"+
			"If the value provably cannot be the cached one, add a cachedEngramExemptions entry "+
			"with the STRUCTURAL reason — not an observation that nothing currently reads it.", b.String())
	}

	for key, reason := range cachedEngramExemptions {
		if !seenExempt[key] {
			t.Errorf("cachedEngramExemptions has a stale entry %q (%s): no in-place mutation of a "+
				"cache-returned engram was found there. The site was fixed, moved or renamed — drop "+
				"the exemption so it stops reading as coverage.", key, reason)
		}
	}

	t.Logf("census scanned %d non-test files; %d function(s) bind a cache-returned engram, %d offender(s), %d exempt",
		scanned, len(sourceKeys), len(offenders), len(seenExempt))
}

// cachedEngramKnownSources is the census's floor on its own INPUT SET: every
// "<relpath>:<func>" here must be seen to bind a value returned by GetEngram,
// GetEngrams or an L1Cache Get.
//
// These are exactly the seven functions #858's AST census found mutating that
// pointer in place, all seven now fixed with Clone. They are the floor because
// each is, by construction, a function this census must be able to SEE — if the
// walk or the source matcher loses one, it has lost the ability to notice a
// revert of that fix.
//
// A count would not do. `len(sources) >= 7` tells the next author that
// something vanished, not what; naming them makes a legitimate removal a diff a
// reviewer can read.
var cachedEngramKnownSources = map[string]string{
	"internal/storage/batch.go:pebbleStoreBatch.mutateEngram": "#858 itself — the reported race, reached from " +
		"Engine.EvolveAt via SupersedeEngram and from UpdateEngramState",
	"internal/storage/engram.go:PebbleStore.SoftDelete": "read-modify-write under casLocks; the stripe lock does " +
		"not cover recall's unlocked readers, demonstrated under -race",
	"internal/storage/engram.go:PebbleStore.UpdateTagsLocked": "replaced eng.Tags on the shared struct, which is " +
		"also the STO-18 removal-diff source",
	"internal/storage/engram.go:PebbleStore.UpdateConfidence": "demonstrated under -race against a reader goroutine " +
		"doing nothing but GetEngram in a loop",
	"internal/storage/engram.go:PebbleStore.UpdateConfidenceWithContradiction": "the #559 lost-update path; same " +
		"shape as UpdateConfidence",
	"internal/storage/entity.go:PebbleStore.UpdateDigest": "the enrichment worker's write path — no stripe lock at " +
		"all, and it aliased the caller's keyPoints slice into the cached struct",
	"internal/engine/engine.go:Engine.Restore": "mutated the cached struct purely to shape its RETURN value, and " +
		"returned the cache's own pointer to the caller",
}

// cachedEngramExemptions maps "<relpath>:<func>" to the reason an in-place
// field write through a cache-returned engram is safe there. The bar is a
// STRUCTURAL argument, not an observation. A stale entry fails the census.
//
// It is empty, and that is the intended steady state.
var cachedEngramExemptions = map[string]string{}

// cachedEngramFieldWrite is one assignment through a cache-tainted value.
type cachedEngramFieldWrite struct {
	node ast.Node
	lhs  ast.Expr
	root string
}

// cachedEngramSource reports whether e is a call that returns the L1 cache's
// shared *Engram: `x.GetEngram(...)`, `x.GetEngrams(...)`, or `x.cache.Get(...)`
// / `x.engramCache.Get(...)`.
//
// The receiver is deliberately not resolved — go/ast alone cannot type it, and
// requiring a `*PebbleStore` receiver would blind the census to every caller
// that holds the store behind an interface, which is most of internal/engine.
// Over-matching a same-named method on an unrelated type produces a false
// alarm, which is the safe direction.
func cachedEngramSource(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "GetEngram", "GetEngrams":
		return true
	case "Get":
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		return strings.Contains(strings.ToLower(inner.Sel.Name), "cache")
	}
	return false
}

// cachedEngramIdents computes the identifiers in body that hold a
// cache-returned *Engram (or a slice of them).
//
// Propagation is deliberately narrow — a bare `b := a`, an index `e := engs[i]`,
// and a `for _, e := range engs` — because those are the three shapes the real
// call sites use and every wider rule tried here produced false alarms. In
// particular a call-expression RHS does NOT propagate, which is what makes
// `cp := eng.Clone()` bind an untainted name.
func cachedEngramIdents(body *ast.BlockStmt) map[string]bool {
	t := map[string]bool{}
	for round := 0; round < 8; round++ {
		grew := false
		mark := func(n string) {
			if n != "" && n != "_" && !t[n] {
				t[n] = true
				grew = true
			}
		}
		markLHS := func(e ast.Expr) {
			if id, ok := e.(*ast.Ident); ok {
				mark(id.Name)
			}
		}
		visit := func(lhs []ast.Expr, rhs []ast.Expr) {
			if len(rhs) == 1 && cachedEngramSource(rhs[0]) && len(lhs) > 0 {
				markLHS(lhs[0]) // eng, err := s.GetEngram(...)
				return
			}
			if len(lhs) != len(rhs) {
				return
			}
			for i, l := range lhs {
				switch r := rhs[i].(type) {
				case *ast.Ident:
					if t[r.Name] {
						markLHS(l)
					}
				case *ast.IndexExpr:
					if b, ok := r.X.(*ast.Ident); ok && t[b.Name] {
						markLHS(l)
					}
				}
			}
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				visit(node.Lhs, node.Rhs)
			case *ast.ValueSpec:
				var names []ast.Expr
				for _, nm := range node.Names {
					names = append(names, nm)
				}
				visit(names, node.Values)
			case *ast.RangeStmt:
				if x, ok := node.X.(*ast.Ident); ok && t[x.Name] && node.Value != nil {
					markLHS(node.Value)
				}
			}
			return true
		})
		if !grew {
			break
		}
	}
	return t
}

// cloneReleasePositions returns, per tainted identifier, the positions at which
// that identifier is REBOUND to a clone — `eng = eng.Clone()`. From that point
// on in the function the name holds a private struct and mutating it is the
// sanctioned idiom, so sinks after it are not reported.
//
// Only a self-rebinding assignment counts. `cp := eng.Clone()` needs no entry:
// cp was never tainted, because cachedEngramIdents does not propagate through
// a call.
func cloneReleasePositions(body *ast.BlockStmt, tainted map[string]bool) map[string][]token.Pos {
	out := map[string][]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || !tainted[id.Name] {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Clone" {
			return true
		}
		// The release must clone THIS name, not some other engram.
		if src, ok := sel.X.(*ast.Ident); !ok || src.Name != id.Name {
			return true
		}
		out[id.Name] = append(out[id.Name], as.End())
		return true
	})
	return out
}

// cachedEngramFieldWrites returns every assignment that writes THROUGH a
// cache-tainted identifier: a field write (`eng.State = x`), an element write
// (`eng.Tags[0] = x`, which is unsafe even after a bare struct copy), a
// compound assign, and `eng.AccessCount++`.
//
// A write to the bare identifier itself (`eng = other`) is a rebinding, not a
// mutation, and is not a sink.
func cachedEngramFieldWrites(body *ast.BlockStmt, tainted map[string]bool) []cachedEngramFieldWrite {
	var out []cachedEngramFieldWrite
	consider := func(node ast.Node, lhs ast.Expr) {
		if _, bare := lhs.(*ast.Ident); bare {
			return
		}
		id := rootIdent(lhs)
		if id == nil || !tainted[id.Name] {
			return
		}
		out = append(out, cachedEngramFieldWrite{node: node, lhs: lhs, root: id.Name})
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, l := range node.Lhs {
				consider(node, l)
			}
		case *ast.IncDecStmt:
			consider(node, node.X)
		}
		return true
	})
	return out
}

// cachedEngramFixture is the source the census's matcher is tested against. It
// is parsed, never compiled or linked: nothing in it is a real type, and the
// names are invented.
//
// Every function is one shape the matcher must classify. None is hypothetical:
// `strippedLock` is UpdateConfidence, `viaBatchCallback` is mutateEngram's func
// literal, `returnShaping` is Engine.Restore, `rangeOverGetEngrams` is the #492
// dedup shape, and `clonedFirst` is what all seven now look like.
const cachedEngramFixture = `package fixture

type Engram struct {
	State      int
	Tags       []string
	UpdatedAt  int64
}

func (e *Engram) Clone() *Engram { c := *e; return &c }

type store struct{ cache cacheT }
type cacheT struct{}

func (c cacheT) Get(id string) (*Engram, bool) { return nil, false }
func (s *store) GetEngram(id string) (*Engram, error)    { return nil, nil }
func (s *store) GetEngrams(ids []string) ([]*Engram, error) { return nil, nil }

// --- shapes that MUST be reported ---

func directMutate(s *store) {
	eng, _ := s.GetEngram("x")
	eng.State = 1
}

func viaCacheGet(s *store) {
	eng, _ := s.cache.Get("x")
	eng.State = 1
}

func rangeOverGetEngrams(s *store) {
	engs, _ := s.GetEngrams(nil)
	for _, e := range engs {
		e.State = 1
	}
}

func viaIndex(s *store) {
	engs, _ := s.GetEngrams(nil)
	e := engs[0]
	e.State = 1
}

func aliasHop(s *store) {
	eng, _ := s.GetEngram("x")
	a := eng
	b := a
	b.State = 1
}

func incDec(s *store) {
	eng, _ := s.GetEngram("x")
	eng.UpdatedAt++
}

func compoundAssign(s *store) {
	eng, _ := s.GetEngram("x")
	eng.UpdatedAt += 1
}

func sliceElement(s *store) {
	eng, _ := s.GetEngram("x")
	eng.Tags[0] = "x"
}

func viaBatchCallback(s *store, mutate func(*Engram)) {
	eng, _ := s.GetEngram("x")
	mutate(eng)
	eng.UpdatedAt = 1
}

// A release does not retroactively bless a sink ABOVE it.
func mutateThenClone(s *store) {
	eng, _ := s.GetEngram("x")
	eng.State = 1
	eng = eng.Clone()
	eng.State = 2
}

// Cloning a DIFFERENT engram must not release this one.
func clonedTheWrongOne(s *store) {
	eng, _ := s.GetEngram("x")
	other, _ := s.GetEngram("y")
	eng = other.Clone()
	eng.State = 1
}

// --- shapes that must NOT be reported ---

func clonedFirst(s *store) {
	eng, _ := s.GetEngram("x")
	eng = eng.Clone()
	eng.State = 1
	eng.Tags[0] = "x"
}

func separateCloneBinding(s *store) {
	eng, _ := s.GetEngram("x")
	cp := eng.Clone()
	cp.State = 1
}

func readOnly(s *store) {
	eng, _ := s.GetEngram("x")
	local := eng.State
	_ = local
}

func rebindIsNotMutation(s *store, other *Engram) {
	eng, _ := s.GetEngram("x")
	eng = other
	_ = eng
}

func locallyConstructed() {
	eng := &Engram{}
	eng.State = 1
}

func unrelatedGetter(m map[string]*Engram) {
	eng := m["x"]
	eng.State = 1
}
`

// TestCachedEngramCensusMatcher is the census's self-check, and it exists
// because TestCachedEngramMutationCensus cannot vouch for its own matcher.
//
// The census's steady state is ZERO offenders. There is therefore no count it
// can assert on itself: delete cachedEngramFieldWrites' body and it reports a
// serene PASS. That is precisely the failure TestLastAccessElapsedCensus
// shipped with — a vacuity guard that could never fire, green while silently
// losing five of its six sites — and the reason its remedy was a matcher unit
// test over a parsed fixture plus a named floor. Both are reproduced here: this
// is the matcher half, cachedEngramKnownSources is the input-set half.
//
// Neuter cachedEngramSource, cachedEngramIdents, cachedEngramFieldWrites or
// cloneReleasePositions and this names the exact shape that stopped being seen.
func TestCachedEngramCensusMatcher(t *testing.T) {
	want := map[string]struct {
		sinks int
		why   string
	}{
		"directMutate":         {1, "the base shape — eng, _ := s.GetEngram(...); eng.State = 1"},
		"viaCacheGet":          {1, "an L1Cache Get read directly, not through GetEngram"},
		"rangeOverGetEngrams":  {1, "the #492 dedup shape: taint must reach the range value over a GetEngrams slice"},
		"viaIndex":             {1, "taint must survive an index off a GetEngrams slice"},
		"aliasHop":             {1, "taint must survive two ident-to-ident hops"},
		"incDec":               {1, "eng.Field++ is a mutation; an AssignStmt-only matcher misses it"},
		"compoundAssign":       {1, "eng.Field += 1 is a mutation"},
		"sliceElement":         {1, "eng.Tags[0] = x aliases the cached engram's backing array — unsafe even after a bare struct copy"},
		"viaBatchCallback":     {1, "mutateEngram's own shape: a func literal is handed the pointer, and the enclosing body still writes through it"},
		"mutateThenClone":      {1, "a release must not bless a sink ABOVE it — only the second write is safe"},
		"clonedTheWrongOne":    {1, "cloning a DIFFERENT engram into the name must not count as releasing it"},
		"clonedFirst":          {0, "the sanctioned idiom: eng = eng.Clone() releases the name, including for element writes"},
		"separateCloneBinding": {0, "cp := eng.Clone() binds an untainted name — a call RHS must not propagate taint"},
		"readOnly":             {0, "reading a field is not a sink; a matcher that flags this flags every recall"},
		"rebindIsNotMutation":  {0, "assigning to the bare identifier rebinds the local, it does not touch the struct"},
		"locallyConstructed":   {0, "a freshly built &Engram{} is not cache-derived"},
		"unrelatedGetter":      {0, "a map index is not one of the three cache sources"},
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cachedEngramFixture.go", cachedEngramFixture, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	seen := map[string]bool{}
	for _, unit := range censusUnits(f) {
		exp, ok := want[unit.label]
		if !ok {
			if unit.label == "Engram.Clone" || strings.HasPrefix(unit.label, "store.") || strings.HasPrefix(unit.label, "cacheT.") {
				continue // fixture scaffolding
			}
			t.Errorf("fixture unit %q has no expectation — add one, or the fixture is dead weight", unit.label)
			continue
		}
		seen[unit.label] = true

		tainted := cachedEngramIdents(unit.body)
		releases := cloneReleasePositions(unit.body, tainted)
		got := 0
		for _, s := range cachedEngramFieldWrites(unit.body, tainted) {
			if guardedBefore(releases[s.root], s.node.Pos()) {
				continue
			}
			got++
		}
		if got != exp.sinks {
			t.Errorf("%s: matcher reported %d offending write(s), want %d — %s\n"+
				"The census walks the module with this same matcher, and its steady state is ZERO "+
				"offenders, so a shape it stops seeing vanishes SILENTLY: the census still passes, "+
				"having lost the ability to notice that mutation class.",
				unit.label, got, exp.sinks, exp.why)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("expectation %q has no unit in cachedEngramFixture — it was renamed or dropped, "+
				"so that shape is no longer being checked", name)
		}
	}
}
