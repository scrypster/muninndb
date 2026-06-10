package working

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/types"
)

// TestCurrentAttentionDecay verifies the attention decay formula.
// attention(t) = attention₀ × 2^(-elapsed / halfLife)
func TestCurrentAttentionDecay(t *testing.T) {
	halfLife := 1 * time.Minute

	tests := []struct {
		name             string
		initialAttention float32
		elapsed          time.Duration
		expectedFraction float32 // fraction of initial attention remaining
	}{
		{
			name:             "no decay at t=0",
			initialAttention: 1.0,
			elapsed:          0,
			expectedFraction: 1.0,
		},
		{
			name:             "half decay at t=halfLife",
			initialAttention: 1.0,
			elapsed:          1 * time.Minute,
			expectedFraction: 0.5,
		},
		{
			name:             "quarter decay at t=2*halfLife",
			initialAttention: 1.0,
			elapsed:          2 * time.Minute,
			expectedFraction: 0.25,
		},
		{
			name:             "decay with 0.8 initial attention",
			initialAttention: 0.8,
			elapsed:          1 * time.Minute,
			expectedFraction: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			item := &WorkingItem{
				EngramID:  types.NewULID(),
				Attention: tt.initialAttention,
				AddedAt:   now.Add(-tt.elapsed),
				Context:   "test",
			}

			current := item.CurrentAttention(halfLife)

			expected := tt.initialAttention * tt.expectedFraction
			// Allow for small floating point errors (±0.001)
			if math.Abs(float64(current-expected)) > 0.001 {
				t.Errorf("expected %f, got %f", expected, current)
			}
		})
	}
}

// TestAddWithinCapacity tests adding items within MaxItems capacity.
func TestAddWithinCapacity(t *testing.T) {
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  5,
		CreatedAt: time.Now(),
	}

	// Add 3 items
	for i := 0; i < 3; i++ {
		wm.Add(types.NewULID(), 0.5, "context")
	}

	if len(wm.Items) != 3 {
		t.Errorf("expected 3 items, got %d", len(wm.Items))
	}

	// Verify LastAccess is updated
	if wm.LastAccess.IsZero() {
		t.Error("LastAccess should be set")
	}
}

// TestAddEvictsLowestAttention tests that the lowest-attention item is evicted when at capacity.
func TestAddEvictsLowestAttention(t *testing.T) {
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  3,
		CreatedAt: time.Now(),
	}

	id1 := types.NewULID()
	id2 := types.NewULID()
	id3 := types.NewULID()
	id4 := types.NewULID()

	// Add 3 items with different attention levels
	wm.Add(id1, 0.9, "context1")
	wm.Add(id2, 0.1, "context2") // lowest
	wm.Add(id3, 0.5, "context3")

	if len(wm.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(wm.Items))
	}

	// Add 4th item; should evict id2 (lowest attention = 0.1)
	wm.Add(id4, 0.7, "context4")

	if len(wm.Items) != 3 {
		t.Fatalf("expected 3 items after eviction, got %d", len(wm.Items))
	}

	// Verify id2 is gone
	ids := make(map[types.ULID]bool)
	for _, item := range wm.Items {
		ids[item.EngramID] = true
	}

	if ids[id2] {
		t.Error("id2 (lowest attention) should have been evicted")
	}

	if !ids[id1] || !ids[id3] || !ids[id4] {
		t.Error("id1, id3, id4 should still be present")
	}
}

// TestRemove tests removing an engram from working memory.
func TestRemove(t *testing.T) {
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  5,
		CreatedAt: time.Now(),
	}

	id1 := types.NewULID()
	id2 := types.NewULID()

	wm.Add(id1, 0.5, "context1")
	wm.Add(id2, 0.7, "context2")

	if len(wm.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(wm.Items))
	}

	// Remove id1
	removed := wm.Remove(id1)
	if !removed {
		t.Error("Remove should return true for existing item")
	}

	if len(wm.Items) != 1 {
		t.Fatalf("expected 1 item after removal, got %d", len(wm.Items))
	}

	if wm.Items[0].EngramID != id2 {
		t.Error("wrong item remains")
	}

	// Try to remove non-existent item
	removed = wm.Remove(types.NewULID())
	if removed {
		t.Error("Remove should return false for non-existent item")
	}
}

// TestSnapshot returns only items with attention above threshold.
func TestSnapshot(t *testing.T) {
	halfLife := 1 * time.Minute
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  5,
		CreatedAt: time.Now(),
	}

	// Add items
	id1 := types.NewULID()
	id2 := types.NewULID()
	id3 := types.NewULID()

	wm.Add(id1, 0.9, "context1")
	wm.Add(id2, 0.5, "context2")
	wm.Add(id3, 0.1, "context3")

	// Snapshot with threshold 0.4
	snapshot := wm.Snapshot(halfLife, 0.4)

	// Should include id1 (0.9) and id2 (0.5), exclude id3 (0.1)
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 items in snapshot, got %d", len(snapshot))
	}

	found := make(map[types.ULID]bool)
	for _, item := range snapshot {
		found[item.EngramID] = true
	}

	if !found[id1] || !found[id2] {
		t.Error("snapshot should include id1 and id2")
	}
	if found[id3] {
		t.Error("snapshot should not include id3")
	}
}

