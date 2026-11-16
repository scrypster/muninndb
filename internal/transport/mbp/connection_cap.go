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
// so the default is derived from what a connection can make the server hold,
// which was measured (a real server and engine, 400 connections):
//
//	accepted, no HELLO yet        about 5 KiB, 1 goroutine
//	established and idle          about 21 KiB, 2 goroutines
//	established, and has sent a
//	16-byte frame header that
//	declares a 16 MiB payload     16 MiB, for as long as it stays connected
//
// The last one sets the default. It needs no key on a public vault and costs
// the peer sixteen bytes, because a frame's payload buffer is allocated at its
// declared size when the header is read and the read then waits. So the cap is
// the number of such connections that fit in a quarter of the process's memory
// limit: at the default limit of 4 GiB (MUNINN_MEM_LIMIT_GB) that is
// 1 GiB / 16 MiB = 64 connections. A server given a larger memory limit gets a
// proportionally larger cap without being told.
const (
	// maxConnectionsEnv overrides the derived default.
	maxConnectionsEnv = "MUNINN_MBP_MAX_CONNECTIONS"

	// connectionMemoryShare is the fraction of the memory limit that a full
	// set of connections, each holding one frame's worth, may add up to: one
	// part in connectionMemoryShare.
	connectionMemoryShare = 4

	// fallbackMaxConnections is the cap when the process has no memory limit
	// to derive one from: what the derivation gives at the default 4 GiB.
	fallbackMaxConnections = 64

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
	n := memoryLimit / connectionMemoryShare / MaxPayloadSize
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

// refuseAtCap answers a connection the listener has no place for, and closes
// it. The answer is one small frame written to a socket nothing has been
// written to, under a short deadline, so a peer that does not read cannot hold
// up the accept loop. The refusal is logged at most once per capWarnInterval,
// with the number refused since the last line, so a flood of connections does
// not become a flood of log lines.
func (s *Server) refuseAtCap(conn net.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	s.writeErrorFrame(conn, 0, ErrRateLimited, "server is at its connection limit")
	conn.Close()

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
