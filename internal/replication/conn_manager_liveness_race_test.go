package replication

import (
	"net"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// PeerConn.Send marks a connection dead (conn = nil, closed = true) under the
// peer's own lock when a write fails. HasLivePeerAt and RegisterConnKind read
// those two fields holding only the manager's lock, so the read and the write
// shared no lock. The failover tests hit it whenever a claim was broadcast to a
// peer that had just stopped while the discovery loop asked whether that peer
// was live.
//
// The race detector is what fails this test. Without -race it still checks the
// answer after a failed send.
func TestConnManager_LivenessReadsDoNotRaceAFailedSend(t *testing.T) {
	const addr = "10.0.0.1:7000"
	for round := 0; round < 50; round++ {
		m := NewConnManager("local-node")
		local, remote := net.Pipe()
		_ = remote.Close() // every write to local now fails

		p, adopted := m.RegisterConnKind("peer-1", addr, local, kindHello, true)
		if !adopted {
			t.Fatal("first registration was not adopted")
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { // the writer: a failed send tears the connection down
			defer wg.Done()
			<-start
			_ = p.Send(mbp.TypePing, nil)
		}()
		go func() { // the discovery loop's question
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				_ = m.HasLivePeerAt(addr)
			}
		}()
		go func() { // a second handshake from the same peer
			defer wg.Done()
			<-start
			c1, c2 := net.Pipe()
			defer c2.Close()
			if _, ok := m.RegisterConnKind("peer-1", addr, c1, kindHello, false); !ok {
				_ = c1.Close()
			}
		}()
		close(start)
		wg.Wait()
		_ = m.Close()
	}

	// And the plain answer: after a send has failed, the peer is not live.
	m := NewConnManager("local-node")
	defer m.Close()
	local, remote := net.Pipe()
	_ = remote.Close()
	p, _ := m.RegisterConnKind("peer-2", "10.0.0.2:7000", local, kindHello, true)
	if err := p.Send(mbp.TypePing, nil); err == nil {
		t.Fatal("send to a closed peer succeeded")
	}
	if m.HasLivePeerAt("10.0.0.2:7000") {
		t.Error("HasLivePeerAt reports a peer live after a send to it failed")
	}
}
