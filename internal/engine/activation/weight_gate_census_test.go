package activation

import (
	"fmt"
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

// TestWeightGateCensus is the machine check #805 asked for, generalized
// beyond CGDN: "a units/scale census over every constant compared against an
// association weight: assert each is either derived per-vault or documented
// with the steady-state range it is meant to discriminate."
//
// # Why it exists
//
// #801 and #805 are the same defect twice: a hardcoded numeric constant
// (minHopScore = 0.05, epsilon = 0.01) compared against a quantity derived
// from an association edge weight, chosen against the value that quantity
// has at WRITE time and never checked against what it looks like at STEADY
// STATE — which a decay pass moves by 20x before anyone reads it. Both were
// found by two independent human analysis passes, months apart, on the same
// codebase. A rule that makes the shape checkable turns "rediscovered as a
// bug report" into "caught by CI".
//
// # What it asserts
//
// For every named, function-local OR file-scoped `const` declaration in the
// module holding a raw numeric literal (an untyped float or int constant, NOT
// a variable, NOT a struct field, NOT a plasticity/config value — see
// "scope", below), if that constant is used anywhere in the same function as
// one operand of a `<`, `<=`, `>`, `>=` comparison, or a `-` subtraction,
// whose OTHER operand is WEIGHT-TAINTED — a direct `.Weight` selector, an
// identifier literally named `hebbianBoost`/`HebbianBoost` (the field/param
// name this codebase uses for Hebbian association strength throughout
// scoring), or a local variable transitively assigned from either — the
// constant must be a named, documented site in censusKnownSites (or
// censusExemptions, with a stated reason). An undocumented match fails the
// test.
//
// # Scope — deliberately narrow, and why
//
// "Every threshold in the engine" was considered and rejected: separate
// review during this same change found `dynamicFloor := peakWeight * 0.05`
// (internal/storage/association.go) and `restoreWeight := existingPeak * 0.25`
// compared against association weights, but neither is a NAMED CONST — both
// are local variables computed from a per-edge `peakWeight`, i.e. already
// self-scaled to that edge rather than a global magic number. A rule that
// flagged those would be noise: the defect this census targets is a
// **hardcoded** constant applied to a **decaying** quantity, not "a
// comparison involving a weight" in general. Widening the match to bare
// numeric literals (not just named consts) was also tried and rejected: it
// would have flagged `update.Weight > restoreWeight*1.5` and
// `newCoAct >= existingCoAct+3`, neither of which is this defect — a named
// constant is the signal that a human chose this number once and is unlikely
// to ever revisit it, which is exactly how #801 and #805 both went unnoticed
// since the initial commit.
//
// `entityBoostNoiseFloor` (internal/engine/engine_entity_boost.go) was the
// closest real near-miss during development of this census: it IS a named
// float constant compared against a derived quantity (`contribution`), and an
// earlier draft of the taint vocabulary matched on the substring "boost",
// which caught it. It is a FALSE POSITIVE for this rule: `contribution` is
// `entityBoostFactor * idf`, an entity-rarity (IDF) score, not an association
// edge weight — no decay pass ever touches it 20x between write and read. The
// vocabulary below matches only `.Weight` selectors and the exact
// `hebbianBoost`/`HebbianBoost` identifier (this codebase's one carrier of
// Hebbian association strength through scoring), not the word "boost" as a
// substring — narrowed specifically to stop matching that site. Verified by
// TestWeightGateCensusMatcherFixture's notAssociationWeight case.
//
// # What it does NOT catch — the honest boundary
//
//   - Bare numeric literals used inline (not through a named const) never
//     match, by design (see "scope" above) — a literal magic number IS still
//     the same defect shape, but distinguishing "a deliberate calibration
//     constant" from "an incidental literal" (a loop bound, an index) from
//     syntax alone is not reliable, and the false-positive cost was measured
//     as real (the `1.5`/`+3` sites above).
//   - INTER-FUNCTION taint, exactly as in TestLastAccessElapsedCensus: a
//     helper taking a bare `float64` named `w` and comparing it to a constant
//     is invisible here unless the taint is visible from the call site within
//     the same function body.
//   - Non-Hebbian, non-`.Weight` association-derived quantities that don't
//     happen to route through an identifier named `hebbianBoost` (e.g. a
//     future `transitionBoost`-only gate) are not covered by today's
//     vocabulary and would need it extended, with the same false-positive
//     check repeated against the whole module before shipping.
func TestWeightGateCensus(t *testing.T) {
	root := moduleRootForCensus(t)

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
		rel   string
		fn    string
		name  string
		pos   string
		other string
	}
	var found []site
	seenExempt := map[string]bool{}
	foundKeys := map[string]bool{}

	for i, f := range files {
		rel := paths[i]
		for _, unit := range gateCensusUnits(f) {
			for _, m := range weightGatedConstants(unit.body) {
				s := site{rel: rel, fn: unit.label, name: m.constName, pos: fset.Position(m.pos).String(), other: m.otherExpr}
				key := rel + ":" + m.constName
				foundKeys[key] = true
				found = append(found, s)
			}
		}
	}

	var undocumented []site
	for _, s := range found {
		key := s.rel + ":" + s.name
		if _, ok := weightGateKnownSites[key]; ok {
			continue
		}
		if _, ok := weightGateExemptions[key]; ok {
			seenExempt[key] = true
			continue
		}
		undocumented = append(undocumented, s)
	}

	// The FLOOR on the input set, same rationale as TestLastAccessElapsedCensus's
	// censusKnownSites: a walk or matcher change that stops seeing a known site
	// must name it, not silently report fewer matches as a clean pass.
	var missing []string
	for key, why := range weightGateKnownSites {
		if !foundKeys[key] {
			missing = append(missing, "  "+key+"\n      "+why)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("weightGateKnownSites: %d of %d known site(s) were NOT found by this walk:\n%s\n\n"+
			"Either the site was legitimately removed/renamed (update the map in the same commit and "+
			"say what replaced it), or the walk/matcher stopped reaching it and the census is now blind "+
			"there.", len(missing), len(weightGateKnownSites), strings.Join(missing, "\n"))
	}

	total := len(found) + len(seenExempt)
	if total == 0 {
		t.Fatalf("census scanned %d non-test files and found ZERO constant-vs-weight comparisons. "+
			"That is not the expected state (minHopScore and epsilon should both be found) — the "+
			"walk or matcher is broken, not the codebase clean. Re-point it or fix it; do not read this "+
			"as a pass.", scanned)
	}

	if len(undocumented) > 0 {
		sort.Slice(undocumented, func(i, j int) bool { return undocumented[i].pos < undocumented[j].pos })
		var b strings.Builder
		for _, s := range undocumented {
			b.WriteString(fmt.Sprintf("\n  %s  in %s():  const %s vs %s", s.pos, s.fn, s.name, s.other))
		}
		t.Errorf("UNDOCUMENTED constant compared against an association-weight-derived quantity:%s\n\n"+
			"A hardcoded constant compared against, or subtracted from, a value derived from an "+
			"association edge weight is exactly the #801/#805 shape: a threshold chosen against a "+
			"write-time value, applied to a quantity a decay pass moves by ~20x before anyone reads it "+
			"(peakWeight * 0.05 in internal/storage/association.go). Either derive this constant "+
			"per-vault (principle #11), or add it to weightGateKnownSites with the measured "+
			"steady-state range it is meant to discriminate against, or to weightGateExemptions with a "+
			"stated structural reason it cannot carry that risk.", b.String())
	}

	for key, reason := range weightGateExemptions {
		if !seenExempt[key] {
			t.Errorf("weightGateExemptions has a stale entry %q (%s): no matching comparison was found "+
				"there anymore — drop the exemption so it stops reading as coverage.", key, reason)
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].pos < found[j].pos })
	var g []string
	for _, s := range found {
		g = append(g, fmt.Sprintf("%s (%s): const %s vs %s", s.pos, s.fn, s.name, s.other))
	}
	t.Logf("census scanned %d non-test files; %d weight-gated constant site(s), %d exempt:\n  %s",
		scanned, len(found), len(seenExempt), strings.Join(g, "\n  "))
}

