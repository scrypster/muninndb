package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// Invented fixture domain: trail maintenance.

const (
	twinNewID  = "01JTWXNNEW0000000000000000"
	twinHeadA  = "01JTWXNHEADA00000000000000"
	twinHeadB  = "01JTWXNHEADB00000000000000"
	twinVault  = "trail-crew"
	staticTok  = "mdb_static"
	twinOpSeen = "op-already-stored"
)

// twinFake answers every write with a fixed id and hint and always has a
// twin to report, so a missing block can only mean a gate withheld it.
type twinFake struct {
	fakeEngine
	hint      string
	calls     atomic.Int64
	panicky   bool
	res       *engine.WriteTwinResult
	off       bool // the server runs MUNINN_WRITE_RELATED=off
	sawCaller bool
}

func (f *twinFake) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	f.sawCaller = engine.HasWriteTwinCaller(ctx)
	return &mbp.WriteResponse{ID: twinNewID, Hint: f.hint}, nil
}

func (f *twinFake) WriteTwinEnabled() bool { return !f.off }

func (f *twinFake) CheckIdempotency(_ context.Context, opID string) (*storage.IdempotencyReceipt, error) {
	if opID == twinOpSeen {
		return &storage.IdempotencyReceipt{EngramID: twinHeadA}, nil
	}
	return nil, nil
}

func (f *twinFake) WriteTwinInline(ctx context.Context, vault, newID string, _ func(*engine.WriteTwinResult) int) *engine.WriteTwinResult {
	f.calls.Add(1)
	if f.panicky {
		panic("injected block fault")
	}
	if f.res != nil {
		return f.res
	}
	return &engine.WriteTwinResult{Threshold: 0.93, ThresholdSource: "model_hint:bge-small-en-v1.5", Considered: 7,
		Candidates: []engine.WriteTwinCandidate{{ID: twinHeadA, Concept: "culvert spacing", Similarity: 0.951, CreatedAt: time.Now().Add(-14 * time.Minute)}}}
}

func twinPost(t *testing.T, srv *MCPServer, token, session string, args map[string]any) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(mkToolCallBody("muninn_remember", args))))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		r.Header.Set(mcpSessionHeader, session)
	}
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, r)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		return map[string]any{"__error": resp.Error.Message}
	}
	return extractInnerJSON(t, resp)
}

