package episodic

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

// EpisodicStore defines the interface for episodic memory operations.
type EpisodicStore interface {
	// CreateEpisode creates a new open episode.
	CreateEpisode(ctx context.Context, ws [8]byte, title string) (*Episode, error)

	// AppendFrame adds a frame to an episode, atomically incrementing FrameCount.
	AppendFrame(ctx context.Context, ws [8]byte, episodeID types.ULID, engramID types.ULID, note string) (*EpisodeFrame, error)

	// CloseEpisode closes an episode (sets ClosedAt if not already closed).
	CloseEpisode(ctx context.Context, ws [8]byte, episodeID types.ULID) error

	// GetEpisode retrieves an episode by ID.
	GetEpisode(ctx context.Context, ws [8]byte, episodeID types.ULID) (*Episode, error)

	// ListFrames retrieves all frames for an episode in position order.
	ListFrames(ctx context.Context, ws [8]byte, episodeID types.ULID) ([]*EpisodeFrame, error)

	// ListEpisodes retrieves the most recent N episodes for a vault (open or closed).
	ListEpisodes(ctx context.Context, ws [8]byte, limit int) ([]*Episode, error)
}

// PebbleEpisodicStore is the Pebble-backed implementation of EpisodicStore.
type PebbleEpisodicStore struct {
	db        *pebble.DB
	appendMu  sync.Mutex // serializes AppendFrame to prevent frame position collisions
}

// NewPebbleEpisodicStore creates a new episodic store backed by Pebble.
func NewPebbleEpisodicStore(db *pebble.DB) *PebbleEpisodicStore {
	return &PebbleEpisodicStore{db: db}
}

// CreateEpisode creates a new open episode with the given title.
func (s *PebbleEpisodicStore) CreateEpisode(ctx context.Context, ws [8]byte, title string) (*Episode, error) {
	ep := &Episode{
		ID:        types.NewULID(),
		Title:     title,
		CreatedAt: time.Now().UTC(),
		ClosedAt:  nil,
		FrameCount: 0,
	}

	data, err := json.Marshal(ep)
	if err != nil {
		return nil, fmt.Errorf("marshal episode: %w", err)
	}

	key := keys.EpisodeKey(ws, ep.ID)
	if err := s.db.Set(key, data, pebble.NoSync); err != nil {
		return nil, fmt.Errorf("set episode key: %w", err)
	}

	return ep, nil
}

// AppendFrame adds a frame to an episode and increments FrameCount.
// The read-modify-write of FrameCount is serialized with appendMu to prevent
// two concurrent callers from writing to the same frame position.
func (s *PebbleEpisodicStore) AppendFrame(ctx context.Context, ws [8]byte, episodeID types.ULID, engramID types.ULID, note string) (*EpisodeFrame, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	// Retrieve the episode to get the next position
	ep, err := s.GetEpisode(ctx, ws, episodeID)
	if err != nil {
		return nil, fmt.Errorf("get episode: %w", err)
	}
	if ep == nil {
		return nil, fmt.Errorf("episode not found")
	}

	// Check for frame count overflow
	if ep.FrameCount >= math.MaxUint32 {
		return nil, fmt.Errorf("episodic: episode frame count overflow (max %d)", math.MaxUint32)
	}

	// Create the frame
	frame := &EpisodeFrame{
		EpisodeID: episodeID,
		Position:  uint32(ep.FrameCount),
		EngramID:  engramID,
		AddedAt:   time.Now().UTC(),
		Note:      note,
	}

	// Marshal and store the frame
	frameData, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("marshal frame: %w", err)
	}

	// Increment episode FrameCount and update
	ep.FrameCount++
	epData, err := json.Marshal(ep)
	if err != nil {
		return nil, fmt.Errorf("marshal updated episode: %w", err)
	}

	frameKey := keys.EpisodeFrameKey(ws, episodeID, frame.Position)
	epKey := keys.EpisodeKey(ws, episodeID)

	// Atomically commit frame write and FrameCount update so a crash between
	// the two writes cannot leave FrameCount stale (which would cause duplicate
	// frame positions on the next AppendFrame call).
	batch := s.db.NewBatch()
	if err := batch.Set(frameKey, frameData, nil); err != nil {
		batch.Close()
		return nil, fmt.Errorf("episodic: frame write: %w", err)
	}
	if err := batch.Set(epKey, epData, nil); err != nil {
		batch.Close()
		return nil, fmt.Errorf("episodic: episode update: %w", err)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		batch.Close()
		return nil, fmt.Errorf("episodic: commit: %w", err)
	}
	batch.Close()

	return frame, nil
}

