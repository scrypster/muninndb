package plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/enrich"
)

// pendingStore is a PluginStore over a fixed list of engrams. Like
// PebbleStore, every pass's ScanWithoutFlag starts from the head of the list
// and leaves out engrams that carry flagBit or any skipFlags bit, and
// UpdateDigest sets DigestSummarized when it stores a summary.
type pendingStore struct {
	mu        sync.Mutex
	engrams   []*plugin.Engram
	flags     map[plugin.ULID]uint16
	summaries map[plugin.ULID]string
	scans     int
}

func newPendingStore(engrams ...*plugin.Engram) *pendingStore {
	return &pendingStore{
		engrams:   engrams,
		flags:     make(map[plugin.ULID]uint16),
		summaries: make(map[plugin.ULID]string),
	}
}

func (s *pendingStore) pending(flagBit, skipFlags uint16) []*plugin.Engram {
	var out []*plugin.Engram
	for _, e := range s.engrams {
		if f := s.flags[e.ID]; f&flagBit == 0 && f&skipFlags == 0 {
			out = append(out, e)
		}
	}
	return out
}

func (s *pendingStore) CountWithoutFlag(_ context.Context, flagBit, skipFlags uint16) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.pending(flagBit, skipFlags))), nil
}

func (s *pendingStore) ScanWithoutFlag(_ context.Context, flagBit, skipFlags uint16) plugin.EngramIterator {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans++
	return &sliceIterator{engrams: s.pending(flagBit, skipFlags), pos: -1}
}

func (s *pendingStore) SetDigestFlag(_ context.Context, id plugin.ULID, flag uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags[id] |= flag
	return nil
}

func (s *pendingStore) GetDigestFlags(_ context.Context, id plugin.ULID) (uint16, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flags[id], nil
}

func (s *pendingStore) UpdateDigest(_ context.Context, id plugin.ULID, r *plugin.EnrichmentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Summary != "" {
		s.summaries[id] = r.Summary
		s.flags[id] |= plugin.DigestSummarized
	}
	return nil
}

func (s *pendingStore) flagsOf(id plugin.ULID) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flags[id]
}

func (s *pendingStore) summaryOf(id plugin.ULID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaries[id]
}

func (s *pendingStore) scanCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scans
}

func (s *pendingStore) UpdateEmbedding(context.Context, plugin.ULID, []float32) error { return nil }
func (s *pendingStore) UpsertEntity(context.Context, plugin.ULID, plugin.ExtractedEntity) error {
	return nil
}
func (s *pendingStore) LinkEngramToEntity(context.Context, plugin.ULID, string) error { return nil }
func (s *pendingStore) IncrementEntityCoOccurrence(context.Context, plugin.ULID, string, string) error {
	return nil
}
func (s *pendingStore) UpsertRelationship(context.Context, plugin.ULID, plugin.ExtractedRelation) error {
	return nil
}
func (s *pendingStore) HNSWInsert(context.Context, plugin.ULID, []float32) error          { return nil }
func (s *pendingStore) CheckEmbedDim(context.Context, plugin.ULID, int) error             { return nil }
func (s *pendingStore) AutoLinkByEmbedding(context.Context, plugin.ULID, []float32) error { return nil }

type sliceIterator struct {
	engrams []*plugin.Engram
	pos     int
}

func (it *sliceIterator) Next() bool             { it.pos++; return it.pos < len(it.engrams) }
func (it *sliceIterator) Engram() *plugin.Engram { return it.engrams[it.pos] }
func (it *sliceIterator) CurrentWS() [8]byte     { return [8]byte{1} }
func (it *sliceIterator) Close() error           { return nil }

// stageRecordingOllama stands in for Ollama's /api/chat and records, per
// request, the pipeline stage whose system prompt it carries ("probe" for any
// other prompt, which is the connectivity check in Init).
type stageRecordingOllama struct {
	mu     sync.Mutex
	stages []string
}

func (o *stageRecordingOllama) calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.stages...)
}

func newStageRecordingOllama(t *testing.T) (*stageRecordingOllama, string) {
	t.Helper()
	prompts := enrich.DefaultPrompts()
	stageOf := map[string]string{
		prompts.EntitiesSystem:      "entities",
		prompts.RelationshipsSystem: "relationships",
		prompts.ClassifySystem:      "classification",
		prompts.SummarizeSystem:     "summary",
	}
	replies := map[string]string{
		"probe":          "OK",
		"entities":       `{"entities":[{"name":"Ferngrove","type":"project","confidence":0.9}]}`,
		"relationships":  `{"relationships":[]}`,
		"classification": `{"memory_type":"decision","type_label":"storage_decision"}`,
		"summary":        `{"summary":"Ferngrove moves its event store to PostgreSQL.","key_points":["event store on PostgreSQL"]}`,
	}
	o := &stageRecordingOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		stage, ok := stageOf[req.Messages[0].Content]
		if !ok {
			stage = "probe"
		}
		o.mu.Lock()
		o.stages = append(o.stages, stage)
		o.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": replies[stage]},
		})
	}))
	t.Cleanup(srv.Close)
	return o, "ollama://" + srv.Listener.Addr().String() + "/test-model"
}

