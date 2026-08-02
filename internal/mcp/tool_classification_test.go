package mcp

// tool_classification_test.go — the drift gate for MCP tool mode enforcement.
//
// #731 happened because muninn_state sat in the read-only bucket while its
// handler wrote (handleState → mcpEngineAdapter.UpdateState →
// Engine.UpdateLifecycleState). Every test in the suite still passed:
//
//   - TestToolClassification_CoversAllRegisteredHandlers proved only that each
//     tool sits in exactly ONE bucket, never that the bucket is the right one.
//   - The per-mode dispatch loops SELECTED their tools with the very predicate
//     they were meant to police (`if !isReadOnlyTool(name) { continue }`), so a
//     wrong bucket made them assert the vulnerability instead of catching it.
//     Reverting muninn_state to read-only left all four GREEN.
//   - The two hand-typed lists in auth_mk_test.go were not exhaustive, and had
//     silently drifted: the mutating list named 16 of the 23 tools the
//     classifier then covered — 7 omissions, none of which any test noticed.
//     (Those 7 plus muninn_state, newly mutating, are the 8 names #732 adds.)
//
// The gate below replaces all of that with a census, in the shape SEC-15's
// TestAppendMode_MethodCensus established: a per-tool decision a human must
// record, checked against the registry in BOTH directions and against all
// three predicates. Deliberately NO length comparisons anywhere — comparing
// counts between two hand-maintained lists passes happily when both drift the
// same way, which is the trap that made registeredToolNames() useless as a
// cross-check before it was derived from toolHandlers().

import (
	"sort"
	"testing"
)

// toolClass is the security-relevant classification of one MCP tool: the
// declared intent. mutatingTools/readOnlyTools/additiveTools are the
// implementation, and the tests below assert the two agree.
type toolClass string

const (
	// classRead: the handler performs no write. Reachable by observe-, append-
	// and full-mode credentials; REFUSED to write-mode credentials.
	classRead toolClass = "read"
	// classMutating: the handler writes, modifies, or deletes existing state.
	// Reachable by write- and full-mode credentials only.
	classMutating toolClass = "mutating"
	// classAdditive: mutating AND create-new-only (never touches an existing
	// engram), so append-mode credentials may call it too.
	classAdditive toolClass = "additive"
)

// expectedToolClassification is the per-tool security decision, made by a
// human, for every tool in the dispatchToolCall handler map.
//
// ADDING A TOOL: the tests below fail until you add an entry here. Do not copy
// the neighbouring line. Open the handler and answer one question — does any
// code path it reaches write? "Writes" includes lifecycle/state transitions,
// trust stamps, entity merges, lease acquisition and enrichment application,
// not just remember/forget. If yes it is classMutating; if it can ONLY create
// brand-new engrams it is classAdditive; only if it cannot write at all is it
// classRead. Getting this wrong hands an observe-mode credential a write
// (#731).
var expectedToolClassification = map[string]toolClass{
	// ── Read (observe-mode reachable) ──────────────────────────────────────
	"muninn_contradictions":            classRead,
	"muninn_entities":                  classRead,
	"muninn_entity":                    classRead,
	"muninn_entity_clusters":           classRead,
	"muninn_entity_timeline":           classRead,
	"muninn_explain":                   classRead,
	"muninn_export_graph":              classRead,
	"muninn_find_by_entity":            classRead,
	"muninn_get_enrichment_candidates": classRead,
	"muninn_guide":                     classRead,
	"muninn_list_deleted":              classRead,
	"muninn_provenance":                classRead,
	"muninn_read":                      classRead,
	"muninn_recall":                    classRead,
	"muninn_recall_tree":               classRead,
	"muninn_session":                   classRead,
	"muninn_similar_entities":          classRead,
	"muninn_status":                    classRead,
	"muninn_traverse":                  classRead,
	"muninn_where_left_off":            classRead,

	// ── Additive (create-new only; append-mode reachable) ──────────────────
	"muninn_add_child":      classAdditive,
	"muninn_remember":       classAdditive,
	"muninn_remember_batch": classAdditive,
	"muninn_remember_tree":  classAdditive,

	// ── Mutating (write-mode reachable; observe and append refused) ────────
	"muninn_apply_enrichment": classMutating,
	"muninn_claim":            classMutating,
	// Reaches the same store.CompareAndSet lifecycle transition as
	// muninn_state, with expect_state optional — see
	// TestDispatch_WriteMode_AllowsCompareAndSetArchive.
	"muninn_compare_and_set": classMutating,
	"muninn_consolidate":     classMutating,
	// Privileged on top of being mutating: dispatchToolCall additionally
	// requires a full-mode mk_ key and the MUNINN_AGENT_VAULT_CREATE opt-in,
	// so write mode is refused this one tool by a separate guard.
	"muninn_create_workflow_vault": classMutating,
	"muninn_decide":                classMutating,
	"muninn_entity_state":          classMutating,
	"muninn_entity_state_batch":    classMutating,
	"muninn_evolve":                classMutating,
	"muninn_feedback":              classMutating,
	"muninn_forget":                classMutating,
	"muninn_intend":                classMutating,
	"muninn_link":                  classMutating,
	"muninn_merge_entity":          classMutating,
	"muninn_release":               classMutating,
	"muninn_replay_enrichment":     classMutating,
	"muninn_restore":               classMutating,
	"muninn_retry_enrich":          classMutating,
	// #731: handleState → Engine.UpdateLifecycleState. Transitions an EXISTING
	// engram (up to "archived"), so mutating and NOT additive.
	"muninn_state": classMutating,
	"muninn_trust": classMutating,
}

