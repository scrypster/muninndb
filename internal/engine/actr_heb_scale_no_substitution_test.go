package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// TestACTRHebScale_NoZeroSubstitutionAnywhere — the machine check for the
// substitution that has now been written THREE times.
//
// `actr_heb_scale` scales BOTH the Hebbian boost and the PAS transition boost
// inside softplus, so an explicit 0 is the whole-cognitive-prior kill switch.
// auth.ResolvePlasticity ALWAYS populates ResolvedPlasticity.ACTRHebScale (from
// a preset, or from an explicit override clamped to [0, 50]), so there is no
// "unset" to fall back from and every `== 0 -> default` guard on the resolved
// value is a silent substitution of a configured value — principle #1, in the
// hot path.
//
// The three instances, all of the same shape:
//
//	internal/engine/engine.go               if resolved.ACTRHebScale > 0 { ... }   (removed, d17a884)
//	internal/engine/activation/engine.go    if req.ACTRHebScale > 0 { ... }        (removed, d17a884)
//	internal/engine/cognition_trial_measure_test.go
//	                                        hebScale := resolved.ACTRHebScale
//	                                        if hebScale == 0 { hebScale = DefaultACTRHebScale }
//
// The third is the reason this test exists. Nothing in CI caught the first two
// (they were found by hand) and nothing caught the third, which was written
// INTO THE INSTRUMENT that judges whether the cognitive layer earns its keep —
// so a vault deliberately configured `actr_heb_scale: 0` would have been
// measured at 4.0, i.e. the trial would have measured the exact configuration
// whose behaviour it exists to judge as though it were the opposite one.
//
// WHAT IS SCANNED. Every non-test .go file in the repository, PLUS every
// build-tagged measurement harness (`cognitiontrial`, `scoringmeasure`, ...) —
// see ctScanFileIsInScope. A harness is an instrument: its output is a claim
// about production, so it carries production's no-substitution obligation. An
// ordinary _test.go file does not: asserting `r.ACTRHebScale != 0` is exactly
// what a RED test for this bug looks like.
//
// WHAT IS FLAGGED: an `if` whose condition compares an ACTRHebScale-derived
// expression against the literal 0 with ==, != or >, AND whose body (or else
// branch) then WRITES that quantity or names the default. Both halves are
// required, and that is deliberate:
//
//   - The operators. ==, != and > against 0 are the sentinel-conflation shapes:
//     they ask "is it set?" of a value whose 0 is meaningful. `< 0` and `> 50`
//     are CLAMPS (auth/plasticity.go:500-509) and are legitimate.
//   - The write. The banned thing is the SUBSTITUTION, not the observation. A
//     harness that notices actr_heb_scale is 0 and LOGS it — "Delta_H is
//     structurally 0 on this vault, and that is the vault's real behaviour" —
//     is doing exactly the honest thing, and a flat ban on the comparison would
//     forbid it and push the next author toward saying nothing at all.
//
// ALIASING IS FOLLOWED. The third instance compares a LOCAL VARIABLE, not the
// field, so a plain string scan for "ACTRHebScale == 0" misses precisely the
// instance that motivated the test. Within each function body, any identifier
// bound (with `=` or `:=`) from an expression that IS the heb scale — through
// numeric conversions and through other tainted identifiers, but not through an
// arbitrary call, which returns some other quantity — is tainted.
//
// WHAT IT CANNOT SEE: a substitution hidden inside a helper, e.g.
// `hebScale := orDefault(resolved.ACTRHebScale, 4.0)`. Nothing in the tree does
// that today and no lint of this shape would catch it; it is recorded here so
// the next reader knows the boundary rather than trusting a green run too far.
// ---------------------------------------------------------------------------

// ctScanTaggedHarnessTags are the build tags that mark a _test.go file as a
// MEASUREMENT HARNESS rather than an ordinary test. CI never passes them, so
// nothing else checks these files.
var ctScanTaggedHarnessTags = []string{"cognitiontrial", "scoringmeasure"}

// ctScanSkipDirs are directories with no first-party Go source.
var ctScanSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".worktrees": true,
	"testdata": true, ".artifacts": true, "assets": true,
}

