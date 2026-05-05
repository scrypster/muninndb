package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// WebhookSink batches events and POSTs them as a JSON array to a URL.
// Compatible with Splunk HEC, Datadog Logs, Loki (with a vector adapter), etc.
type WebhookSink struct {
	url           string
	client        *http.Client
	flushInterval time.Duration

	mu     sync.Mutex
	batch  []AuditEvent
	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewWebhookSink creates a WebhookSink that flushes at most every flushInterval.
// A flushInterval of 0 uses a default of 5 seconds.
func NewWebhookSink(url string, flushInterval time.Duration) *WebhookSink {
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	s := &WebhookSink{
		url:           url,
		client:        &http.Client{Timeout: 10 * time.Second},
		flushInterval: flushInterval,
		done:          make(chan struct{}),
	}
	s.ticker = time.NewTicker(flushInterval)
	s.wg.Add(1)
	go s.run()
	return s
}

func (s *WebhookSink) Write(e AuditEvent) error {
	s.mu.Lock()
	s.batch = append(s.batch, e)
	s.mu.Unlock()
	return nil
}

func (s *WebhookSink) Close() error {
	s.ticker.Stop()
	close(s.done)
	s.wg.Wait()
	s.flush()
	return nil
}

func (s *WebhookSink) run() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ticker.C:
			s.flush()
		case <-s.done:
			return
		}
	}
}

func (s *WebhookSink) flush() {
	s.mu.Lock()
	if len(s.batch) == 0 {
		s.mu.Unlock()
		return
	}
	toSend := s.batch
	s.batch = nil
	s.mu.Unlock()

	b, err := json.Marshal(toSend)
	if err != nil {
		slog.Warn("audit: webhook: marshal failed", "err", err)
		return
	}
	resp, err := s.client.Post(s.url, "application/json", bytes.NewReader(b))
	if err != nil {
		slog.Warn("audit: webhook: post failed", "url", s.url, "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("audit: webhook: server error", "url", s.url, "status", resp.StatusCode)
	}
}