func rememberArgs(extra map[string]any) map[string]any {
	a := map[string]any{"vault": twinVault, "content": "culverts on the ridge trail are spaced every forty meters", "concept": "culvert spacing"}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func TestWriteTwin_SkippedForDuplicateIdempotentUpsert(t *testing.T) {
	for _, c := range []struct {
		name  string
		hint  string
		extra map[string]any
	}{
		{"duplicate_content", "duplicate_content", nil},
		{"engine idempotent", "idempotent", nil},
		{"op_id receipt hit", "", map[string]any{"op_id": twinOpSeen}},
		{"upsert", "upsert-created", map[string]any{"op_id": "doc-7", "upsert_mode": true}},
		{"upsert evolved", "upsert-evolved", map[string]any{"op_id": "doc-7", "upsert_mode": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &twinFake{hint: c.hint}
			out := twinPost(t, newTestServerWith(f), "", "", rememberArgs(c.extra))
			require.NotContains(t, out, "__error")
			require.NotContains(t, out, "related", "the returned id IS the existing memory — no block")
			require.Zero(t, f.calls.Load(), "the check must not even run")
		})
	}
	// Control: a fresh write does get the block from the same fake.
	f := &twinFake{}
	out := twinPost(t, newTestServerWith(f), "", "", rememberArgs(nil))
	require.Contains(t, out, "related")
}

func TestWriteTwin_WithheldUnlessFullMode(t *testing.T) {
	keys := newMockKeyStore(
		auth.APIKey{ID: "full", Vault: twinVault, Mode: auth.ModeFull},
		auth.APIKey{ID: "write", Vault: twinVault, Mode: auth.ModeWrite},
		auth.APIKey{ID: "append", Vault: twinVault, Mode: auth.ModeAppend},
		auth.APIKey{ID: "observe", Vault: twinVault, Mode: auth.ModeObserve},
	)
	exp := time.Now().Add(time.Hour)
	for _, c := range []struct {
		name  string
		token string
		caps  capabilityValidator
		show  bool
	}{
		{"static token", staticTok, nil, true},
		{"mk full", "mk_full", nil, true},
		{"mk write", "mk_write", nil, false},
		{"mk append", "mk_append", nil, false},
		{"mk observe", "mk_observe", nil, false},
		{"cap write", "cap_w", stubCapStore{cap: auth.Capability{Vault: twinVault, Mode: auth.ModeWrite, ExpiresAt: &exp}}, false},
		{"cap full", "cap_f", stubCapStore{cap: auth.Capability{Vault: twinVault, Mode: auth.ModeFull, ExpiresAt: &exp}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &twinFake{}
			srv := New(":0", f, staticTok, keys, c.caps, nil)
			out := twinPost(t, srv, c.token, "", rememberArgs(nil))
			if c.name == "mk observe" {
				require.Contains(t, out, "__error", "observe cannot write at all")
				return
			}
			require.NotContains(t, out, "__error")
			if c.show {
				require.Contains(t, out, "related", "a full-mode credential sees the block")
				return
			}
			require.NotContains(t, out, "related", "a %s credential must learn nothing about what exists", c.name)
			require.Zero(t, f.calls.Load(), "and the check must not run inline for it (no added latency)")
		})
	}
}

func TestWriteTwin_PanicAfterCommitStillReturnsTheWrite_MCP(t *testing.T) {
	f := &twinFake{panicky: true}
	out := twinPost(t, newTestServerWith(f), "", "", rememberArgs(nil))
	require.NotContains(t, out, "__error")
	require.Equal(t, twinNewID, out["id"], "the committed write's normal response must still be sent")
	require.NotContains(t, out, "related")
	require.EqualValues(t, 1, f.calls.Load())
}

func TestWriteTwin_NoSessionKeyedState(t *testing.T) {
	f := &twinFake{}
	srv := newTestServerWith(f)
	for i, sess := range []string{"sess-a", "sess-b", "sess-a", ""} {
		out := twinPost(t, srv, "", sess, rememberArgs(nil))
		rel, ok := out["related"].(map[string]any)
		require.True(t, ok, "write %d (session %q) must get the block: nothing suppresses by session", i, sess)
		c := rel["candidates"].([]any)[0].(map[string]any)
		require.Equal(t, twinHeadA, c["id"])
	}
	require.EqualValues(t, 4, f.calls.Load())
	// Structural: neither half of the feature reads a client session id.
	for _, path := range []string{"write_twin.go", "../engine/engine_write_twin.go"} {
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, banned := range []string{"noticeSession", "mcpSessionHeader", "Mcp-Session-Id", "SessionHash", "CallerIdentityFromContext"} {
			require.NotContains(t, string(src), banned, "%s must not key anything on a client-supplied session (%s)", path, banned)
		}
	}
}

func twinResult(n int, concept string) *engine.WriteTwinResult {
	r := &engine.WriteTwinResult{Threshold: 0.93, ThresholdSource: "model_hint:bge-small-en-v1.5", Considered: 9}
	for i, id := range []string{twinHeadA, twinHeadB}[:n] {
		r.Candidates = append(r.Candidates, engine.WriteTwinCandidate{ID: id, Concept: concept,
			Similarity: 0.97 - 0.01*float64(i), CreatedAt: time.Now().Add(-time.Duration(10+i) * time.Minute)})
	}
	return r
}

func TestWriteTwin_OptionsUpdateFirstAndNameRetiredID(t *testing.T) {
	for _, n := range []int{1, 2} {
		b := buildRelatedBlock(twinNewID, twinResult(n, "water bar angle"), time.Now())
		require.NotEmpty(t, b.Options)
		first := b.Options[0]
		require.Contains(t, first.Call, `muninn_link(source_id="`+twinNewID+`",target_id="`+twinHeadA+`",relation="supersedes")`,
			"the update direction is listed first")
		require.NotNil(t, first.Retires)
		require.Equal(t, twinHeadA, *first.Retires, "the update option retires the OLDER memory")
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		var generic map[string]any
		require.NoError(t, json.Unmarshal(raw, &generic))
		for i, o := range generic["options"].([]any) {
			_, has := o.(map[string]any)["retires"]
			require.True(t, has, "option %d must state what it retires (null when nothing)", i)
		}
		last := b.Options[len(b.Options)-1]
		require.Equal(t, "none", last.Call)
		require.Nil(t, last.Retires)
		// Evidence fields.
		for _, c := range b.Candidates {
			require.Regexp(t, `^[+-]\d\.\d{3}$`, c.VsThreshold)
			require.Regexp(t, `ago$`, c.Written)
		}
		require.Equal(t, 0.93, b.Basis.Threshold)
		require.Equal(t, "1h", b.Basis.Window)
	}
}

func TestWriteTwin_NeverOffersOldSupersedesNew(t *testing.T) {
	for _, n := range []int{1, 2} {
		b := buildRelatedBlock(twinNewID, twinResult(n, "water bar angle"), time.Now())
		for i, o := range b.Options {
			for _, head := range []string{twinHeadA, twinHeadB} {
				require.NotContains(t, o.Call, `source_id="`+head+`"`, "option %d offers old-supersedes-new", i)
			}
			if o.Retires != nil && *o.Retires == twinNewID {
				require.NotZero(t, i, "forgetting the new copy must never be the first option")
				require.Contains(t, o.Call, "muninn_forget")
			}
		}
		require.NotContains(t, b.Options[0].Call, "muninn_forget")
	}
}

func TestWriteTwin_SerializedSizeCap(t *testing.T) {
	for _, concept := range []string{
		strings.Repeat("<", 200),            // HTML-escaped to 6 bytes each by encoding/json
		strings.Repeat("排水溝の間隔", 30),        // 3-byte runes
		"water bar angle on the ridge spur", // ordinary
	} {
		b := buildRelatedBlock(twinNewID, twinResult(2, concept), time.Now())
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), writetwin.MaxBlockBytes, "block is %d bytes for concept %.20q", len(raw), concept)
		require.NotEmpty(t, b.Candidates, "shrinking must keep at least one candidate")
		require.NotEmpty(t, b.Options)
		require.NotNil(t, b.Basis)
	}
	// Recorded, not wished away: with 26-character ids and options that each
	// name the id they retire, two candidates measure 1,184-1,312 bytes, so
	// under the 1,000-byte budget the block shows ONE and the second head is
	// logged shown=false (the engine is told via the render callback).
	b := buildRelatedBlock(twinNewID, twinResult(2, "water bar angle on the ridge spur"), time.Now())
	require.Len(t, b.Candidates, 1)
	require.Equal(t, twinHeadA, b.Candidates[0].ID, "the most similar head is the one kept")
}