func TestACTRHebScale_NoZeroSubstitutionAnywhere(t *testing.T) {
	root := ctScanRepoRoot(t)
	fset := token.NewFileSet()

	var findings []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (ctScanSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !ctScanFileIsInScope(path, string(src)) {
			return nil
		}
		if !strings.Contains(string(src), "ACTRHebScale") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		findings = append(findings, ctScanFileForSubstitution(fset, f, rel)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, f := range findings {
		t.Errorf("ACTRHebScale zero-substitution: %s\n"+
			"    auth.ResolvePlasticity ALWAYS populates ACTRHebScale, so there is no unset to "+
			"fall back from: comparing it (or a local aliased from it) against 0 with ==, != or > "+
			"and substituting a default silently replaces a CONFIGURED value — the documented "+
			"whole-cognitive-prior kill switch — with 4.0. This is principle #1 and it has now "+
			"been written three times (d17a884 removed two). Pass the resolved value through. If "+
			"a genuinely unset case is reachable, FAIL LOUDLY naming it; never substitute.", f)
	}
}

// ctScanRepoRoot walks up from the package directory to the module root.
func ctScanRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate go.mod above %s", dir)
	return ""
}

// ctScanFileIsInScope reports whether a file carries the no-substitution
// obligation: every non-test file, plus every build-tagged measurement harness.
func ctScanFileIsInScope(path, src string) bool {
	base := filepath.Base(path)
	if base == "actr_heb_scale_no_substitution_test.go" {
		return false // this file quotes the banned shapes in its own documentation
	}
	if !strings.HasSuffix(base, "_test.go") {
		return true
	}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			break // past the header comments; no build line found
		}
		if !strings.HasPrefix(line, "//go:build") {
			continue
		}
		for _, tag := range ctScanTaggedHarnessTags {
			if strings.Contains(line, tag) {
				return true
			}
		}
	}
	return false
}

// ctScanFileForSubstitution returns one finding per banned comparison.
func ctScanFileForSubstitution(fset *token.FileSet, f *ast.File, rel string) []string {
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		tainted := ctScanTaintedLocals(fn.Body)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			cmp := ctScanBannedComparison(ifs.Cond, tainted)
			if cmp == nil {
				return true
			}
			if !ctScanBranchSubstitutes(ifs.Body, tainted) &&
				!ctScanBranchSubstitutes(ifs.Else, tainted) {
				return true
			}
			pos := fset.Position(cmp.Pos())
			out = append(out, fmt.Sprintf("%s:%d: if %s { ... substitutes ... }",
				rel, pos.Line, ctScanRender(fset, cmp)))
			return true
		})
	}
	return out
}

