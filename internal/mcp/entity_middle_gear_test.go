package mcp

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── the middle gear ──────────────────────────────────────────────────────────
//
// A bare muninn_remember(content) is ~20 tokens and produces a memory that is
// invisible to every entity-based tool. A fully-declared write is ~8x the
// content in JSON. Four independent evaluators said the same thing: under time
// pressure with a user waiting, they take the cheap path — so the cheap path is
// what the graph actually gets.
//
// These tests pin the middle gear: entities as BARE STRINGS, whose type the
// server resolves from the entity table it ALREADY has, and inline [[markup]]
// in the content itself. Neither mechanism may invent a name the caller did not
// supply (#713 pollution risk: hand-adjudicated precision of INFERRED entities
// measured ~0.76 — only caller-declared names are trusted here).

// fixtureLookup is a synthetic entity table standing in for a vault's own
// known entities. Synthetic fixtures only — no real user data.
func fixtureLookup(table map[string]string) entityTypeLookup {
	return func(name string) (string, bool) {
		t, ok := table[strings.ToLower(name)]
		return t, ok
	}
}

var testEntityTable = map[string]string{
	"postgresql":   "database",
	"auth service": "service",
	"go":           "language",
}

func entityNames(ents []mbp.InlineEntity) []string {
	out := make([]string, len(ents))
	for i, e := range ents {
		out[i] = e.Name + ":" + e.Type
	}
	return out
}

