package mbp

import "time"

// DispatchedFrameClasses returns, by name, the class of every frame type
// dispatchFrame routes. It exists for the external test package
// (mode_matrix_test.go), which cannot see frameRoutes and must not be able to
// borrow its expectations from it.
func DispatchedFrameClasses() map[uint8]string {
	out := make(map[uint8]string, len(frameRoutes))
	for frameType, route := range frameRoutes {
		if route.handle == nil {
			out[frameType] = "no handler"
			continue
		}
		out[frameType] = route.class.String()
	}
	return out
}

// SetDecodeBudgetForTest replaces the per-connection decoded-bytes budget for
// connections accepted after the call, so a test can watch the budget hold a
// frame back without megabyte payloads.
func (s *Server) SetDecodeBudgetForTest(bytes int64) { s.decodeBudget = bytes }

// DecodedInFlightForTest reports the decompressed payload charged to every
// connection's budget right now, in bytes.
func (s *Server) DecodedInFlightForTest() int64 { return s.decodedInFlight.Load() }

// SetHandshakeTimeoutForTest replaces the time a connection has to complete
// HELLO, for connections accepted after the call.
func (s *Server) SetHandshakeTimeoutForTest(d time.Duration) { s.handshakeTimeout = d }

// SetMaxConnectionsForTest replaces the listener's connection cap. Call it
// before the server serves.
func (s *Server) SetMaxConnectionsForTest(n int) { s.maxConns = n }

// ConnectionCapWarningsForTest reports how many times the server has logged
// that it is refusing connections at its cap.
func (s *Server) ConnectionCapWarningsForTest() int64 { return s.capWarnings.Load() }

// SetReplyStallTimeoutForTest replaces how long a connection may take none of
// its replies before it is closed, for connections accepted after the call.
func (s *Server) SetReplyStallTimeoutForTest(d time.Duration) { s.replyStall = d }

// UnwrittenReplyBytesForTest reports the reply payload queued on every
// connection and not yet written, in bytes.
func (s *Server) UnwrittenReplyBytesForTest() int64 { return s.repliesUnwritten.Load() }

// OpenConnectionsForTest reports how many connections the listener holds.
func (s *Server) OpenConnectionsForTest() int64 { return s.openConns.Load() }

// StalledConnectionsClosedForTest reports how many connections the server has
// closed because they stopped taking their replies.
func (s *Server) StalledConnectionsClosedForTest() int64 { return s.stalledClosed.Load() }
