package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/scrypster/muninndb/internal/auth"
	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/plugin"
)

// These handlers are mounted at /api/admin/ and require admin session auth.

// countingWriter wraps http.ResponseWriter and tracks how many bytes have been written.
// It is used by the export handler to detect whether streaming has begun before
// deciding how to handle an error from ExportVault.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(p)
	cw.n += int64(n)
	return n, err
}

// isValidVaultName returns true if name is a valid vault name: 1–64 characters,
// containing only lowercase letters, digits, hyphens, and underscores.
func isValidVaultName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// canonicalize returns a lowercase alphanumeric-only string for collision detection.
// Used to detect near-duplicate vault names (e.g. "datadog-clone" == "datadogclone").
func canonicalize(name string) string {
	return strings.Map(func(r rune) rune {
		lower := unicode.ToLower(r)
		if lower >= 'a' && lower <= 'z' || lower >= '0' && lower <= '9' {
			return lower
		}
		return -1
	}, name)
}

// vaultNameCollision checks if newName canonically collides with any of the
// provided existing vault names. It returns the first conflicting name found,
// or empty string if there is no collision. An exact name match is not a
// collision (it represents an update/overwrite of an existing vault).
func vaultNameCollision(existingNames []string, newName string) string {
	canon := canonicalize(newName)
	for _, existing := range existingNames {
		if existing == newName {
			// Exact match is an update, not a collision.
			continue
		}
		if canonicalize(existing) == canon {
			return existing
		}
	}
	return ""
}

func (s *Server) handleCreateAPIKey(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Vault   string `json:"vault"`
			Label   string `json:"label"`
			Mode    string `json:"mode"`
			Expires string `json:"expires"` // optional: duration like "90d", "1y", or RFC3339 date
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}
		if req.Vault == "" {
			req.Vault = "default"
		}
		if !isValidVaultName(req.Vault) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name")
			return
		}
		if len(req.Label) > 256 {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "label too long")
			return
		}
		if req.Mode != "" && req.Mode != "full" && req.Mode != "observe" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "mode must be 'full' or 'observe'")
			return
		}
		var expiresAt *time.Time
		if req.Expires != "" {
			t, err := parseKeyExpiry(req.Expires)
			if err != nil {
				s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid expires: "+err.Error())
				return
			}
			expiresAt = &t
		}
		token, key, err := authStore.GenerateAPIKey(req.Vault, req.Label, req.Mode, expiresAt)
		if err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, err.Error())
			return
		}
		s.sendJSON(w, http.StatusCreated, map[string]interface{}{
			"token": token, // shown once
			"key":   key,
		})
	}
}

// parseKeyExpiry parses an expiry specification into an absolute time.
// Accepted formats:
//   - Nd  — N days from now (e.g. "90d")
//   - Ny  — N years from now (e.g. "1y")
//   - RFC3339 date/datetime string (e.g. "2027-01-01" or "2027-01-01T00:00:00Z")
func parseKeyExpiry(s string) (time.Time, error) {
	now := time.Now()
	if len(s) >= 2 {
		unit := s[len(s)-1]
		numStr := s[:len(s)-1]
		switch unit {
		case 'd', 'D':
			var n int
			if _, err := fmt.Sscanf(numStr, "%d", &n); err == nil && n > 0 {
				return now.AddDate(0, 0, n), nil
			}
		case 'y', 'Y':
			var n int
			if _, err := fmt.Sscanf(numStr, "%d", &n); err == nil && n > 0 {
				return now.AddDate(n, 0, 0), nil
			}
		}
	}
	// Try RFC3339 datetime.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if t.Before(now) {
			return time.Time{}, fmt.Errorf("expiry date is in the past")
		}
		return t, nil
	}
	// Try date-only (YYYY-MM-DD).
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if t.Before(now) {
			return time.Time{}, fmt.Errorf("expiry date is in the past")
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("use Nd, Ny, or RFC3339 date (e.g. '90d', '1y', '2027-01-01')")
}

