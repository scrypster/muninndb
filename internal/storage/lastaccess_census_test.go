package storage

import (
	"fmt"
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
// from a LastAccess-derived value — time.Since(x), y.Sub(x), x.Sub(y), or the
// integer-nanosecond form `nowNs - x.LastAccess.UnixNano()` — must be lexically
// preceded, in the same function, by a call to IsUnsetTimestamp on a
// LastAccess-tainted value, or appear in censusExemptions with a stated reason.
//
// "Function" includes a PACKAGE-LEVEL func literal; see censusUnits.
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
//   - Elapsed time in integer nanoseconds is now COVERED (see
//     elapsedNanosFromLastAccess) but only through an epoch conversion the
//     matcher can see — `x.UnixNano()`, or a local holding one. Arithmetic on a
//     nanosecond value that reached the function as a bare int64 parameter is
//     inter-function taint, above.
//
// Stating the boundary matters: a partial matcher read as full coverage is
// worse than no matcher, which is the lesson TestPointGetReadersAreCovered
// records in this same package. Two of the entries above were NOT in the first
// version of this list and were found by injecting probes into a real package —
// a package-level func literal (the walk visited only *ast.FuncDecl) and the
// integer-nanosecond form (outside the list entirely, and the idiom
// mbp.ActivationItem's int64 LastAccess already forces). Both are now covered.
// A boundary list assembled by introspection is a hypothesis; probe it.
//
// # The input set is checked too, and that is a SEPARATE property
//
// TestLastAccessCensusMatcherSeesLaunderedCopies pins the matcher. It does not
// pin the WalkDir that feeds it, and the two are different properties: adding
// three directory names to the walk's skip list dropped four of the six sites
// and left BOTH tests green, because "found zero unguarded sites" is exactly
// what "found no files" looks like. `censusKnownSites` is the floor — six
// file:func pairs that must each be present AND guarded — so a walk change that
// loses one has to name it.
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
	guardedKeys := map[string]bool{}

	for i, f := range files {
		rel := paths[i]
		for _, unit := range censusUnits(f) {
			tainted := taintedLastAccessIdents(unit.body)
			guards := lastAccessGuardPositions(unit.body, tainted)

			for _, call := range elapsedFromLastAccess(unit.body, tainted) {
				s := site{
					rel:  rel,
					fn:   unit.label,
					pos:  fset.Position(call.Pos()).String(),
					expr: exprString(fset, call),
				}
				key := rel + ":" + s.fn
				if guardedBefore(guards, call.Pos()) {
					guarded = append(guarded, s)
					guardedKeys[key] = true
					continue
				}
				if _, ok := censusExemptions[key]; ok {
					seenExempt[key] = true
					continue
				}
				unguarded = append(unguarded, s)
			}
		}
	}

	// The FLOOR on the input set. See censusKnownSites.
	var missing []string
	for key, why := range censusKnownSites {
		if !guardedKeys[key] {
			missing = append(missing, "  "+key+"\n      "+why)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("censusKnownSites: %d of %d known guard site(s) were NOT found as guarded by this walk:\n%s\n\n"+
			"Exactly one of these is true, and they need opposite responses:\n"+
			"  (a) the site was legitimately removed or renamed — update censusKnownSites in the same commit, "+
			"stating what replaced it;\n"+
			"  (b) the walk stopped reaching it, or the matcher stopped seeing it — the census is now blind "+
			"there and reports PASS on an unguarded site.\n"+
			"This floor exists because (b) is invisible without it: adding three directory names to the "+
			"WalkDir skip list dropped four of the six sites and left both this test and the matcher "+
			"self-check green.",
			len(missing), len(censusKnownSites), strings.Join(missing, "\n"))
	}

	// Vacuity backstop. This fires only if the FIELD ITSELF disappears. It is
	// the WEAKEST of the three checks in this file and is kept only for the
	// case censusKnownSites cannot describe (every site legitimately gone):
	// it does not detect a broken matcher (two live sites reach the sink off a
	// direct `.LastAccess` selector, so total >= 2 with the taint analysis
	// deleted outright — measured), and it does not detect a narrowed walk.
	// Those are censusKnownSites' job above and
	// TestLastAccessCensusMatcherSeesLaunderedCopies' job below.
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

// censusKnownSites is the census's floor on its own INPUT SET: every
// "<relpath>:<func>" here must be found by the walk AND reported as guarded.
//
// It exists because the previous round's self-check pinned the wrong half. That
// check drives taintedLastAccessIdents / lastAccessGuardPositions /
// elapsedFromLastAccess against a fixture, which is real — but it never drives
// the filepath.WalkDir that FEEDS them, and the census had no floor on how many
// files that walk was allowed to return. Adding three directory names to the
// skip list —
//
//	name == "activation" || name == "trigger" || name == "mcp"
//
// — dropped four of the six sites and left BOTH tests green, reporting
// "2 guarded site(s)" as a PASS. A matcher self-check and an input-set check are
// different properties; this is the second one.
//
// Yes, this is a floor of the shape an earlier round rejected as brittle. The
// rejection was of a bare `len(guarded) >= 6`, and it was right: a count tells
// the next author that something vanished, not what. Naming the sites is what
// makes the failure actionable — a legitimate removal updates this map in the
// same commit and says what replaced the site, which is a diff a reviewer can
// read.
var censusKnownSites = map[string]string{
	"internal/cognitive/decay.go:DecayWorker.processBatch": "the decay worker's batch scan — computes elapsed time straight off " +
		"a `.LastAccess` selector with no local copy, so it is one of the two sites a selector-only matcher still sees",
	"internal/engine/activation/engine.go:computeComponents": "weighted_sum scoring: the recency term and the Ebbinghaus decay " +
		"factor. This is the site #810's silently-empty recall came out of, and the site the original grep-based enumeration missed",
	"internal/engine/activation/engine.go:computeACTR": "ACT-R base-level activation — the DEFAULT fusion mode, and the one that " +
		"degrades SOONEST as LastAccess is rewound (measured: 7 days of source age vs weighted_sum's ~45)",
	"internal/engine/engine.go:Engine.PruneVault": "the pruner's base-level scan — an unset LastAccess here means ~740,000 days " +
		"idle, i.e. prune everything",
	"internal/engine/trigger/system.go:TriggerScore": "the push path's recency term (wRecency = 0.10). Live, fed persisted " +
		"metadata by the periodic sweep, and named as DEAD by the enumeration this census replaced",
	"internal/mcp/handlers.go:augmentAnnotations": "MCP staleness. Needs its own guard on top of the ERF decode repair because " +
		"it reads item.LastAccess (int64 nanos), through which the repair is invisible by construction",
}

// censusUnit is one function BODY the census analyses, with a label for
// reporting. A unit is an *ast.FuncDecl, or a PACKAGE-LEVEL *ast.FuncLit.
type censusUnit struct {
	label string
	body  *ast.BlockStmt
}

// censusUnits returns every function body in f that the census must analyse.
//
// The package-level func literal case is the blind spot S2: the walk used to
// iterate `f.Decls` looking only for *ast.FuncDecl, so
//
//	var scoreAge = func(m *storage.EngramMeta, now time.Time) float64 {
//	        return now.Sub(m.LastAccess).Hours() / 24.0
//	}
//
// was invisible — not because of any limitation in the taint analysis, but
// because the body was never handed to it. Found by injecting a probe into a
// real package, not by reasoning about the walk.
//
// Func literals NESTED inside a FuncDecl need no special handling: ast.Inspect
// descends into them from the enclosing body, so they are already analysed (with
// the enclosing function's taint, which over-approximates in the safe
// direction). Only the outermost literal of a package-level declaration is
// added here, for the same reason.
func censusUnits(f *ast.File) []censusUnit {
	var out []censusUnit
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out = append(out, censusUnit{label: funcLabel(d), body: d.Body})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range vs.Values {
					name := "_"
					if i < len(vs.Names) {
						name = vs.Names[i].Name
					}
					n := 0
					ast.Inspect(v, func(node ast.Node) bool {
						fl, ok := node.(*ast.FuncLit)
						if !ok {
							return true
						}
						label := name + " (func literal)"
						if n > 0 {
							label = fmt.Sprintf("%s (func literal #%d)", name, n+1)
						}
						n++
						out = append(out, censusUnit{label: label, body: fl.Body})
						return false // nested literals ride along with this body
					})
				}
			}
		}
	}
	return out
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

