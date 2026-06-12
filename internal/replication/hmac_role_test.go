package replication

import (
	"crypto/hmac"
	"crypto/sha256"
	"net"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #538: JoinRequest.Role must be covered by the HMAC at protocol v2+.
// A v2 sender hashes nodeID+role; a v1 sender hashes nodeID only (backward compat).

func TestHandleJoinRequest_V2_RoleInHMAC_Accepted(t *testing.T) {
	const secret = "test-secret"
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatal(err)
	}
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", secret, es, newTestRepLog(t), mgr)
	handler.LeaderInfo = func() (bool, string, string) { return true, "cortex-1", "" }

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("lobe-1"))
	h.Write([]byte{uint8(RoleReplica)})
	correctHash := h.Sum(nil)

	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn("lobe-1", "10.0.0.2:8479", cortexConn)

	req := mbp.JoinRequest{
		NodeID: "lobe-1", Addr: "10.0.0.2:8479",
		SecretHash: correctHash, Role: uint8(RoleReplica),
		ProtocolVersion: 2,
	}
	if resp := handler.HandleJoinRequest(req, peer); !resp.Accepted {
		t.Errorf("v2 join with role-in-HMAC should be accepted: %s", resp.RejectReason)
	}
}

func TestHandleJoinRequest_V2_LegacyHMAC_Rejected(t *testing.T) {
	const secret = "test-secret"
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatal(err)
	}
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", secret, es, newTestRepLog(t), mgr)
	handler.LeaderInfo = func() (bool, string, string) { return true, "cortex-1", "" }

	// Legacy HMAC (no role) sent in a v2 request — must be rejected.
	hLegacy := hmac.New(sha256.New, []byte(secret))
	hLegacy.Write([]byte("lobe-1"))
	legacyHash := hLegacy.Sum(nil)

	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn("lobe-1", "10.0.0.2:8479", cortexConn)

	req := mbp.JoinRequest{
		NodeID: "lobe-1", Addr: "10.0.0.2:8479",
		SecretHash: legacyHash, Role: uint8(RoleReplica),
		ProtocolVersion: 2,
	}
	if resp := handler.HandleJoinRequest(req, peer); resp.Accepted {
		t.Error("v2 join with old (no-role) HMAC should be rejected")
	}
}

func TestHandleJoinRequest_V1_LegacyHMAC_Accepted(t *testing.T) {
	const secret = "test-secret"
	es := newTestEpochStore(t)
	if err := es.ForceSet(1); err != nil {
		t.Fatal(err)
	}
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", secret, es, newTestRepLog(t), mgr)
	handler.LeaderInfo = func() (bool, string, string) { return true, "cortex-1", "" }

	// v1 senders hash only nodeID — must remain accepted for rolling-upgrade compat.
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("lobe-1"))
	legacyHash := h.Sum(nil)

	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn("lobe-1", "10.0.0.2:8479", cortexConn)

	req := mbp.JoinRequest{
		NodeID: "lobe-1", Addr: "10.0.0.2:8479",
		SecretHash: legacyHash, Role: uint8(RoleReplica),
		ProtocolVersion: 1, // old binary
	}
	if resp := handler.HandleJoinRequest(req, peer); !resp.Accepted {
		t.Errorf("v1 join with legacy HMAC must still be accepted (rolling upgrade): %s", resp.RejectReason)
	}
}

func TestValidSecret_V2_RoleInHMAC(t *testing.T) {
	const secret = "test-secret"
	es := newTestEpochStore(t)
	handler := NewJoinHandler("cortex-1", secret, es, newTestRepLog(t), NewConnManager("cortex-1"))

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("probe-node"))
	h.Write([]byte{uint8(RolePrimary)})
	v2Hash := h.Sum(nil)

	hOld := hmac.New(sha256.New, []byte(secret))
	hOld.Write([]byte("probe-node"))
	v1Hash := hOld.Sum(nil)

	if !handler.ValidSecret("probe-node", uint8(RolePrimary), 2, v2Hash) {
		t.Error("v2 probe with role-in-HMAC should be valid")
	}
	if handler.ValidSecret("probe-node", uint8(RolePrimary), 2, v1Hash) {
		t.Error("v2 probe with legacy HMAC (no role) should be invalid")
	}
	if !handler.ValidSecret("probe-node", uint8(RolePrimary), 1, v1Hash) {
		t.Error("v1 probe with legacy HMAC should still be valid")
	}
}
