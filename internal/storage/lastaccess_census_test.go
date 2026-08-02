package storage

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLastAccessElapsedCensus is the machine check that replaces a prose claim.
//
// # Why it exists
//
// #810 shipped with the sentence "the four read-side guard sites" in three
// places — an invariant, a source comment, and a PR body — each derived from a
// grep. The enumeration was wrong three separate times in a single PR: it
// missed engine.computeComponents, it miscounted the pre-existing copies as
// four when the merge base had two, and it named the dead DecayWorker as the
// one unguarded site while trigger.TriggerScore — LIVE, and fed persisted
// metadata by the periodic sweep — had the identical shape. A grep enumerates
// what its author thought to search for. This walks the AST of every non-test
// Go file in the module and enumerates what is actually there.
//
// The governing rule, stated so it is checkable: a set named in prose must be
// regenerable from a mechanism, not asserted by grep.
//
// # What it asserts
//
// For every function in the module, every computation of an ELAPSED INTERVAL
// from a LastAccess-derived value — time.Since(x), y.Sub(x), or x.Sub(y) where
// x is LastAccess-tainted — must be lexically preceded, in the same function,
// by a call to IsUnsetTimestamp on a LastAccess-tainted value, or appear in
// censusExemptions with a stated reason.
//
// "LastAccess-tainted" is a monotone intra-function dataflow: a value is
// tainted if it is a `.LastAccess` selector, or is assigned from an expression
// containing one (transitively). That is what lets it see through the local
// copy MOST guarded sites use — `lastAccess := eng.LastAccess`,
// `lastAccess := time.Unix(0, item.LastAccess)`. Five of the six live sites
// have that shape; DecayWorker.processBatch computes `now.Sub(c.LastAccess)`
// off a direct selector and needs no dataflow at all, as does the
// working.Manager.GC exemption. So a selector-only matcher would NOT report
// zero and look green — it would report exactly those two and lose the other
// five, which is why the vacuity check below cannot be a count and is instead
// TestLastAccessCensusMatcherSeesLaunderedCopies: a fixture-driven unit test of
// the matcher itself. (An earlier revision of this comment, and the commit
// message that carried it, both claimed "EVERY guarded site" and "zero sites";
// both were false, and the guard designed on them was vacuous.)
//
// # What it does NOT catch — the honest boundary
//
//   - INTER-FUNCTION taint. A helper that takes a bare time.Time parameter and
//     computes elapsed time from it is invisible here; the sentinel would have
//     to be laundered through a call. No such helper exists today (the census
//     would have to grow a call graph to see one).
//   - Taint that leaves the function through a NON-ROOTED assignment target.
//     `h.la = m.LastAccess` taints `h` (see rootIdent), so the intra-function
//     sink is caught — but the taint is recorded against the local root name
//     only. Storing into a value that outlives the function (a package-level
//     var, a field of a pointer receiver read back in another method) and
//     computing elapsed time THERE is inter-function taint by another route,
//     and is not seen.
//   - Guards that are LEXICALLY earlier but do not actually dominate the sink —
//     a guard inside an unrelated `if` branch satisfies this check. It pins that
//     someone thought about the sentinel at that site, not that the branch is
//     reachable. The behavioural pins do the rest.
//   - Non-elapsed misuse: rendering LastAccess to a wire field, or an
//     `.IsZero()`-only guard (which is FALSE for the 1754 sentinel — that is
//     the whole bug). Those are pinned behaviourally, not here.
//
// Stating the boundary matters: a partial matcher read as full coverage is
// worse than no matcher, which is the lesson TestPointGetReadersAreCovered
// records in this same package.
func TestLastAccessElapsedCensus(t *testing.T) {
	root := moduleRoot(t)

	fset := token.NewFileSet()
	var files []*ast.File
	var paths []string
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
		files = append(files, f)
		paths = append(paths, filepath.ToSlash(rel))
		scanned++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("census scanned no non-test Go files under %s — wrong module root?", root)
	}

	type site struct {
		rel  string
		fn   string
		pos  string
		expr string
	}
	var unguarded, guarded []site
	seenExempt := map[string]bool{}

	for i, f := range files {
		rel := paths[i]
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			tainted := taintedLastAccessIdents(fn.Body)
			guards := lastAccessGuardPositions(fn.Body, tainted)

			for _, call := range elapsedFromLastAccess(fn.Body, tainted) {
				s := site{
					rel:  rel,
					fn:   funcLabel(fn),
					pos:  fset.Position(call.Pos()).String(),
					expr: exprString(fset, call),
				}
				if guardedBefore(guards, call.Pos()) {
					guarded = append(guarded, s)
					continue
				}
				key := rel + ":" + s.fn
				if _, ok := censusExemptions[key]; ok {
					seenExempt[key] = true
					continue
				}
				unguarded = append(unguarded, s)
			}
		}
	}

	// Vacuity floor, and its honest limit. This fires only if the FIELD ITSELF
	// disappears — renamed, or the walk stopped finding files. It does NOT
	// detect a broken matcher: two live sites (DecayWorker.processBatch and the
	// working.Manager.GC exemption) compute elapsed time off a direct
	// `.LastAccess` selector, so total is >= 2 even with the taint analysis
	// deleted outright. That was measured, not assumed: neutering
	// taintedLastAccessIdents to return an empty map lost five of the six sites
	// and this check still passed. The matcher's own health is pinned by
	// TestLastAccessCensusMatcherSeesLaunderedCopies instead.
	total := len(guarded) + len(unguarded) + len(seenExempt)
	if total == 0 {
		t.Fatalf("census found NO elapsed-from-LastAccess computations in %d files. The field was "+
			"renamed, or the walk found nothing; either way this test is now vacuous. "+
			"Re-point it or delete it deliberately.", scanned)
	}

	if len(unguarded) > 0 {
		sort.Slice(unguarded, func(i, j int) bool { return unguarded[i].pos < unguarded[j].pos })
		var b strings.Builder
		for _, s := range unguarded {
			b.WriteString("\n  " + s.pos + "  in " + s.fn + "():  " + s.expr)
		}
		t.Errorf("UNGUARDED elapsed-time computation from a LastAccess value:%s\n\n"+
			"An unset LastAccess is either the plain zero time (year 1) or erf.ZeroTimeSentinelNanos "+
			"(year 1754, whose IsZero() is FALSE). Both yield ~740,000 days elapsed, which silently "+
			"zeroes every recency/decay term downstream — a silently-empty recall on a weighted_sum "+
			"vault (#810), and a subscription that never fires on the push path.\n\n"+
			"Fix it:\n"+
			"    la := <x>.LastAccess\n"+
			"    if storage.IsUnsetTimestamp(la) { la = now }   // never accessed == just written\n"+
			"    daysSince := now.Sub(la).Hours() / 24.0\n\n"+
			"If the value provably cannot carry the sentinel (e.g. an in-memory session field never "+
			"round-tripped through ERF), add a censusExemptions entry with the reason instead — but "+
			"say WHY it cannot, not that it currently does not.", b.String())
	}

	for key, reason := range censusExemptions {
		if !seenExempt[key] {
			t.Errorf("censusExemptions has a stale entry %q (%s): no unguarded elapsed-from-LastAccess "+
				"computation was found there. The site was fixed, moved or renamed — drop the "+
				"exemption so it stops reading as coverage.", key, reason)
		}
	}

	sort.Slice(guarded, func(i, j int) bool { return guarded[i].pos < guarded[j].pos })
	var g []string
	for _, s := range guarded {
		g = append(g, s.pos+" ("+s.fn+")")
	}
	t.Logf("census scanned %d non-test files; %d guarded site(s), %d exempt:\n  %s",
		scanned, len(guarded), len(seenExempt), strings.Join(g, "\n  "))
}

