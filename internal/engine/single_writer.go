package engine

import (
	"context"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ErrNotLeader and NotLeaderError live in mbp — the package every transport
// already imports — so REST, gRPC, MBP and MCP can classify a refused write
// without importing the engine (which would cycle: engine imports mbp).
// They are re-exported here because the engine is what returns them.
var ErrNotLeader = mbp.ErrNotLeader

// NotLeaderError carries the leader hint; see mbp.NotLeaderError for the
// reject-vs-forward argument and the stated residual.
type NotLeaderError = mbp.NotLeaderError

// WriteGate reports whether this node may ORIGINATE a client write. It returns
// nil when the node is the Cortex and a *NotLeaderError otherwise.
//
// Standalone (non-cluster) servers install no gate at all: a nil gate means
// "there is no leader to be wrong about" and every write is allowed. That is
// fail-open, and correct — this is a cluster-topology check, not an auth check
// (principle #4: fail closed on auth, fail open on presentation; a node with no
// cluster has nothing to present the wrong answer about).
type WriteGate func() error

// SetWriteGate installs the cluster single-writer gate. cmd/muninn wires it to
// the ClusterCoordinator when, and only when, cluster mode is enabled.
func (e *Engine) SetWriteGate(g WriteGate) { e.writeGate = g }

// refuseNonLeaderWrite is the single chokepoint for the cluster single-writer
// guarantee at the engine layer. EVERY engine method that originates state —
// additive or destructive — calls it (via refuseWrite, or directly for the
// additive methods that have no append gate).
//
// It lives at the engine rather than in each transport for the same reason
// refuseAppend does: the guarantee then holds on MCP, REST, gRPC, MBP and the
// embedded library at once, and a new transport inherits it for free. Putting it
// in the storage layer was considered and rejected — the invariant being
// enforced is "a CLIENT-ORIGINATED write may only happen on the Cortex", and
// storage cannot tell a client write from the applier replaying the leader's.
func (e *Engine) refuseNonLeaderWrite() error {
	g := e.writeGate
	if g == nil {
		return nil
	}
	return g()
}

// refuseWrite is the combined gate for methods that modify or delete existing
// state: cluster single-writer first, then append-mode.
//
// Order matters and is deliberate. Both errors are true for an append-mode
// credential on a Lobe; the cluster one is the ACTIONABLE one (retry over
// there), while "your credential cannot do this" would send the operator to
// audit a key that is fine.
func (e *Engine) refuseWrite(ctx context.Context) error {
	if err := e.refuseNonLeaderWrite(); err != nil {
		return err
	}
	return e.refuseAppend(ctx)
}
