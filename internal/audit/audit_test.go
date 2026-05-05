package audit_test

import (
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/audit"
)

func TestLogger_LogAndDrain(t *testing.T) {
	var mu sync.Mutex
	var got []audit.AuditEvent

	sink := audit.SinkFunc(func(e audit.AuditEvent) error {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		return nil
	})

	l := audit.New(audit.Config{BufferSize: 64}, sink)
	l.Log(audit.AuditEvent{Action: "vault.delete", Result: "ok"})
	l.Log(audit.AuditEvent{Action: "api_key.create", Result: "ok"})

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if got[0].Action != "vault.delete" {
		t.Errorf("want vault.delete, got %s", got[0].Action)
	}
	if got[1].Action != "api_key.create" {
		t.Errorf("want api_key.create, got %s", got[1].Action)
	}
}

func TestLogger_DropOnFullBuffer(t *testing.T) {
	block := make(chan struct{})
	var count int
	sink := audit.SinkFunc(func(e audit.AuditEvent) error {
		<-block
		count++
		return nil
	})

	l := audit.New(audit.Config{BufferSize: 1}, sink)
	l.Log(audit.AuditEvent{Action: "a"})
	done := make(chan struct{})
	go func() {
		l.Log(audit.AuditEvent{Action: "b"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Log() blocked on a full buffer")
	}
	close(block)
	_ = l.Close()
}

func TestNoopSink(t *testing.T) {
	l := audit.New(audit.Config{BufferSize: 4}, audit.NoopSink{})
	l.Log(audit.AuditEvent{Action: "anything"})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNilLogger_Safe(t *testing.T) {
	var l *audit.Logger
	l.Log(audit.AuditEvent{Action: "safe"})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMultiSink_FanOut(t *testing.T) {
	var mu sync.Mutex
	counts := [2]int{}

	sinkA := audit.SinkFunc(func(e audit.AuditEvent) error { mu.Lock(); counts[0]++; mu.Unlock(); return nil })
	sinkB := audit.SinkFunc(func(e audit.AuditEvent) error { mu.Lock(); counts[1]++; mu.Unlock(); return nil })

	l := audit.New(audit.Config{BufferSize: 16}, sinkA, sinkB)
	for i := 0; i < 5; i++ {
		l.Log(audit.AuditEvent{Action: "x"})
	}
	_ = l.Close()

	mu.Lock()
	defer mu.Unlock()
	if counts[0] != 5 || counts[1] != 5 {
		t.Errorf("want [5 5], got %v", counts)
	}
}
