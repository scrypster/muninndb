package replication

// IsFollower reports whether this node is DEFINITIVELY a follower — a Lobe
// (RoleReplica) or an Observer. RolePrimary, RoleSentinel and RoleUnknown all
// report false.
//
// The asymmetry is deliberate. Callers use this to SUPPRESS work that only a
// leader owes anyone, so an uncertain role must behave like a leader: doing
// leader work on a follower wastes resources, while skipping leader work on a
// node that turns out to be the leader loses data. RoleUnknown is the startup
// window before this node has joined or been elected.
func (c *ClusterCoordinator) IsFollower() bool {
	switch c.Role() {
	case RoleReplica, RoleObserver:
		return true
	default:
		return false
	}
}

// LocalAppendFunc builds the storage-layer RepLogAppend callback for this node.
//
// #826: the callback used to be wired unconditionally on every cluster node, so
// a Lobe appended a full-size replication entry — the whole key and value of the
// write — for every one of its OWN local writes: recall's last-access touches,
// Hebbian weight updates, decay, dream. Nothing on a Lobe ever reads that log
// (only a Cortex serves a stream) and nothing prunes it (the periodic prune is
// leader-gated), so it grew forever. Measured on a production lobe: 152,866
// entries, more than the Cortex it followed.
//
// The gate is at the SOURCE rather than in the pruner because a suppressed
// append costs nothing, while a prune has to decode and delete what should
// never have been written.
//
// Suppression is fail-open by construction (see IsFollower): only a node that
// has positively established itself as a Lobe or Observer stops appending. A
// node in RoleUnknown — the startup window, before election or join completes —
// still appends, so a leader serving writes before its coordinator settles can
// never silently drop them out of the stream its followers read.
//
// A promoted follower resumes appending the moment its role changes; its own
// followers rejoin by snapshot, which is already how a role change is healed.
//
// coordinator may be nil (cluster mode with no coordinator), in which case
// every append proceeds.
func LocalAppendFunc(coordinator *ClusterCoordinator, log *ReplicationLog) func(op uint8, key, value []byte) error {
	return func(op uint8, key, value []byte) error {
		if coordinator != nil && coordinator.IsFollower() {
			return nil
		}
		_, err := log.Append(WALOp(op), key, value)
		return err
	}
}
