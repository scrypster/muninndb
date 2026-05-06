package search

import (
	"context"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// WorkerIndexer adapts a search text backend to the existing FTS worker boundary.
type WorkerIndexer struct{ Backend TextIndexer }

// IndexEngram indexes a worker job through the configured text backend.
func (w WorkerIndexer) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string, createdAt int64) error {
	if w.Backend == nil {
		return nil
	}
	return w.Backend.IndexText(context.Background(), ws, &storage.Engram{
		ID:        storage.ULID(id),
		Concept:   concept,
		CreatedBy: createdBy,
		Content:   content,
		Tags:      tags,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
	})
}