// weightGateKnownSites is the census's floor on its own input set: every
// "<relpath>:<constName>" here must be found by the walk, AND each entry
// records whether the constant's range was actually checked — this census
// asserts a site is DOCUMENTED, not that it is HEALTHY; runPhase5TransitiveInference's
// minWeight is the "checked and fine" case, kept in the same map as the two
// "checked and dead" cases so a reader sees both outcomes of doing the work.
var weightGateKnownSites = map[string]string{
	"internal/engine/activation/engine.go:minHopScore": "#801 — phase5Traverse's hop gate. Compared " +
		"against `propagated`, which is `baseScore * assoc.Weight * boost * hopPenalty^depth`. " +
		"Documented INERT at the constant with the measured seed-score ceiling (0.0686559 unfiltered) " +
		"and pinned by TestPhase5Traverse_InertAtTheMeasuredSeedCeiling.",
	"internal/engine/activation/engine.go:epsilon": "#805 — CGDN's Hebbian rescue floor in " +
		"computeGatedActivation. Compared (via subtraction) against hebbianBoost, an association edge " +
		"weight. Documented INERT-on-its-own-terms at the constant: steady-state RelCoActivated p50 " +
		"is 0.0005, twenty times below epsilon=0.01, so `rescue` floors to 0 for essentially every " +
		"live Hebbian edge even once #768 (CGDN's separate, prerequisite defect) is repaired.",
	"internal/consolidation/transitive.go:minWeight": "Found by this census, not by prose — and the " +
		"HEALTHY counter-example to the other two. runPhase5TransitiveInference gates transitive-edge " +
		"inference at effectiveAB/effectiveBC >= 0.7 (max of Weight and PeakWeight). Its own comment " +
		"already reasons about the achievable range: autoassoc mints edges at 0.3, archive restore " +
		"returns them at peakWeight*0.25, so neither route can reach 0.7 — the threshold was chosen " +
		"specifically to be unreachable by anything except a genuinely repeated co-activation, and a " +
		"dangling edge (the risk the comment names) cannot climb to it. Left as-is: this is a constant " +
		"correctly checked against the quantities that can actually reach it, which is what the other " +
		"two entries in this map should have been.",
}

