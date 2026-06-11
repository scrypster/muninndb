package replication

import (
	"net"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #522 Step 4: RegisterConnKind enforces the simultaneous-dial / kind precedence
// so a pair converges on one conn without flapping.
func TestRegisterConnKind_Precedence(t *testing.T) {
	mgr := NewConnManager("self")
	mkpipe := func() net.Conn { a, b := net.Pipe(); t.Cleanup(func() { a.Close(); b.Close() }); return a }

	// Canonical hello adopts an empty slot.
	if _, ok := mgr.RegisterConnKind("peer", "peer:8479", mkpipe(), kindHello, true); !ok {
		t.Fatal("canonical hello should adopt an empty slot")
	}
	// Non-canonical hello does NOT evict a live conn.
	if _, ok := mgr.RegisterConnKind("peer", "peer:8479", mkpipe(), kindHello, false); ok {
		t.Error("non-canonical hello must not evict a live conn")
	}
	// Canonical hello DOES replace a live hello conn.
	if _, ok := mgr.RegisterConnKind("peer", "peer:8479", mkpipe(), kindHello, true); !ok {
		t.Error("canonical hello should replace a live hello conn")
	}

	// A live JOIN conn is never evicted by a hello.
	mgr.RegisterConn("peer2", "peer2:8479", mkpipe()) // kindJoin
	if _, ok := mgr.RegisterConnKind("peer2", "peer2:8479", mkpipe(), kindHello, true); ok {
		t.Error("a live join conn must not be evicted by a hello")
	}
}

// helloHMAC covers role, so a role-tampered hello is rejected; open mode (no
// secret) accepts anything.
func TestVerifyHello_HMAC(t *testing.T) {
	c, _ := newTestCoordinator(t, "primary") // ClusterSecret is empty → open mode

	if !c.verifyHello(mbp.PeerHello{NodeID: "x"}) {
		t.Error("open mode (no secret) should accept any hello")
	}

	c.cfg.ClusterSecret = "s3cr3t"
	good := mbp.PeerHello{NodeID: "x", Addr: "x:1", Role: uint8(RoleReplica)}
	good.SecretHash = helloHMAC("s3cr3t", "x", "x:1", uint8(RoleReplica))
	if !c.verifyHello(good) {
		t.Error("a valid HMAC should verify")
	}

	tampered := good
	tampered.Role = uint8(RolePrimary) // flip role without recomputing the MAC
	if c.verifyHello(tampered) {
		t.Error("role-tampered hello must be rejected (HMAC covers role)")
	}

	wrongSecret := mbp.PeerHello{NodeID: "x", Addr: "x:1", Role: uint8(RoleReplica)}
	wrongSecret.SecretHash = helloHMAC("other", "x", "x:1", uint8(RoleReplica))
	if c.verifyHello(wrongSecret) {
		t.Error("hello signed with the wrong secret must be rejected")
	}
}