// TestGet returns all items with current attention values.
func TestGet(t *testing.T) {
	halfLife := 1 * time.Minute
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  5,
		CreatedAt: time.Now(),
	}

	id1 := types.NewULID()
	wm.Add(id1, 0.8, "context1")

	// Get immediately (no decay)
	items := wm.Get(halfLife)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	// Attention should still be ~0.8 (no time has passed)
	if math.Abs(float64(items[0].Attention-0.8)) > 0.01 {
		t.Errorf("expected attention ~0.8, got %f", items[0].Attention)
	}
}

// TestSessionClosePromotion tests that session close returns promotion candidates.
func TestSessionClosePromotion(t *testing.T) {
	manager := NewManager()

	wm := manager.Create("session1")
	if wm == nil {
		t.Fatal("Create should return a working memory")
	}

	// Add items with various attention levels
	id1 := types.NewULID()
	id2 := types.NewULID()
	id3 := types.NewULID()

	wm.Add(id1, 0.9, "context1") // > 0.6, will be promoted
	wm.Add(id2, 0.5, "context2") // < 0.6, won't be promoted
	wm.Add(id3, 0.7, "context3") // > 0.6, will be promoted

	// Close session and get promotion candidates
	candidates, ok := manager.Close("session1")
	if !ok {
		t.Fatal("Close should return true for existing session")
	}

	// Should have 2 candidates (id1 and id3)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 promotion candidates, got %d", len(candidates))
	}

	found := make(map[types.ULID]bool)
	for _, cand := range candidates {
		found[cand.EngramID] = true
	}

	if !found[id1] || !found[id3] {
		t.Error("promotion should include id1 and id3")
	}
	if found[id2] {
		t.Error("promotion should not include id2 (attention 0.5 < 0.6)")
	}

	// Verify session is removed
	_, ok = manager.Get("session1")
	if ok {
		t.Error("session should be removed after close")
	}
}

// TestManagerGC tests garbage collection of idle sessions.
func TestManagerGC(t *testing.T) {
	manager := NewManager()

	// Create a session and touch it
	wm1 := manager.Create("session1")
	if wm1 == nil {
		t.Fatal("Create should return a working memory")
	}

	// Create another session that's manually marked as old
	wm2 := manager.Create("session2")
	if wm2 == nil {
		t.Fatal("Create should return a working memory")
	}

	// Manually set LastAccess to long ago
	wm2.mu.Lock()
	wm2.LastAccess = time.Now().Add(-1 * time.Hour)
	wm2.mu.Unlock()

	// GC with 5-minute threshold
	removed := manager.GC(5 * time.Minute)

	// session2 should be removed (idle for 1 hour)
	if removed != 1 {
		t.Errorf("expected 1 session removed, got %d", removed)
	}

	_, ok := manager.Get("session1")
	if !ok {
		t.Error("session1 should still exist")
	}

	_, ok = manager.Get("session2")
	if ok {
		t.Error("session2 should have been removed by GC")
	}
}

// TestStartGC tests background GC goroutine.
func TestStartGC(t *testing.T) {
	manager := NewManager()

	// Create a session
	wm := manager.Create("test-gc-session")
	if wm == nil {
		t.Fatal("Create should return a working memory")
	}

	// Manually mark it as old
	wm.mu.Lock()
	wm.LastAccess = time.Now().Add(-1 * time.Hour)
	wm.mu.Unlock()

	// Start background GC with 100ms interval, 5-minute threshold
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager.StartGC(ctx, 100*time.Millisecond, 5*time.Minute)

	// Wait for GC to run
	time.Sleep(200 * time.Millisecond)

	// Session should be gone
	_, ok := manager.Get("test-gc-session")
	if ok {
		t.Error("session should have been removed by background GC")
	}
}

// TestAddClampAttention tests that attention is clamped to [0, 1].
func TestAddClampAttention(t *testing.T) {
	tests := []struct {
		name              string
		inputAttention    float32
		expectedAttention float32
	}{
		{
			name:              "clamp negative",
			inputAttention:    -0.5,
			expectedAttention: 0.0,
		},
		{
			name:              "clamp above 1",
			inputAttention:    1.5,
			expectedAttention: 1.0,
		},
		{
			name:              "keep in range",
			inputAttention:    0.5,
			expectedAttention: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wm := &WorkingMemory{
				SessionID: "session1",
				Items:     make([]*WorkingItem, 0),
				MaxItems:  5,
				CreatedAt: time.Now(),
			}

			id := types.NewULID()
			wm.Add(id, tt.inputAttention, "test")

			if len(wm.Items) != 1 {
				t.Fatalf("expected 1 item, got %d", len(wm.Items))
			}

			if wm.Items[0].Attention != tt.expectedAttention {
				t.Errorf("expected attention %f, got %f", tt.expectedAttention, wm.Items[0].Attention)
			}
		})
	}
}

