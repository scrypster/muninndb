package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Third security review: the per-(credential, memory, UTC day) access-bump
// decision must come from DURABLE state, so a process restart in the middle
// of a day cannot grant a second bump.

// TestFeedback_RestartMidDayCannotBumpTwice: label (bump), restart, label the
// same memory from the same credential through a fresh query id the same day
// — still one bump. Both forms.
func TestFeedback_RestartMidDayCannotBumpTwice(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	day := time.Now().UTC() // frozen at the real time: see TestFeedback_LegacyFormCappedPerDay
	clock := func() time.Time { return day }
	le.eng.labelNow = clock
	ctx := caller("tok-a", "sess-a")
	const vault = "restart-bump"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem, legacyMem := resp.Activations[0].ID, resp.Activations[1].ID
	le.label(ctx, vault, resp.QueryID, true, mem)
	require.NoError(t, le.eng.RecordFeedback(ctx, vault, legacyMem, true))
	require.Equal(t, uint32(1), le.accessCount(vault, mem))
	require.Equal(t, uint32(1), le.accessCount(vault, legacyMem))

	le.reopen(testSecret)
	le.eng.labelNow = clock
	le.label(ctx, vault, le.recall(ctx, vault, false).QueryID, true, mem)
	le.label(ctx, vault, le.recall(ctx, vault, true).QueryID, true, mem)
	require.NoError(t, le.eng.RecordFeedback(ctx, vault, legacyMem, true))
	require.NoError(t, le.eng.RecordFeedback(ctx, vault, mem, true))
	require.Equal(t, uint32(1), le.accessCount(vault, mem), "a restart must not reset the day's bump cap")
	require.Equal(t, uint32(1), le.accessCount(vault, legacyMem))

	// The next UTC day allows one more.
	day = day.Add(24 * time.Hour)
	le.label(ctx, vault, le.recall(ctx, vault, false).QueryID, true, mem)
	require.Equal(t, uint32(2), le.accessCount(vault, mem))
}
