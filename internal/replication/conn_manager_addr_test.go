package replication

import (
	"net"
	"testing"
)

// #522 Step 0: OnAddrChanged previously routed through AddPeer, which closes the
// live connection and replaces it — tearing down the replication stream on a
// benign address readvertisement. UpdatePeerAddr must update the address while
// leaving the live connection intact.
func TestConnManager_UpdatePeerAddr_PreservesLiveConn(t *testing.T) {
	mgr := NewConnManager("self")
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	p := mgr.RegisterConn("peer-1", "old:8479", c1)
	if !p.IsConnected() {
		t.Fatal("expected connected peer after RegisterConn")
	}

	mgr.UpdatePeerAddr("peer-1", "new:8479")

	p2, ok := mgr.GetPeer("peer-1")
	if !ok {
		t.Fatal("peer missing after UpdatePeerAddr")
	}
	if p2 != p {
		t.Error("expected the SAME PeerConn (live conn preserved), got a replacement")
	}
	if !p2.IsConnected() {
		t.Error("UpdatePeerAddr must NOT close the live connection")
	}
	if p2.Addr() != "new:8479" {
		t.Errorf("addr = %q, want new:8479", p2.Addr())
	}
}

// For a peer that has no entry yet, UpdatePeerAddr adds a disconnected one.
func TestConnManager_UpdatePeerAddr_AddsWhenAbsent(t *testing.T) {
	mgr := NewConnManager("self")
	mgr.UpdatePeerAddr("peer-x", "host:8479")
	p, ok := mgr.GetPeer("peer-x")
	if !ok {
		t.Fatal("expected peer to be added")
	}
	if p.Addr() != "host:8479" {
		t.Errorf("addr = %q, want host:8479", p.Addr())
	}
	if p.IsConnected() {
		t.Error("expected disconnected placeholder")
	}
}
