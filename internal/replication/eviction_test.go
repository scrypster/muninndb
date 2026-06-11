package replication

import (
	"net"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #534: EvictIfConn disconnects only the matching conn (never a replacement), and
// keeps the entry so the address survives for re-dial.
func TestEvictIfConn(t *testing.T) {
	mgr := NewConnManager("self")
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	p := mgr.RegisterConn("peer", "peer:1", a)
	if !p.IsConnected() {
		t.Fatal("peer should be connected after RegisterConn")
	}

	// A different conn must NOT evict (identity check protects a replacement).
	c, d := net.Pipe()
	t.Cleanup(func() { c.Close(); d.Close() })
	if mgr.EvictIfConn("peer", c) {
		t.Error("must not evict when the conn does not match")
	}
	if !p.IsConnected() {
		t.Error("peer should still be connected after a non-matching evict")
	}

	// The matching conn evicts.
	if !mgr.EvictIfConn("peer", a) {
		t.Error("should evict the matching conn")
	}
	if p.IsConnected() {
		t.Error("peer should be disconnected after evict")
	}
	// Entry kept so the address survives for re-dial.
	if _, ok := mgr.GetPeer("peer"); !ok {
		t.Error("entry should be kept after eviction (addr preserved for re-dial)")
	}
}

// #534: a write error marks the PeerConn dead so IsConnected reports false and a
// new conn can replace it.
func TestPeerConn_SendMarksDeadOnError(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close() })
	p := NewPeerConnFromConn("peer", "peer:1", a)
	b.Close() // closing the far end makes the next write fail

	if err := p.Send(mbp.TypePing, nil); err == nil {
		t.Error("expected Send to fail writing to a closed pipe")
	}
	if p.IsConnected() {
		t.Error("conn must be marked dead after a write error")
	}
}
