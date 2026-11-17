package mbp_test

import (
	"encoding/binary"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// rawDial opens a TCP connection and sends nothing.
func rawDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// closedWithin reports how long the server took to close conn, or false if it
// was still open after bound. Anything the server sent first (an error frame)
// is read and discarded.
func closedWithin(conn net.Conn, bound time.Duration) (time.Duration, bool) {
	start := time.Now()
	_ = conn.SetReadDeadline(start.Add(bound))
	buf := make([]byte, 4096)
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return time.Since(start), false
			}
			return time.Since(start), true
		}
	}
}

// helloBytes is a complete, valid HELLO frame for a session with no key.
func helloBytes(t *testing.T, vault string) []byte {
	t.Helper()
	payload, err := mbp.EncodeMsgpack(&mbp.HelloRequest{Version: "1", AuthMethod: "none", Vault: vault})
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, mbp.FramePrefixSize)
	header[0], header[1] = 0x01, mbp.TypeHello
	binary.BigEndian.PutUint32(header[4:8], uint32(len(payload)))
	return append(header, payload...)
}

// awaitGoroutines waits for the process to be back to at most `want`
// goroutines.
func awaitGoroutines(want int, bound time.Duration) int {
	deadline := time.Now().Add(bound)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandshakeHasADeadline: a connection has a fixed time to complete HELLO.
// One that does not is closed and everything it held is released; one that
// does is not on a clock any more.
//
// Before this there was no deadline at all. A peer that connected and sent
// nothing, or half a HELLO, held a goroutine and a file descriptor for as long
// as it kept the socket open, and needed no credential to do it.
//
// The deadline is shortened for the test; in production it is 15 seconds.
func TestHandshakeHasADeadline(t *testing.T) {
	const deadline = 400 * time.Millisecond
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetHandshakeTimeoutForTest(deadline) })
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", vault, err)
	}
	// Generous: the point is "closed", against "open for ever".
	const bound = 20 * deadline

	t.Run("a connection that sends nothing", func(t *testing.T) {
		took, closed := closedWithin(rawDial(t, env.addr), bound)
		if !closed {
			t.Fatalf("still open after %s; the deadline is %s", took, deadline)
		}
		if took < deadline/2 {
			t.Errorf("closed after %s, well before the %s deadline", took, deadline)
		}
	})

	t.Run("a connection that sends half a HELLO", func(t *testing.T) {
		hello := helloBytes(t, vault)
		for name, part := range map[string][]byte{
			"half the frame header":           hello[:mbp.FramePrefixSize/2],
			"the header and half the payload": hello[:mbp.FramePrefixSize+(len(hello)-mbp.FramePrefixSize)/2],
		} {
			conn := rawDial(t, env.addr)
			if _, err := conn.Write(part); err != nil {
				t.Fatal(err)
			}
			if took, closed := closedWithin(conn, bound); !closed {
				t.Errorf("%s: still open after %s; the deadline is %s", name, took, deadline)
			}
		}
	})

	// The deadline is on the handshake as a whole, not on each read: a peer
	// that keeps sending, one byte at a time, does not keep the clock from
	// running out.
	t.Run("a connection that dribbles its HELLO", func(t *testing.T) {
		hello := helloBytes(t, vault)
		conn := rawDial(t, env.addr)
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for _, b := range hello {
				select {
				case <-stop:
					return
				case <-time.After(deadline / 4): // each byte arrives well inside the deadline
				}
				if _, err := conn.Write([]byte{b}); err != nil {
					return
				}
			}
		}()
		if took, closed := closedWithin(conn, bound); !closed {
			t.Errorf("still open after %s although the whole HELLO would take %s to arrive; the deadline is %s",
				took, time.Duration(len(hello))*deadline/4, deadline)
		}
	})

	t.Run("nothing is left behind", func(t *testing.T) {
		runtime.GC()
		baseline := runtime.NumGoroutine()
		const stuck = 40
		conns := make([]net.Conn, stuck)
		for i := range conns {
			conns[i] = rawDial(t, env.addr)
		}
		// Each is now held by a server goroutine waiting for its HELLO.
		held := baseline
		for until := time.Now().Add(5 * time.Second); held < baseline+stuck && time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
			held = runtime.NumGoroutine()
		}
		t.Logf("%d silent connections: %d goroutines, from a baseline of %d", stuck, held, baseline)
		for _, conn := range conns {
			if took, closed := closedWithin(conn, bound); !closed {
				t.Fatalf("a silent connection was still open after %s", took)
			}
		}
		if after := awaitGoroutines(baseline, 10*time.Second); after > baseline {
			t.Errorf("%d goroutines after the silent connections were closed; baseline %d", after, baseline)
		}
	})

	t.Run("a HELLO that arrives just inside the deadline", func(t *testing.T) {
		conn := rawDial(t, env.addr)
		time.Sleep(deadline / 2)
		if _, err := conn.Write(helloBytes(t, vault)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		f, err := mbp.ReadFrame(conn)
		if err != nil || f.Type != mbp.TypeHelloOK {
			t.Fatalf("HELLO sent after %s: frame %v, err %v; want HELLO_OK", deadline/2, f, err)
		}

		// Once the handshake is done the deadline is gone: the connection sits
		// idle for several deadlines and is still served.
		time.Sleep(3 * deadline)
		payload, _ := mbp.EncodeMsgpack(&mbp.PingRequest{Data: "watch"})
		if err := mbp.WriteFrame(conn, &mbp.Frame{Version: 0x01, Type: mbp.TypePing, CorrelationID: 1, Payload: payload}); err != nil {
			t.Fatalf("write PING after idling: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if f, err := mbp.ReadFrame(conn); err != nil || f.Type != mbp.TypePong {
			t.Fatalf("PING after idling for %s: frame %v, err %v; want PONG", 3*deadline, f, err)
		}
	})
}

// TestHelloPayloadIsBounded: a HELLO carries a version, a key and a vault name.
// A frame header that claims megabytes for it is refused on the header alone,
// before the server allocates what it claims or waits for it to arrive.
//
// Before this, sixteen bytes — a header declaring a 16 MiB HELLO — made the
// server allocate 16 MiB for a connection that had not authenticated, and hold
// it while it waited for a payload that need never come.
func TestHelloPayloadIsBounded(t *testing.T) {
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetHandshakeTimeoutForTest(5 * time.Second) })

	header := make([]byte, mbp.FramePrefixSize)
	header[0], header[1] = 0x01, mbp.TypeHello
	binary.BigEndian.PutUint32(header[4:8], mbp.MaxPayloadSize)

	conn := rawDial(t, env.addr)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := conn.Write(header); err != nil {
		t.Fatal(err)
	}
	// Refused at once: well inside the 5s handshake deadline, with no payload sent.
	took, closed := closedWithin(conn, 2*time.Second)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	t.Logf("a 16-byte header declaring a %d MiB HELLO: closed=%v after %s, %.2f MiB allocated",
		mbp.MaxPayloadSize>>20, closed, took, float64(allocated)/(1<<20))
	if !closed {
		t.Errorf("the server is waiting for a %d MiB HELLO payload", mbp.MaxPayloadSize>>20)
	}
	if allocated > 1<<20 {
		t.Errorf("the server allocated %.2f MiB for an unauthenticated connection's HELLO; want it refused on the header", float64(allocated)/(1<<20))
	}
}