func TestBareStringEntities_ResolveFromKnownTable(t *testing.T) {
	args := map[string]any{
		"entities": []any{"PostgreSQL", "Auth Service"},
	}
	req := &mbp.WriteRequest{Content: "unchanged"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if len(req.Entities) != 2 {
		t.Fatalf("bare-string entities must be accepted, got %d: %v", len(req.Entities), entityNames(req.Entities))
	}
	if req.Entities[0].Name != "PostgreSQL" || req.Entities[0].Type != "database" {
		t.Errorf("entities[0] = %+v, want {PostgreSQL database} resolved from the vault's own table", req.Entities[0])
	}
	if req.Entities[1].Name != "Auth Service" || req.Entities[1].Type != "service" {
		t.Errorf("entities[1] = %+v, want {Auth Service service}", req.Entities[1])
	}
	if rep.malformed != 0 {
		t.Errorf("bare strings are not malformed, got malformed=%d", rep.malformed)
	}
	if len(rep.unresolvedNames) != 0 {
		t.Errorf("both names are known; unresolvedNames = %v", rep.unresolvedNames)
	}
}

func TestBareStringEntities_UnknownNameBecomesOtherAndIsCounted(t *testing.T) {
	args := map[string]any{
		"entities": []any{"PostgreSQL", "Frobnitz Gateway"},
	}
	req := &mbp.WriteRequest{Content: "unchanged"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if len(req.Entities) != 2 {
		t.Fatalf("an unknown name must still be STORED (coverage beats classification), got %v", entityNames(req.Entities))
	}
	if req.Entities[1].Type != "other" {
		t.Errorf("unknown name type = %q, want \"other\"", req.Entities[1].Type)
	}
	// Never silently: the caller supplied something we could not type, so say so.
	if len(rep.unresolvedNames) != 1 || rep.unresolvedNames[0] != "Frobnitz Gateway" {
		t.Fatalf("unresolved name must be COUNTED and named, got %v", rep.unresolvedNames)
	}
	hint := rep.hint()
	if !strings.Contains(hint, "Frobnitz Gateway") || !strings.Contains(hint, "other") {
		t.Errorf("hint must name the unresolved entity and the type it got, got %q", hint)
	}
}

func TestObjectEntities_StillWork(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			map[string]any{"name": "Redis", "type": "database"},
			map[string]any{"name": "Kubernetes", "type": "tool"},
		},
	}
	req := &mbp.WriteRequest{Content: "unchanged"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if len(req.Entities) != 2 || req.Entities[0].Type != "database" || req.Entities[1].Type != "tool" {
		t.Fatalf("object form must be unchanged, got %v", entityNames(req.Entities))
	}
	// A declared type wins over the table, always (principle #1).
	if rep.malformed != 0 || len(rep.unresolvedNames) != 0 || len(rep.coercedTypes) != 0 {
		t.Errorf("clean object form must report nothing, got %+v", rep)
	}
}

func TestMixedEntityArray_StringsAndObjects(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			"PostgreSQL",
			map[string]any{"name": "Redis", "type": "database"},
			"Auth Service",
			map[string]any{"name": "Nomad"}, // name, no type -> resolve like a bare string
		},
	}
	req := &mbp.WriteRequest{Content: "unchanged"}
	parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	want := []string{"PostgreSQL:database", "Redis:database", "Auth Service:service", "Nomad:other"}
	got := entityNames(req.Entities)
	if len(got) != len(want) {
		t.Fatalf("mixed array: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mixed array[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// A declared type that is not one of the 14 is still coerced to "other" — but
// the caller supplied it, so it must be reported, never swallowed.
func TestDeclaredUnknownType_IsCoercedAndReported(t *testing.T) {
	args := map[string]any{
		"entities": []any{map[string]any{"name": "Nomad", "type": "orchestrator"}},
	}
	req := &mbp.WriteRequest{Content: "unchanged"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if len(req.Entities) != 1 || req.Entities[0].Type != "other" {
		t.Fatalf("unknown declared type must still store the entity as other, got %v", entityNames(req.Entities))
	}
	if len(rep.coercedTypes) != 1 || rep.coercedTypes[0] != "orchestrator" {
		t.Fatalf("coerced type must be counted and named, got %v", rep.coercedTypes)
	}
	if h := rep.hint(); !strings.Contains(h, "orchestrator") {
		t.Errorf("hint must name the coerced type, got %q", h)
	}
}

// The mechanism must NEVER manufacture an entity the caller did not name.
func TestNoMarkup_ContentUnchangedAndNoEntitiesInvented(t *testing.T) {
	const content = "Migrated the Auth Service to PostgreSQL 16 last Tuesday."
	req := &mbp.WriteRequest{Content: content}
	rep := parseEnrichmentArgs(map[string]any{}, req, fixtureLookup(testEntityTable))

	if req.Content != content {
		t.Errorf("content must be byte-identical when no markup is used:\n got %q\nwant %q", req.Content, content)
	}
	if len(req.Entities) != 0 {
		t.Errorf("no entity may be inferred from content: got %v", entityNames(req.Entities))
	}
	if rep.markup != 0 {
		t.Errorf("markup count = %d, want 0", rep.markup)
	}
}

func TestInlineMarkup_ExtractsEntitiesAndStripsBrackets(t *testing.T) {
	req := &mbp.WriteRequest{Content: "Migrated [[Auth Service]] to [[PostgreSQL]] 16"}
	rep := parseEnrichmentArgs(map[string]any{}, req, fixtureLookup(testEntityTable))

	if want := "Migrated Auth Service to PostgreSQL 16"; req.Content != want {
		t.Errorf("stored content = %q, want brackets stripped: %q", req.Content, want)
	}
	got := entityNames(req.Entities)
	want := []string{"Auth Service:service", "PostgreSQL:database"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("markup entities = %v, want %v", got, want)
	}
	if rep.markup != 2 {
		t.Errorf("markup count = %d, want 2", rep.markup)
	}
	if h := rep.hint(); !strings.Contains(h, "[[") {
		t.Errorf("stripping the caller's content must be reported, got hint %q", h)
	}
}

func TestInlineMarkup_DedupesAgainstDeclaredEntities(t *testing.T) {
	args := map[string]any{"entities": []any{"PostgreSQL"}}
	req := &mbp.WriteRequest{Content: "[[postgresql]] and [[Redis]]"}
	parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if req.Content != "postgresql and Redis" {
		t.Errorf("content = %q", req.Content)
	}
	got := entityNames(req.Entities)
	if len(got) != 2 {
		t.Fatalf("case-insensitive dedup expected, got %v", got)
	}
	if got[0] != "PostgreSQL:database" {
		t.Errorf("declared spelling must win, got %v", got)
	}
}

// Nothing that is not [[name]] may be touched.
func TestInlineMarkup_LeavesNonEntityBracketsAlone(t *testing.T) {
	for _, content := range []string{
		"arr[[i]+1] = 3",
		"a single [bracket] is not markup",
		"[[]] is empty",
		"[[a name that is far too long to plausibly be an entity and just keeps going and going and going]]",
	} {
		req := &mbp.WriteRequest{Content: content}
		rep := parseEnrichmentArgs(map[string]any{}, req, fixtureLookup(testEntityTable))
		if len(req.Entities) != 0 || rep.markup != 0 {
			t.Errorf("content %q must not yield markup entities, got %v", content, entityNames(req.Entities))
		}
		if req.Content != content {
			t.Errorf("content %q must be left byte-identical, got %q", content, req.Content)
		}
	}
}

// Item-level near-miss keys: an object with `entity_name` instead of `name` is
// today a total loss (-32602 or a silently dropped entity). Keep the entity,
// and name the accepted parameters.
func TestEntityObject_NearMissKeyIsAcceptedAndNamed(t *testing.T) {
	args := map[string]any{
		"entities": []any{map[string]any{"entity_name": "PostgreSQL", "entity_type": "database"}},
	}
	req := &mbp.WriteRequest{Content: "x"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))

	if len(req.Entities) != 1 || req.Entities[0].Name != "PostgreSQL" || req.Entities[0].Type != "database" {
		t.Fatalf("near-miss keys must not cost the entity, got %v", entityNames(req.Entities))
	}
	h := rep.hint()
	if !strings.Contains(h, "entity_name") || !strings.Contains(h, "'name'") {
		t.Errorf("hint must name both the wrong key and the accepted one, got %q", h)
	}
}

// Top-level near-miss: `entity` instead of `entities` drops the whole field.
func TestTopLevelNearMissKey_IsReported(t *testing.T) {
	args := map[string]any{"entity": []any{"PostgreSQL"}}
	req := &mbp.WriteRequest{Content: "x"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))
	h := rep.hint()
	if !strings.Contains(h, "entity") || !strings.Contains(h, "entities") {
		t.Errorf("an unknown near-miss parameter must be named alongside the accepted one, got %q", h)
	}
}

func TestGenuinelyMalformedEntity_StillCounted(t *testing.T) {
	args := map[string]any{"entities": []any{42.0, map[string]any{"type": "database"}, "  "}}
	req := &mbp.WriteRequest{Content: "x"}
	rep := parseEnrichmentArgs(args, req, fixtureLookup(testEntityTable))
	if len(req.Entities) != 0 {
		t.Fatalf("nothing nameable here, got %v", entityNames(req.Entities))
	}
	if rep.malformed != 3 {
		t.Errorf("malformed = %d, want 3", rep.malformed)
	}
	if h := rep.hint(); !strings.Contains(h, "bare string") {
		t.Errorf("the malformed hint must name the accepted forms including bare strings, got %q", h)
	}
}

// ── schema ───────────────────────────────────────────────────────────────────

func TestRememberSchema_EntitiesAcceptStringItems(t *testing.T) {
	for _, tool := range []string{"muninn_remember", "muninn_remember_batch"} {
		items := rememberEntityItemsSchema(t, tool)
		types, ok := items["type"].([]string)
		if !ok {
			t.Fatalf("%s: entities.items.type must be a []string union allowing bare names, got %T (%v)",
				tool, items["type"], items["type"])
		}
		var hasString, hasObject bool
		for _, s := range types {
			hasString = hasString || s == "string"
			hasObject = hasObject || s == "object"
		}
		if !hasString || !hasObject {
			t.Errorf("%s: entities.items.type = %v, want both \"string\" and \"object\"", tool, types)
		}
		// `required:["name","type"]` is the same class of client-side hard
		// rejection the enum was: a caller who cannot type an entity loses the
		// whole entity. type must not be required.
		req, _ := items["required"].([]string)
		for _, r := range req {
			if r == "type" {
				t.Errorf("%s: entities.items must NOT require \"type\" — that drops the whole entity "+
					"when the caller cannot classify it (the enum lesson, #713)", tool)
			}
		}
	}
}

// ── end to end through the handler ───────────────────────────────────────────

type entityResolvingEngine struct {
	*fakeEngine
	writes     []*mbp.WriteRequest
	listCalls  int32
	entityRows []EntitySummary
}

func (e *entityResolvingEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.writes = append(e.writes, req)
	return &mbp.WriteResponse{ID: "e2e-id"}, nil
}

func (e *entityResolvingEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	e.writes = append(e.writes, reqs...)
	return e.fakeEngine.WriteBatch(ctx, reqs)
}

func (e *entityResolvingEngine) ListEntities(_ context.Context, _ string, _ int, _ string) ([]EntitySummary, error) {
	atomic.AddInt32(&e.listCalls, 1)
	return e.entityRows, nil
}

func newEntityResolvingServer() (*MCPServer, *entityResolvingEngine) {
	eng := &entityResolvingEngine{
		fakeEngine: &fakeEngine{},
		entityRows: []EntitySummary{
			{Name: "PostgreSQL", Type: "database", State: "active"},
			{Name: "Auth Service", Type: "service", State: "active"},
		},
	}
	return newTestServerWith(eng), eng
}

func TestRememberE2E_BareStringEntitiesReachTheEngine(t *testing.T) {
	srv, eng := newEntityResolvingServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":` +
		`{"vault":"default","content":"cut over the read path","entities":["PostgreSQL","Frobnitz Gateway"]}}}`
	resp := decodeResp(t, postRPC(t, srv, body).Body.String())
	out := extractInnerJSON(t, resp)

	if len(eng.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(eng.writes))
	}
	got := entityNames(eng.writes[0].Entities)
	if len(got) != 2 || got[0] != "PostgreSQL:database" || got[1] != "Frobnitz Gateway:other" {
		t.Fatalf("entities reaching the engine = %v", got)
	}
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, "Frobnitz Gateway") {
		t.Errorf("hint must report the unresolved name, got %q", hint)
	}
}