// ------------------------------------------------- real engine -----

type twinServer struct {
	eng   *engine.Engine
	store *storage.PebbleStore
	srv   *MCPServer
}

func newTwinServer(t *testing.T, mode writetwin.Mode, optIn bool) *twinServer {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	require.NoError(t, err)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	act := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trig := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	as := auth.NewStore(db)
	if optIn {
		on := true
		require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: twinVault, Plasticity: &auth.PlasticityConfig{Version: 1, WriteTwinInline: &on}}))
	}
	eng := engine.NewEngine(engine.EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: act,
		TriggerSystem: trig, Embedder: embedder, EmbedModelName: "bge-small-en-v1.5", WriteTwinMode: mode})
	t.Cleanup(func() { eng.Stop(); store.Close() })
	return &twinServer{eng: eng, store: store, srv: New(":0", NewEngineAdapter(eng, nil, nil), "", nil, nil, nil)}
}

func (ts *twinServer) rawRemember(t *testing.T, content string) string {
	t.Helper()
	w := postRPC(t, ts.srv, string(mkToolCallBody("muninn_remember", map[string]any{"vault": twinVault, "content": content, "concept": "culvert spacing"})))
	return w.Body.String()
}

var ulidRE = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`)

// TestWriteTwin_ResponseByteIdenticalOffAndShadow: a remember response is
// byte-identical (ids aside) with the feature off, in shadow — even when the
// shadow worker has found a twin — and in mode 1 for a vault not opted in.
func TestWriteTwin_ResponseByteIdenticalOffAndShadow(t *testing.T) {
	const first = "culverts on the ridge trail are spaced every forty meters"
	const second = "ridge trail culverts sit forty meters apart"
	body := func(mode writetwin.Mode, optIn bool) string {
		ts := newTwinServer(t, mode, optIn)
		b1 := ts.rawRemember(t, first)
		// Feed the shadow ring a near-identical vector for the first write,
		// so a twin genuinely exists when the second lands.
		ws := ts.store.ResolveVaultPrefix(twinVault)
		id1, err := storage.ParseULID(ulidRE.FindString(b1))
		require.NoError(t, err)
		e1, err := ts.store.GetEngram(storage.ContextWithNoAccessCacheStamp(context.Background()), ws, id1)
		require.NoError(t, err)
		v := make([]float32, 8)
		v[0] = 1
		ts.eng.WriteTwinOnEmbedded(ws, e1, v)
		b2 := ts.rawRemember(t, second)
		id2, err := storage.ParseULID(ulidRE.FindString(b2))
		require.NoError(t, err)
		e2, err := ts.store.GetEngram(storage.ContextWithNoAccessCacheStamp(context.Background()), ws, id2)
		require.NoError(t, err)
		w := make([]float32, 8)
		w[0], w[1] = 0.99, 0.141
		ts.eng.WriteTwinOnEmbedded(ws, e2, w)
		ts.eng.WaitWriteTimeIdle()
		return ulidRE.ReplaceAllString(b1+"\n"+b2, "<ID>")
	}
	off := body(writetwin.Off, false)
	require.NotContains(t, off, "related")
	require.Equal(t, off, body(writetwin.Shadow, true), "shadow must not change a byte of the response")
	require.Equal(t, off, body(writetwin.Inline, false), "mode 1 in a vault that has not opted in is shadow")
}

// TestWriteTwin_SupersedesOptionRetiresOldFromDefaultRecall executes the
// block's own option strings against a real engine and checks that the id
// each claims to retire is the one that leaves default recall.
func TestWriteTwin_SupersedesOptionRetiresOldFromDefaultRecall(t *testing.T) {
	callRE := regexp.MustCompile(`^(muninn_\w+)\((.*)\)$`)
	argRE := regexp.MustCompile(`(\w+)="([^"]*)"`)
	run := func(t *testing.T, ts *twinServer, call string) {
		m := callRE.FindStringSubmatch(call)
		require.NotNil(t, m, "option call %q is not a tool call", call)
		args := map[string]any{"vault": twinVault}
		for _, a := range argRE.FindAllStringSubmatch(m[2], -1) {
			args[a[1]] = a[2]
		}
		w := postRPC(t, ts.srv, string(mkToolCallBody(m[1], args)))
		require.Nil(t, decodeResp(t, w.Body.String()).Error, "executing %s", call)
	}
	recallIDs := func(t *testing.T, ts *twinServer) map[string]bool {
		ts.eng.WaitWriteTimeIdle()
		w := postRPC(t, ts.srv, string(mkToolCallBody("muninn_recall", map[string]any{"vault": twinVault, "query": "culvert ridge trail forty meters", "threshold": 0.001})))
		out := extractInnerJSON(t, decodeResp(t, w.Body.String()))
		got := map[string]bool{}
		for _, id := range memoryIDs(t, out) {
			got[id] = true
		}
		return got
	}
	for _, pick := range []string{"update", "forget"} {
		t.Run(pick, func(t *testing.T) {
			ts := newTwinServer(t, writetwin.Off, false)
			old := ulidRE.FindString(ts.rawRemember(t, "culverts on the ridge trail are spaced every forty meters"))
			nw := ulidRE.FindString(ts.rawRemember(t, "ridge trail culverts sit forty meters apart"))
			before := recallIDs(t, ts)
			require.True(t, before[old] && before[nw], "precondition: both recall: %v", before)
			b := buildRelatedBlock(nw, &engine.WriteTwinResult{Threshold: 0.93, ThresholdSource: "model_hint:bge-small-en-v1.5",
				Candidates: []engine.WriteTwinCandidate{{ID: old, Concept: "culvert spacing", Similarity: 0.95, CreatedAt: time.Now()}}}, time.Now())
			var opt RelatedOption
			for _, o := range b.Options {
				if (pick == "update" && strings.HasPrefix(o.Call, "muninn_link")) || (pick == "forget" && strings.HasPrefix(o.Call, "muninn_forget")) {
					opt = o
					break
				}
			}
			require.NotNil(t, opt.Retires)
			run(t, ts, opt.Call)
			after := recallIDs(t, ts)
			kept := old
			if *opt.Retires == old {
				kept = nw
			}
			require.False(t, after[*opt.Retires], "%q claims to retire %s, but it is still in default recall: %v", opt.Call, *opt.Retires, after)
			require.True(t, after[kept], "%q must leave %s in default recall: %v", opt.Call, kept, after)
		})
	}
}