// censusMatcherFixture is the source the census's own matcher is tested
// against. It is parsed, never compiled or linked: nothing in it is a real
// type, and the names are invented.
//
// Every function is one shape the matcher must classify. The shapes are not
// hypothetical — `localCopy` is what five of the six live sites look like,
// `directSelector` is DecayWorker.processBatch, and the four launder shapes are
// the ones a bare-`*ast.Ident` assignment target silently dropped.
const censusMatcherFixture = `package fixture

import "time"

type record struct {
	LastAccess time.Time
	CreatedAt  time.Time
}

type holder struct{ la time.Time }

// --- shapes that MUST be seen ---

func localCopy(rec record, now time.Time) time.Duration {
	lastAccess := rec.LastAccess
	return now.Sub(lastAccess)
}

func localCopyGuarded(rec record, now time.Time) time.Duration {
	lastAccess := rec.LastAccess
	if IsUnsetTimestamp(lastAccess) {
		lastAccess = now
	}
	return now.Sub(lastAccess)
}

func transitiveCopy(rec record) time.Duration {
	a := rec.LastAccess
	b := a
	return time.Since(b)
}

func copyThroughCall(rec record) time.Duration {
	la := time.Unix(0, rec.LastAccess.UnixNano())
	return time.Since(la)
}

func directSelector(rec record, now time.Time) time.Duration {
	return now.Sub(rec.LastAccess)
}

func structFieldLaunder(rec record, h *holder) time.Duration {
	h.la = rec.LastAccess
	return time.Since(h.la)
}

func sliceElemLaunder(rec record, buf []time.Time) time.Duration {
	buf[0] = rec.LastAccess
	return time.Since(buf[0])
}

func mapLaunder(rec record, mm map[string]time.Time) time.Duration {
	mm["k"] = rec.LastAccess
	return time.Since(mm["k"])
}

func pointerLaunder(rec record, p *time.Time) time.Duration {
	*p = rec.LastAccess
	return time.Since(*p)
}

// --- shapes that must NOT be seen, or must not count as guarded ---

func unrelatedField(rec record) time.Duration {
	c := rec.CreatedAt
	return time.Since(c)
}

func guardOnADifferentValue(rec record, now time.Time) time.Duration {
	lastAccess := rec.LastAccess
	created := rec.CreatedAt
	if IsUnsetTimestamp(created) {
		created = now
	}
	return now.Sub(lastAccess)
}
`

