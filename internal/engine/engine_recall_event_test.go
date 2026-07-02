package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestRecall_PersistsRecallEvent verifies that a recall persists its
// surfaced set keyed by the event ULID returned as the response query_id
// (issue #573).
func TestRecall_PersistsRecallEvent(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "recall-event-persist"
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database",
		Content: "PostgreSQL is the primary relational database for transactional workloads",
	})
	require.NoError(t, err)
	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"primary relational database"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Activations, "test needs at least one surfaced engram")

	// The response query_id must be the persisted event's ULID.
	eventID, err := storage.ParseULID(resp.QueryID)
	require.NoError(t, err, "query_id must be a ULID when the recall event was persisted, got %q", resp.QueryID)

	ws := eng.store.ResolveVaultPrefix(vault)
	ev, err := eng.store.GetRecallEvent(ctx, ws, eventID, storage.RecallPurposeCalibration)
	require.NoError(t, err)
	require.NotNil(t, ev, "recall event must be persisted")

	require.Equal(t, []string{"primary relational database"}, ev.Context)
	require.Len(t, ev.Entries, len(resp.Activations))
	for i, item := range resp.Activations {
		require.Equal(t, item.ID, ev.Entries[i].ID, "entry %d ID must match the surfaced activation", i)
		require.InDelta(t, item.Score, ev.Entries[i].Score, 1e-6, "entry %d score must match", i)
	}
}

// TestRecall_ObserveModeDoesNotPersist verifies that observe-mode recalls —
// pure reads — write no recall event. The calibration harness reads in
// observe mode precisely so it cannot poison the signal it is fitting.
func TestRecall_ObserveModeDoesNotPersist(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "recall-event-observe"
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database",
		Content: "PostgreSQL is the primary relational database for transactional workloads",
	})
	require.NoError(t, err)
	awaitFTS(t, eng)

	observeCtx := context.WithValue(ctx, auth.ContextMode, auth.ModeObserve)
	resp, err := eng.Activate(observeCtx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"primary relational database"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Activations)
	require.True(t, strings.HasPrefix(resp.QueryID, "q-"),
		"observe-mode query_id must stay the ephemeral counter, got %q", resp.QueryID)

	ws := eng.store.ResolveVaultPrefix(vault)
	count := 0
	require.NoError(t, eng.store.ScanRecallEvents(ctx, ws, storage.RecallPurposeCalibration, func(_ storage.ULID, _ *storage.RecallEvent) error {
		count++
		return nil
	}))
	require.Zero(t, count, "observe mode must persist no recall events")
}

// TestRecall_NoResultsNoEvent verifies that a recall surfacing nothing
// persists nothing.
func TestRecall_NoResultsNoEvent(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "recall-event-empty"
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"nothing matches this"},
		MaxResults: 10,
		Threshold:  0.99,
	})
	require.NoError(t, err)
	require.Empty(t, resp.Activations)
	require.True(t, strings.HasPrefix(resp.QueryID, "q-"),
		"empty recalls keep the ephemeral query_id, got %q", resp.QueryID)

	ws := eng.store.ResolveVaultPrefix(vault)
	count := 0
	require.NoError(t, eng.store.ScanRecallEvents(ctx, ws, storage.RecallPurposeCalibration, func(_ storage.ULID, _ *storage.RecallEvent) error {
		count++
		return nil
	}))
	require.Zero(t, count)
}
