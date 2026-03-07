package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/replication"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type enableClusterRequest struct {
	Role          string `json:"role"`
	BindAddr      string `json:"bind_addr"`
	ClusterSecret string `json:"cluster_secret"`
	CortexAddr    string `json:"cortex_addr"`
}

type clusterTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	TTLSecs   int       `json:"ttl_seconds"`
}

type clusterSettingsRequest struct {
	HeartbeatMS   *int  `json:"heartbeat_ms"`
	SDOWNBeats    *int  `json:"sdown_beats"`
	CCSIntervalS  *int  `json:"ccs_interval_seconds"`
	ReconcileHeal *bool `json:"reconcile_on_heal"`
}

type testNodeRequest struct {
	Addr string `json:"addr"`
}

type testNodeResponse struct {
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *Server) handleAdminClusterToken(w http.ResponseWriter, r *http.Request) {
	tm := s.joinTokenManager()
	if tm == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "cluster not enabled")
		return
	}
	tok, err := tm.Generate()
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("generate token: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, clusterTokenResponse{
		Token:     tok,
		ExpiresAt: time.Now().Add(tm.TTL()),
		TTLSecs:   int(tm.TTL().Seconds()),
	})
}

func (s *Server) handleAdminClusterRegenerateToken(w http.ResponseWriter, r *http.Request) {
	s.handleAdminClusterToken(w, r)
}

func (s *Server) handleAdminClusterEnable(w http.ResponseWriter, r *http.Request) {
	// Idempotent: if cluster is already running, return success.
	if s.coordinator != nil {
		s.sendJSON(w, http.StatusOK, map[string]any{"enabled": true, "role": s.coordinator.Role()})
		return
	}
	var req enableClusterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON")
		return
	}
	if req.Role == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "role is required")
		return
	}
	validRoles := map[string]bool{"primary": true, "replica": true, "sentinel": true, "observer": true}
	if !validRoles[req.Role] {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "role must be one of: primary, replica, sentinel, observer")
		return
	}
	if req.BindAddr == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "bind_addr is required")
		return
	}
	if req.Role != "primary" && req.CortexAddr == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "cortex_addr is required for non-primary roles")
		return
	}
	// Start from defaults so fields like LeaseTTL and HeartbeatMS are never
	// persisted as zero (which would cause a crash on the next restart).
	cfg := config.ClusterDefaults()
	cfg.Enabled = true
	cfg.Role = req.Role
	cfg.BindAddr = req.BindAddr
	cfg.ClusterSecret = req.ClusterSecret
	if req.CortexAddr != "" {
		cfg.Seeds = []string{req.CortexAddr}
	}
	if err := s.enableClusterRuntime(r.Context(), cfg); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("enable cluster: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"enabled": true, "role": req.Role})
}

func (s *Server) handleAdminClusterDisable(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	s.DisableCluster()
	if err := s.persistClusterDisabled(); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("persist config: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

func (s *Server) handleAdminClusterAddNode(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "cluster not enabled")
		return
	}
	var req struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON")
		return
	}
	if req.Addr == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "addr is required")
		return
	}
	tm := s.joinTokenManager()
	if tm != nil {
		if err := tm.Validate(req.Token); err != nil {
			s.sendError(r, w, http.StatusUnauthorized, ErrAuthFailed, fmt.Sprintf("token: %v", err))
			return
		}
	}
	s.coordinator.ConnManager().AddPeer("pending-"+req.Addr, req.Addr)
	s.sendJSON(w, http.StatusAccepted, map[string]any{
		"addr":    req.Addr,
		"message": "peer registered; node should now connect and complete join handshake",
	})
}

func (s *Server) handleAdminClusterRemoveNode(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "cluster not enabled")
		return
	}
	nodeID := r.PathValue("id")
	if nodeID == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "node id is required in path")
		return
	}
	if r.URL.Query().Get("drain") == "true" {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if s.coordinator.ReplicaLag(nodeID) == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err := s.coordinator.RemoveNode(nodeID); err != nil {
		if errors.Is(err, replication.ErrSelfRemoval) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "cannot remove self from cluster")
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("remove node: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"removed": nodeID})
}

func (s *Server) handleAdminClusterFailover(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "cluster not enabled")
		return
	}
	if !s.coordinator.IsLeader() {
		s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, "this node is not the current Cortex")
		return
	}
	var req struct {
		TargetNodeID string `json:"target_node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON")
		return
	}
	if req.TargetNodeID == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "target_node_id is required")
		return
	}
	if err := s.coordinator.GracefulFailover(r.Context(), req.TargetNodeID); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("failover: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"initiated": true, "target": req.TargetNodeID})
}

func (s *Server) handleAdminClusterRotateTLS(w http.ResponseWriter, r *http.Request) {
	if s.coordinator == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "cluster not enabled")
		return
	}
	tls := s.coordinator.TLSManager()
	if tls == nil {
		s.sendError(r, w, http.StatusServiceUnavailable, ErrShardUnavailable, "TLS not configured")
		return
	}
	nodes := s.coordinator.KnownNodes()
	nodeID := ""
	if len(nodes) > 0 {
		nodeID = nodes[0].NodeID
	}
	if err := tls.RotateCert(nodeID); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, fmt.Sprintf("rotate: %v", err))
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"rotated": true})
}

func (s *Server) handleAdminClusterSettings(w http.ResponseWriter, r *http.Request) {
	var req clusterSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON")
		return
	}
	if req.HeartbeatMS != nil && *req.HeartbeatMS <= 0 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "heartbeat_ms must be > 0")
		return
	}
	if req.SDOWNBeats != nil && *req.SDOWNBeats < 1 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "sdown_beats must be >= 1")
		return
	}
	if req.CCSIntervalS != nil && *req.CCSIntervalS < 5 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "ccs_interval_seconds must be >= 5")
		return
	}
	if s.coordinator != nil && req.HeartbeatMS != nil {
		s.coordinator.MSP().SetHeartbeatInterval(time.Duration(*req.HeartbeatMS) * time.Millisecond)
	}
	_ = s.applyAndPersistSettings(req)
	s.sendJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) handleAdminClusterTestNode(w http.ResponseWriter, r *http.Request) {
	var req testNodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON")
		return
	}
	if req.Addr == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "addr is required")
		return
	}
	start := time.Now()
	reachable, testErr := replication.TestNodeReachability(r.Context(), req.Addr)
	resp := testNodeResponse{Reachable: reachable, LatencyMS: time.Since(start).Milliseconds()}
	if testErr != nil {
		resp.Error = testErr.Error()
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAdminClusterEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.sendError(r, w, http.StatusInternalServerError, ErrInternal, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if s.coordinator == nil {
		fmt.Fprintf(w, "event: error\ndata: {\"error\":\"cluster not enabled\"}\n\n")
		flusher.Flush()
		return
	}
	ch, unsub := s.coordinator.RepLog().Subscribe()
	defer unsub()

	ctx := r.Context()
	var lastSeq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			entries, err := s.coordinator.RepLog().ReadSince(lastSeq, 50)
			if err != nil {
				continue
			}
			for _, e := range entries {
				data, _ := json.Marshal(map[string]any{
					"seq": e.Seq, "op": e.Op.String(), "key": string(e.Key),
				})
				fmt.Fprintf(w, "event: entry\ndata: %s\n\n", data)
				lastSeq = e.Seq
			}
			flusher.Flush()
		}
	}
}

// joinTokenManager returns the coordinator's token manager, or nil.
func (s *Server) joinTokenManager() *replication.JoinTokenManager {
	if s.coordinator == nil {
		return nil
	}
	return s.coordinator.JoinTokenManager()
}
