package engine

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// stopWorkerOnly closes the worker's stop channel and waits for its final
// drain — shutdown() minus the idle wait, so a test can act in between.
func stopWorkerOnly(t *writeTwin) {
	t.stopped.Do(func() { t.cancel(); close(t.stop) })
	<-t.done
}

// TestWriteTwin_ShutdownCannotStrandAHookJob is the decomposed reproduction:
// a hook admitted just before the worker's final drain must not leave a job
// in the channel with in-flight accounting nobody will ever release.
func TestWriteTwin_ShutdownCannotStrandAHookJob(t *testing.T) {
	e := &Engine{twin: newWriteTwin(writetwin.Shadow)}
	go e.runWriteTwinWorker()
	tw := e.twin
	tw.testBeforeEnqueue = func() { stopWorkerOnly(tw) } // the drain happens in the gap
	vec := make([]float32, 8)
	vec[0] = 1
	e.WriteTwinOnEmbedded([8]byte{1}, &storage.Engram{ID: storage.NewULID(), CreatedAt: time.Now()}, vec)

	done := make(chan struct{})
	go func() { tw.shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second): // watchdog: a hang is the defect
		tw.idleMu.Lock()
		inflight := tw.inflight
		tw.idleMu.Unlock()
		t.Fatalf("shutdown hung 2s; inflight=%d, queued=%d", inflight, len(tw.jobs))
	}
}

// TestWriteTwin_ShutdownRacesRealHooks stresses the real hook against the real
// shutdown. Each trial must finish; a stranded job hangs it.
func TestWriteTwin_ShutdownRacesRealHooks(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // jobs on a store-less engine panic-and-recover
	defer slog.SetDefault(prev)
	vec := make([]float32, 8)
	vec[0] = 1
	for trial := 0; trial < 300; trial++ {
		e := &Engine{twin: newWriteTwin(writetwin.Shadow)}
		go e.runWriteTwinWorker()
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					e.WriteTwinOnEmbedded([8]byte{2}, &storage.Engram{ID: storage.NewULID(), CreatedAt: time.Now()}, vec)
				}
			}()
		}
		done := make(chan struct{})
		go func() { e.twin.shutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			e.twin.idleMu.Lock()
			inflight := e.twin.inflight
			e.twin.idleMu.Unlock()
			t.Fatalf("trial %d: shutdown hung; inflight=%d queued=%d", trial, inflight, len(e.twin.jobs))
		}
		wg.Wait()
	}
}
