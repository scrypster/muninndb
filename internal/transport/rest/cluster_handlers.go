package rest

import (
	"net/http"

	"github.com/scrypster/muninndb/internal/replication"
)

// clusterInfoResponse is the response for GET /v1/cluster/info.
type clusterInfoResponse struct {
	NodeID       string              `json:"node_id"`
	Role         string              `json:"role"`
	IsLeader     bool                `json:"is_leader"`
	Epoch        uint64              `json:"epoch"`
	FencingToken uint64              `json:"fencing_token"`
	CortexID     string              `json:"cortex_id"`
	Members      []clusterMemberInfo `json:"members"`
}

// clusterDisabledResponse is returned when the coordinator is nil.
type clusterDisabledResponse struct {
	Enabled bool `json:"enabled"`
}

// clusterMemberInfo describes a single member in the cluster.
type clusterMemberInfo struct {
	NodeID  string `json:"node_id"`
	Addr    string `json:"addr"`
	Role    string `json:"role"`
	LastSeq uint64 `json:"last_seq"`
}

// clusterHealthResponse is the response for GET /v1/cluster/health.
type clusterHealthResponse struct {
	Status         string `json:"status"`
	Role           string `json:"role,omitempty"`
	IsLeader       bool   `json:"is_leader,omitempty"`
	Epoch          uint64 `json:"epoch,omitempty"`
	ReplicationLag uint64 `json:"replication_lag,omitempty"`
}

// clusterNodesResponse is the response for GET /v1/cluster/nodes.
type clusterNodesResponse struct {
	Nodes []clusterMemberInfo `json:"nodes"`
	Count int                 `json:"count"`
}

// handleClusterInfo returns cluster topology and current node status.
// GET /v1/cluster/info
func (s *Server) handleClusterInfo(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, clusterDisabledResponse{Enabled: false})
		return
	}

	members := membersFromCoordinator(s.coordinator)
	selfID := ""
	if len(members) > 0 {
		selfID = members[0].NodeID
	}

	resp := clusterInfoResponse{
		NodeID:       selfID,
		Role:         s.coordinator.Role().String(),
		IsLeader:     s.coordinator.IsLeader(),
		Epoch:        s.coordinator.CurrentEpoch(),
		FencingToken: s.coordinator.FencingToken(),
		CortexID:     s.coordinator.CortexID(),
		Members:      members,
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// handleClusterHealth returns a health check suitable for load balancer probes.
// GET /v1/cluster/health
func (s *Server) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, clusterHealthResponse{Status: "ok", Role: "standalone"})
		return
	}

	role := s.coordinator.Role()
	lag := s.coordinator.ReplicationLag()

	status := clusterStatus(role, lag)
	resp := clusterHealthResponse{
		Status:         status,
		Role:           role.String(),
		IsLeader:       s.coordinator.IsLeader(),
		Epoch:          s.coordinator.CurrentEpoch(),
		ReplicationLag: lag,
	}

	httpStatus := http.StatusOK
	if status == "down" {
		httpStatus = http.StatusServiceUnavailable
	}
	s.sendJSON(w, httpStatus, resp)
}

// handleClusterNodes returns the member list for cluster discovery.
// GET /v1/cluster/nodes
func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, clusterNodesResponse{Nodes: []clusterMemberInfo{}, Count: 0})
		return
	}

	members := membersFromCoordinator(s.coordinator)
	s.sendJSON(w, http.StatusOK, clusterNodesResponse{Nodes: members, Count: len(members)})
}

// membersFromCoordinator converts coordinator node info into clusterMemberInfo slices.
func membersFromCoordinator(coord *replication.ClusterCoordinator) []clusterMemberInfo {
	nodes := coord.ReconciledMembers()
	members := make([]clusterMemberInfo, 0, len(nodes))
	for _, n := range nodes {
		members = append(members, clusterMemberInfo{
			NodeID:  n.NodeID,
			Addr:    n.Addr,
			Role:    n.Role.String(),
			LastSeq: n.LastSeq,
		})
	}
	return members
}

// cognitiveConsistencyResponse is the response for GET /v1/cluster/cognitive/consistency.
type cognitiveConsistencyResponse struct {
	Score      float64            `json:"score"`
	Assessment string             `json:"assessment"`
	NodeScores map[string]float64 `json:"node_scores"`
	SampledAt  string             `json:"sampled_at"`
}

// handleCognitiveConsistency returns the last computed CCS measurement.
// GET /v1/cluster/cognitive/consistency
func (s *Server) handleCognitiveConsistency(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, cognitiveConsistencyResponse{
			Score:      1.0,
			Assessment: "excellent",
			NodeScores: map[string]float64{},
			SampledAt:  "",
		})
		return
	}

	result := s.coordinator.CognitiveConsistency()
	resp := cognitiveConsistencyResponse{
		Score:      result.Score,
		Assessment: result.Assessment,
		NodeScores: result.NodeScores,
		SampledAt:  result.SampledAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// clusterStatus derives the health status string from role and replication lag.
func clusterStatus(role replication.NodeRole, lag uint64) string {
	if role == replication.RoleUnknown {
		return "down"
	}
	if role == replication.RolePrimary {
		return "ok"
	}
	// Lobe (replica/observer/sentinel) lag thresholds
	if lag >= 10000 {
		return "down"
	}
	if lag >= 1000 {
		return "degraded"
	}
	return "ok"
}
