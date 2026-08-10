package auth

import (
	"log/slog"

	"github.com/cockroachdb/pebble"
)

// Replication op codes. These mirror internal/replication.WALOp; auth cannot
// import that package (it would cycle), and the values are part of the wire
// format, so they are pinned here and asserted against the replication package
// by a test in cmd/muninn.
const (
	opSet    uint8 = 1
	opDelete uint8 = 2
	opBatch  uint8 = 3
)

// clusterHooks carries the two cluster seams the auth store needs. Both are nil
// on a standalone server, which is the whole of the non-cluster behaviour: no
// gate, no replication, unchanged.
type clusterHooks struct {
	// replicate appends a committed write to the cluster replication log. Wired
	// on the Cortex (and harmlessly on every node, since a follower's own log is
	// never streamed anywhere).
	replicate func(op uint8, key, value []byte) error
	// writeGate returns a non-nil error when this node may not originate writes.
	writeGate func() error
}

// SetReplicator wires the cluster replication log so that CONFIGURATION —
// vault configs (which carry per-vault plasticity), API keys, capability
// tokens, admin records — travels the same replication path as engrams.
//
// Before this, every auth write went straight to Pebble via s.db.Set /
// batch.Commit, bypassing PebbleStore and therefore RepLogAppend entirely. A
// vault created or a key minted on the Cortex reached a Lobe only if that Lobe
// happened to (re)join afterwards and take a full snapshot — the snapshot
// iterates the whole DB, so it always carried config; the incremental stream
// never did. That is the reported "cluster replicates engrams but not
// configuration", and its sharp edge: after a failover the new Cortex serves
// the OLD defaults (#596 issue 2, #631 claim 2).
//
// Exposure note: this ships API-key STORAGE HASHES and admin bcrypt hashes over
// the cluster wire — which is TLS'd and HMAC-authenticated — and adds no new
// exposure, because the join snapshot already streams the entire keyspace
// including exactly these records. Raw tokens are never persisted, so they are
// not replicated either.
func (s *Store) SetReplicator(fn func(op uint8, key, value []byte) error) {
	s.cluster.replicate = fn
}

// SetWriteGate installs the cluster single-writer gate (#596). With it
// installed, a configuration write that reaches a Lobe is refused instead of
// being committed locally and silently diverging. The engine has the same seam;
// see internal/engine/single_writer.go for the reject-vs-forward argument.
//
// Deliberately NOT gated: node-local bootstrap seeding (auth.Bootstrap and the
// MUNINN_ADMIN_PASSWORD seed), which runs before any cluster role exists and
// establishes this node's own ability to be administered at all. Those paths
// call the store before SetWriteGate is wired, so they cannot be caught by it.
func (s *Store) SetWriteGate(fn func() error) {
	s.cluster.writeGate = fn
}

// refuseNonLeaderWrite is the chokepoint every mutating auth method calls.
func (s *Store) refuseNonLeaderWrite() error {
	g := s.cluster.writeGate
	if g == nil {
		return nil
	}
	return g()
}

// set commits a single key and replicates it. Replaces bare s.db.Set on every
// mutating path so a new auth write cannot forget to replicate.
func (s *Store) set(key, value []byte) error {
	if err := s.db.Set(key, value, pebble.Sync); err != nil {
		return err
	}
	s.replicate(opSet, key, value)
	return nil
}

// del removes a single key and replicates the removal.
func (s *Store) del(key []byte) error {
	if err := s.db.Delete(key, pebble.Sync); err != nil {
		return err
	}
	s.replicate(opDelete, key, nil)
	return nil
}

// commit commits a batch and replicates it as a single OpBatch, mirroring
// PebbleStore.replicateBatch: Repr() stays valid after Commit and before Close.
func (s *Store) commit(b *pebble.Batch) error {
	if err := b.Commit(pebble.Sync); err != nil {
		return err
	}
	if !b.Empty() {
		if repr := b.Repr(); len(repr) > 0 {
			s.replicate(opBatch, nil, repr)
		}
	}
	return nil
}

// replicate is best-effort and non-fatal, matching PebbleStore.replicateBatch:
// the local commit already succeeded, so failing the caller here would report a
// write that did happen as a write that did not.
func (s *Store) replicate(op uint8, key, value []byte) {
	fn := s.cluster.replicate
	if fn == nil {
		return
	}
	if err := fn(op, key, value); err != nil {
		slog.Warn("auth: replication log append failed — this configuration change will not reach the Lobes",
			"op", op, "err", err)
	}
}
