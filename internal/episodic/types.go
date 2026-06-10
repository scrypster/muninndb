package episodic

import (
	"time"

	"github.com/scrypster/muninndb/internal/types"
)

// Episode represents a durable ordered episode — a sequence of engram activations
// grouped into a temporal narrative.
type Episode struct {
	ID         types.ULID `json:"id"`
	Title      string     `json:"title"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at"`   // nil = open episode
	FrameCount int        `json:"frame_count"` // monotonic frame count
}

// EpisodeFrame represents a single frame (engram activation) within an episode.
type EpisodeFrame struct {
	EpisodeID types.ULID `json:"episode_id"`
	Position  uint32     `json:"position"` // monotonic, 0-indexed
	EngramID  types.ULID `json:"engram_id"`
	AddedAt   time.Time  `json:"added_at"`
	Note      string     `json:"note"`
}
