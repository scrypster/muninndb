package mbp_test

import (
	crand "crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// seedLargeMemories writes n memories of about 16 KiB each to vault, all
// matching the query "dredging", so that one ACTIVATE for many results is
// answered with a reply of several hundred kilobytes.
func seedLargeMemories(t *testing.T, env *matrixEnv, vault string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		// Random, so that it does not compress away: the server compresses
		// large replies, and what it queues is the compressed frame. In hex,
		// so that it is one word of the same length in every memory and the
		// recall scores them alike from run to run.
		noise := make([]byte, 7600)
		if _, err := crand.Read(noise); err != nil {
			t.Fatal(err)
		}
		_, err := env.eng.Write(fullCtx(), &mbp.WriteRequest{
			Vault:   vault,
			Concept: "dredging log " + strconv.Itoa(i),
			Content: "dredging berth " + strconv.Itoa(i) + " closed for dredging. " + hex.EncodeToString(noise), // about 15.2 KiB
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// largeReplySize asks for the large reply on a connection that reads, until the
// memories are indexed and the reply has stopped growing, and returns its size.
// Indexing is not immediate and takes as long as the machine makes it.
func largeReplySize(t *testing.T, env *matrixEnv, vault string) int {
	t.Helper()
	probe := dialMBP(t, env.addr, "", vault)
	defer probe.conn.Close()
	deadline := time.Now().Add(90 * time.Second)
	last, same := 0, 0
	for {
		_ = probe.conn.SetDeadline(time.Now().Add(60 * time.Second))
		r := probe.send(mbp.TypeActivate, activateLarge(vault))
		if r.frameType != mbp.TypeActivateResp {
			t.Fatalf("ACTIVATE answered frame 0x%02X %q", r.frameType, r.message)
		}
		if n := len(r.payload); n >= 200<<10 && n == last {
			if same++; same == 2 {
				return n
			}
		} else {
			last, same = n, 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture: one reply is %d bytes; want several hundred kilobytes, and steady", last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func activateLarge(vault string) []byte {
	payload, _ := mbp.EncodeMsgpack(&mbp.ActivateRequest{Vault: vault, Context: []string{"dredging"}, MaxResults: 100, ReadOnly: true})
	return payload
}

func awaitOpenConnections(env *matrixEnv, want int64, bound time.Duration) int64 {
	deadline := time.Now().Add(bound)
	for {
		n := env.srv.OpenConnectionsForTest()
		if n == want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAConnectionThatDoesNotReadItsRepliesIsBoundedAndThenClosed pins what one
// connection can make the server hold in replies it never reads, and for how
// long.
//
// A reply is queued for its connection and written by one goroutine. The queue
// held 256 frames of any size and the write had no deadline, so a client that
// sent requests and never read the answers kept every reply in memory for as
// long as it stayed connected: measured, one connection with no key held 192
// MiB with sixteen frames, and the arithmetic limit was about 5 GiB.
//
// Now the unread replies of one connection are bounded, the server stops
// reading from a connection that is not taking its replies, and a connection
// that takes nothing at all for a bounded time is closed.
func TestAConnectionThatDoesNotReadItsRepliesIsBoundedAndThenClosed(t *testing.T) {
	const stall = 2 * time.Second
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetReplyStallTimeoutForTest(stall) })
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatal(err)
	}
	seedLargeMemories(t, env, vault, 60)

	replyBytes := largeReplySize(t, env, vault)
	if n := awaitOpenConnections(env, 0, 5*time.Second); n != 0 {
		t.Fatalf("fixture: %d connections open before the test", n)
	}

	client := dialMBP(t, env.addr, "", vault)
	if tc, ok := client.conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	base := heapInUse()
	const requests = 200
	request := activateLarge(vault)
	// The server stops reading once this connection's replies back up, so the
	// requests are written from a goroutine that may be left blocked.
	go func() {
		for i := 0; i < requests; i++ {
			_ = client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := mbp.WriteFrame(client.conn, &mbp.Frame{Version: 0x01, Type: mbp.TypeActivate, CorrelationID: uint64(1000 + i), Payload: request}); err != nil {
				return
			}
		}
	}()

	// Sample once the replies have backed up as far as they are going to: the
	// figure has stopped rising. The connection is closed some time after
	// that, so the highest figure seen is the one that matters.
	var unwritten int64
	steadySince := time.Now()
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		now := env.srv.UnwrittenReplyBytesForTest()
		if now > unwritten {
			unwritten, steadySince = now, time.Now()
		}
		if unwritten > 0 && (time.Since(steadySince) > stall/2 || env.srv.StalledConnectionsClosedForTest() > 0) {
			break
		}
	}
	held := heapInUse() - base
	t.Logf("%d requests for a %d KiB reply each, none read: %.1f MiB of replies queued, heap in use %+.1f MiB", requests, replyBytes>>10, float64(unwritten)/(1<<20), float64(held)/(1<<20))
	const bound = 17 << 20 // one frame's worth of large replies, and a megabyte of small ones
	if unwritten > bound {
		t.Errorf("%.1f MiB of replies are queued for one connection; the bound is %d MiB", float64(unwritten)/(1<<20), bound>>20)
	}
	// The queue is the exact figure. The heap is a coarse one, taken while the
	// requests already admitted are still being worked on: it is here to catch
	// the replies piling up again, which was the whole of requests x reply.
	if limit := int64(requests * replyBytes / 2); held > limit {
		t.Errorf("the heap grew by %.1f MiB for one connection that reads nothing (%d requests x %d KiB is %d MiB)", float64(held)/(1<<20), requests, replyBytes>>10, requests*replyBytes>>20)
	}

	if unwritten == 0 {
		t.Fatalf("fixture: no reply was ever queued")
	}

	// It is taking nothing, so it is closed, and everything it held is let go.
	if n := awaitOpenConnections(env, 0, stall+60*time.Second); n != 0 {
		t.Fatalf("the connection was still open a minute after it stopped reading; it should have been closed")
	}
	if n := env.srv.StalledConnectionsClosedForTest(); n != 1 {
		t.Errorf("connections closed for not reading = %d; want 1", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for env.srv.UnwrittenReplyBytesForTest() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if left := env.srv.UnwrittenReplyBytesForTest(); left != 0 {
		t.Errorf("%d bytes of replies are still counted after the connection closed", left)
	}
	if after := heapInUse() - base; after > 16<<20 {
		t.Errorf("heap in use is still %+.1f MiB after the connection closed", float64(after)/(1<<20))
	}
}

// TestASlowReaderIsServedNotClosed: a client that takes its replies slowly, but
// keeps taking them, is slow and not gone. Every request it sent is answered
// with a real answer, and the connection stays open, however long the whole
// exchange takes compared with the time a connection may take nothing.
func TestASlowReaderIsServedNotClosed(t *testing.T) {
	const stall = 400 * time.Millisecond
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetReplyStallTimeoutForTest(stall) })
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatal(err)
	}
	seedLargeMemories(t, env, vault, 60)
	largeReplySize(t, env, vault)

	client := dialMBP(t, env.addr, "", vault)
	const requests = 48
	request := activateLarge(vault)
	wrote := make(chan error, 1)
	go func() {
		for i := 0; i < requests; i++ {
			if err := mbp.WriteFrame(client.conn, &mbp.Frame{Version: 0x01, Type: mbp.TypeActivate, CorrelationID: uint64(1000 + i), Payload: request}); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- nil
	}()

	start := time.Now()
	answers, refusals := 0, map[string]int{}
	for answers < requests { // answers counts every frame that came back, refusals included
		_ = client.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			t.Fatalf("after %d answers, %s in: %v", answers, time.Since(start).Round(time.Millisecond), err)
		}
		if f.Type == mbp.TypeActivateResp {
			answers++
		} else {
			var ep mbp.ErrorPayload
			_ = mbp.DecodeMsgpack(f.Payload, &ep)
			refusals[ep.Message]++
			answers++
		}
		time.Sleep(60 * time.Millisecond) // slow, and steady
	}
	took := time.Since(start)
	t.Logf("%d requests answered over %s (a connection may take nothing for %s); refusals: %v", answers, took.Round(time.Millisecond), stall, refusals)
	if len(refusals) != 0 {
		t.Errorf("a reader that kept reading had requests refused: %v", refusals)
	}
	if took < 3*stall {
		t.Fatalf("fixture: the exchange took %s; it has to outlast the stall timeout several times to show anything", took)
	}
	if err := <-wrote; err != nil {
		t.Errorf("writing the requests: %v", err)
	}
	if n := env.srv.StalledConnectionsClosedForTest(); n != 0 {
		t.Errorf("%d connections were closed for not reading; this one was reading", n)
	}
	if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Errorf("after the exchange, PING answered frame 0x%02X %q", r.frameType, r.message)
	}
}

// TestEveryPipelinedRequestIsAnswered: a client may send many requests before
// it reads any answer. Every one is answered. The reply queue holds 256
// frames, and a reply that found it full used to be dropped without a word:
// the client waited for an answer that was never coming. Now the server stops
// reading requests while the queue could not hold their answers, so the
// requests wait in the socket and none is lost.
//
// The replies here are small ones, which are always queued, and there are far
// more of them than the socket buffers hold, so the writer really is held up
// and it is the reader's waiting that keeps the queue from overflowing.
func TestEveryPipelinedRequestIsAnswered(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatal(err)
	}
	client := dialMBP(t, env.addr, "", vault)
	if tc, ok := client.conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}

	const requests = 20000
	payload, _ := mbp.EncodeMsgpack(&mbp.PingRequest{Data: strings.Repeat("w", 900)})
	wrote := make(chan error, 1)
	go func() {
		for i := 0; i < requests; i++ {
			if err := mbp.WriteFrame(client.conn, &mbp.Frame{Version: 0x01, Type: mbp.TypePing, CorrelationID: uint64(1 + i), Payload: payload}); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- nil
	}()

	// Read nothing for a while: the server gets as far as it will on its own.
	time.Sleep(500 * time.Millisecond)
	if unread := env.srv.UnwrittenReplyBytesForTest(); unread > 256*4096 {
		t.Errorf("%d bytes of small replies are queued for one connection; the queue holds 256 of at most 4 KiB", unread)
	}

	seen := map[uint64]bool{}
	for len(seen) < requests {
		_ = client.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			break
		}
		if f.Type != mbp.TypePong {
			t.Fatalf("frame 0x%02X in answer to a PING", f.Type)
		}
		seen[f.CorrelationID] = true
	}
	if len(seen) != requests {
		t.Errorf("%d of %d pipelined PINGs were answered; the rest were dropped", len(seen), requests)
	}
	if err := <-wrote; err != nil {
		t.Errorf("writing the requests: %v", err)
	}
}