func (s *Server) handleListAPIKeys(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vault := r.URL.Query().Get("vault")
		if vault == "" {
			vault = "default"
		}
		if !isValidVaultName(vault) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name")
			return
		}
		keys, err := authStore.ListAPIKeys(vault)
		if err != nil {
			if errors.Is(err, engine.ErrVaultNotFound) {
				s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, fmt.Sprintf("vault %q not found", vault))
				return
			}
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to list API keys")
			return
		}
		s.sendJSON(w, http.StatusOK, map[string]interface{}{"keys": keys})
	}
}

func (s *Server) handleRevokeAPIKey(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		vault := r.URL.Query().Get("vault")
		if vault == "" {
			vault = "default"
		}
		if !isValidVaultName(vault) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name")
			return
		}
		if err := authStore.RevokeAPIKey(vault, id); err != nil {
			s.sendError(r, w, http.StatusNotFound, ErrEngramNotFound, err.Error())
			return
		}
		s.sendJSON(w, http.StatusOK, map[string]interface{}{"revoked": id})
	}
}

func (s *Server) handleChangeAdminPassword(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username    string `json:"username"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}
		if req.NewPassword == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_password is required")
			return
		}
		if err := authStore.ChangeAdminPassword(req.Username, req.NewPassword); err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}
		s.sendJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Server) handleSetVaultConfig(authStore *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cfg auth.VaultConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
			return
		}
		if cfg.Name == "" {
			cfg.Name = "default"
		}
		if !isValidVaultName(cfg.Name) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name")
			return
		}

		// Collision check: detect near-duplicate vault names unless ?force=true.
		if r.URL.Query().Get("force") != "true" {
			existingNames := s.collectVaultNames(r, authStore)
			if conflict := vaultNameCollision(existingNames, cfg.Name); conflict != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{
					"error":      "vault name collision detected",
					"code":       "VAULT_NAME_COLLISION",
					"conflict":   conflict,
					"normalized": canonicalize(cfg.Name),
				})
				return
			}
		}

		if err := authStore.SetVaultConfig(cfg); err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}
		s.sendJSON(w, http.StatusOK, cfg)
	}
}

// collectVaultNames merges vault names from the engine and the auth store into
// a deduplicated slice. Used for canonicalize-based collision detection.
func (s *Server) collectVaultNames(r *http.Request, authStore *auth.Store) []string {
	seen := make(map[string]struct{})
	var names []string

	if s.engine != nil {
		if engineVaults, err := s.engine.ListVaults(r.Context()); err == nil {
			for _, n := range engineVaults {
				if _, ok := seen[n]; !ok {
					seen[n] = struct{}{}
					names = append(names, n)
				}
			}
		}
	}

	if authStore != nil {
		if cfgs, err := authStore.ListVaultConfigs(); err == nil {
			for _, cfg := range cfgs {
				if _, ok := seen[cfg.Name]; !ok {
					seen[cfg.Name] = struct{}{}
					names = append(names, cfg.Name)
				}
			}
		}
	}

	return names
}

// handleGetVaultPlasticity returns the raw PlasticityConfig (may be nil) and
// the fully-resolved config for the named vault.
func (s *Server) handleGetVaultPlasticity(as *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
			return
		}
		if !isValidVaultName(name) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
			return
		}
		vc, err := as.GetVaultConfig(name)
		if err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to read vault config: "+err.Error())
			return
		}
		resolved := auth.ResolvePlasticity(vc.Plasticity)
		s.sendJSON(w, http.StatusOK, map[string]any{
			"config":   vc.Plasticity,
			"resolved": resolved,
		})
	}
}

// handlePutVaultPlasticity updates the PlasticityConfig for the named vault.
func (s *Server) handlePutVaultPlasticity(as *auth.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
			return
		}
		if !isValidVaultName(name) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
			return
		}
		var cfg auth.PlasticityConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid JSON: "+err.Error())
			return
		}
		// Validate override field ranges if provided
		if cfg.HopDepth != nil && (*cfg.HopDepth < 0 || *cfg.HopDepth > 8) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "hop_depth must be 0–8")
			return
		}
		for _, fw := range []*float32{cfg.SemanticWeight, cfg.FTSWeight, cfg.RelevanceFloor} {
			if fw != nil && (*fw < 0 || *fw > 1) {
				s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "weight fields must be 0–1")
				return
			}
		}
		if cfg.TemporalHalflife != nil && *cfg.TemporalHalflife <= 0 {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "temporal_halflife must be > 0")
			return
		}
		if cfg.TraversalProfile != nil {
			if *cfg.TraversalProfile == "" {
				cfg.TraversalProfile = nil
			} else if !activation.ValidProfileName(*cfg.TraversalProfile) {
				s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram,
					fmt.Sprintf("invalid traversal_profile %q: must be one of: default, causal, confirmatory, adversarial, structural", *cfg.TraversalProfile))
				return
			}
		}
		if cfg.RecallMode != nil && *cfg.RecallMode != "" {
			if !auth.ValidRecallMode(*cfg.RecallMode) {
				s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram,
					fmt.Sprintf("invalid recall_mode %q: valid values are semantic, recent, balanced, deep", *cfg.RecallMode))
				return
			}
		}
		preset := cfg.Preset
		if preset == "" {
			preset = "default"
		}
		if !auth.ValidPlasticityPreset(preset) {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "unknown preset: "+preset)
			return
		}
		cfg.Preset = preset

		vc, err := as.GetVaultConfig(name)
		if err != nil {
			slog.Warn("put vault plasticity: failed to read existing config, creating new",
				"vault", name, "err", err)
			vc = auth.VaultConfig{Name: name}
		}
		vc.Plasticity = &cfg
		if err := as.SetVaultConfig(vc); err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "store error: "+err.Error())
			return
		}
		resolved := auth.ResolvePlasticity(&cfg)
		s.sendJSON(w, http.StatusOK, map[string]any{
			"config":   &cfg,
			"resolved": resolved,
		})
	}
}

// MCPInfoResponse is the response for GET /api/admin/mcp-info.
type MCPInfoResponse struct {
	// URL is the full MCP endpoint URL that AI tools connect to.
	URL string `json:"url"`
	// TokenConfigured indicates whether a bearer token is required for MCP access.
	TokenConfigured bool `json:"token_configured"`
}

// handleMCPInfo returns the MCP endpoint URL and token status for the Connect UI.
func (s *Server) handleMCPInfo(w http.ResponseWriter, r *http.Request) {
	addr := s.mcpAddr
	if addr == "" {
		addr = ":8750"
	}
	// Use net.SplitHostPort to correctly handle all valid net.Listen address forms:
	// bare ":port", "0.0.0.0:port", "host:port", and IPv6 bracket notation "[::1]:port".
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Fallback for misconfigured or empty addresses.
		host = "127.0.0.1"
		port = "8750"
	}
	// A wildcard listen address (empty string, 0.0.0.0, or ::) means the server
	// is reachable on any interface; use 127.0.0.1 to avoid IPv6 dual-stack ambiguity.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	mcpURL := "http://" + host + ":" + port + "/mcp"
	s.sendJSON(w, http.StatusOK, MCPInfoResponse{
		URL:             mcpURL,
		TokenConfigured: s.mcpHasToken,
	})
}

// handleEntityGraph returns the entity→relationship graph for the vault.
// This proxies engine.ExportGraph so the browser does not need to call the MCP
// server directly — which fails in remote deployments where the MCP address
// resolves to 127.0.0.1 from the server's perspective but not the browser's.
func (s *Server) handleEntityGraph(w http.ResponseWriter, r *http.Request) {
	vault := r.URL.Query().Get("vault")
	if vault == "" {
		vault = "default"
	}
	if !isValidVaultName(vault) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid vault name")
		return
	}
	includeEngrams := r.URL.Query().Get("include_engrams") != "false"

	graph, err := s.engine.ExportGraph(r.Context(), vault, includeEngrams)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}

	resp := EntityGraphResponse{
		Nodes: make([]EntityGraphNode, 0, len(graph.Nodes)),
		Edges: make([]EntityGraphEdge, 0, len(graph.Edges)),
	}
	for _, n := range graph.Nodes {
		resp.Nodes = append(resp.Nodes, EntityGraphNode{ID: n.ID, Type: n.Type})
	}
	for _, e := range graph.Edges {
		resp.Edges = append(resp.Edges, EntityGraphEdge{
			From:    e.From,
			To:      e.To,
			RelType: e.RelType,
			Weight:  e.Weight,
		})
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// EmbedStatusResponse is the response for GET /api/admin/embed/status.
type EmbedStatusResponse struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Enabled       bool   `json:"enabled"`
	EmbeddedCount int64  `json:"embedded_count"` // -1 = unknown
	TotalCount    int64  `json:"total_count"`    // -1 = unknown
	Indexing      bool   `json:"indexing"`
}

// handleEmbedStatus returns the current embedder configuration and indexing state.
func (s *Server) handleEmbedStatus(w http.ResponseWriter, r *http.Request) {
	resp, err := s.engine.Stat(r.Context(), &StatRequest{})
	totalCount := int64(-1)
	if err == nil {
		totalCount = int64(resp.EngramCount)
	}

	embeddedCount := s.engine.CountEmbedded(r.Context())

	s.sendJSON(w, http.StatusOK, EmbedStatusResponse{
		Provider:      s.embedProvider,
		Model:         s.embedModel,
		Enabled:       s.embedProvider != "" && s.embedProvider != "none",
		EmbeddedCount: embeddedCount,
		TotalCount:    totalCount,
		Indexing:      embeddedCount >= 0 && totalCount >= 0 && embeddedCount < totalCount,
	})
}

// PluginStatusResponse is one entry in GET /api/admin/plugins.
type PluginStatusResponse struct {
	Name      string    `json:"name"`
	Tier      int       `json:"tier"` // 2=embed, 3=enrich
	Healthy   bool      `json:"healthy"`
	LastCheck time.Time `json:"last_check"`
	Error     string    `json:"error,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
}

