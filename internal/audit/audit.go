package audit

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// AuditEvent is a single immutable audit record.
type AuditEvent struct {
	Timestamp  time.Time         `json:"ts"`
	EventID    string            `json:"event_id"`
	ActorType  string            `json:"actor_type"`
	ActorID    string            `json:"actor_id"`
	Action     string            `json:"action"`
	TargetType string            `json:"target_type,omitempty"`
	TargetID   string            `json:"target_id,omitempty"`
	Result     string            `json:"result"`
	Error      string            `json:"error,omitempty"`
	RequestID  string            `json:"request_id,omitempty"`
	ClientIP   string            `json:"client_ip,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Sink receives audit events. Implementations must be safe for concurrent use.
type Sink interface {
	Write(AuditEvent) error
	io.Closer
}

// SinkFunc is a function that satisfies Sink. Close is a no-op.
type SinkFunc func(AuditEvent) error

func (f SinkFunc) Write(e AuditEvent) error { return f(e) }
func (f SinkFunc) Close() error             { return nil }

// NoopSink discards all events.
type NoopSink struct{}

func (NoopSink) Write(AuditEvent) error { return nil }
func (NoopSink) Close() error           { return nil }

// Config holds Logger tuning parameters.
type Config struct {
	BufferSize int
}

func (c Config) bufferSize() int {
	if c.BufferSize <= 0 {
		return 4096
	}
	return c.BufferSize
}

// Logger asynchronously fans audit events out to one or more Sinks.
// A nil *Logger is safe: Log and Close are no-ops.
type Logger struct {
	ch     chan AuditEvent
	sinks  []Sink
	wg     sync.WaitGroup
	closed atomic.Bool
}

// New creates a Logger that drains events to all provided sinks.
func New(cfg Config, sinks ...Sink) *Logger {
	l := &Logger{
		ch:    make(chan AuditEvent, cfg.bufferSize()),
		sinks: sinks,
	}
	l.wg.Add(1)
	go l.run()
	return l
}

// Log enqueues an event. Never blocks. Safe to call on a nil *Logger.
func (l *Logger) Log(e AuditEvent) {
	if l == nil || l.closed.Load() {
		return
	}
	select {
	case l.ch <- e:
	default:
	}
}

// Close flushes pending events, closes sinks, waits for drain goroutine.
// Safe to call on a nil *Logger.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.closed.Store(true)
	close(l.ch)
	l.wg.Wait()
	for _, s := range l.sinks {
		_ = s.Close()
	}
	return nil
}

func (l *Logger) run() {
	defer l.wg.Done()
	for e := range l.ch {
		for _, s := range l.sinks {
			_ = s.Write(e)
		}
	}
}