type holder struct{ la, created time.Time }

// --- shapes that MUST be seen ---

// S2: a sink inside a PACKAGE-LEVEL func literal. The walk used to visit only
// *ast.FuncDecl, so this body was never analysed at all.
var packageLevelFuncLit = func(rec record, now time.Time) time.Duration {
	return now.Sub(rec.LastAccess)
}

// S3: elapsed time computed in integer nanoseconds. Not inter-function taint
// and not "non-elapsed misuse" — outside the stated boundary list entirely, and
// the idiom this codebase already forces, since mbp.ActivationItem.LastAccess
// is an int64 nanosecond field.
func integerNanosElapsed(rec record, now time.Time) int64 {
	return now.UnixNano() - rec.LastAccess.UnixNano()
}

func integerNanosElapsedGuarded(rec record, now time.Time) int64 {
	la := rec.LastAccess
	if IsUnsetTimestamp(la) {
		la = now
	}
	return now.UnixNano() - la.UnixNano()
}

func guardOnTheLaunderedPath(rec record, h *holder, now time.Time) time.Duration {
	h.la = rec.LastAccess
	if IsUnsetTimestamp(h.la) {
		h.la = now
	}
	return now.Sub(h.la)
}

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

// The FALSE NEGATIVE the "over-tainting can only produce a false alarm" claim
// said was impossible. h.la = m.LastAccess taints the ROOT h, so a guard on
// h.created — an unrelated field — used to satisfy the sink below.
func guardOnADifferentFieldOfATaintedRoot(rec record, h *holder, now time.Time) time.Duration {
	h.la = rec.LastAccess
	if IsUnsetTimestamp(h.created) {
		h.created = now
	}
	return now.Sub(rec.LastAccess)
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
		"localCopy":          {sinks: 1, guarded: false, why: "the local-copy shape five of the six live sites use"},
		"localCopyGuarded":   {sinks: 1, guarded: true, why: "the guarded local copy — the IsUnsetTimestamp call must be recognised on the tainted ident"},
		"transitiveCopy":     {sinks: 1, guarded: false, why: "taint must survive a second hop (a := x.LastAccess; b := a)"},
		"copyThroughCall":    {sinks: 1, guarded: false, why: "taint must survive being rebuilt through a call (time.Unix(0, ...))"},
		"directSelector":     {sinks: 1, guarded: false, why: "the no-dataflow shape (DecayWorker.processBatch)"},
		"structFieldLaunder": {sinks: 1, guarded: false, why: "h.la = rec.LastAccess must taint h"},
		"sliceElemLaunder":   {sinks: 1, guarded: false, why: "buf[0] = rec.LastAccess must taint buf"},
		"mapLaunder":         {sinks: 1, guarded: false, why: `mm["k"] = rec.LastAccess must taint mm`},
		"pointerLaunder":     {sinks: 1, guarded: false, why: "*p = rec.LastAccess must taint p"},
		"packageLevelFuncLit (func literal)": {sinks: 1, guarded: false, why: "S2: a sink in a package-level func literal — the walk visited only *ast.FuncDecl, " +
			"so this body was never analysed. Found by injecting a probe into a real package"},
		"integerNanosElapsed": {sinks: 1, guarded: false, why: "S3: elapsed time in integer nanos (now.UnixNano() - x.LastAccess.UnixNano()). " +
			"Not covered by the Since/Sub matcher, and it is the idiom mbp.ActivationItem's int64 LastAccess forces"},
		"integerNanosElapsedGuarded": {sinks: 1, guarded: true, why: "S3 with its guard — the integer-nanos shape must still be able to be satisfied"},
		"guardOnTheLaunderedPath":    {sinks: 1, guarded: true, why: "a guard on the EXACT laundered path (h.la) must still count, or narrowing the guard side over-narrows"},
		"unrelatedField":             {sinks: 0, guarded: false, why: "CreatedAt is not LastAccess — a matcher that flags this flags everything"},
		"guardOnADifferentValue":     {sinks: 1, guarded: false, why: "an IsUnsetTimestamp call on an UNTAINTED value must not count as this site's guard"},
		"guardOnADifferentFieldOfATaintedRoot": {sinks: 1, guarded: false, why: "the FALSE NEGATIVE: h.la = m.LastAccess taints the root h, so a guard on h.created — " +
			"an unrelated field — used to satisfy an unrelated, genuinely unguarded sink. This is why " +
			"over-tainting is NOT conservative: the same tainted set feeds the guard side"},
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "censusMatcherFixture.go", censusMatcherFixture, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	seen := map[string]bool{}
	for _, unit := range censusUnits(f) {
		want, ok := cases[unit.label]
		if !ok {
			t.Errorf("fixture unit %q has no expectation — add one, or the fixture is dead weight", unit.label)
			continue
		}
		seen[unit.label] = true

		tainted := taintedLastAccessIdents(unit.body)
		guards := lastAccessGuardPositions(unit.body, tainted)
		sinks := elapsedFromLastAccess(unit.body, tainted)

		if len(sinks) != want.sinks {
			var got []string
			for _, s := range sinks {
				got = append(got, exprString(fset, s))
			}
			t.Errorf("%s: matcher found %d elapsed-from-LastAccess site(s), want %d — %s\n  found: %v\n"+
				"The census walks the module with this same matcher. A shape it stops seeing vanishes "+
				"from the census SILENTLY: the census still passes, having lost a guard site.",
				unit.label, len(sinks), want.sinks, want.why, got)
			continue
		}
		if want.sinks == 0 {
			continue
		}
		gotGuarded := guardedBefore(guards, sinks[0].Pos())
		if gotGuarded != want.guarded {
			t.Errorf("%s: site guarded=%v, want %v — %s", unit.label, gotGuarded, want.guarded, want.why)
		}
	}

	for name := range cases {
		if !seen[name] {
			t.Errorf("expectation %q has no unit in censusMatcherFixture — it was renamed or dropped, "+
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

// lastAccessTaint is the result of the intra-function taint analysis, and the
// asymmetry between its two halves is load-bearing.
//
//   - roots is the conservative, over-approximating set: `h.la = m.LastAccess`
//     taints the whole of `h`. It drives the SINK side, where over-approximating
//     is safe — the worst case is a false alarm, resolved by an exemption with a
//     stated reason.
//   - paths is the EXACT assignment target ("h.la", "buf[0]", `mm["k"]`, "*p").
//     It drives the GUARD side, where over-approximating is NOT safe.
//
// That distinction is the correction of a claim this census shipped with:
// "it over-taints, which can only produce a false alarm, never a false
// all-clear." False. The same tainted set fed lastAccessGuardPositions, so
// over-tainting also widened what counted as a GUARD, and
//
//	h.la = m.LastAccess                      // taints all of `h`
//	if IsUnsetTimestamp(h.created) { ... }    // guard on an UNRELATED field
//	return now.Sub(m.LastAccess).Hours()/24   // genuinely unguarded
//
// was reported GUARDED. Reproduced by censusMatcherFixture's
// guardOnADifferentFieldOfATaintedRoot.
type lastAccessTaint struct {
	roots map[string]bool
	paths map[string]bool
	// epoch is the subset of roots holding an INTEGER epoch conversion of a
	// tainted time (`laNs := m.LastAccess.UnixNano()`). It is tracked separately
	// because the integer-nanos sink (see elapsedNanosFromLastAccess) must not
	// match on plain taint — doing so reported three false alarms on real code.
	epoch map[string]bool
}

// taintedLastAccessIdents computes the monotone fixpoint of identifiers in body
// that hold a LastAccess-derived value, plus the exact paths assigned. Monotone
// means an identifier that is later overwritten with a safe value
// (`lastAccess = now`, the guard's own repair) stays tainted — deliberately,
// since the sink after it is exactly the computation we want to see.
func taintedLastAccessIdents(body *ast.BlockStmt) lastAccessTaint {
	tainted := map[string]bool{}
	paths := map[string]bool{}
	epoch := map[string]bool{}
	for round := 0; round < 8; round++ {
		grew := false
		mark := func(lhs ast.Expr, rhs ast.Expr) {
			id := rootIdent(lhs)
			if id == nil || id.Name == "_" || rhs == nil {
				return
			}
			if !epoch[id.Name] && isEpochValued(rhs, tainted, epoch) {
				epoch[id.Name] = true
				grew = true
			}
			if !containsTainted(rhs, tainted) {
				return
			}
			if p := exprPath(lhs); p != "" && !paths[p] {
				paths[p] = true
				grew = true
			}
			if tainted[id.Name] {
				return
			}
			tainted[id.Name] = true
			grew = true
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
	return lastAccessTaint{roots: tainted, paths: paths, epoch: epoch}
}

// exprPath renders an assignment target as a stable path string, or "" if the
// shape is one this census does not track exactly:
//
//	h.la     -> "h.la"
//	buf[0]   -> "buf[0]"
//	mm["k"]  -> `mm["k"]`
//	*p       -> "*p"
//	(x)      -> "x"
//
// It is deliberately fset-free so the same string is produced for the fixture
// and for module source.
func exprPath(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.ParenExpr:
		return exprPath(n.X)
	case *ast.SelectorExpr:
		if base := exprPath(n.X); base != "" {
			return base + "." + n.Sel.Name
		}
	case *ast.StarExpr:
		if base := exprPath(n.X); base != "" {
			return "*" + base
		}
	case *ast.IndexExpr:
		base := exprPath(n.X)
		if base == "" {
			return ""
		}
		switch k := n.Index.(type) {
		case *ast.BasicLit:
			return base + "[" + k.Value + "]"
		case *ast.Ident:
			return base + "[" + k.Name + "]"
		}
	}
	return ""
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
// Tainting the ROOT over-approximates. `h.la = m.LastAccess` taints the whole
// of `h`, so a later `time.Since(h.anythingElse)` in the same function is
// flagged too. On the SINK side that is safe: the worst case is a false alarm,
// resolved by an exemption with a stated reason. Matching only a bare
// `*ast.Ident` target, which is what this did before, under-taints — it
// launders the sentinel through all four shapes above and reports the sink as
// absent, and `grep LastAccess` WOULD have found `h.la = m.LastAccess`.
//
// It is NOT safe on the guard side, and an earlier version of this comment said
// it was ("can only ever produce a false ALARM, never a false all-clear"). The
// same set fed lastAccessGuardPositions, so a guard on any OTHER field of the
// same tainted root silenced a genuinely unguarded sink — a false all-clear.
// Guards now match on lastAccessTaint.paths, the exact assignment target; see
// that type and guardArgIsTainted.
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

// guardArgIsTainted decides whether an IsUnsetTimestamp argument counts as a
// guard on a LastAccess-derived value. It is deliberately STRICTER than
// containsTainted (which drives the sink side): a bare tainted identifier, an
// expression containing a `.LastAccess` selector, or the exact laundered path
// — but not an arbitrary other member of a tainted root. See lastAccessTaint.
func guardArgIsTainted(arg ast.Expr, t lastAccessTaint) bool {
	if containsLastAccessSelector(arg) {
		return true
	}
	stripped := arg
	for {
		p, ok := stripped.(*ast.ParenExpr)
		if !ok {
			break
		}
		stripped = p.X
	}
	if id, ok := stripped.(*ast.Ident); ok {
		return t.roots[id.Name]
	}
	if p := exprPath(stripped); p != "" && t.paths[p] {
		return true
	}
	return false
}

// lastAccessGuardPositions returns the positions of every IsUnsetTimestamp call
// in body whose argument is LastAccess-derived, per guardArgIsTainted.
func lastAccessGuardPositions(body *ast.BlockStmt, tainted lastAccessTaint) []token.Pos {
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
			if guardArgIsTainted(arg, tainted) {
				out = append(out, call.Pos())
				break
			}
		}
		return true
	})
	return out
}

// elapsedFromLastAccess returns every expression in body that computes an
// elapsed interval from a LastAccess-derived value:
//
//	time.Since(x), y.Sub(x), x.Sub(y)   — the time.Time idiom
//	nowNs - x.LastAccess.UnixNano()     — the integer-nanosecond idiom (S3)
//
// The second shape is not an exotic hypothetical: mbp.ActivationItem.LastAccess
// is an int64 nanosecond field, so every transport-facing consumer is already
// one step from writing it, and the sentinel poisons integer subtraction exactly
// as badly as it poisons Sub(). It was outside the census's stated boundary list
// entirely — neither inter-function taint nor "non-elapsed misuse" — which is
// the worst place for a gap to be, because the boundary list reads as complete.
func elapsedFromLastAccess(body *ast.BlockStmt, t lastAccessTaint) []ast.Expr {
	tainted := t.roots
	var out []ast.Expr
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

	return append(out, elapsedNanosFromLastAccess(body, t, out)...)
}

// elapsedNanosFromLastAccess returns every integer-nanosecond elapsed-time
// subtraction in body: `X - Y` where an operand is an epoch conversion of a
// LastAccess-derived value (`la.UnixNano()`), or an identifier holding one.
//
// The operand test is DELIBERATELY narrower than plain taint. "any SUB with a
// tainted operand" was tried first and reported three false alarms on real
// code, all of them arithmetic that merely happened to mention a tainted value
// downstream of an already-guarded Sub — `len(entries) - 1` in
// Engine.activateCore, and the `ln(n) - d*ln(...)` base-level term in both
// computeACTR and Engine.PruneVault. A census that cries wolf gets exemptions
// written to silence it, and exemptions are how coverage rots.
func elapsedNanosFromLastAccess(body *ast.BlockStmt, t lastAccessTaint, callSinks []ast.Expr) []ast.Expr {
	var out []ast.Expr
	isEpochOperand := func(e ast.Expr) bool {
		return isEpochValued(e, t.roots, t.epoch)
	}
	ast.Inspect(body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.SUB {
			return true
		}
		if !isEpochOperand(bin.X) && !isEpochOperand(bin.Y) {
			return true
		}
		// Do not double-report: a SUB enclosing an already-matched Since/Sub
		// call is the same site.
		for _, c := range callSinks {
			if c.Pos() >= bin.Pos() && c.End() <= bin.End() {
				return true
			}
		}
		out = append(out, bin)
		return true
	})
	return out
}

// numericConversions are the wrappers isEpochValued sees through. Anything else
// is treated as a computation, not a carrier.
var numericConversions = map[string]bool{
	"int64": true, "int": true, "uint64": true, "int32": true, "uint32": true, "float64": true,
}

// isEpochValued reports whether e IS an integer epoch conversion of a
// LastAccess-tainted time — `la.UnixNano()`, `int64(la.Unix())`, or an
// identifier already known to hold one.
//
// "Is", not "contains", and that distinction had to be measured. A `contains`
// test propagates through the taint fixpoint by mere mention: one seed
// (`items[i].LastAccess = eng.LastAccess.UnixNano()`) marked `items`, then every
// local derived from `items` inherited it, and Engine.activateCore ended with
// seven of seven locals epoch-marked and `len(entries) - 1` reported as an
// unguarded elapsed-time computation.
func isEpochValued(e ast.Expr, tainted, epoch map[string]bool) bool {
	for {
		switch n := e.(type) {
		case *ast.ParenExpr:
			e = n.X
		case *ast.Ident:
			return epoch[n.Name]
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && numericConversions[id.Name] && len(n.Args) == 1 {
				e = n.Args[0]
				continue
			}
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			switch sel.Sel.Name {
			case "UnixNano", "Unix", "UnixMilli", "UnixMicro":
				return containsTainted(sel.X, tainted)
			}
			return false
		default:
			return false
		}
	}
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