// handlePlugins returns the list of registered plugins with runtime status.
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if s.pluginRegistry == nil {
		s.sendJSON(w, http.StatusOK, []PluginStatusResponse{})
		return
	}
	list := s.pluginRegistry.List()
	out := make([]PluginStatusResponse, 0, len(list))
	for _, p := range list {
		entry := PluginStatusResponse{
			Name:      p.Name,
			Tier:      int(p.Tier),
			Healthy:   p.Healthy,
			LastCheck: p.LastCheck,
			Error:     p.Error,
		}
		if p.Tier == plugin.TierEmbed {
			entry.Provider = s.embedProvider
			entry.Model = s.embedModel
		} else if p.Tier == plugin.TierEnrich {
			entry.Provider = s.enrichProvider
			entry.Model = s.enrichModel
		}
		out = append(out, entry)
	}
	s.sendJSON(w, http.StatusOK, out)
}

// handleGetPluginConfig returns the saved plugin configuration from disk.
func (s *Server) handleGetPluginConfig(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		s.sendJSON(w, http.StatusOK, plugincfg.PluginConfig{})
		return
	}
	cfg, err := plugincfg.LoadPluginConfig(s.dataDir)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to load plugin config: "+err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, cfg)
}

// handlePutPluginConfig saves plugin configuration to disk.
// A server restart is required for changes to take effect.
func (s *Server) handlePutPluginConfig(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "data directory not configured")
		return
	}
	var cfg plugincfg.PluginConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body: "+err.Error())
		return
	}
	if err := plugincfg.SavePluginConfig(s.dataDir, cfg); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to save plugin config: "+err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, cfg)
}