// TestLastAccessCensusMatcherSeesLaunderedCopies is the census's self-check.
//
// TestLastAccessElapsedCensus cannot vouch for its own matcher. Its vacuity
// guard is `total == 0`, and two live sites reach the sink through a bare
// `.LastAccess` selector with no dataflow involved at all — so deleting the
// taint analysis entirely leaves total == 2 and the census reports PASS while
// having lost five of its six sites. That was demonstrated, not theorised.
//
// This test pins the matcher directly instead: a fixture with one function per
// shape, run through the same taintedLastAccessIdents / elapsedFromLastAccess /
// lastAccessGuardPositions the census uses. Neuter any of the three and this
// fails with the specific shape that stopped being seen.
func TestLastAccessCensusMatcherSeesLaunderedCopies(t *testing.T) {
	cases := map[string]struct {
		sinks   int
		guarded bool
		why     string
	}{
		"localCopy":              {sinks: 1, guarded: false, why: "the local-copy shape five of the six live sites use"},
		"localCopyGuarded":       {sinks: 1, guarded: true, why: "the guarded local copy — the IsUnsetTimestamp call must be recognised on the tainted ident"},
		"transitiveCopy":         {sinks: 1, guarded: false, why: "taint must survive a second hop (a := x.LastAccess; b := a)"},
		"copyThroughCall":        {sinks: 1, guarded: false, why: "taint must survive being rebuilt through a call (time.Unix(0, ...))"},
		"directSelector":         {sinks: 1, guarded: false, why: "the no-dataflow shape (DecayWorker.processBatch)"},
		"structFieldLaunder":     {sinks: 1, guarded: false, why: "h.la = rec.LastAccess must taint h"},
		"sliceElemLaunder":       {sinks: 1, guarded: false, why: "buf[0] = rec.LastAccess must taint buf"},
		"mapLaunder":             {sinks: 1, guarded: false, why: `mm["k"] = rec.LastAccess must taint mm`},
		"pointerLaunder":         {sinks: 1, guarded: false, why: "*p = rec.LastAccess must taint p"},
		"unrelatedField":         {sinks: 0, guarded: false, why: "CreatedAt is not LastAccess — a matcher that flags this flags everything"},
		"guardOnADifferentValue": {sinks: 1, guarded: false, why: "an IsUnsetTimestamp call on an UNTAINTED value must not count as this site's guard"},
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "censusMatcherFixture.go", censusMatcherFixture, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	seen := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		want, ok := cases[fn.Name.Name]
		if !ok {
			t.Errorf("fixture function %q has no expectation — add one, or the fixture is dead weight", fn.Name.Name)
			continue
		}
		seen[fn.Name.Name] = true

		tainted := taintedLastAccessIdents(fn.Body)
		guards := lastAccessGuardPositions(fn.Body, tainted)
		sinks := elapsedFromLastAccess(fn.Body, tainted)

		if len(sinks) != want.sinks {
			var got []string
			for _, s := range sinks {
				got = append(got, exprString(fset, s))
			}
			t.Errorf("%s(): matcher found %d elapsed-from-LastAccess site(s), want %d — %s\n  found: %v\n"+
				"The census walks the module with this same matcher. A shape it stops seeing vanishes "+
				"from the census SILENTLY: the census still passes, having lost a guard site.",
				fn.Name.Name, len(sinks), want.sinks, want.why, got)
			continue
		}
		if want.sinks == 0 {
			continue
		}
		gotGuarded := guardedBefore(guards, sinks[0].Pos())
		if gotGuarded != want.guarded {
			t.Errorf("%s(): site guarded=%v, want %v — %s", fn.Name.Name, gotGuarded, want.guarded, want.why)
		}
	}

	for name := range cases {
		if !seen[name] {
			t.Errorf("expectation %q has no function in censusMatcherFixture — it was renamed or dropped, "+
				"so that shape is no longer being checked", name)
		}
	}
}