// TestPingEchoesASmallPayloadOnly: PONG repeats what PING carried. Nothing
// needs megabytes repeated back, and it was the one frame whose reply was as
// large as the sender chose to make it, with no key and nothing stored.
func TestPingEchoesASmallPayloadOnly(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatal(err)
	}
	client := dialMBP(t, env.addr, "", vault)

	fits := strings.Repeat("w", 900)
	r := client.request(mbp.TypePing, &mbp.PingRequest{Data: fits})
	var pong mbp.PongResponse
	if r.frameType != mbp.TypePong || mbp.DecodeMsgpack(r.payload, &pong) != nil || pong.Data != fits {
		t.Errorf("a 900-byte PING: frame 0x%02X %q; want it echoed whole", r.frameType, r.message)
	}

	for _, size := range []int{2 << 10, 1 << 20, 12 << 20} {
		r := client.request(mbp.TypePing, &mbp.PingRequest{Data: strings.Repeat("w", size)})
		if r.frameType != mbp.TypeError || !strings.Contains(r.message, "ping payload too large") {
			t.Errorf("a %d-byte PING: frame 0x%02X %.60q; want it refused as too large", size, r.frameType, r.message)
		}
		if len(r.payload) > 512 {
			t.Errorf("a %d-byte PING was answered with %d bytes", size, len(r.payload))
		}
	}
	if r := client.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
		t.Errorf("after the refusals, PING answered frame 0x%02X %q", r.frameType, r.message)
	}
}

