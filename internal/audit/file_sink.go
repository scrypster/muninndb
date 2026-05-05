package audit

import (
	"encoding/json"
	"os"
	"sync"
)

// FileSink writes one JSON object per line (NDJSON) to an append-only file.
// Safe for concurrent use.
type FileSink struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

// NewFileSink opens (or creates) the file at path in append mode.
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileSink{f: f, enc: json.NewEncoder(f)}, nil
}

// Write encodes e as a single JSON line.
func (s *FileSink) Write(e AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(e)
}

// Close flushes and closes the underlying file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
