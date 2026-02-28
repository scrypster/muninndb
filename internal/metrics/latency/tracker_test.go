package latency

import (
	"testing"
	"time"
)

func TestTracker_RecordAndPercentiles(t *testing.T) {
	tr := New()
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	for i := 1; i <= 100; i++ {
		tr.Record(ws, "write", time.Duration(i)*time.Millisecond)
	}
	stats := tr.For(ws, "write")
	if stats.Count != 100 {
		t.Errorf("Count = %d, want 100", stats.Count)
	}
	if stats.P50Ms < 45 || stats.P50Ms > 55 {
		t.Errorf("P50 = %.1f, want ~50", stats.P50Ms)
	}
	if stats.P95Ms < 90 || stats.P95Ms > 100 {
		t.Errorf("P95 = %.1f, want ~95", stats.P95Ms)
	}
	if stats.P99Ms < 95 || stats.P99Ms > 100 {
		t.Errorf("P99 = %.1f, want ~99", stats.P99Ms)
	}
}

func TestTracker_EmptyStats(t *testing.T) {
	tr := New()
	ws := [8]byte{1}
	stats := tr.For(ws, "read")
	if stats.Count != 0 {
		t.Errorf("Count = %d, want 0", stats.Count)
	}
}

func TestTracker_Snapshot(t *testing.T) {
	tr := New()
	ws1 := [8]byte{1}
	ws2 := [8]byte{2}
	tr.Record(ws1, "write", 5*time.Millisecond)
	tr.Record(ws1, "activate", 10*time.Millisecond)
	tr.Record(ws2, "write", 20*time.Millisecond)
	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Errorf("Snapshot has %d vaults, want 2", len(snap))
	}
	if len(snap[ws1]) != 2 {
		t.Errorf("Vault 1 has %d operations, want 2", len(snap[ws1]))
	}
}

func TestTracker_RingWrap(t *testing.T) {
	tr := New()
	ws := [8]byte{1}
	for i := 0; i < ringSize+500; i++ {
		tr.Record(ws, "write", time.Millisecond)
	}
	stats := tr.For(ws, "write")
	if stats.Count != int64(ringSize+500) {
		t.Errorf("Count = %d, want %d", stats.Count, ringSize+500)
	}
	if stats.P50Ms < 0.5 || stats.P50Ms > 1.5 {
		t.Errorf("P50 = %.1f, want ~1.0", stats.P50Ms)
	}
}