// CloseEpisode closes an episode by setting ClosedAt to the current time.
// Acquires appendMu to serialize the read-modify-write against concurrent AppendFrame calls.
func (s *PebbleEpisodicStore) CloseEpisode(ctx context.Context, ws [8]byte, episodeID types.ULID) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	ep, err := s.GetEpisode(ctx, ws, episodeID)
	if err != nil {
		return fmt.Errorf("get episode: %w", err)
	}
	if ep == nil {
		return fmt.Errorf("episode not found")
	}

	if ep.ClosedAt != nil {
		// Already closed; idempotent
		return nil
	}

	now := time.Now().UTC()
	ep.ClosedAt = &now

	data, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("marshal episode: %w", err)
	}

	key := keys.EpisodeKey(ws, episodeID)
	if err := s.db.Set(key, data, pebble.NoSync); err != nil {
		return fmt.Errorf("set episode key: %w", err)
	}

	return nil
}

// GetEpisode retrieves an episode by ID, or returns nil if not found.
func (s *PebbleEpisodicStore) GetEpisode(ctx context.Context, ws [8]byte, episodeID types.ULID) (*Episode, error) {
	key := keys.EpisodeKey(ws, episodeID)
	val, closer, err := s.db.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("get key: %w", err)
	}
	defer closer.Close()

	result := make([]byte, len(val))
	copy(result, val)

	var ep Episode
	if err := json.Unmarshal(result, &ep); err != nil {
		return nil, fmt.Errorf("unmarshal episode: %w", err)
	}

	return &ep, nil
}

// ListFrames retrieves all frames for an episode in position order.
func (s *PebbleEpisodicStore) ListFrames(ctx context.Context, ws [8]byte, episodeID types.ULID) ([]*EpisodeFrame, error) {
	// Construct the prefix for all frames in this episode
	// Prefix: 0x1A | ws(8) | episode_id(16) | 0xFF
	prefix := make([]byte, 1+8+16+1)
	prefix[0] = 0x1A
	copy(prefix[1:9], ws[:])
	copy(prefix[9:25], episodeID[:])
	prefix[25] = 0xFF

	// Calculate upper bound (next byte after 0xFF, which is 0x100 but we'll wrap)
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xFF {
			upper[i]++
			break
		}
	}

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("create iterator: %w", err)
	}
	defer iter.Close()

	var frames []*EpisodeFrame
	for valid := iter.First(); valid; valid = iter.Next() {
		val := iter.Value()
		result := make([]byte, len(val))
		copy(result, val)

		var frame EpisodeFrame
		if err := json.Unmarshal(result, &frame); err != nil {
			return nil, fmt.Errorf("unmarshal frame: %w", err)
		}
		frames = append(frames, &frame)
	}

	return frames, nil
}

// ListEpisodes retrieves the most recent N episodes for a vault.
// Since episodes are stored with ULID keys (sorted by timestamp), we scan
// the 0x15 | ws | episode_id prefix and return the N most recent (last N scanned in reverse).
func (s *PebbleEpisodicStore) ListEpisodes(ctx context.Context, ws [8]byte, limit int) ([]*Episode, error) {
	// Prefix for all episodes in this vault: 0x1A | ws(8)
	prefix := make([]byte, 1+8)
	prefix[0] = 0x1A
	copy(prefix[1:9], ws[:])

	// Calculate upper bound
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xFF {
			upper[i]++
			break
		}
	}

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("create iterator: %w", err)
	}
	defer iter.Close()

	// Collect episodes (skip frame keys which contain 0xFF at byte 25)
	var episodes []*Episode
	for valid := iter.Last(); valid && len(episodes) < limit; valid = iter.Prev() {
		key := iter.Key()
		// Skip frame keys: if byte 25 is 0xFF, it's a frame key
		if len(key) >= 26 && key[25] == 0xFF {
			continue
		}

		val := iter.Value()
		result := make([]byte, len(val))
		copy(result, val)

		var ep Episode
		if err := json.Unmarshal(result, &ep); err != nil {
			return nil, fmt.Errorf("unmarshal episode: %w", err)
		}
		episodes = append(episodes, &ep)
	}

	// Reverse to get chronological order (most recent last)
	for i := 0; i < len(episodes)/2; i++ {
		j := len(episodes) - 1 - i
		episodes[i], episodes[j] = episodes[j], episodes[i]
	}

	return episodes, nil
}
