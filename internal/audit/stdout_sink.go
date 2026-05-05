package audit

import (
	"encoding/json"
	"os"
	"sync"
)

// StdoutSink writes each event as a JSON line to stdout.
// The "_stream" field is set to "audit" so log-shippers (Vector, Fluentbit, etc.)
// can route audit lines separately from application logs.
type StdoutSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewStdoutSink returns a sink that writes to os.Stdout.
func NewStdoutSink() *StdoutSink {
	return &StdoutSink{enc: json.NewEncoder(os.Stdout)}
}

func (s *StdoutSink) Write(e AuditEvent) error {
	type wire struct {
		Stream string `json:"_stream"`
		AuditEvent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(wire{Stream: "audit", AuditEvent: e})
}

func (s *StdoutSink) Close() error { return nil }