// censusExemptions maps "<relpath>:<func>" to the reason an elapsed-time
// computation from a LastAccess value needs no unset-timestamp guard. The bar
// is a STRUCTURAL argument that the value cannot carry the sentinel, not an
// observation that it currently does not. A stale entry fails the census.
var censusExemptions = map[string]string{
	"internal/working/manager.go:Manager.GC": "working.WorkingMemory.LastAccess is an in-process session " +
		"field: it is set to time.Now() at Create and at every Touch, is never ERF-encoded, never " +
		"persisted to Pebble and never populated from a decoded record, so neither unset shape can " +
		"reach it. Its zero value would also make GC evict the session, which is fail-safe.",
}

// funcLabel renders "Recv.Name" for a method and "Name" for a function.
func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	if idx, ok := t.(*ast.IndexExpr); ok { // generic receiver
		if id, ok := idx.X.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
	}
	return fn.Name.Name
}

// containsLastAccessSelector reports whether e contains a `.LastAccess` selector.
func containsLastAccessSelector(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "LastAccess" {
			found = true
		}
		return !found
	})
	return found
}

// containsTainted reports whether e is, or contains, a LastAccess-derived value.
func containsTainted(e ast.Node, tainted map[string]bool) bool {
	if containsLastAccessSelector(e) {
		return true
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && tainted[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

// taintedLastAccessIdents computes the monotone fixpoint of identifiers in body
// that hold a LastAccess-derived value. Monotone means an identifier that is
// later overwritten with a safe value (`lastAccess = now`, the guard's own
// repair) stays tainted — deliberately, since the sink after it is exactly the
// computation we want to see.
func taintedLastAccessIdents(body *ast.BlockStmt) map[string]bool {
	tainted := map[string]bool{}
	for round := 0; round < 8; round++ {
		grew := false
		mark := func(lhs ast.Expr, rhs ast.Expr) {
			id := rootIdent(lhs)
			if id == nil || id.Name == "_" || tainted[id.Name] {
				return
			}
			if rhs != nil && containsTainted(rhs, tainted) {
				tainted[id.Name] = true
				grew = true
			}
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					switch {
					case len(node.Rhs) == len(node.Lhs):
						mark(lhs, node.Rhs[i])
					case len(node.Rhs) == 1:
						// v, err := f(x.LastAccess) — conservatively taint all.
						mark(lhs, node.Rhs[0])
					}
				}
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if i < len(node.Values) {
						mark(name, node.Values[i])
					} else if len(node.Values) == 1 {
						mark(name, node.Values[0])
					}
				}
			}
			return true
		})
		if !grew {
			break
		}
	}
	return tainted
}