// weightGateExemptions maps "<relpath>:<constName>" to a stated structural
// reason a constant-vs-weight comparison found by the walk needs no
// documented steady-state range. Empty today; kept as a named map (not
// removed) so the shape matches TestLastAccessElapsedCensus's censusExemptions
// and a future legitimate exemption has somewhere to go without inventing the
// convention under time pressure.
var weightGateExemptions = map[string]string{}

// gateCensusUnit is one function body the census analyses, with a label.
type gateCensusUnit struct {
	label string
	body  *ast.BlockStmt
}

// gateCensusUnits returns every *ast.FuncDecl body in f, plus package-level
// *ast.FuncLit bodies (mirrors TestLastAccessElapsedCensus's censusUnits —
// see its comment for why the literal case matters).
func gateCensusUnits(f *ast.File) []gateCensusUnit {
	var out []gateCensusUnit
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out = append(out, gateCensusUnit{label: gateFuncLabel(d), body: d.Body})
			}
		case *ast.GenDecl:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
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
						out = append(out, gateCensusUnit{label: label, body: fl.Body})
						return false
					})
				}
			}
		}
	}
	return out
}

func gateFuncLabel(fn *ast.FuncDecl) string {
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
	return fn.Name.Name
}

// weightGateMatch is one (constant vs weight-tainted quantity) site.
type weightGateMatch struct {
	constName string
	pos       token.Pos
	otherExpr string
}