// TestARequestThatWaitedIsCheckedAgainstItsSessionAgain: a request is checked
// against its session when it is dispatched (SEC-23). A READ or ACTIVATE may
// then wait for a slow client to take earlier replies, for as long as that
// takes. If its key is revoked while it waits, it is not carried out when room
// comes: it is answered the way any frame of an ended session is.
func TestARequestThatWaitedIsCheckedAgainstItsSessionAgain(t *testing.T) {
	env := newMatrixEnvWith(t, func(s *mbp.Server) { s.SetReplyStallTimeoutForTest(5 * time.Minute) })
	const vault = "pilot-station"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: false}); err != nil {
		t.Fatal(err)
	}
	const open = "harbour-notices" // largeReplySize probes with no key
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: open, Public: true}); err != nil {
		t.Fatal(err)
	}
	seedLargeMemories(t, env, vault, 60)
	seedLargeMemories(t, env, open, 60)
	largeReplySize(t, env, open)

	token, key, err := env.store.GenerateAPIKey(vault, "waits", auth.ModeObserve, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := dialMBP(t, env.addr, token, vault)
	if tc, ok := client.conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	// Wait for the vault under test to answer in full too.
	deadline := time.Now().Add(90 * time.Second)
	for {
		if r := client.send(mbp.TypeActivate, activateLarge(vault)); len(r.payload) >= 200<<10 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("fixture: a reply from %s is %d bytes", vault, len(r.payload))
		}
		time.Sleep(100 * time.Millisecond)
	}
	env.rec.take()

	const requests = 40
	request := activateLarge(vault)
	go func() {
		for i := 0; i < requests; i++ {
			if err := mbp.WriteFrame(client.conn, &mbp.Frame{Version: 0x01, Type: mbp.TypeActivate, CorrelationID: uint64(1000 + i), Payload: request}); err != nil {
				return
			}
		}
	}()

	// Nothing is read, so replies back up until requests are waiting for room.
	// Then it goes quiet: the engine is no longer being called.
	countActivates := func() int {
		n := 0
		env.rec.mu.Lock()
		for _, c := range env.rec.calls {
			if c == "Activate" {
				n++
			}
		}
		env.rec.mu.Unlock()
		return n
	}
	// awaitQuiet waits until the engine has not been called for a while, and
	// returns how many requests have been carried out by then.
	awaitQuiet := func(quiet time.Duration) int {
		n, since := countActivates(), time.Now()
		for deadline := time.Now().Add(90 * time.Second); time.Since(since) < quiet; time.Sleep(20 * time.Millisecond) {
			if now := countActivates(); now != n {
				n, since = now, time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatal("fixture: the engine never went quiet")
			}
		}
		return n
	}
	if n := awaitQuiet(time.Second); n == 0 || n >= requests {
		t.Fatalf("fixture: %d of %d requests were carried out with nothing read; some must have been, and some must be waiting", n, requests)
	}

	if err := env.store.RevokeAPIKey(vault, key.ID); err != nil {
		t.Fatal(err)
	}
	// A request that already had its room when the key was revoked is being
	// carried out and finishes: it was admitted before. Let those finish. From
	// here on, the only way the engine can be called is for a request that got
	// its room AFTER the revocation, which is what must not happen.
	carriedOut := awaitQuiet(3 * time.Second)

	// Now the client reads. Room comes back, and the requests that waited wake.
	replies, refused := 0, 0
	for {
		_ = client.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		f, err := mbp.ReadFrame(client.conn)
		if err != nil {
			break // the server closes an ended session
		}
		if f.Type == mbp.TypeActivateResp {
			replies++
			continue
		}
		var ep mbp.ErrorPayload
		_ = mbp.DecodeMsgpack(f.Payload, &ep)
		if ep.Code == mbp.ErrAuthFailed && ep.Message == "api key is no longer valid" {
			refused++
		} else {
			t.Errorf("unexpected answer: frame 0x%02X %d %q", f.Type, ep.Code, ep.Message)
		}
	}
	t.Logf("%d requests carried out by the time the revocation had settled; after it: %d replies read (all built before), %d refused", carriedOut, replies, refused)
	if after := countActivates(); after != carriedOut {
		t.Errorf("%d requests were carried out after the key was revoked (%d before, %d after)", after-carriedOut, carriedOut, after)
	}
	if replies > carriedOut {
		t.Errorf("%d replies were delivered; only %d requests were ever carried out", replies, carriedOut)
	}
	if refused == 0 {
		t.Errorf("no request was refused: none of them was waiting, so this showed nothing")
	}
}