// registeredToolSet returns registeredToolNames() as a set, failing if it
// yields a duplicate. (It is derived from map keys today, so it cannot — this
// is here so the helper stays honest if that ever changes back.)
func registeredToolSet(t *testing.T) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	for _, name := range registeredToolNames() {
		if set[name] {
			t.Errorf("registeredToolNames() lists %q more than once", name)
		}
		set[name] = true
	}
	return set
}

// sortedClassKeys gives deterministic iteration and failure output.
func sortedClassKeys(m map[string]toolClass) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestToolClassification_EveryRegisteredToolIsDeclared is direction 1:
// registered → declared. A tool added to toolHandlers() with no entry in
// expectedToolClassification fails here, forcing an explicit per-tool decision
// instead of letting the author satisfy the suite by editing a classification
// table alone — which is exactly what a well-meaning author would do, and what
// no existing test caught.
func TestToolClassification_EveryRegisteredToolIsDeclared(t *testing.T) {
	for _, name := range registeredToolNames() {
		if _, ok := expectedToolClassification[name]; !ok {
			t.Errorf("tool %q is registered but has no entry in expectedToolClassification — "+
				"read its handler and declare classRead / classAdditive / classMutating; "+
				"a wrong bucket hands an observe-mode credential a write (#731)", name)
		}
	}
}

// TestToolClassification_NoStaleDeclarations is direction 2: declared →
// registered. Catches a renamed or removed tool whose old name lingers in the
// expectation table, which would otherwise keep passing forever while covering
// nothing.
func TestToolClassification_NoStaleDeclarations(t *testing.T) {
	registered := registeredToolSet(t)
	for _, name := range sortedClassKeys(expectedToolClassification) {
		if !registered[name] {
			t.Errorf("expectedToolClassification declares %q, which no handler registers — "+
				"the tool was renamed or removed; drop the stale entry", name)
		}
	}
}

// TestToolClassification_PredicatesMatchDeclarations asserts the three
// predicates against the declared intent, positively AND negatively. Because
// every predicate is pinned for every tool, this subsumes the old
// exactly-one-bucket check: neither-bucket and both-buckets each fail at least
// one assertion here.
func TestToolClassification_PredicatesMatchDeclarations(t *testing.T) {
	registered := registeredToolSet(t)
	for _, name := range sortedClassKeys(expectedToolClassification) {
		if !registered[name] {
			continue // already reported by TestToolClassification_NoStaleDeclarations
		}
		want := expectedToolClassification[name]
		wantMutating := want == classMutating || want == classAdditive
		wantReadOnly := want == classRead
		wantAdditive := want == classAdditive

		if got := isMutatingTool(name); got != wantMutating {
			t.Errorf("isMutatingTool(%q) = %v, want %v (declared %s)", name, got, wantMutating, want)
		}
		if got := isReadOnlyTool(name); got != wantReadOnly {
			t.Errorf("isReadOnlyTool(%q) = %v, want %v (declared %s)", name, got, wantReadOnly, want)
		}
		if got := isAdditiveTool(name); got != wantAdditive {
			t.Errorf("isAdditiveTool(%q) = %v, want %v (declared %s)", name, got, wantAdditive, want)
		}
	}
}

// TestToolClassification_NoUnregisteredNamesInClassifiers is the reverse
// direction against the CLASSIFIERS themselves, which only became assertable
// when they stopped being `switch` bodies (#732). A dead or misspelled name
// inside a switch was invisible: nothing could enumerate the cases, so
// "muninn_rememberr" sat in the mutating set forever, covering nothing and
// leaving the real tool wherever it fell.
func TestToolClassification_NoUnregisteredNamesInClassifiers(t *testing.T) {
	registered := registeredToolSet(t)
	tables := []struct {
		label string
		table map[string]bool
	}{
		{"mutatingTools", mutatingTools},
		{"readOnlyTools", readOnlyTools},
		{"additiveTools", additiveTools},
	}
	for _, tc := range tables {
		names := make([]string, 0, len(tc.table))
		for name := range tc.table {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !registered[name] {
				t.Errorf("%s contains %q, which no handler registers — dead or misspelled entry", tc.label, name)
			}
		}
	}
}

// TestToolClassification_AdditiveIsSubsetOfMutating pins the overlay
// relationship the isAdditiveTool doc comment asserts ("keep this a strict
// subset of mutatingTools"). Nothing enforced it before: an additive-but-not-
// mutating tool would be reachable by append AND by observe, since the
// observe gate only checks isReadOnlyTool.
func TestToolClassification_AdditiveIsSubsetOfMutating(t *testing.T) {
	for _, name := range registeredToolNames() {
		if isAdditiveTool(name) && !isMutatingTool(name) {
			t.Errorf("tool %q is additive but not mutating — additiveTools must stay a strict "+
				"subset of mutatingTools, or append-mode reaches a tool observe-mode also reaches", name)
		}
	}
}

// TestToolClassification_UnknownToolIsUnclassified keeps the fail-closed
// default honest: a name in no bucket is what makes dispatch reject unknown
// tools for observe/write/append credentials.
func TestToolClassification_UnknownToolIsUnclassified(t *testing.T) {
	const bogus = "muninn_nonexistent"
	if registeredToolSet(t)[bogus] {
		t.Fatalf("%q is registered — pick a different probe name", bogus)
	}
	if isMutatingTool(bogus) || isReadOnlyTool(bogus) || isAdditiveTool(bogus) {
		t.Errorf("unknown tool %q must be classified as nothing (fail-closed)", bogus)
	}
}