// weightGatedConstants finds every local named numeric const in body, and
// every place it is compared (<,<=,>,>=) or subtracted against a
// weight-tainted expression elsewhere in the SAME body.
func weightGatedConstants(body *ast.BlockStmt) []weightGateMatch {
	consts := localNumericConstNames(body)
	if len(consts) == 0 {
		return nil
	}
	tainted := taintedWeightIdents(body)

	var out []weightGateMatch
	seen := map[string]bool{} // constName -> already reported once for this body
	report := func(name string, pos token.Pos, other ast.Expr) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, weightGateMatch{constName: name, pos: pos, otherExpr: renderExpr(other)})
	}

	ast.Inspect(body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch bin.Op {
		case token.LSS, token.LEQ, token.GTR, token.GEQ, token.SUB:
		default:
			return true
		}
		xName, xIsConst := constIdentName(bin.X, consts)
		yName, yIsConst := constIdentName(bin.Y, consts)
		xTainted := isWeightTainted(bin.X, tainted)
		yTainted := isWeightTainted(bin.Y, tainted)

		if xIsConst && yTainted && !yIsConst {
			report(xName, bin.Pos(), bin.Y)
		}
		if yIsConst && xTainted && !xIsConst {
			report(yName, bin.Pos(), bin.X)
		}
		return true
	})
	return out
}

// localNumericConstNames returns the names of every const declared directly
// inside body (via a const DeclStmt) whose value is a raw numeric literal
// (INT or FLOAT BasicLit) — i.e. a hardcoded calibration constant, not a
// computed value, a struct field, or a config-derived variable.
func localNumericConstNames(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	for _, stmt := range body.List {
		decl, ok := stmt.(*ast.DeclStmt)
		if !ok {
			continue
		}
		gd, ok := decl.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok &&
					(lit.Kind == token.FLOAT || lit.Kind == token.INT) {
					out[name.Name] = true
				}
			}
		}
	}
	return out
}

// constIdentName reports whether e is a bare identifier naming one of consts,
// and returns that name.
func constIdentName(e ast.Expr, consts map[string]bool) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, consts[id.Name]
}

// isWeightTainted reports whether e is, or was assigned from (transitively,
// per taintedWeightIdents), an association edge weight.
func isWeightTainted(e ast.Expr, tainted map[string]bool) bool {
	if containsWeightSelectorOrIdent(e) {
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

// containsWeightSelectorOrIdent reports whether e directly contains a
// `.Weight` selector or the exact identifier hebbianBoost/HebbianBoost — the
// two vocabulary items, deliberately narrow (see the test's doc comment for
// why "boost" as a substring is rejected).
func containsWeightSelectorOrIdent(e ast.Node) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if node.Sel.Name == "Weight" {
				found = true
			}
		case *ast.Ident:
			if strings.EqualFold(node.Name, "hebbianBoost") {
				found = true
			}
		}
		return !found
	})
	return found
}