// The entity table is scanned at most once per call, and only when a name
// actually needs resolving.
func TestRememberE2E_ResolverIsLazyAndScansOnce(t *testing.T) {
	srv, eng := newEntityResolvingServer()
	fullyTyped := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":` +
		`{"vault":"default","content":"a","entities":[{"name":"Redis","type":"database"}]}}}`
	postRPC(t, srv, fullyTyped)
	if n := atomic.LoadInt32(&eng.listCalls); n != 0 {
		t.Errorf("a fully-typed write must not touch the entity table, got %d scans", n)
	}

	bare := `{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"muninn_remember","arguments":` +
		`{"vault":"default","content":"b","entities":["PostgreSQL","Auth Service","Redis"]}}}`
	postRPC(t, srv, bare)
	if n := atomic.LoadInt32(&eng.listCalls); n != 1 {
		t.Errorf("three bare names in one call must cost exactly one entity-table scan, got %d", n)
	}
}

func TestRememberE2E_MarkupStripsBracketsFromStoredContent(t *testing.T) {
	srv, eng := newEntityResolvingServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":` +
		`{"vault":"default","content":"Migrated [[Auth Service]] to [[PostgreSQL]] 16"}}}`
	postRPC(t, srv, body)

	if len(eng.writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(eng.writes))
	}
	if want := "Migrated Auth Service to PostgreSQL 16"; eng.writes[0].Content != want {
		t.Errorf("stored content = %q, want %q", eng.writes[0].Content, want)
	}
	if got := entityNames(eng.writes[0].Entities); len(got) != 2 {
		t.Errorf("markup entities = %v", got)
	}
}

