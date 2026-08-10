package mbp

import (
	"errors"
	"fmt"
)

// ErrNotLeader is the sentinel every transport matches with errors.Is to turn a
// refused write into its own "you are on the wrong node" response. The concrete
// error is always a *NotLeaderError, which carries the leader hint.
var ErrNotLeader = errors.New("this node is not the cluster's Cortex; writes are accepted only on the Cortex")

// NotLeaderError is returned when a client write reaches a node that is not the
// current Cortex. It carries where to go instead, so a caller can retry against
// the right node rather than guess.
//
// WHY REJECT AND NOT FORWARD (#596). The reporter asked for either; both close
// the silent-loss hole. Reject won on four counts:
//
//  1. Forwarding needs a node-to-node WRITE rpc that does not exist. The cluster
//     wire is one-directional by construction — join/heartbeat/vote/ack plus
//     leader→follower log shipping. There is no request/response write frame, no
//     correlation id, and no way to carry the ORIGINAL caller's credential to the
//     Cortex. A Lobe that forwarded under cluster authority would be a confused
//     deputy able to write into any vault. That is an RFC, not an increment.
//  2. Forwarding makes split-brain worse, not better. With two nodes each
//     believing they lead, rejection confines the damage to the node that is
//     wrong about itself; every node that correctly knows it follows refuses and
//     names a leader. Forwarding would have those correct followers actively pump
//     third-party traffic into whichever partition they believe in, amplifying
//     the divergence instead of bounding it.
//  3. Rejection introduces no wait. There is no new timeout, no new deadlock
//     edge, and no "forwarded and applied" vs "forwarded and lost" ambiguity to
//     resolve — the codebase has idempotency keys for client writes but nothing
//     equivalent on the cluster wire.
//  4. The precedent is in-tree (principle #7): HandleJoinRequest already rejects
//     a non-leader join with RejectReason "not cortex" plus CortexID/CortexAddr.
//     This is the same redirect shape, applied to client writes.
//
// RESIDUAL, stated rather than hidden: the gate is evaluated once, at the top of
// the operation. A node demoted between the check and the commit still commits
// that one write. Closing that needs the fencing token to be validated on the
// write path — ValidateFencingToken exists in internal/replication and has no
// production caller today. Tracked separately; the window is one operation wide
// and cannot grow with load, whereas the bug being fixed here was unbounded.
type NotLeaderError struct {
	// Role is this node's current cluster role ("lobe", "sentinel", "observer",
	// or "" while a role is being established).
	Role string
	// LeaderID / LeaderAddr are the Cortex this node believes in. Both may be
	// empty during an election — the write is still refused, because "we do not
	// know who leads" is precisely when accepting it would diverge.
	LeaderID   string
	LeaderAddr string
}

func (e *NotLeaderError) Error() string {
	role := e.Role
	if role == "" {
		role = "non-cortex"
	}
	switch {
	case e.LeaderID != "" && e.LeaderAddr != "":
		return fmt.Sprintf("%s (this node's role is %s); retry against the Cortex %q at %s",
			ErrNotLeader.Error(), role, e.LeaderID, e.LeaderAddr)
	case e.LeaderID != "":
		return fmt.Sprintf("%s (this node's role is %s); retry against the Cortex %q",
			ErrNotLeader.Error(), role, e.LeaderID)
	default:
		return fmt.Sprintf("%s (this node's role is %s); no Cortex is currently known — "+
			"the cluster may be mid-election, retry shortly", ErrNotLeader.Error(), role)
	}
}

// Is makes errors.Is(err, ErrNotLeader) true for every NotLeaderError.
func (e *NotLeaderError) Is(target error) bool { return target == ErrNotLeader }
