package activation_test

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestActivationLogRecord verifies that a recorded entry is returned by RecentForVault.
func TestActivationLogRecord(t *testing.T) {
	log := &activation.ActivationLog{}

	id := storage.NewULID()
	now := time.Now()
	log.Record(activation.LogEntry{
		VaultID:   42,
		At:        now,
		EngramIDs: []storage.ULID{id},
		Scores:    []float64{0.75},
	})

	entries := log.RecentForVault(42, 10)
	if len(entries) != 1 {
		t.Fatalf("RecentForVault: got %d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.VaultID != 42 {
		t.Errorf("VaultID = %d, want 42", e.VaultID)
	}
	if !e.At.Equal(now) {
		t.Errorf("At = %v, want %v", e.At, now)
	}
	if len(e.EngramIDs) != 1 || e.EngramIDs[0] != id {
		t.Errorf("EngramIDs = %v, want [%v]", e.EngramIDs, id)
	}
	if len(e.Scores) != 1 || e.Scores[0] != 0.75 {
		t.Errorf("Scores = %v, want [0.75]", e.Scores)
	}
}

// TestActivationLogRecentForVault verifies that entries for vault A do not
// appear when querying vault B.
func TestActivationLogRecentForVault(t *testing.T) {
	log := &activation.ActivationLog{}

	idA := storage.NewULID()
	idB := storage.NewULID()
	base := time.Now()

	log.Record(activation.LogEntry{VaultID: 1, At: base, EngramIDs: []storage.ULID{idA}})
	log.Record(activation.LogEntry{VaultID: 2, At: base.Add(time.Second), EngramIDs: []storage.ULID{idB}})

	// Vault 1 should only see idA.
	vault1 := log.RecentForVault(1, 10)
	if len(vault1) != 1 {
		t.Fatalf("vault 1: got %d entries, want 1", len(vault1))
	}
	if vault1[0].EngramIDs[0] != idA {
		t.Errorf("vault 1 entry ID = %v, want %v", vault1[0].EngramIDs[0], idA)
	}

	// Vault 2 should only see idB.
	vault2 := log.RecentForVault(2, 10)
	if len(vault2) != 1 {
		t.Fatalf("vault 2: got %d entries, want 1", len(vault2))
	}
	if vault2[0].EngramIDs[0] != idB {
		t.Errorf("vault 2 entry ID = %v, want %v", vault2[0].EngramIDs[0], idB)
	}

	// An unknown vault returns nil without panic.
	unknown := log.RecentForVault(999, 10)
	if unknown != nil {
		t.Errorf("unknown vault: got %v, want nil", unknown)
	}
}

// TestActivationLogCapacity verifies that recording more than vaultLogCap
// entries does not panic and that the ring buffer retains exactly vaultLogCap
// entries (the most recent ones, newest first).
func TestActivationLogCapacity(t *testing.T) {
	log := &activation.ActivationLog{}

	const cap = 1000 // must match vaultLogCap in log.go
	const writes = cap + 200

	base := time.Now()
	for i := 0; i < writes; i++ {
		log.Record(activation.LogEntry{
			VaultID: 7,
			At:      base.Add(time.Duration(i) * time.Millisecond),
		})
	}

	entries := log.RecentForVault(7, writes) // ask for more than recorded
	if len(entries) != cap {
		t.Errorf("after %d writes: got %d entries, want %d (ring cap)", writes, len(entries), cap)
	}

	// Newest entry should be the last one written (index writes-1).
	want := base.Add(time.Duration(writes-1) * time.Millisecond)
	if !entries[0].At.Equal(want) {
		t.Errorf("newest entry At = %v, want %v", entries[0].At, want)
	}

	// Oldest retained entry should be write number (writes - cap).
	wantOldest := base.Add(time.Duration(writes-cap) * time.Millisecond)
	if !entries[cap-1].At.Equal(wantOldest) {
		t.Errorf("oldest entry At = %v, want %v", entries[cap-1].At, wantOldest)
	}
}