func TestRememberBatchE2E_BareStringsPerItem(t *testing.T) {
	srv, eng := newEntityResolvingServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":` +
		`{"vault":"default","memories":[{"content":"one","entities":["PostgreSQL"]},` +
		`{"content":"two","entities":["Auth Service","Frobnitz Gateway"]}]}}}`
	postRPC(t, srv, body)

	if len(eng.writes) != 2 {
		t.Fatalf("expected 2 writes, got %d", len(eng.writes))
	}
	if got := entityNames(eng.writes[0].Entities); len(got) != 1 || got[0] != "PostgreSQL:database" {
		t.Errorf("memories[0] entities = %v", got)
	}
	if got := entityNames(eng.writes[1].Entities); len(got) != 2 || got[1] != "Frobnitz Gateway:other" {
		t.Errorf("memories[1] entities = %v", got)
	}
}

// A vault whose entity table is empty (or whose scan fails) must still accept
// bare strings — degrade to "other", never reject.
func TestBareStrings_SurviveAnEmptyEntityTable(t *testing.T) {
	req := &mbp.WriteRequest{Content: "x"}
	rep := parseEnrichmentArgs(map[string]any{"entities": []any{"Anything"}}, req, nil)
	if len(req.Entities) != 1 || req.Entities[0].Type != "other" {
		t.Fatalf("nil resolver must degrade to other, got %v", entityNames(req.Entities))
	}
	if len(rep.unresolvedNames) != 1 {
		t.Errorf("still reported, got %v", rep.unresolvedNames)
	}
}
