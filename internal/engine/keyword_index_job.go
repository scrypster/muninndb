package engine

import (
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// keywordIndexJob builds the keyword index job for a memory that has just been
// stored. Every path that stores a NEW memory uses it (Write, WriteBatch,
// Evolve, AddChild, RememberTree), so none of them can index fewer fields than
// another. They used to build the job by hand and drifted: Evolve left out the
// tags its new version inherits, and RememberTree submitted no job at all.
//
// A path that changes the indexed fields of a memory that already exists must
// go through fts.ReindexEngram instead (Engine.UpdateTags does). One path does
// not yet: the dream dedup pass merges tags into the surviving memory with a
// storage-level tag update, so those merged tags are not in the index.
func keywordIndexJob(ws [8]byte, id storage.ULID, eng *storage.Engram) fts.IndexJob {
	return fts.IndexJob{
		WS:        ws,
		ID:        [16]byte(id),
		Concept:   eng.Concept,
		CreatedBy: eng.CreatedBy,
		Content:   eng.Content,
		Tags:      eng.Tags,
	}
}