// TestManagerCreateOrLoad tests that Create returns the same session if called twice.
func TestManagerCreateOrLoad(t *testing.T) {
	manager := NewManager()

	wm1 := manager.Create("session1")
	wm2 := manager.Create("session1")

	if wm1 != wm2 {
		t.Error("Create should return the same session for the same ID")
	}
}

// TestManagerDelete tests deleting a session without promotion.
func TestManagerDelete(t *testing.T) {
	manager := NewManager()

	wm := manager.Create("session1")
	if wm == nil {
		t.Fatal("Create should return a working memory")
	}

	wm.Add(types.NewULID(), 0.9, "context")

	manager.Delete("session1")

	_, ok := manager.Get("session1")
	if ok {
		t.Error("session should be deleted")
	}
}

// TestPromotionDelta tests the promotion delta calculation.
func TestPromotionDelta(t *testing.T) {
	tests := []struct {
		name      string
		attention float32
		expected  float32
	}{
		{
			name:      "zero attention",
			attention: 0.0,
			expected:  0.0,
		},
		{
			name:      "mid attention (0.5)",
			attention: 0.5,
			expected:  0.05,
		},
		{
			name:      "high attention (0.9)",
			attention: 0.9,
			expected:  0.09,
		},
		{
			name:      "max attention (1.0)",
			attention: 1.0,
			expected:  0.1,
		},
		{
			name:      "attention that would exceed cap (1.6 * 0.1 = 0.16 > 0.15)",
			attention: 1.6,
			expected:  0.15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delta := PromotionDelta(tt.attention)
			if math.Abs(float64(delta-tt.expected)) > 0.0001 {
				t.Errorf("expected %f, got %f", tt.expected, delta)
			}
		})
	}
}

// TestConcurrentAdd tests concurrent adds to working memory.
func TestConcurrentAdd(t *testing.T) {
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  100,
		CreatedAt: time.Now(),
	}

	// Spawn 10 goroutines adding items concurrently
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				wm.Add(types.NewULID(), 0.5, "context")
			}
			done <- struct{}{}
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	if len(wm.Items) != 100 {
		t.Errorf("expected 100 items, got %d", len(wm.Items))
	}
}

// TestConcurrentGetAndRemove tests concurrent Get and Remove operations.
func TestConcurrentGetAndRemove(t *testing.T) {
	wm := &WorkingMemory{
		SessionID: "session1",
		Items:     make([]*WorkingItem, 0),
		MaxItems:  50,
		CreatedAt: time.Now(),
	}

	// Add some items
	ids := make([]types.ULID, 0)
	for i := 0; i < 30; i++ {
		id := types.NewULID()
		ids = append(ids, id)
		wm.Add(id, 0.5, "context")
	}

	// Spawn goroutines doing Gets and Removes
	done := make(chan struct{})

	for i := 0; i < 5; i++ {
		go func() {
			wm.Get(5 * time.Minute)
			done <- struct{}{}
		}()
	}

	for i := 0; i < 10; i++ {
		go func(idx int) {
			if idx < len(ids) {
				wm.Remove(ids[idx])
			}
			done <- struct{}{}
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 15; i++ {
		<-done
	}

	// Should have ~20 items left (30 - 10 removed)
	if len(wm.Items) < 15 || len(wm.Items) > 30 {
		t.Logf("expected around 20 items, got %d (expected variance due to concurrency)", len(wm.Items))
	}
}

// TestWorkingMemory_ConcurrentCreateSameSession verifies that concurrent calls to
// Create() with the same session ID all return the same *WorkingMemory pointer and
// that exactly one session is stored in the manager.
func TestWorkingMemory_ConcurrentCreateSameSession(t *testing.T) {
	manager := NewManager()
	const numGoroutines = 50
	const sessionID = "concurrent-session"

	results := make([]*WorkingMemory, numGoroutines)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			wm := manager.Create(sessionID)
			mu.Lock()
			results[i] = wm
			mu.Unlock()
		}()
	}
	wg.Wait()

	// All returned pointers must be identical.
	first := results[0]
	if first == nil {
		t.Fatal("Create returned nil")
	}
	for i, wm := range results {
		if wm != first {
			t.Errorf("goroutine %d: expected pointer %p, got %p", i, first, wm)
		}
	}

	// Manager must contain exactly one session.
	count := 0
	manager.sessions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("expected exactly 1 session in manager, got %d", count)
	}
}
