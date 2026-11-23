package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/embedlane"
	"github.com/scrypster/muninndb/internal/plugin"
)

// Invented fixture domain: a community greenhouse's watering log.
const (
	ollamaHoldText  = "Bench 4: tomatoes watered twice, drip line on the east wall replaced."
	ollamaQueryText = "which bench needs a new drip line"
	ollamaProbeText = "dimension detection probe" // what Init embeds; never reported
)

// ollamaWatchdog is a FAILURE DETECTOR, never a timing assertion: it turns a
// hang into a test failure instead of a stuck CI job. No ordering in these
// tests depends on it.
const ollamaWatchdog = 10 * time.Second

// ollamaArrival is one embed request as the stub received it.
type ollamaArrival struct {
	path  string
	texts []string
}

// ollamaStub is an httptest Ollama. It answers Init's probes and serves
// /api/embed, or with legacy=true answers /api/embed with 404 (so Init picks
// the legacy path) and serves /api/embeddings. Every embed request except
// Init's probe is reported on arrivals BEFORE it is answered. A request whose
// first text is ollamaHoldText is not answered until release(): the stub's
// model of a batch request Ollama has started and cannot interrupt.
type ollamaStub struct {
	srv      *httptest.Server
	arrivals chan ollamaArrival
	release  func()
}

