package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
)

// PluginFactory is a factory function for creating plugins.
type PluginFactory func(tier string, cfg PluginConfig) (Plugin, error)

// AdminHandler handles the POST /api/admin/plugins endpoint.
type AdminHandler struct {
	registry *Registry
	store    PluginStore
	factory  PluginFactory
	procs    map[string]*RetroactiveProcessor // name -> processor
	mu       sync.Mutex
}

// AdminRequest is the incoming JSON request for plugin management.
type AdminRequest struct {
	Action   string            `json:"action"`   // "add" or "remove"
	Tier     string            `json:"tier"`     // "embed" or "enrich"
	Provider string            `json:"provider"` // provider URL
	APIKey   string            `json:"api_key"`
	Name     string            `json:"name"`     // for remove
	Options  map[string]string `json:"options"`
}

// AdminResponse is the JSON response.
type AdminResponse struct {
	OK              bool   `json:"ok"`
	PluginName      string `json:"plugin_name,omitempty"`
	RetroactiveTotal int64 `json:"retroactive_total,omitempty"`
	Message         string `json:"message,omitempty"`
}

// NewAdminHandler creates a new admin handler.
func NewAdminHandler(registry *Registry, store PluginStore, factory PluginFactory) *AdminHandler {
	return &AdminHandler{
		registry: registry,
		store:    store,
		factory:  factory,
		procs:    make(map[string]*RetroactiveProcessor),
	}
}

// ServeHTTP handles the HTTP request.
func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: "method not allowed",
		})
		return
	}

	ctx := r.Context()

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: "failed to read request body",
		})
		return
	}

	var req AdminRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("invalid JSON: %v", err),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	switch req.Action {
	case "add":
		h.handleAdd(w, ctx, req)
	case "remove":
		h.handleRemove(w, ctx, req)
	default:
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("unknown action: %q", req.Action),
		})
	}
}

func (h *AdminHandler) handleAdd(w http.ResponseWriter, ctx context.Context, req AdminRequest) {
	// Validate request
	if req.Provider == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: "provider URL is required",
		})
		return
	}

	if req.Tier == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: "tier is required (embed or enrich)",
		})
		return
	}

	// Parse and validate provider URL
	_, err := ParseProviderURL(req.Provider)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("invalid provider URL: %v", err),
		})
		return
	}

	// Create plugin config
	pluginCfg := PluginConfig{
		ProviderURL: req.Provider,
		APIKey:      req.APIKey,
		Options:     req.Options,
	}

	// Create plugin via factory
	plugin, err := h.factory(req.Tier, pluginCfg)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("failed to create plugin: %v", err),
		})
		return
	}

	// Register plugin
	if err := h.registry.Register(plugin); err != nil {
		if errClose := plugin.Close(); errClose != nil {
			slog.Warn("failed to close plugin on registration error", "error", errClose)
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("failed to register plugin: %v", err),
		})
		return
	}

	slog.Info("plugin registered", "name", plugin.Name(), "tier", req.Tier)

	// Count unprocessed engrams
	var flagBit uint8
	if req.Tier == "embed" {
		flagBit = DigestEmbed
	} else {
		flagBit = DigestEnrich
	}

	var skipFlags uint8
	if flagBit == DigestEmbed {
		skipFlags = DigestEmbedFailed
	} else {
		skipFlags = DigestEnrichFailed
	}
	total, err := h.store.CountWithoutFlag(ctx, flagBit, skipFlags)
	if err != nil {
		slog.Warn("failed to count unprocessed engrams", "error", err)
		total = 0
	}

	// Start retroactive processor
	h.mu.Lock()
	processor := NewRetroactiveProcessor(h.store, plugin, flagBit)
	processor.Start(ctx)
	h.procs[plugin.Name()] = processor
	h.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(AdminResponse{
		OK:               true,
		PluginName:       plugin.Name(),
		RetroactiveTotal: total,
		Message:          fmt.Sprintf("plugin %q registered and processing started", plugin.Name()),
	})
}

func (h *AdminHandler) handleRemove(w http.ResponseWriter, ctx context.Context, req AdminRequest) {
	// Validate request
	if req.Name == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: "plugin name is required",
		})
		return
	}

	// Stop retroactive processor if running
	h.mu.Lock()
	if proc, exists := h.procs[req.Name]; exists {
		proc.Stop()
		delete(h.procs, req.Name)
	}
	h.mu.Unlock()

	// Unregister plugin
	if err := h.registry.Unregister(req.Name); err != nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(AdminResponse{
			OK:      false,
			Message: fmt.Sprintf("failed to unregister plugin: %v", err),
		})
		return
	}

	slog.Info("plugin unregistered", "name", req.Name)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(AdminResponse{
		OK:      true,
		Message: fmt.Sprintf("plugin %q removed", req.Name),
	})
}
