package mcp

// server_sse_test.go — concurrency tests for SSE session mutex (#174).
//
// Root cause: sseSessionsMu was sync.Mutex; findSSEChannelsByToken and
// handleSSEMessage held an exclusive write lock during read-only map
// operations. Under rapid consecutive stores (e.g. Claude Code batch
// remember calls), concurrent POST goroutines serialized at the mutex,
// causing temporary MCP server unresponsiveness.
//
// Fix: sync.Mutex → sync.RWMutex; read paths use RLock/RUnlock.
//
// Run with -race to verify correctness under concurrent read/write access.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSSESessions_ConcurrentReadWrite is a race-detector test that exercises
// concurrent reads (findSSEChannelsByToken) and writes (add/delete sessions)
// simultaneously. Run with: go test -race ./internal/mcp/...
func TestSSESessions_ConcurrentReadWrite(t *testing.T) {
	srv := newTestServer()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Writer: continuously add and remove sessions, simulating SSE stream
	// connect/disconnect events.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			id := fmt.Sprintf("sess%d", i%10)
			ch := make(chan []byte, 64)

			srv.sseSessionsMu.Lock()
			srv.sseSessions[id] = &sseSession{ch: ch, authToken: "tok"}
			srv.sseSessionsMu.Unlock()

			runtime.Gosched()

			srv.sseSessionsMu.Lock()
			delete(srv.sseSessions, id)
			srv.sseSessionsMu.Unlock()
		}
	}()

	// Readers: simulate concurrent POST /mcp requests each calling
	// findSSEChannelsByToken. With the old sync.Mutex, these serialized.
	// With sync.RWMutex, they run concurrently.
	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = srv.findSSEChannelsByToken("tok")
				runtime.Gosched()
			}
		}()
	}

	wg.Wait()
	// If this completes without -race warnings, lock discipline is correct.
}

// TestFindSSEChannelsByToken_ReturnsCorrectChannels verifies that
// findSSEChannelsByToken returns only channels for the matching token.
func TestFindSSEChannelsByToken_ReturnsCorrectChannels(t *testing.T) {
	srv := newTestServer()

	ch1 := make(chan []byte, 4)
	ch2 := make(chan []byte, 4)
	ch3 := make(chan []byte, 4)

	srv.sseSessionsMu.Lock()
	srv.sseSessions["s1"] = &sseSession{ch: ch1, authToken: "token-a"}
	srv.sseSessions["s2"] = &sseSession{ch: ch2, authToken: "token-b"}
	srv.sseSessions["s3"] = &sseSession{ch: ch3, authToken: "token-a"}
	srv.sseSessionsMu.Unlock()

	got := srv.findSSEChannelsByToken("token-a")
	if len(got) != 2 {
		t.Errorf("expected 2 channels for token-a, got %d", len(got))
	}

	got = srv.findSSEChannelsByToken("token-b")
	if len(got) != 1 {
		t.Errorf("expected 1 channel for token-b, got %d", len(got))
	}

	got = srv.findSSEChannelsByToken("no-such-token")
	if len(got) != 0 {
		t.Errorf("expected 0 channels for unknown token, got %d", len(got))
	}
}

// TestFindSSEChannelsByToken_EmptyMap verifies correct behavior when no
// sessions exist.
func TestFindSSEChannelsByToken_EmptyMap(t *testing.T) {
	srv := newTestServer()
	got := srv.findSSEChannelsByToken("any-token")
	if len(got) != 0 {
		t.Errorf("expected 0 channels from empty map, got %d", len(got))
	}
}

// BenchmarkFindSSEChannelsByToken_Parallel measures throughput of concurrent
// readers. With sync.Mutex (bug), goroutines serialize. With sync.RWMutex
// (fix), readers run in parallel — throughput should scale with GOMAXPROCS.
//
// Run: go test -bench=BenchmarkFindSSEChannelsByToken_Parallel ./internal/mcp/...
func BenchmarkFindSSEChannelsByToken_Parallel(b *testing.B) {
	srv := newTestServer()

	// Populate with 5 sessions (typical Claude Code usage: 1-3 SSE streams).
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sess%d", i)
		srv.sseSessions[id] = &sseSession{
			ch:        make(chan []byte, 64),
			authToken: "bench-token",
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = srv.findSSEChannelsByToken("bench-token")
		}
	})
}
