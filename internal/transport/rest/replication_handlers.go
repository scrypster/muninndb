package rest

import "net/http"

// replicationStatusResponse is the response for GET /v1/replication/status.
type replicationStatusResponse struct {
	Enabled    bool               `json:"enabled"`
	Role       string             `json:"role,omitempty"`
	IsLeader   bool               `json:"is_leader,omitempty"`
	Epoch      uint64             `json:"epoch"`
	NodeID     string             `json:"node_id,omitempty"`
	KnownNodes []nodeInfoResponse `json:"known_nodes,omitempty"`
}

// nodeInfoResponse is a single node in the known_nodes list.
type nodeInfoResponse struct {
	NodeID  string `json:"node_id"`
	Addr    string `json:"addr"`
	Role    string `json:"role"`
	LastSeq uint64 `json:"last_seq"`
}

// replicationLagResponse is the response for GET /v1/replication/lag.
type replicationLagResponse struct {
	Lag  uint64 `json:"lag"`
	Role string `json:"role"`
}

// replicationPromoteResponse is the response for POST /v1/replication/promote.
type replicationPromoteResponse struct {
	Triggered bool `json:"triggered"`
}

// handleReplicationStatus returns real cluster status when a coordinator is
// wired in, or {"enabled": false} when cluster mode is disabled.
// GET /v1/replication/status
func (s *Server) handleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, replicationStatusResponse{Enabled: false})
		return
	}

	nodes := s.coordinator.KnownNodes()
	peers := make([]nodeInfoResponse, 0, len(nodes))
	selfID := ""
	if len(nodes) > 0 {
		selfID = nodes[0].NodeID
		for _, n := range nodes[1:] {
			peers = append(peers, nodeInfoResponse{
				NodeID:  n.NodeID,
				Addr:    n.Addr,
				Role:    n.Role.String(),
				LastSeq: n.LastSeq,
			})
		}
	}

	resp := replicationStatusResponse{
		Enabled:    true,
		Role:       s.coordinator.Role().String(),
		IsLeader:   s.coordinator.IsLeader(),
		Epoch:      s.coordinator.CurrentEpoch(),
		NodeID:     selfID,
		KnownNodes: peers,
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// handleReplicationLag returns the replication lag for this node.
// GET /v1/replication/lag
func (s *Server) handleReplicationLag(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrStorageError, "cluster disabled")
		return
	}

	resp := replicationLagResponse{
		Lag:  s.coordinator.ReplicationLag(),
		Role: s.coordinator.Role().String(),
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// handleReplicationPromote triggers a new election to promote a node.
// POST /v1/replication/promote
func (s *Server) handleReplicationPromote(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrStorageError, "cluster disabled")
		return
	}

	if err := s.coordinator.Election().StartElection(r.Context()); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}

	s.sendJSON(w, http.StatusOK, replicationPromoteResponse{Triggered: true})
}

// HandleReplicationStatus is the legacy exported name kept for backward compatibility.
// Delegates to the internal handler.
func (s *Server) HandleReplicationStatus(w http.ResponseWriter, r *http.Request) {
	s.handleReplicationStatus(w, r)
}

// HandleReplicationLag is the legacy exported name kept for backward compatibility.
func (s *Server) HandleReplicationLag(w http.ResponseWriter, r *http.Request) {
	s.handleReplicationLag(w, r)
}

// HandlePromoteReplica is the legacy exported name kept for backward compatibility.
func (s *Server) HandlePromoteReplica(w http.ResponseWriter, r *http.Request) {
	s.handleReplicationPromote(w, r)
}
