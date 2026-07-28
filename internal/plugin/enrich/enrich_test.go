package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine/circuit"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestEnrichServiceNew_Ollama tests creating an EnrichService for Ollama.
func TestEnrichServiceNew_Ollama(t *testing.T) {
	es, err := NewEnrichService("ollama://localhost:11434/llama3.2")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-ollama" {
		t.Fatalf("Expected name 'enrich-ollama', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "llama3.2" {
		t.Fatalf("Expected model 'llama3.2', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_OpenAI tests creating an EnrichService for OpenAI.
func TestEnrichServiceNew_OpenAI(t *testing.T) {
	es, err := NewEnrichService("openai://gpt-4o-mini")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-openai" {
		t.Fatalf("Expected name 'enrich-openai', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "gpt-4o-mini" {
		t.Fatalf("Expected model 'gpt-4o-mini', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_Anthropic tests creating an EnrichService for Anthropic.
func TestEnrichServiceNew_Anthropic(t *testing.T) {
	es, err := NewEnrichService("anthropic://claude-haiku")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-anthropic" {
		t.Fatalf("Expected name 'enrich-anthropic', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "claude-haiku" {
		t.Fatalf("Expected model 'claude-haiku', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_InvalidScheme tests error handling for invalid schemes.
func TestEnrichServiceNew_InvalidScheme(t *testing.T) {
	_, err := NewEnrichService("invalid://localhost:11434/model")
	if err == nil {
		t.Fatalf("Expected error for invalid scheme, got nil")
	}
}

func TestEnrichService_Init(t *testing.T) {
	mock := NewMockLLMProvider()
	es := &EnrichService{
		provider: mock,
		name:     "enrich-mock",
		provCfg: &plugin.ProviderConfig{
			Scheme:  plugin.SchemeOllama,
			BaseURL: "http://localhost:11434",
			Model:   "test",
		},
	}

	err := es.Init(context.Background(), plugin.PluginConfig{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if es.pipeline == nil {
		t.Fatal("pipeline should be initialized after Init")
	}
	if es.limiter == nil {
		t.Fatal("limiter should be initialized after Init")
	}
}

func TestEnrichService_Init_ProviderError(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.customComplete = func(ctx context.Context, system, user string) (string, error) {
		return "", context.DeadlineExceeded
	}
	// Override Init to fail
	failInit := &failingInitProvider{}
	es := &EnrichService{
		provider: failInit,
		name:     "enrich-fail",
		provCfg: &plugin.ProviderConfig{
			Scheme:  plugin.SchemeOllama,
			BaseURL: "http://localhost:11434",
			Model:   "test",
		},
	}
	_ = mock

	err := es.Init(context.Background(), plugin.PluginConfig{})
	if err == nil {
		t.Fatal("expected Init to fail when provider Init fails")
	}
}

type failingInitProvider struct{}

func (f *failingInitProvider) Name() string { return "fail" }
func (f *failingInitProvider) Init(_ context.Context, _ LLMProviderConfig) error {
	return context.DeadlineExceeded
}
func (f *failingInitProvider) Complete(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *failingInitProvider) Close() error { return nil }

func TestEnrichService_Enrich_NotInitialized(t *testing.T) {
	es := &EnrichService{
		provider: NewMockLLMProvider(),
		name:     "enrich-mock",
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	_, err := es.Enrich(context.Background(), eng)
	if err == nil {
		t.Fatal("expected error when pipeline is nil")
	}
}

func TestEnrichService_Enrich_WhenClosed(t *testing.T) {
	es := &EnrichService{
		provider: NewMockLLMProvider(),
		name:     "enrich-mock",
		closed:   true,
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	_, err := es.Enrich(context.Background(), eng)
	if err == nil {
		t.Fatal("expected error when service is closed")
	}
}

func TestEnrichService_Enrich_Success(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	es := &EnrichService{
		provider: mock,
		pipeline: pipeline,
		limiter:  limiter,
		name:     "enrich-mock",
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "test", Content: "hello world"}
	result, err := es.Enrich(context.Background(), eng)
	if err != nil {
		t.Fatalf("Enrich failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestEnrichService_HTTP429Then200HonorsRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	readNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(wait time.Duration) {
		clockMu.Lock()
		now = now.Add(wait)
		clockMu.Unlock()
	}
	var requestMu sync.Mutex
	var requestTimes []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestMu.Lock()
		requestTimes = append(requestTimes, readNow())
		attempt := len(requestTimes)
		requestMu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"recovered\",\"key_points\":[\"ok\"]}"}}]}`))
	}))
	defer srv.Close()

	provider := NewOpenAILLMProvider()
	provider.baseURL = srv.URL
	provider.model = "test"
	provider.apiKey = "test-key"
	pipeline := NewPipeline(provider, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{
		provider: provider,
		pipeline: pipeline,
		breaker:  circuit.New(5, time.Hour),
		nowFn:    readNow,
		waitFn: func(_ context.Context, wait time.Duration) error {
			advance(wait)
			return nil
		},
		jitterFn: func(wait time.Duration) time.Duration { return wait },
	}
	eng := &storage.Engram{ID: storage.NewULID(), Concept: "original", Content: "pending"}

	if _, err := es.Enrich(context.Background(), eng); !plugin.IsRetryableProviderError(err) {
		t.Fatalf("first Enrich error = %v, want typed 429", err)
	}
	result, err := es.Enrich(context.Background(), eng)
	if err != nil || result == nil || result.Summary != "recovered" {
		t.Fatalf("recovery result = (%#v, %v)", result, err)
	}
	requestMu.Lock()
	defer requestMu.Unlock()
	if len(requestTimes) != 2 {
		t.Fatalf("requests = %d, want 2", len(requestTimes))
	}
	if got := requestTimes[1].Sub(requestTimes[0]); got < 5*time.Second {
		t.Fatalf("second request after %v, want at least 5s", got)
	}
}

func TestEnrichService_OlderConcurrentSuccessCannotClearNewThrottle(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	readNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	var waitsMu sync.Mutex
	var waits []time.Duration
	entered := make(chan int, 2)
	releaseThrottle := make(chan struct{})
	releaseOlderSuccess := make(chan struct{})
	var calls atomic.Int32
	mock := NewMockLLMProvider()
	mock.customComplete = func(context.Context, string, string) (string, error) {
		call := int(calls.Add(1))
		if call <= 2 {
			entered <- call
		}
		switch call {
		case 1:
			<-releaseThrottle
			return "", &plugin.ProviderError{
				Provider: "fake", StatusCode: 429, Retryable: true,
				RetryAfter: 5 * time.Second, HasRetryAfter: true,
			}
		case 2:
			<-releaseOlderSuccess
		}
		return `{"summary":"ok","key_points":[]}`, nil
	}
	pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{
		provider: mock,
		pipeline: pipeline,
		breaker:  circuit.New(5, time.Hour),
		nowFn:    readNow,
		waitFn: func(_ context.Context, wait time.Duration) error {
			waitsMu.Lock()
			waits = append(waits, wait)
			waitsMu.Unlock()
			clockMu.Lock()
			now = now.Add(wait)
			clockMu.Unlock()
			return nil
		},
		jitterFn: func(wait time.Duration) time.Duration { return wait },
	}
	eng := &storage.Engram{ID: storage.NewULID()}
	results := make(chan error, 2)
	go func() { _, err := es.Enrich(context.Background(), eng); results <- err }()
	go func() { _, err := es.Enrich(context.Background(), eng); results <- err }()
	<-entered
	<-entered // both calls were admitted before either outcome

	close(releaseThrottle)
	if err := <-results; !plugin.IsRetryableProviderError(err) {
		t.Fatalf("first completed result = %v, want throttle", err)
	}
	close(releaseOlderSuccess)
	if err := <-results; err != nil {
		t.Fatalf("older concurrent call = %v, want success", err)
	}

	if _, err := es.Enrich(context.Background(), eng); err != nil {
		t.Fatalf("post-throttle request failed: %v", err)
	}
	waitsMu.Lock()
	defer waitsMu.Unlock()
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s]; older success cleared newer throttle", waits)
	}
}

func TestEnrichService_RepeatedThrottleUsesBoundedExponentialBackoff(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	var waits []time.Duration
	requests := 0
	mock := NewMockLLMProvider()
	mock.customComplete = func(context.Context, string, string) (string, error) {
		requests++
		return "", &plugin.ProviderError{Provider: "fake", StatusCode: 429, Retryable: true}
	}
	pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{
		provider: mock,
		pipeline: pipeline,
		breaker:  circuit.New(100, time.Hour),
		nowFn:    func() time.Time { return now },
		waitFn: func(_ context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			now = now.Add(wait)
			return nil
		},
		jitterFn: func(wait time.Duration) time.Duration { return wait },
	}
	eng := &storage.Engram{ID: storage.NewULID()}

	for range 4 {
		if _, err := es.Enrich(context.Background(), eng); !plugin.IsRetryableProviderError(err) {
			t.Fatalf("Enrich error = %v, want retryable throttle", err)
		}
	}
	if requests != 4 {
		t.Fatalf("requests = %d, want exactly one per attempt", requests)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
}

func TestExponentialProviderBackoffIsCapped(t *testing.T) {
	if got := exponentialProviderBackoff(100); got != providerBackoffMax {
		t.Fatalf("backoff = %v, want cap %v", got, providerBackoffMax)
	}
}

func TestEnrichService_BackpressureWaitHonorsContextCancellation(t *testing.T) {
	now := time.Now()
	mock := NewMockLLMProvider()
	requests := 0
	mock.customComplete = func(context.Context, string, string) (string, error) {
		requests++
		return "", &plugin.ProviderError{
			Provider: "fake", StatusCode: 429, Retryable: true,
			RetryAfter: time.Minute, HasRetryAfter: true,
		}
	}
	pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{
		provider: mock,
		pipeline: pipeline,
		breaker:  circuit.New(5, time.Hour),
		nowFn:    func() time.Time { return now },
		waitFn: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
		jitterFn: func(wait time.Duration) time.Duration { return wait },
	}
	eng := &storage.Engram{ID: storage.NewULID()}
	_, _ = es.Enrich(context.Background(), eng)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := es.Enrich(ctx, eng)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Enrich error = %v, want context.Canceled", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, canceled wait must not call provider again", requests)
	}
}

func TestEnrichService_AuthenticationFailureStillOpensCircuit(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.customComplete = func(context.Context, string, string) (string, error) {
		return "", &plugin.ProviderError{Provider: "fake", StatusCode: 401, Retryable: false}
	}
	pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{provider: mock, pipeline: pipeline, breaker: circuit.New(1, time.Hour)}
	eng := &storage.Engram{ID: storage.NewULID()}

	if _, err := es.Enrich(context.Background(), eng); err == nil || plugin.IsRetryableProviderError(err) {
		t.Fatalf("first Enrich error = %v, want observable non-retryable auth failure", err)
	}
	if _, err := es.Enrich(context.Background(), eng); !errors.Is(err, circuit.ErrOpen) {
		t.Fatalf("second Enrich error = %v, want circuit.ErrOpen", err)
	}
}

func TestEnrichService_Close_Idempotent(t *testing.T) {
	es, err := NewEnrichService("ollama://localhost:11434/test")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if err := es.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := es.Close(); err != nil {
		t.Fatalf("second Close should be idempotent: %v", err)
	}
}

func TestEnrichService_CreateRateLimiter(t *testing.T) {
	es := &EnrichService{}

	tests := []struct {
		scheme plugin.ProviderScheme
		name   string
	}{
		{plugin.SchemeOllama, "ollama"},
		{plugin.SchemeOpenAI, "openai"},
		{plugin.SchemeAnthropic, "anthropic"},
		{"unknown", "default"},
	}

	for _, tt := range tests {
		limiter := es.createRateLimiter(tt.scheme)
		if limiter == nil {
			t.Fatalf("createRateLimiter(%s) returned nil", tt.name)
		}
	}
}