// taintedWeightIdents computes the fixpoint of local identifiers assigned
// (directly or transitively) from a weight-tainted expression, mirroring
// TestLastAccessElapsedCensus's taintedLastAccessIdents.
func taintedWeightIdents(body *ast.BlockStmt) map[string]bool {
	tainted := map[string]bool{}
	for round := 0; round < 8; round++ {
		grew := false
		mark := func(lhs ast.Expr, rhs ast.Expr) {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name == "_" || rhs == nil {
				return
			}
			if tainted[id.Name] {
				return
			}
			if isWeightTainted(rhs, tainted) {
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

func renderExpr(e ast.Expr) string {
	switch n := e.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return renderExpr(n.X) + "." + n.Sel.Name
	case *ast.BinaryExpr:
		return renderExpr(n.X) + " " + n.Op.String() + " " + renderExpr(n.Y)
	case *ast.CallExpr:
		fn := ""
		switch f := n.Fun.(type) {
		case *ast.Ident:
			fn = f.Name
		case *ast.SelectorExpr:
			fn = renderExpr(f)
		}
		return fn + "(...)"
	case *ast.BasicLit:
		return n.Value
	}
	return "<expr>"
}

// moduleRootForCensus walks up from the working directory to the directory
// holding go.mod. Separate from storage's moduleRoot (unexported, different
// package) but identical in behaviour.
func moduleRootForCensus(t *testing.T) string {
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

// TestWeightGateCensusMatcherFixture is the census's self-check, mirroring
// TestLastAccessCensusMatcherSeesLaunderedCopies: TestWeightGateCensus cannot
// vouch for its own matcher, since a broken taint analysis or a narrowed walk
// can both look identical to "the codebase is clean" (zero matches). This
// pins the matcher directly against one function body per shape.
func TestWeightGateCensusMatcherFixture(t *testing.T) {
	const fixture = `package fixture

type Assoc struct{ Weight float64 }

func directWeightSelector(a Assoc) bool {
	const gateFloor = 0.05
	return a.Weight < gateFloor
}

func transitivelyTaintedLocal(a Assoc, boost, penalty float64) bool {
	const minHop = 0.05
	propagated := a.Weight * boost * penalty
	return propagated < minHop
}

func subtractionForm(hebbianBoost float64) float64 {
	const epsilon = 0.01
	rescue := hebbianBoost - epsilon
	return rescue
}

func hebbianBoostCaseInsensitive(HebbianBoost float64) bool {
	const floor = 0.02
	return HebbianBoost > floor
}

func notAssociationWeight(idf float64) bool {
	// entityBoostNoiseFloor's real shape: a named float const compared
	// against a derived quantity that has nothing to do with association
	// weight. Must NOT match — this is the exact false positive an earlier
	// vocabulary (matching the substring "boost") produced against
	// internal/engine/engine_entity_boost.go.
	const entityBoostFactor = 0.15
	const noiseFloor = 0.001
	contribution := entityBoostFactor * idf
	return contribution < noiseFloor
}

func unrelatedConstant(x float64) bool {
	const cap = 100.0
	return x < cap
}

func constVsConstIsNotThisShape() bool {
	const a = 0.05
	const b = 0.1
	return a < b
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", fixture, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	cases := map[string]struct {
		wantConsts []string
		why        string
	}{
		"directWeightSelector":        {[]string{"gateFloor"}, "the base case: const compared directly against a.Weight"},
		"transitivelyTaintedLocal":    {[]string{"minHop"}, "taint must survive `propagated := a.Weight * boost * penalty` before the comparison"},
		"subtractionForm":             {[]string{"epsilon"}, "the SUB shape (hebbianBoost - epsilon), not just comparison operators"},
		"hebbianBoostCaseInsensitive": {[]string{"floor"}, "the vocabulary matches hebbianBoost/HebbianBoost case-insensitively"},
		"notAssociationWeight":        {nil, "FALSE POSITIVE CHECK: entityBoostNoiseFloor's real shape must NOT match — " + "'boost' as a bare substring is not in the vocabulary, only .Weight and the exact hebbianBoost identifier"},
		"unrelatedConstant":           {nil, "an ordinary constant with no weight/hebbian involvement must not match"},
		"constVsConstIsNotThisShape":  {nil, "two named consts compared to each other is not this defect — neither is a decaying quantity"},
	}

	seen := map[string]bool{}
	for _, unit := range gateCensusUnits(f) {
		want, ok := cases[unit.label]
		if !ok {
			t.Errorf("fixture func %q has no expectation — add one, or the fixture is dead weight", unit.label)
			continue
		}
		seen[unit.label] = true

		got := weightGatedConstants(unit.body)
		var gotNames []string
		for _, m := range got {
			gotNames = append(gotNames, m.constName)
		}
		sort.Strings(gotNames)
		wantSorted := append([]string(nil), want.wantConsts...)
		sort.Strings(wantSorted)

		if strings.Join(gotNames, ",") != strings.Join(wantSorted, ",") {
			t.Errorf("%s: matcher found consts %v, want %v — %s", unit.label, gotNames, want.wantConsts, want.why)
		}
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("expectation %q has no unit in the fixture — it was renamed or dropped", name)
		}
	}
}