// TestRetroactiveEnrich_LightModeSummarizedIsDoneNotFailure pins the
// background half of #883. In light mode a memory that already has a summary
// leaves the pipeline nothing to do: Run returns ErrNothingToEnrich without
// calling the provider. The background pass must mark such a memory done,
// must not count it as an error, and must not pick it up again.
//
// Everything below the store is real: EnrichService with its circuit breaker
// (5 consecutive failures open it), the light-mode pipeline, the Ollama HTTP
// provider against a local stub, and the processor's own pass. Before the fix
// the breaker counted ErrNothingToEnrich as a provider failure, so five
// summarized memories in a row opened it; the sixth got circuit.ErrOpen, was
// counted as an error, and stopped the pass before the memory that needed a
// summary.
func TestRetroactiveEnrich_LightModeSummarizedIsDoneNotFailure(t *testing.T) {
	ctx := context.Background()
	ollama, url := newStageRecordingOllama(t)
	svc, err := enrich.NewEnrichService(url)
	if err != nil {
		t.Fatalf("NewEnrichService: %v", err)
	}
	svc.SetEnrichConfig(&config.PluginConfig{EnrichMode: "light"})
	if err := svc.Init(ctx, plugin.PluginConfig{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	afterInit := len(ollama.calls())

	// More summarized memories than the breaker tolerates, then one without a
	// summary. A caller-supplied summary sets DigestSummarized at write time
	// (#500), so the processor reads it from the flags as well as the field.
	const summarized = 8
	var engrams []*plugin.Engram
	for i := 0; i < summarized; i++ {
		var id plugin.ULID
		id[15] = byte(i + 1)
		engrams = append(engrams, &plugin.Engram{ID: id, Concept: "note", Content: "already summarized", Summary: "caller summary"})
	}
	var lastID plugin.ULID
	lastID[15] = summarized + 1
	needsSummary := &plugin.Engram{ID: lastID, Concept: "event store choice", Content: "Ferngrove moves its event store to PostgreSQL."}
	store := newPendingStore(append(engrams, needsSummary)...)
	for _, e := range engrams {
		store.flags[e.ID] = plugin.DigestSummarized
	}

	rp := plugin.NewRetroactiveProcessor(store, svc, plugin.DigestEnrich)

	// Pass 1 reaches every memory and finishes all of them.
	rp.ProcessBatch(ctx)
	st := rp.Stats()
	if st.Errors != 0 || st.Processed != summarized+1 {
		t.Fatalf("pass 1: errors=%d processed=%d, want errors=0 processed=%d", st.Errors, st.Processed, summarized+1)
	}
	for i, e := range engrams {
		if f := store.flagsOf(e.ID); f&plugin.DigestEnrich == 0 || f&plugin.DigestEnrichFailed != 0 {
			t.Fatalf("summarized memory %d: flags=%#x, want DigestEnrich set and DigestEnrichFailed clear", i+1, f)
		}
	}
	if f := store.flagsOf(needsSummary.ID); f&plugin.DigestEnrich == 0 {
		t.Fatalf("the memory without a summary was not enriched: flags=%#x", f)
	}
	if store.summaryOf(needsSummary.ID) == "" {
		t.Fatal("the memory without a summary got no summary")
	}
	if got := ollama.calls()[afterInit:]; !reflect.DeepEqual(got, []string{"summary"}) {
		t.Fatalf("provider calls in pass 1 = %v, want one summary call for the one memory that needed it", got)
	}

	// Pass 2 finds nothing pending: no scan, no provider call, no new errors.
	scans := store.scanCount()
	rp.ProcessBatch(ctx)
	if got := store.scanCount(); got != scans {
		t.Fatalf("pass 2 scanned again (%d scans, want %d): finished memories are still pending", got, scans)
	}
	if got := ollama.calls()[afterInit:]; len(got) != 1 {
		t.Fatalf("provider calls after pass 2 = %v, want still only the one from pass 1", got)
	}
	if st := rp.Stats(); st.Errors != 0 || st.Processed != summarized+1 {
		t.Fatalf("after pass 2: errors=%d processed=%d, want errors=0 processed=%d", st.Errors, st.Processed, summarized+1)
	}
}
