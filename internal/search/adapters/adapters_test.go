package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/search/adapters"
)

type stubTextSearcher struct {
	hits []search.Hit
	err  error
}

func (s stubTextSearcher) SearchText(context.Context, [8]byte, string, int) ([]search.Hit, error) {
	return s.hits, s.err
}

type stubVectorSearcher struct {
	hits []search.Hit
	err  error
}

func (s stubVectorSearcher) SearchVector(context.Context, [8]byte, []float32, search.VectorSearchOptions) ([]search.Hit, error) {
	return s.hits, s.err
}

func TestActivationAdaptersConvertHits(t *testing.T) {
	ctx := context.Background()
	id := [16]byte{1, 2, 3}
	fts := adapters.ActivationFTS{B: stubTextSearcher{hits: []search.Hit{{ID: id, Score: 1.5}}}}
	vec := adapters.ActivationVector{B: stubVectorSearcher{hits: []search.Hit{{ID: id, Score: 2.5}}}}

	textHits, err := fts.Search(ctx, [8]byte{}, "query", 1)
	if err != nil {
		t.Fatalf("ActivationFTS.Search: %v", err)
	}
	if len(textHits) != 1 || [16]byte(textHits[0].ID) != id || textHits[0].Score != 1.5 {
		t.Fatalf("ActivationFTS hits = %#v", textHits)
	}
	vectorHits, err := vec.Search(ctx, [8]byte{}, []float32{1}, 1, nil)
	if err != nil {
		t.Fatalf("ActivationVector.Search: %v", err)
	}
	if len(vectorHits) != 1 || [16]byte(vectorHits[0].ID) != id || vectorHits[0].Score != 2.5 {
		t.Fatalf("ActivationVector hits = %#v", vectorHits)
	}
}

func TestTriggerAdaptersConvertHits(t *testing.T) {
	ctx := context.Background()
	id := [16]byte{4, 5, 6}
	fts := adapters.TriggerFTS{B: stubTextSearcher{hits: []search.Hit{{ID: id, Score: 3.5}}}}
	vec := adapters.TriggerVector{B: stubVectorSearcher{hits: []search.Hit{{ID: id, Score: 4.5}}}}

	textHits, err := fts.Search(ctx, [8]byte{}, "query", 1)
	if err != nil {
		t.Fatalf("TriggerFTS.Search: %v", err)
	}
	if len(textHits) != 1 || [16]byte(textHits[0].ID) != id || textHits[0].Score != 3.5 {
		t.Fatalf("TriggerFTS hits = %#v", textHits)
	}
	vectorHits, err := vec.Search(ctx, [8]byte{}, []float32{1}, 1)
	if err != nil {
		t.Fatalf("TriggerVector.Search: %v", err)
	}
	if len(vectorHits) != 1 || [16]byte(vectorHits[0].ID) != id || vectorHits[0].Score != 4.5 {
		t.Fatalf("TriggerVector hits = %#v", vectorHits)
	}
}

func TestAdaptersNilBackendReturnNoHits(t *testing.T) {
	ctx := context.Background()
	if hits, err := (adapters.ActivationFTS{}).Search(ctx, [8]byte{}, "query", 1); err != nil || len(hits) != 0 {
		t.Fatalf("ActivationFTS nil backend hits=%#v err=%v, want no hits nil error", hits, err)
	}
	if hits, err := (adapters.ActivationVector{}).Search(ctx, [8]byte{}, []float32{1}, 1, nil); err != nil || len(hits) != 0 {
		t.Fatalf("ActivationVector nil backend hits=%#v err=%v, want no hits nil error", hits, err)
	}
	if hits, err := (adapters.TriggerFTS{}).Search(ctx, [8]byte{}, "query", 1); err != nil || len(hits) != 0 {
		t.Fatalf("TriggerFTS nil backend hits=%#v err=%v, want no hits nil error", hits, err)
	}
	if hits, err := (adapters.TriggerVector{}).Search(ctx, [8]byte{}, []float32{1}, 1); err != nil || len(hits) != 0 {
		t.Fatalf("TriggerVector nil backend hits=%#v err=%v, want no hits nil error", hits, err)
	}
}

func TestAdaptersPropagateBackendErrors(t *testing.T) {
	wantErr := errors.New("backend failed")
	ctx := context.Background()
	if _, err := (adapters.ActivationFTS{B: stubTextSearcher{err: wantErr}}).Search(ctx, [8]byte{}, "query", 1); !errors.Is(err, wantErr) {
		t.Fatalf("ActivationFTS error = %v, want %v", err, wantErr)
	}
	if _, err := (adapters.ActivationVector{B: stubVectorSearcher{err: wantErr}}).Search(ctx, [8]byte{}, []float32{1}, 1, nil); !errors.Is(err, wantErr) {
		t.Fatalf("ActivationVector error = %v, want %v", err, wantErr)
	}
	if _, err := (adapters.TriggerFTS{B: stubTextSearcher{err: wantErr}}).Search(ctx, [8]byte{}, "query", 1); !errors.Is(err, wantErr) {
		t.Fatalf("TriggerFTS error = %v, want %v", err, wantErr)
	}
	if _, err := (adapters.TriggerVector{B: stubVectorSearcher{err: wantErr}}).Search(ctx, [8]byte{}, []float32{1}, 1); !errors.Is(err, wantErr) {
		t.Fatalf("TriggerVector error = %v, want %v", err, wantErr)
	}
}
