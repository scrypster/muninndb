package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TimelineEntry represents a single point in an entity's timeline.
type TimelineEntry struct {
	EngramID  string    `json:"engram_id"`
	Concept   string    `json:"concept"`
	CreatedAt time.Time `json:"created_at"`
	Summary   string    `json:"summary"`
}

// EntityTimeline represents the complete timeline of an entity from first mention to now.
type EntityTimeline struct {
	Entity       string           `json:"entity"`
	FirstSeen    time.Time        `json:"first_seen"`
	MentionCount int              `json:"mention_count"`
	Entries      []TimelineEntry  `json:"timeline"`
	Count        int              `json:"count"`
}

// GetEntityTimeline returns a chronological view of when an entity first appeared
// in memory and how it has evolved. Scans the entity reverse index (0x23) to find
// all engrams mentioning the entity, then collects timeline entries sorted by
// creation time (oldest first). Results are capped at limit.
func (e *Engine) GetEntityTimeline(ctx context.Context, vault string, entityName string, limit int) (*EntityTimeline, error) {
	if entityName == "" {
		return nil, fmt.Errorf("entity_name is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Get the entity record to check if it exists and get FirstSeen + MentionCount.
	entityRecord, err := e.store.GetEntityRecord(ctx, entityName)
	if err != nil {
		return nil, fmt.Errorf("get entity record: %w", err)
	}
	if entityRecord == nil {
		return nil, fmt.Errorf("entity_name not found in entity registry")
	}

	// Resolve the vault prefix.
	ws := e.store.ResolveVaultPrefix(vault)

	// Scan all engrams mentioning this entity.
	var entries []TimelineEntry
	err = e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		if len(entries) >= limit {
			return fmt.Errorf("limit reached") // sentinel to stop scanning
		}

		// Fetch the engram to extract concept, created_at, and summary.
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			return nil // skip missing/deleted
		}

		// Skip soft-deleted and archived engrams.
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			return nil
		}

		entries = append(entries, TimelineEntry{
			EngramID:  id.String(),
			Concept:   eng.Concept,
			CreatedAt: eng.CreatedAt,
			Summary:   eng.Summary,
		})
		return nil
	})
	if err != nil && err.Error() != "limit reached" {
		return nil, fmt.Errorf("scan entity engrams: %w", err)
	}

	// Sort by CreatedAt ascending (oldest first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	// Convert FirstSeen from unix nanos to time.Time.
	firstSeen := time.Unix(0, entityRecord.FirstSeen).UTC()

	return &EntityTimeline{
		Entity:       entityName,
		FirstSeen:    firstSeen,
		MentionCount: int(entityRecord.MentionCount),
		Entries:      entries,
		Count:        len(entries),
	}, nil
}
