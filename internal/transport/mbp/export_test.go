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