// ctScanTaintedLocals collects identifiers assigned, anywhere in the body, from
// an expression that mentions ACTRHebScale — transitively, so a chain of
// aliases is followed. Iterated to a fixed point because an alias may be
// assigned before the expression that taints its source is even parsed.
func ctScanTaintedLocals(body *ast.BlockStmt) map[string]bool {
	tainted := map[string]bool{}
	for pass := 0; pass < 4; pass++ {
		grew := false
		ast.Inspect(body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			// Only plain binding (`=`, `:=`) aliases a value. A compound
			// assignment (`+=`, ...) accumulates something DERIVED from it and
			// is not the same quantity — treating it as an alias flagged
			// `selfCheckBad == 0` in the trial harness, which counts
			// disagreeing candidates and has nothing to do with a heb scale.
			if as.Tok != token.ASSIGN && as.Tok != token.DEFINE {
				return true
			}
			mentions := false
			for _, rhs := range as.Rhs {
				if ctScanMentions(rhs, tainted) {
					mentions = true
				}
			}
			if !mentions {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" && !tainted[id.Name] {
					tainted[id.Name] = true
					grew = true
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

// ctScanBannedComparison returns the ==/!=/> comparison of a heb-scale
// expression against literal 0 inside a condition, descending through && / || /
// ! so a compound guard cannot hide one.
func ctScanBannedComparison(cond ast.Expr, tainted map[string]bool) *ast.BinaryExpr {
	switch v := cond.(type) {
	case *ast.ParenExpr:
		return ctScanBannedComparison(v.X, tainted)
	case *ast.UnaryExpr:
		return ctScanBannedComparison(v.X, tainted)
	case *ast.BinaryExpr:
		switch v.Op {
		case token.LAND, token.LOR:
			if b := ctScanBannedComparison(v.X, tainted); b != nil {
				return b
			}
			return ctScanBannedComparison(v.Y, tainted)
		case token.EQL, token.NEQ, token.GTR:
			var other ast.Expr
			switch {
			case ctScanIsZeroLit(v.Y):
				other = v.X
			case ctScanIsZeroLit(v.X):
				other = v.Y
			default:
				return nil
			}
			if ctScanMentions(other, tainted) {
				return v
			}
		}
	}
	return nil
}

// ctScanBranchSubstitutes reports whether a branch WRITES the heb scale (an
// assignment whose target is tainted, or a short var decl of one) or names the
// package default — the `return DefaultACTRHebScale` form of the same bug.
func ctScanBranchSubstitutes(branch ast.Node, tainted map[string]bool) bool {
	if branch == nil {
		return false
	}
	found := false
	ast.Inspect(branch, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if ctScanMentions(lhs, tainted) {
					found = true
				}
			}
		case *ast.Ident:
			if strings.Contains(v.Name, "DefaultACTRHebScale") {
				found = true
			}
		case *ast.SelectorExpr:
			if v.Sel != nil && strings.Contains(v.Sel.Name, "DefaultACTRHebScale") {
				found = true
			}
		}
		return !found
	})
	return found
}

// ctScanNumericConversions are the call shapes that PRESERVE the value rather
// than deriving something new from it. `float32(resolved.ACTRHebScale)` is
// still the heb scale; `ctSelfCheck(rows, hebScale, tol)` is not.
var ctScanNumericConversions = map[string]bool{
	"float32": true, "float64": true, "int": true, "int32": true, "int64": true,
	"uint32": true, "uint64": true,
}

// ctScanMentions reports whether an expression IS the heb scale — reading
// ACTRHebScale directly (as a field or an ident) or reading a tainted local,
// possibly through numeric conversions. It deliberately does NOT descend into
// the arguments of an ordinary call: a function OF the heb scale returns some
// other quantity, and treating it as the heb scale produces false findings on
// unrelated `== 0` checks.
func ctScanMentions(e ast.Expr, tainted map[string]bool) bool {
	switch v := e.(type) {
	case nil:
		return false
	case *ast.Ident:
		return v.Name == "ACTRHebScale" || tainted[v.Name]
	case *ast.SelectorExpr:
		return (v.Sel != nil && v.Sel.Name == "ACTRHebScale") || ctScanMentions(v.X, tainted)
	case *ast.ParenExpr:
		return ctScanMentions(v.X, tainted)
	case *ast.StarExpr:
		return ctScanMentions(v.X, tainted)
	case *ast.UnaryExpr:
		return ctScanMentions(v.X, tainted)
	case *ast.BinaryExpr:
		return ctScanMentions(v.X, tainted) || ctScanMentions(v.Y, tainted)
	case *ast.IndexExpr:
		return ctScanMentions(v.X, tainted)
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		if !ok || !ctScanNumericConversions[id.Name] {
			return false
		}
		for _, a := range v.Args {
			if ctScanMentions(a, tainted) {
				return true
			}
		}
		return false
	}
	return false
}

func ctScanIsZeroLit(e ast.Expr) bool {
	bl, ok := e.(*ast.BasicLit)
	if !ok {
		return false
	}
	switch bl.Kind {
	case token.INT, token.FLOAT:
	default:
		return false
	}
	s := strings.TrimRight(strings.TrimRight(bl.Value, "0"), ".")
	return s == "" || s == "0"
}

func ctScanRender(fset *token.FileSet, e ast.Expr) string {
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, e); err != nil {
		return "<unprintable>"
	}
	return sb.String()
}
