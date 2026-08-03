package rest

import (
	"errors"
	"net/http"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// StatusNotCortex is the HTTP status for a write that reached a node which is
// not the cluster Cortex.
//
// 421 Misdirected Request is the one status in the registry that means exactly
// this: "the request was directed at a server that is not able to produce a
// response for it" (RFC 9110 §15.5.20). 409 Conflict says the resource is in a
// conflicting state (it is not), 503 Service Unavailable invites a retry against
// the SAME node (exactly wrong), and 307 would make a browser silently re-POST
// credentials to a host the operator never named. 421 is also explicitly
// retryable against a different connection, which is the action we want.
const StatusNotCortex = http.StatusMisdirectedRequest

// HeaderCortexID / HeaderCortexAddr carry the redirect target so a client can
// retry without parsing prose. Both are omitted when no Cortex is known (mid-
// election); the body still says so.
const (
	HeaderCortexID   = "X-Muninn-Cortex-Id"
	HeaderCortexAddr = "X-Muninn-Cortex-Addr"
)

// SetWriteGate installs the cluster single-writer gate. cmd/muninn wires it to
// the ClusterCoordinator when cluster mode is enabled; a nil gate (every
// standalone server) makes withCortexWrite a pass-through.
func (s *Server) SetWriteGate(g func() error) { s.writeGate = g }

// withCortexWrite refuses a write route on a non-Cortex node BEFORE the handler
// runs, with 421 and the leader hint.
//
// It is applied explicitly per route rather than by HTTP method, and that is the
// point. Method-based blanket rejection would also refuse POST
// /api/admin/cluster/failover, /cluster/enable, /replication/promote and the
// rest of the cluster-administration surface — the very routes an operator needs
// on a Lobe to RECOVER from a lost Cortex. A route list is drift-prone; blocking
// the recovery path is worse, and the engine and auth stores both hold the same
// gate as a backstop, so a route missing from this list still cannot diverge —
// it just reports the refusal less precisely.
func (s *Server) withCortexWrite(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.writeGate != nil {
			if err := s.writeGate(); err != nil {
				s.sendNotCortex(r, w, err)
				return
			}
		}
		h(w, r)
	}
}

// sendNotCortex writes the 421 response, including the leader hint as headers
// when the error carries one.
func (s *Server) sendNotCortex(r *http.Request, w http.ResponseWriter, err error) {
	var nle *mbp.NotLeaderError
	if errors.As(err, &nle) {
		if nle.LeaderID != "" {
			w.Header().Set(HeaderCortexID, nle.LeaderID)
		}
		if nle.LeaderAddr != "" {
			w.Header().Set(HeaderCortexAddr, nle.LeaderAddr)
		}
	}
	s.sendError(r, w, StatusNotCortex, mbp.ErrNotCortex, err.Error())
}