func newOllamaStub(t *testing.T, legacy bool) *ollamaStub {
	t.Helper()
	hold := make(chan struct{})
	var once sync.Once
	s := &ollamaStub{
		arrivals: make(chan ollamaArrival, 256),
		release:  func() { once.Do(func() { close(hold) }) },
	}
	answer := func(w http.ResponseWriter, r *http.Request, path string, texts []string) bool {
		if len(texts) == 1 && texts[0] == ollamaProbeText {
			return true
		}
		s.arrivals <- ollamaArrival{path: path, texts: texts}
		if len(texts) > 0 && texts[0] == ollamaHoldText {
			select {
			case <-hold:
			case <-r.Context().Done():
				return false
			}
		}
		return true
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/embed" && !legacy:
			var req ollamaBatchEmbedRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !answer(w, r, r.URL.Path, req.Input) {
				return
			}
			out := ollamaBatchEmbedResponse{Embeddings: make([][]float64, len(req.Input))}
			for i := range out.Embeddings {
				out.Embeddings[i] = []float64{0.1, 0.2, 0.3}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/api/embeddings" && legacy:
			var req ollamaEmbedRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !answer(w, r, r.URL.Path, []string{req.Prompt}) {
				return
			}
			_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: []float64{0.1, 0.2, 0.3}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// Cleanups run last-in first-out: release any held request, then close.
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.release)
	return s
}

func (s *ollamaStub) hostPort() string { return s.srv.Listener.Addr().String() }

// initOllama returns a provider initialised against the stub.
func initOllama(t *testing.T, s *ollamaStub) *OllamaProvider {
	t.Helper()
	p := &OllamaProvider{}
	if _, err := p.Init(context.Background(), ProviderHTTPConfig{BaseURL: "http://" + s.hostPort(), Model: "m"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// initOllamaService returns an EmbedService built from an ollama:// URL
// pointing at the stub, with query (e.g. "?batch_size=3") appended.
func initOllamaService(t *testing.T, s *ollamaStub, query string) *EmbedService {
	t.Helper()
	svc, err := NewEmbedService("ollama://" + s.hostPort() + "/m" + query)
	if err != nil {
		t.Fatalf("NewEmbedService: %v", err)
	}
	if err := svc.Init(context.Background(), plugin.PluginConfig{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func ollamaTexts(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s %d: seedlings potted, vents opened at noon, soil moisture logged.", prefix, i)
	}
	return out
}

func ollamaEmbedAsync(ctx context.Context, p *OllamaProvider, texts []string) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := p.EmbedBatch(ctx, texts)
		ch <- err
	}()
	return ch
}

// awaitArrival waits for the next embed request at the stub and checks its
// first text.
func awaitArrival(t *testing.T, s *ollamaStub, wantFirst, what string) {
	t.Helper()
	select {
	case got := <-s.arrivals:
		if len(got.texts) == 0 || got.texts[0] != wantFirst {
			t.Fatalf("waiting for %s, Ollama received %q first", what, got.texts)
		}
	case <-time.After(ollamaWatchdog):
		t.Fatalf("%s never reached Ollama", what)
	}
}

// awaitParked waits until a call is parked on the batch lane, and fails if a
// request reaches the stub instead: that request skipped the lane.
func awaitParked(t *testing.T, s *ollamaStub, parked <-chan struct{}, what string) {
	t.Helper()
	select {
	case got := <-s.arrivals:
		t.Fatalf("%s reached Ollama (%q) while a batch request was still in flight; want it held on the batch lane", what, got.texts)
	case <-parked:
	case <-time.After(ollamaWatchdog):
		t.Fatalf("%s neither reached Ollama nor waited for the batch lane", what)
	}
}

func awaitDone(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(ollamaWatchdog):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// TestOllamaProvider_QueryNotQueuedBehindSecondBatch: while one background
// request is in flight at Ollama (held open by the stub), a second background
// call is not sent but waits on the provider's batch lane, and a marked query
// is sent at once and completes. So Ollama holds at most one batch request
// ahead of the query. Before the lane, the second background request went
// straight to Ollama and the query queued behind both. The lane follows the
// caller's mark, not the call's shape: an unmarked single text waits like any
// batch, and a marked call above ollamaQueryLaneMaxTexts takes the batch
// lane. A call waiting on the lane gives up when its context ends. Both
// endpoints: the legacy /api/embeddings path takes the same lanes.
func TestOllamaProvider_QueryNotQueuedBehindSecondBatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		path   string
	}{
		{"batch endpoint", false, "/api/embed"},
		{"legacy endpoint", true, "/api/embeddings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newOllamaStub(t, tc.legacy)
			p := initOllama(t, stub)
			if p.useLegacyAPI != tc.legacy {
				t.Fatalf("useLegacyAPI = %v, want %v", p.useLegacyAPI, tc.legacy)
			}
			parked := make(chan struct{}, 64)
			p.lane().onWait = func() { parked <- struct{}{} }
			ctx := context.Background()

			// A: background work, in flight at Ollama until release().
			a := ollamaEmbedAsync(ctx, p, []string{ollamaHoldText})
			awaitArrival(t, stub, ollamaHoldText, "the first background request")

			// B: a second background call, shaped like a query (one text) but
			// unmarked. It must wait on the lane, not reach Ollama.
			b := ollamaEmbedAsync(ctx, p, []string{"Bench 7: basil cuttings rooted."})
			awaitParked(t, stub, parked, "a second background request")

			// Q: a marked query. It must reach Ollama now and complete
			// while A is still held, without waiting on the lane.
			q := ollamaEmbedAsync(embedlane.WithQuery(ctx), p, []string{ollamaQueryText})
			select {
			case got := <-stub.arrivals:
				if got.path != tc.path || len(got.texts) != 1 || got.texts[0] != ollamaQueryText {
					t.Fatalf("Ollama received %s %q, want the marked query on %s", got.path, got.texts, tc.path)
				}
			case <-parked:
				t.Fatal("the marked query waited on the batch lane")
			case <-time.After(ollamaWatchdog):
				t.Fatal("the marked query never reached Ollama")
			}
			if err := awaitDone(t, q, "the marked query"); err != nil {
				t.Fatalf("marked query failed: %v", err)
			}

			// A marked call above the query cap is background work.
			big := ollamaEmbedAsync(embedlane.WithQuery(ctx), p, ollamaTexts("Bench 9", ollamaQueryLaneMaxTexts+1))
			awaitParked(t, stub, parked, fmt.Sprintf("a marked call of %d texts (cap %d)", ollamaQueryLaneMaxTexts+1, ollamaQueryLaneMaxTexts))

			// A call waiting on the lane gives up when its context ends.
			cctx, cancel := context.WithCancel(ctx)
			c := ollamaEmbedAsync(cctx, p, []string{"Bench 2: compost turned."})
			awaitParked(t, stub, parked, "a cancellable background request")
			cancel()
			err := awaitDone(t, c, "the cancelled background request")
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "ollama embed: batch lane busy") {
				t.Fatalf("cancelled waiter returned %v, want an \"ollama embed: batch lane busy\" error wrapping context.Canceled", err)
			}

			// Release A: everything still waiting runs, one request at a time.
			stub.release()
			for what, ch := range map[string]<-chan error{"first background": a, "second background": b, "oversized marked": big} {
				if err := awaitDone(t, ch, what); err != nil {
					t.Fatalf("%s call failed: %v", what, err)
				}
			}
		})
	}
}

// TestOllamaProvider_BatchSizeOverride: the chunk each request carries is
// ollamaDefaultBatchSize unless the provider URL sets batch_size, and a host
// can raise it above the default. Asserted on what Ollama receives through
// the service's BatchEmbedder, which chunks by MaxBatchSize. The legacy
// endpoint stays at one text per request.
func TestOllamaProvider_BatchSizeOverride(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		query  string
		texts  int
		want   []int
	}{
		{"default", false, "", 20, []int{8, 8, 4}},
		{"lowered", false, "?batch_size=3", 7, []int{3, 3, 1}},
		{"raised", false, "?batch_size=64", 100, []int{64, 36}},
		{"legacy ignores it", true, "?batch_size=16", 3, []int{1, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newOllamaStub(t, tc.legacy)
			svc := initOllamaService(t, stub, tc.query)
			if _, err := svc.Embed(context.Background(), ollamaTexts("Bench", tc.texts)); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			var got []int
			for len(got) < len(tc.want) {
				select {
				case a := <-stub.arrivals:
					got = append(got, len(a.texts))
				default:
					t.Fatalf("Ollama received chunks %v, want %v", got, tc.want)
				}
			}
			select {
			case a := <-stub.arrivals:
				t.Fatalf("Ollama received an extra chunk of %d after %v, want %v", len(a.texts), got, tc.want)
			default:
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("Ollama received chunks %v, want %v", got, tc.want)
			}
		})
	}
}

