package mbp

import (
	"log/slog"
	"math"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"time"
)

// The listener holds at most a fixed number of connections at once (SEC-28).
// A connection counts from the moment it is accepted, before it has said
// HELLO: the cap is what bounds the connections nobody has authenticated.
//
// # Where the default comes from
//
// There was no listener-level connection limit anywhere in the tree to follow,
// so the default is derived from what one connection can make the server hold.
//
// Before it has said HELLO a connection holds about 5 KiB and may send at most
// 64 KiB (SEC-27). Established and idle it holds about 21 KiB (measured, 400
// connections against a real engine). What sets the default is an established
// connection doing its worst, and that is bounded by connMemoryBound
// (reply_flow.go), 69 MiB:
//
//	requests being handled   20 MiB   connRequestGate, and the one frame read past it
//	decompressed payloads    32 MiB   connDecodeBudget
//	replies not yet read     17 MiB   connReplyBudget, and a queue of small ones
//
// The first of those includes the cheapest thing a connection can do: a frame
// header that declares a 16 MiB payload makes the server set 16 MiB aside and
// wait for it, for sixteen bytes and no key on a public vault.
//
// The cap is the number of connections at that bound that fit in a quarter of
// the process's memory limit. At the default limit of 4 GiB
// (MUNINN_MEM_LIMIT_GB) that is 1 GiB / 69 MiB = 14, which is under the floor
// of 16, so the default is 16. At 16 GiB it is 59. A server given a larger
// memory limit gets a proportionally larger cap without being told.
//
// An earlier version of this derivation divided by 16 MiB, one frame's worth,
// and arrived at 64. That was what a connection held while a frame was being
// read, not what it could hold: replies it did not read were not bounded at
// all then.
//
// What the bound does not cover is the working memory of the requests being
// handled: what the engine allocates to carry out up to maxConcurrentFrames
// requests at once. That is the cost of serving them, on any transport.
const (
	// maxConnectionsEnv overrides the derived default.
	maxConnectionsEnv = "MUNINN_MBP_MAX_CONNECTIONS"

	// connectionMemoryShare is the fraction of the memory limit that a full
	// set of connections, each at connMemoryBound, may add up to: one part in
	// connectionMemoryShare.
	connectionMemoryShare = 4

	// fallbackMaxConnections is the cap when the process has no memory limit
	// to derive one from: what the derivation gives at the default 4 GiB.
	fallbackMaxConnections = 16

	// minDerivedConnections keeps a server with a very small memory limit
	// usable. An explicit override may still go lower.
	minDerivedConnections = 16

	// capWarnInterval is how often, at most, refusals at the cap are logged.
	capWarnInterval = 10 * time.Second
)

// derivedMaxConnections is the default cap for the process's memory limit.
func derivedMaxConnections(memoryLimit int64) int {
	if memoryLimit <= 0 || memoryLimit == math.MaxInt64 {
		return fallbackMaxConnections
	}
	n := memoryLimit / connectionMemoryShare / connMemoryBound
	if n < minDerivedConnections {
		return minDerivedConnections
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(n)
}

// resolveMaxConnections returns the cap in force: the override if one is set
// and valid, otherwise the default derived from the memory limit. An override
// that is not a positive number is refused loudly and the default is used.
func resolveMaxConnections() int {
	def := derivedMaxConnections(debug.SetMemoryLimit(-1))
	raw := os.Getenv(maxConnectionsEnv)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		slog.Warn("mbp: invalid connection limit, using the default", "env", maxConnectionsEnv, "value", raw, "default", def)
		return def
	}
	return n
}

// admitConnection takes a place for a newly accepted connection, or reports
// that the listener is full.
func (s *Server) admitConnection() bool {
	for {
		cur := s.openConns.Load()
		if cur >= int64(s.maxConns) {
			return false
		}
		if s.openConns.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (s *Server) releaseConnection() { s.openConns.Add(-1) }

// Refusing a connection must cost the accept loop nothing, whatever the peer
// does. Telling a refused client why is a courtesy, and it is I/O with a peer
// nobody has authenticated: on a TLS listener the first write runs the TLS
// handshake, which READS from the peer, so a write deadline alone does not
// bound it. When the refusal was written from the accept loop, one peer that
// connected at the cap and sent nothing stopped the listener accepting anyone.
//
// So the accept loop never touches a refused connection beyond handing it to a
// small fixed set of workers, without waiting, or closing it when they are
// busy. The workers put the whole exchange — handshake, write, everything —
// under one deadline.
const (
	// refusalWorkers answer refused connections. Each can be held for
	// refusalDeadline by a peer that does nothing.
	refusalWorkers = 2
	// refusalQueue is how many refused connections may wait for a worker.
	// Beyond that a refused connection is simply closed.
	refusalQueue = 16
	// refusalDeadline bounds everything a worker does with one connection.
	refusalDeadline = 500 * time.Millisecond
)

// refuseAtCap disposes of a connection the listener has no place for. It does
// no I/O with the peer and never waits: the connection goes to a refusal
// worker if one has room for it, and is closed otherwise. The refusal is logged
// at most once per capWarnInterval, with the number refused since the last
// line, so a flood of connections does not become a flood of log lines.
func (s *Server) refuseAtCap(conn net.Conn) {
	select {
	case s.refusals <- conn:
	default:
		// No worker has room (or the server is not serving): no courtesy frame.
		// Closing a connection that has not been written to does not wait on
		// the peer, TLS or not.
		conn.Close()
	}

	refused := s.capRefused.Add(1)
	now := time.Now().UnixNano()
	last := s.capLastWarn.Load()
	if now-last < int64(capWarnInterval) || !s.capLastWarn.CompareAndSwap(last, now) {
		return
	}
	s.capRefused.Add(-refused)
	s.capWarnings.Add(1)
	slog.Warn("mbp: at the connection limit, refusing new connections",
		"limit", s.maxConns, "refused_since_last_warning", refused, "override", maxConnectionsEnv)
}

// startRefusers starts the workers that answer refused connections. They stop
// when the server shuts down.
func (s *Server) startRefusers() {
	s.refusals = make(chan net.Conn, refusalQueue)
	for i := 0; i < refusalWorkers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-s.shutdown:
					for {
						select {
						case conn := <-s.refusals:
							conn.Close()
						default:
							return
						}
					}
				case conn := <-s.refusals:
					answerRefused(s, conn)
				}
			}
		}()
	}
}

// answerRefused tells one refused connection why, and closes it. Everything it
// does with the peer is under one deadline, reads and writes both.
func answerRefused(s *Server, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(refusalDeadline))
	s.writeErrorFrame(conn, 0, ErrRateLimited, "server is at its connection limit")
}
