package mbp_test

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// tryHello dials, sends a HELLO for a session with no key, and reports what
// came back: whether the session was established, and otherwise the error the
// server refused it with ("" when it just closed the connection).
func tryHello(t *testing.T, addr, vault string) (conn net.Conn, established bool, refusal string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, false, "dial: " + err.Error()
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(helloBytes(t, vault)); err != nil {
		// The server may already have answered and closed: read what it said.
	}
	f, err := mbp.ReadFrame(conn)
	if err != nil {
		return conn, false, ""
	}
	if f.Type == mbp.TypeHelloOK {
		_ = conn.SetDeadline(time.Time{})
		return conn, true, ""
	}
	var ep mbp.ErrorPayload
	_ = mbp.DecodeMsgpack(f.Payload, &ep)
	return conn, false, ep.Message
}

// pingOK reports whether an established connection still answers a PING.
func pingOK(conn net.Conn) bool {
	payload, _ := mbp.EncodeMsgpack(&mbp.PingRequest{Data: "watch"})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if err := mbp.WriteFrame(conn, &mbp.Frame{Version: 0x01, Type: mbp.TypePing, CorrelationID: 99, Payload: payload}); err != nil {
		return false
	}
	f, err := mbp.ReadFrame(conn)
	return err == nil && f.Type == mbp.TypePong
}

// TestConnectionCap: the listener holds at most a fixed number of connections.
// One more is refused with an answer and closed; a burst does not get past the
// cap or disturb the connections already there; the refusals are logged once
// for the burst, not once each; and a connection that closes gives its place
// back.
//
// Before this there was no limit on the number of connections at all.
func TestConnectionCap(t *testing.T) {
	const limit = 4
	env := newMatrixEnvWith(t, func(s *mbp.Server) {
		s.SetMaxConnectionsForTest(limit)
		s.SetHandshakeTimeoutForTest(500 * time.Millisecond)
	})
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}

	held := make([]net.Conn, 0, limit)
	for i := 0; i < limit; i++ {
		conn, ok, refusal := tryHello(t, env.addr, vault)
		if !ok {
			t.Fatalf("connection %d of %d was refused: %q", i+1, limit, refusal)
		}
		held = append(held, conn)
	}

	t.Run("one more is refused, with an answer", func(t *testing.T) {
		conn, ok, refusal := tryHello(t, env.addr, vault)
		if ok {
			t.Fatalf("connection %d was accepted; the limit is %d", limit+1, limit)
		}
		if !strings.Contains(refusal, "connection limit") {
			t.Errorf("refused with %q; want it to say the server is at its connection limit", refusal)
		}
		// And closed, not left open.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := mbp.ReadFrame(conn); err == nil {
			t.Errorf("the refused connection is still being served")
		} else if ne, isNet := err.(net.Error); isNet && ne.Timeout() {
			t.Errorf("the refused connection was left open")
		}
	})

	t.Run("a burst does not get past the cap", func(t *testing.T) {
		const burst = 60
		var wg sync.WaitGroup
		var mu sync.Mutex
		accepted := 0
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok, _ := tryHello(t, env.addr, vault); ok {
					mu.Lock()
					accepted++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if accepted != 0 {
			t.Errorf("%d of %d connections in the burst were accepted with the server already at its limit of %d", accepted, burst, limit)
		}
		for i, conn := range held {
			if !pingOK(conn) {
				t.Errorf("established connection %d stopped being served during the burst", i+1)
			}
		}
		// Sixty-one refusals so far, all inside one warning interval.
		if warnings := env.srv.ConnectionCapWarningsForTest(); warnings != 1 {
			t.Errorf("%d warnings logged for %d refusals; want one for the interval, not one per connection", warnings, burst+1)
		}
	})

	t.Run("closing a connection frees its place", func(t *testing.T) {
		held[0].Close()
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, ok, refusal := tryHello(t, env.addr, vault)
			if ok {
				held[0] = conn
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no place was freed within 10s of a connection closing (last refusal %q)", refusal)
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Full again.
		if _, ok, _ := tryHello(t, env.addr, vault); ok {
			t.Errorf("a connection past the limit was accepted after the freed place was taken")
		}
	})

	// A connection counts from the moment it is accepted, not from HELLO: the
	// cap is what bounds the connections nobody has authenticated yet.
	t.Run("connections that have not said HELLO count too", func(t *testing.T) {
		for _, conn := range held {
			conn.Close()
		}
		silent := make([]net.Conn, 0, limit)
		deadline := time.Now().Add(10 * time.Second)
		for len(silent) < limit {
			conn, err := net.DialTimeout("tcp", env.addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			// A refused connection is answered and closed at once; a held one
			// hears nothing until the handshake deadline.
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if _, err := mbp.ReadFrame(conn); err != nil {
				if ne, isNet := err.(net.Error); isNet && ne.Timeout() {
					silent = append(silent, conn)
					continue
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("could only place %d silent connections", len(silent))
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, ok, _ := tryHello(t, env.addr, vault); ok {
			t.Errorf("a connection was accepted while %d silent ones held every place", limit)
		}
		// The handshake deadline clears them out, and the places come back.
		deadline = time.Now().Add(10 * time.Second)
		for {
			if _, ok, _ := tryHello(t, env.addr, vault); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the places held by silent connections never came back")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
