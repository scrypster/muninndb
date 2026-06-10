package cognitive

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// mockEngineStore is a test double that implements storage.EngineStore.
// Only GetMetadata is implemented; all other methods panic to catch unexpected calls.
type mockEngineStore struct {
	storage.EngineStore // embed to satisfy the interface with panics for unimplemented methods
	metadataResult      []*storage.EngramMeta
	metadataErr         error
}

func (m *mockEngineStore) GetMetadata(ctx context.Context, ws [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error) {
	return m.metadataResult, m.metadataErr
}

func TestDecayStoreAdapterGetMetadataBatchNilEntries(t *testing.T) {
	// Arrange: some non-nil and some nil entries
	now := time.Now()
	result := []*storage.EngramMeta{
		{
			ID:          storage.ULID{1},
			LastAccess:  now,
			AccessCount: 5,
			Stability:   14.0,
			Relevance:   0.8,
		},
		nil, // nil entry — should produce zero DecayMeta
		{
			ID:          storage.ULID{3},
			LastAccess:  now,
			AccessCount: 2,
			Stability:   7.0,
			Relevance:   0.5,
		},
	}

	mock := &mockEngineStore{metadataResult: result}
	adapter := NewDecayStoreAdapter(mock)

	ids := [][16]byte{{1}, {2}, {3}}
	metas, err := adapter.GetMetadataBatch(context.Background(), [8]byte{}, ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("expected 3 metas, got %d", len(metas))
	}

	// First entry should be populated
	if metas[0].ID != [16]byte{1} {
		t.Errorf("metas[0].ID = %v, want {1}", metas[0].ID)
	}
	if metas[0].Relevance != 0.8 {
		t.Errorf("metas[0].Relevance = %f, want 0.8", metas[0].Relevance)
	}

	// Second entry (nil input) should be zero value
	var zero DecayMeta
	if metas[1] != zero {
		t.Errorf("metas[1] should be zero DecayMeta, got %+v", metas[1])
	}

	// Third entry should be populated
	if metas[2].ID != [16]byte{3} {
		t.Errorf("metas[2].ID = %v, want {3}", metas[2].ID)
	}
}

func TestNewHebbianStoreAdapterNotNil(t *testing.T) {
	mock := &mockEngineStore{}
	adapter := NewHebbianStoreAdapter(mock)
	if adapter == nil {
		t.Fatal("NewHebbianStoreAdapter returned nil")
	}
}

func TestNewDecayStoreAdapterNotNil(t *testing.T) {
	mock := &mockEngineStore{}
	adapter := NewDecayStoreAdapter(mock)
	if adapter == nil {
		t.Fatal("NewDecayStoreAdapter returned nil")
	}
}

func TestNewContradictStoreAdapterNotNil(t *testing.T) {
	mock := &mockEngineStore{}
	adapter := NewContradictStoreAdapter(mock)
	if adapter == nil {
		t.Fatal("NewContradictStoreAdapter returned nil")
	}
}

func TestNewConfidenceStoreAdapterNotNil(t *testing.T) {
	mock := &mockEngineStore{}
	adapter := NewConfidenceStoreAdapter(mock)
	if adapter == nil {
		t.Fatal("NewConfidenceStoreAdapter returned nil")
	}
}