// ollamaTimeoutRecorder records, for every embed request, how many texts it
// carried and how long its deadline left when it was sent.
type ollamaTimeoutRecorder struct {
	mu   sync.Mutex
	seen []ollamaTimedRequest
}

type ollamaTimedRequest struct {
	texts       int
	hasDeadline bool
	left        time.Duration
}

func (r *ollamaTimeoutRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && (req.URL.Path == "/api/embed" || req.URL.Path == "/api/embeddings") {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		n := 1
		if req.URL.Path == "/api/embed" {
			var b ollamaBatchEmbedRequest
			_ = json.Unmarshal(body, &b)
			n = len(b.Input)
		}
		dl, ok := req.Context().Deadline()
		r.mu.Lock()
		r.seen = append(r.seen, ollamaTimedRequest{texts: n, hasDeadline: ok, left: time.Until(dl)})
		r.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(req)
}

// TestOllamaProvider_RequestTimeoutFollowsChunk: every request's deadline is
// 30s + 10s per text it carries, for the chunk actually sent. That includes
// each one-text request on the legacy endpoint, which had no deadline of its
// own before it held the batch lane.
func TestOllamaProvider_RequestTimeoutFollowsChunk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		query  string
		texts  int
		want   []int // texts per request
	}{
		{"batch endpoint", false, "?batch_size=3", 7, []int{3, 3, 1}},
		{"legacy endpoint", true, "", 2, []int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newOllamaStub(t, tc.legacy)
			svc := initOllamaService(t, stub, tc.query)
			rec := &ollamaTimeoutRecorder{}
			svc.provider.(*OllamaProvider).client = &http.Client{Transport: rec}
			if _, err := svc.Embed(context.Background(), ollamaTexts("Bench", tc.texts)); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.seen) != len(tc.want) {
				t.Fatalf("saw %d requests, want %d", len(rec.seen), len(tc.want))
			}
			for i, r := range rec.seen {
				if r.texts != tc.want[i] {
					t.Fatalf("request %d carried %d texts, want %d", i, r.texts, tc.want[i])
				}
				if !r.hasDeadline {
					t.Fatalf("request %d (%d texts) was sent with no deadline", i, r.texts)
				}
				want := 30*time.Second + time.Duration(r.texts)*10*time.Second
				if r.left > want || r.left < want-2*time.Second {
					t.Fatalf("request %d (%d texts) had %s left at send, want %s (30s + 10s per text)", i, r.texts, r.left.Round(time.Millisecond), want)
				}
			}
		})
	}
}