// handleRenameVault renames a vault (metadata-only, no engram data changes).
// POST /api/admin/vaults/{name}/rename
// Body: {"new_name": "new-vault-name"}
// Response 200: {"old_name": "...", "new_name": "..."}
func (s *Server) handleRenameVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	var req struct {
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewName == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_name required in body")
		return
	}
	if !isValidVaultName(req.NewName) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_name must be 1–64 lowercase alphanumeric characters, hyphens, or underscores")
		return
	}
	if req.NewName == name {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_name must differ from current name")
		return
	}

	// Collision check: detect near-duplicate target name unless ?force=true.
	// Exclude the vault being renamed so it doesn't collide with itself.
	if r.URL.Query().Get("force") != "true" {
		existingNames := s.collectVaultNames(r, s.authStore)
		// Remove the current vault name so renaming doesn't self-collide.
		filtered := make([]string, 0, len(existingNames))
		for _, n := range existingNames {
			if n != name {
				filtered = append(filtered, n)
			}
		}
		if conflict := vaultNameCollision(filtered, req.NewName); conflict != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error":      "vault name collision detected",
				"code":       "VAULT_NAME_COLLISION",
				"conflict":   conflict,
				"normalized": canonicalize(req.NewName),
			})
			return
		}
	}

	if err := s.engine.RenameVault(r.Context(), name, req.NewName); err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrVaultJobActive) {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, err.Error())
			return
		}
		if errors.Is(err, engine.ErrVaultNameCollision) {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]string{
		"old_name": name,
		"new_name": req.NewName,
	})
}