// TestWriteTwin_OffIsInertOnTheMCPPath: with the check off, a remember
// allocates no writer context, never calls the check, carries no block, the
// guide is unchanged, and resolved plasticity JSON carries no new field.
func TestWriteTwin_OffIsInertOnTheMCPPath(t *testing.T) {
	f := &twinFake{off: true}
	out := twinPost(t, newTestServerWith(f), "", "", rememberArgs(nil))
	require.NotContains(t, out, "related")
	require.False(t, f.sawCaller, "off must not allocate the writer context")
	require.Zero(t, f.calls.Load())
	on := &twinFake{}
	twinPost(t, newTestServerWith(on), "", "", rememberArgs(nil))
	require.True(t, on.sawCaller, "control: on, the writer context is attached")

	guide := func(mode writetwin.Mode) string {
		ts := newTwinServer(t, mode, false)
		w := postRPC(t, ts.srv, string(mkToolCallBody("muninn_guide", map[string]any{"vault": twinVault})))
		resp := decodeResp(t, w.Body.String())
		require.Nil(t, resp.Error)
		raw, _ := json.Marshal(resp.Result)
		return string(raw)
	}
	require.NotContains(t, guide(writetwin.Off), "Related memories on write")
	require.Contains(t, guide(writetwin.Shadow), "Related memories on write")

	raw, err := json.Marshal(auth.ResolvePlasticity(nil))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "write_twin", "an unset vault resolves to byte-identical plasticity JSON")
}