// rootIdent walks an assignment target down to the identifier it is rooted at:
//
//	h.la      -> h        (struct field)
//	buf[0]    -> buf      (slice/array element)
//	mm["k"]   -> mm       (map entry)
//	*p        -> p        (pointer indirection)
//	(x)       -> x
//
// It returns nil for a target with no identifier at its root.
//
// Tainting the ROOT is deliberately conservative. `h.la = m.LastAccess` taints
// the whole of `h`, so a later `time.Since(h.anythingElse)` in the same
// function is flagged too. That over-taints, which can only ever produce a
// false ALARM at the census — noisy, and resolved by an exemption with a
// stated reason. Matching only a bare `*ast.Ident` target, which is what this
// did before, under-taints: it launders the sentinel through all four shapes
// above and reports the sink as absent. The census exists because it beats a
// grep, and `grep LastAccess` WOULD have found `h.la = m.LastAccess`.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch n := e.(type) {
		case *ast.Ident:
			return n
		case *ast.ParenExpr:
			e = n.X
		case *ast.SelectorExpr:
			e = n.X
		case *ast.IndexExpr:
			e = n.X
		case *ast.StarExpr:
			e = n.X
		default:
			return nil
		}
	}
}

// lastAccessGuardPositions returns the positions of every IsUnsetTimestamp call
// in body whose argument is LastAccess-derived.
func lastAccessGuardPositions(body *ast.BlockStmt, tainted map[string]bool) []token.Pos {
	var out []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if name != "IsUnsetTimestamp" {
			return true
		}
		for _, arg := range call.Args {
			if containsTainted(arg, tainted) {
				out = append(out, call.Pos())
				break
			}
		}
		return true
	})
	return out
}

// elapsedFromLastAccess returns every call in body that computes an elapsed
// interval from a LastAccess-derived value: time.Since(x), y.Sub(x), x.Sub(y).
func elapsedFromLastAccess(body *ast.BlockStmt, tainted map[string]bool) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Since":
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "time" {
				return true
			}
			if len(call.Args) == 1 && containsTainted(call.Args[0], tainted) {
				out = append(out, call)
			}
		case "Sub":
			// y.Sub(x) — either operand carrying LastAccess makes the
			// resulting duration sentinel-poisoned.
			if containsTainted(sel.X, tainted) {
				out = append(out, call)
				return true
			}
			if len(call.Args) == 1 && containsTainted(call.Args[0], tainted) {
				out = append(out, call)
			}
		}
		return true
	})
	return out
}

func guardedBefore(guards []token.Pos, sink token.Pos) bool {
	for _, g := range guards {
		if g < sink {
			return true
		}
	}
	return false
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "<unprintable>"
	}
	return b.String()
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