// handleDeleteVault deletes a vault and all its data.
// Requires X-Allow-Default: true to delete the "default" vault.
func (s *Server) handleDeleteVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	if name == "default" {
		if r.Header.Get("X-Allow-Default") != "true" {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, "cannot delete default vault without X-Allow-Default: true header")
			return
		}
	}
	if err := s.engine.DeleteVault(r.Context(), name); err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrVaultJobActive) {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClearVault removes all engrams from a vault, leaving the vault intact.
// Requires X-Allow-Default: true to clear the "default" vault.
func (s *Server) handleClearVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	if name == "default" {
		if r.Header.Get("X-Allow-Default") != "true" {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, "cannot clear default vault without X-Allow-Default: true header")
			return
		}
	}
	if err := s.engine.ClearVault(r.Context(), name); err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCloneVault starts an async clone of the named vault.
// POST /api/admin/vaults/{name}/clone
// Body: {"new_name": "vault-b"}
// Response 202: {"job_id": "..."}
func (s *Server) handleCloneVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	var req struct {
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewName == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_name required in body")
		return
	}
	if !isValidVaultName(req.NewName) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "new_name must be 1–64 lowercase alphanumeric characters, hyphens, or underscores")
		return
	}
	if req.NewName == name {
		s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, "cannot clone a vault onto itself")
		return
	}
	job, err := s.engine.StartClone(r.Context(), name, req.NewName)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrVaultNameCollision) {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": job.ID})
}

