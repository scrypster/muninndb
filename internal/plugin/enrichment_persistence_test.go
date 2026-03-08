package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPersistEnrichmentResult_NilResult(t *testing.T) {
	store := &mockPluginStore{}

	err := PersistEnrichmentResult(context.Background(), store, ULID{}, nil)
	if err == nil {
		t.Fatal("expected error for nil enrichment result")
	}
	if !strings.Contains(err.Error(), "nil result") {
		t.Fatalf("expected nil result error, got %v", err)
	}
}

func TestPersistEnrichmentResult_EntityFailureDoesNotSetDigestEntities(t *testing.T) {
	store := &mockPluginStore{
		linkErr: errors.New("link fail"),
	}
	result := &EnrichmentResult{
		Summary: "summary",
		Entities: []ExtractedEntity{
			{Name: "postgres", Type: "database", Confidence: 0.9},
		},
	}

	err := PersistEnrichmentResult(context.Background(), store, ULID{}, result)
	if err == nil {
		t.Fatal("expected entity persistence error")
	}
	if !strings.Contains(err.Error(), `link entity "postgres"`) {
		t.Fatalf("expected link failure in error, got %v", err)
	}
	if hasSetFlag(store, DigestEntities) {
		t.Fatalf("DigestEntities flag should not be set after entity persistence failure; flags=%v", store.setFlags)
	}
}

func TestPersistEnrichmentResult_CoOccurrenceFailureDoesNotSetDigestEntities(t *testing.T) {
	store := &mockPluginStore{
		coOccurErr: errors.New("co-occurrence fail"),
	}
	result := &EnrichmentResult{
		Entities: []ExtractedEntity{
			{Name: "postgres", Type: "database", Confidence: 0.9},
			{Name: "redis", Type: "database", Confidence: 0.8},
		},
	}

	err := PersistEnrichmentResult(context.Background(), store, ULID{}, result)
	if err == nil {
		t.Fatal("expected co-occurrence persistence error")
	}
	if !strings.Contains(err.Error(), `increment entity co-occurrence "postgres"/"redis"`) {
		t.Fatalf("expected co-occurrence failure in error, got %v", err)
	}
	if hasSetFlag(store, DigestEntities) {
		t.Fatalf("DigestEntities flag should not be set after co-occurrence failure; flags=%v", store.setFlags)
	}
}

func TestPersistEnrichmentResult_RelationshipFailureDoesNotSetDigestRelationships(t *testing.T) {
	store := &mockPluginStore{
		upsertRelErr: errors.New("relationship fail"),
	}
	result := &EnrichmentResult{
		Relationships: []ExtractedRelation{
			{FromEntity: "postgres", ToEntity: "redis", RelType: "depends_on", Weight: 0.8},
		},
	}

	err := PersistEnrichmentResult(context.Background(), store, ULID{}, result)
	if err == nil {
		t.Fatal("expected relationship persistence error")
	}
	if !strings.Contains(err.Error(), `upsert relationship "postgres"->"redis" (depends_on)`) {
		t.Fatalf("expected relationship failure in error, got %v", err)
	}
	if hasSetFlag(store, DigestRelationships) {
		t.Fatalf("DigestRelationships flag should not be set after relationship persistence failure; flags=%v", store.setFlags)
	}
}

func TestPersistEnrichmentResult_EmptyEntityStageMarksEntityAndRelationshipFlags(t *testing.T) {
	store := &mockPluginStore{}
	result := &EnrichmentResult{
		Entities: []ExtractedEntity{},
	}

	if err := PersistEnrichmentResult(context.Background(), store, ULID{}, result); err != nil {
		t.Fatalf("PersistEnrichmentResult: %v", err)
	}
	if !hasSetFlag(store, DigestEntities) {
		t.Fatalf("expected DigestEntities flag, got %v", store.setFlags)
	}
	if !hasSetFlag(store, DigestRelationships) {
		t.Fatalf("expected DigestRelationships flag when entity extraction found no entities, got %v", store.setFlags)
	}
}

func hasSetFlag(store *mockPluginStore, flag uint8) bool {
	for _, got := range store.setFlags {
		if got == flag {
			return true
		}
	}
	return false
}
