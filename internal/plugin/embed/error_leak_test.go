package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProviders_HTTPErrorDoesNotLeakRequestBody proves that when an
// embed provider's remote endpoint rejects a request and echoes the
// offending input back in the error body (the common shape for a 400/413
// content-policy or oversize rejection), the sentinel memory text from that
// echoed body never reaches the Go error returned to the caller — which is
// what ultimately reaches a WARN log line at internal/plugin/retroactive.go
// ("error", embedErr) and, from there, the log ring buffer backing the web
// console. Same class already closed for the enrich transport (#750);
// #790 is the embed transport's turn.
func TestProviders_HTTPErrorDoesNotLeakRequestBody(t *testing.T) {
	const sentinel = "the-quarterly-ballet-recital-notes-are-under-the-couch-cushion"

	newLeakyServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"content rejected: ` + sentinel + `"}`))
		}))
	}

	cases := []struct {
		name     string
		provider Provider
	}{
		{"openai", &OpenAIProvider{}},
		{"cohere", &CohereProvider{}},
		{"google", &GoogleProvider{}},
		{"jina", &JinaProvider{}},
		{"voyage", &VoyageProvider{}},
		{"mistral", &MistralProvider{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newLeakyServer()
			defer server.Close()

			cfg := ProviderHTTPConfig{
				BaseURL: "http://" + server.Listener.Addr().String(),
				Model:   "test-model",
				APIKey:  "test-key",
			}

			// Init also hits the same always-400 server and shares the same
			// body-interpolation bug on the same code path in most providers.
			_, initErr := tc.provider.Init(context.Background(), cfg)
			if initErr != nil && strings.Contains(initErr.Error(), sentinel) {
				t.Errorf("%s: Init error leaked request/response body text: %v", tc.name, initErr)
			}

			_, err := tc.provider.EmbedBatch(context.Background(), []string{sentinel})
			if err == nil {
				t.Fatalf("%s: expected an error from EmbedBatch against a 400 server", tc.name)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("%s: EmbedBatch error leaked memory text into the error string: %v", tc.name, err)
			}
		})
	}
}

// TestOllamaProvider_HTTPErrorDoesNotLeakRequestBody covers Ollama
// separately: its Init probes GET / before ever reaching the embed
// endpoint, so a single always-400 handler never lets it reach EmbedBatch.
// Route / to 200 and /api/embed to the leaky 400 responder instead.
func TestOllamaProvider_HTTPErrorDoesNotLeakRequestBody(t *testing.T) {
	const sentinel = "the-quarterly-ballet-recital-notes-are-under-the-couch-cushion"

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"content rejected: ` + sentinel + `"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	p := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	_, err := p.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to fail against an always-400 /api/embed")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("Init error leaked request/response body text: %v", err)
	}
}
