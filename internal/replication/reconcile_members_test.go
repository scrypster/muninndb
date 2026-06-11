package replication

import "testing"

// #516 part 2: cluster-info shows joined lobes as synthetic seed-<addr> entries
// with role "unknown" because the member list comes from the MSP peer set and is
// never reconciled with the join-handshake identities. reconcileMembers replaces
// a seed placeholder with the real joined member (node-id, role, last seq) when
// their addresses match.
func TestReconcileMembers_ReplacesSeedPlaceholders(t *testing.T) {
	base := []NodeInfo{
		{NodeID: "cortex", Addr: "cortex:8479", Role: RolePrimary},
		{NodeID: "seed-lobe1:8479", Addr: "lobe1:8479", Role: RoleUnknown},
		{NodeID: "seed-lobe2:8479", Addr: "lobe2:8479", Role: RoleUnknown},
	}
	joined := []NodeInfo{
		{NodeID: "lobe1", Addr: "lobe1:8479", Role: RoleReplica, LastSeq: 5},
		{NodeID: "lobe2", Addr: "lobe2:8479", Role: RoleReplica, LastSeq: 3},
	}

	got := reconcileMembers(base, joined)

	if len(got) != 3 {
		t.Fatalf("expected 3 members, got %d", len(got))
	}
	byAddr := make(map[string]NodeInfo, len(got))
	for _, m := range got {
		byAddr[m.Addr] = m
	}
	if m := byAddr["cortex:8479"]; m.NodeID != "cortex" || m.Role != RolePrimary {
		t.Errorf("cortex entry altered: %+v", m)
	}
	if m := byAddr["lobe1:8479"]; m.NodeID != "lobe1" || m.Role != RoleReplica || m.LastSeq != 5 {
		t.Errorf("lobe1 not reconciled to real identity: %+v", m)
	}
	if m := byAddr["lobe2:8479"]; m.NodeID != "lobe2" || m.Role != RoleReplica || m.LastSeq != 3 {
		t.Errorf("lobe2 not reconciled to real identity: %+v", m)
	}
}

// A joined member with no matching MSP peer is still included.
func TestReconcileMembers_IncludesUnmatchedJoined(t *testing.T) {
	base := []NodeInfo{{NodeID: "cortex", Addr: "cortex:8479", Role: RolePrimary}}
	joined := []NodeInfo{{NodeID: "lobe9", Addr: "lobe9:8479", Role: RoleReplica, LastSeq: 1}}

	got := reconcileMembers(base, joined)

	if len(got) != 2 {
		t.Fatalf("expected 2 members, got %d", len(got))
	}
	found := false
	for _, m := range got {
		if m.NodeID == "lobe9" && m.Role == RoleReplica {
			found = true
		}
	}
	if !found {
		t.Error("unmatched joined member lobe9 missing from reconciled list")
	}
}