// handleMergeVault starts an async merge of the named source vault into a target vault.
// POST /api/admin/vaults/{name}/merge-into
// Body: {"target": "vault-b", "delete_source": false}
// Response 202: {"job_id": "..."}
func (s *Server) handleMergeVault(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("name")
	if source == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "source vault name required")
		return
	}
	var req struct {
		Target       string `json:"target"`
		DeleteSource bool   `json:"delete_source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "target required in body")
		return
	}
	if !isValidVaultName(req.Target) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "target must be 1–64 lowercase alphanumeric characters, hyphens, or underscores")
		return
	}
	if source == req.Target {
		s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, "source and target must be different")
		return
	}
	job, err := s.engine.StartMerge(r.Context(), source, req.Target, req.DeleteSource)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": job.ID})
}

// handleExportVault exports a vault as a .muninn archive.
// GET /api/admin/vaults/{name}/export
// Query params: reset_metadata=true (optional)
// Response: application/gzip stream with Content-Disposition attachment.
func (s *Server) handleExportVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	resetMeta := r.URL.Query().Get("reset_metadata") == "true"

	filename := name + ".muninn"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")

	// Wrap the response writer to track how many bytes have been streamed.
	// This lets us distinguish between a pre-stream error (safe to send HTTP 500)
	// and a mid-stream error (headers already committed; must abort the connection).
	cw := &countingWriter{ResponseWriter: w}

	_, err := s.engine.ExportVault(r.Context(), name, s.embedModel, 0, resetMeta, cw)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) && cw.n == 0 {
			// No bytes written yet — safe to send a proper JSON error response.
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		if cw.n == 0 {
			// Error before any bytes were written — safe to send HTTP 500.
			slog.Error("rest: export vault failed before streaming", "vault", name, "err", err)
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "export failed")
			return
		}
		// If ExportVault fails after streaming has begun (cw.n > 0),
		// we abort the handler to signal the client with a truncated stream.
		// The client's gzip decoder will return an error on the incomplete data.
		// See https://pkg.go.dev/net/http#ErrAbortHandler
		slog.Error("rest: export vault failed mid-stream; aborting connection", "vault", name, "err", err, "bytes_written", cw.n)
		panic(http.ErrAbortHandler)
	}
}

// handleImportVault imports a .muninn archive into a new vault.
// POST /api/admin/vaults/import?vault=NAME
// Body: raw .muninn archive (gzip'd tar)
// Response 202: {"job_id": "..."}
func (s *Server) handleImportVault(w http.ResponseWriter, r *http.Request) {
	vaultName := r.URL.Query().Get("vault")
	if vaultName == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault query parameter required")
		return
	}
	if !isValidVaultName(vaultName) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	resetMeta := r.URL.Query().Get("reset_metadata") == "true"

	job, err := s.engine.StartImport(r.Context(), vaultName, s.embedModel, 0, resetMeta, r.Body)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrVaultNameCollision) {
			s.sendError(r, w, http.StatusConflict, ErrVaultForbidden, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": job.ID})
}

// handleReindexFTSVault rebuilds the FTS index for a vault using the current
// Porter2-stemmed tokenizer.
// POST /api/admin/vaults/{name}/reindex-fts
// Response 200: {"vault": "...", "engrams_reindexed": N}
func (s *Server) handleReindexFTSVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}
	count, err := s.engine.ReindexFTSVault(r.Context(), name)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"vault":             name,
		"engrams_reindexed": count,
	})
}

// handleReembedVault clears stale embeddings for a vault so the RetroactiveProcessor
// re-embeds everything with the current model.
// POST /api/admin/vaults/{name}/reembed
// Body (optional): {"model": "bge-small-en-v1.5"}
// Response 202: {"job_id": "..."}
func (s *Server) handleReembedVault(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}

	// Parse optional model from body.
	model := s.embedModel
	var req struct {
		Model string `json:"model"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Model != "" {
			model = req.Model
		}
	}

	job, err := s.engine.StartReembedVault(r.Context(), name, model)
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"job_id": job.ID})
}

// handleExportVaultMarkdown exports a vault as a markdown .tgz archive.
// GET /api/admin/vaults/{name}/export-markdown
// Response: application/gzip stream with Content-Disposition attachment.
func (s *Server) handleExportVaultMarkdown(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name required")
		return
	}
	if !isValidVaultName(name) {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "vault name contains invalid characters")
		return
	}

	// Check vault exists by listing engrams (limit 1).
	_, err := s.engine.ListEngrams(r.Context(), &ListEngramsRequest{Vault: name, Limit: 1})
	if err != nil {
		if errors.Is(err, engine.ErrVaultNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, fmt.Sprintf("vault %q not found", name))
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, "failed to access vault")
		return
	}

	filename := name + ".markdown.tgz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")

	if err := writeVaultMarkdownExport(r.Context(), s.engine, name, w); err != nil {
		slog.Error("rest: markdown export failed", "vault", name, "err", err)
		// Headers already committed; nothing to do but log.
	}
}

// handleVaultJobStatus returns the current status of a vault clone/merge job.
// GET /api/admin/vaults/{name}/job-status?job_id=...
// Response 200: StatusSnapshot JSON
func (s *Server) handleVaultJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	if jobID == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "job_id query parameter required")
		return
	}
	job, ok := s.engine.GetVaultJob(jobID)
	if !ok {
		s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, "job not found")
		return
	}
	name := r.PathValue("name")
	if job.Source != name && job.Target != name {
		s.sendError(r, w, http.StatusNotFound, ErrVaultNotFound, "job not found for this vault")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job.Snapshot())
}
