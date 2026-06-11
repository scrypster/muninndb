package plugin

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Registry is the thread-safe plugin registry. Singleton per engine instance.
type Registry struct {
	mu        sync.RWMutex
	plugins   map[string]Plugin     // name -> plugin
	embed     EmbedPlugin           // at most one
	enrich    EnrichPlugin          // at most one
	healthy   map[string]bool       // name -> health status
	lastCheck map[string]time.Time  // name -> time of last SetHealthy/SetUnhealthy call
	lastError map[string]string     // name -> last error message (empty if healthy)
	failed    map[string]PluginTier // name -> tier of a plugin that failed to init (no live instance)
}

// NewRegistry creates an empty plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins:   make(map[string]Plugin),
		healthy:   make(map[string]bool),
		lastCheck: make(map[string]time.Time),
		lastError: make(map[string]string),
		failed:    make(map[string]PluginTier),
	}
}

// RegisterFailed records a plugin that failed to initialize and therefore has
// no live instance to Register. It surfaces in List() as an unhealthy entry
// with the init error populated, so the UI can distinguish a genuinely absent
// plugin ("Not configured") from one that was configured but failed to start
// (e.g. an invalid/unavailable enrich model). A subsequent successful
// Register of the same name clears the recorded failure.
func (r *Registry) RegisterFailed(name string, tier PluginTier, err error) {
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A live plugin of the same name takes precedence over a recorded failure.
	if _, exists := r.plugins[name]; exists {
		return
	}
	r.failed[name] = tier
	r.healthy[name] = false
	r.lastCheck[name] = time.Now()
	if err != nil {
		r.lastError[name] = err.Error()
	} else {
		r.lastError[name] = "plugin initialization failed"
	}
}

// Register adds a plugin. Returns an error if:
// - A plugin with the same name is already registered
// - An embed plugin is registered when one already exists
// - An enrich plugin is registered when one already exists
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return fmt.Errorf("cannot register nil plugin")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	name := p.Name()

	// Check for duplicate name
	if _, exists := r.plugins[name]; exists {
		_ = p.Close()
		return fmt.Errorf("plugin %q is already registered", name)
	}

	tier := p.Tier()

	// Check at-most-one constraint for embed
	if tier == TierEmbed {
		if r.embed != nil {
			return fmt.Errorf("an embed plugin is already registered; unregister it first")
		}
		// Try to assert to EmbedPlugin
		embed, ok := p.(EmbedPlugin)
		if !ok {
			return fmt.Errorf("plugin %q claims to be TierEmbed but does not implement EmbedPlugin", name)
		}
		r.embed = embed
	}

	// Check at-most-one constraint for enrich
	if tier == TierEnrich {
		if r.enrich != nil {
			return fmt.Errorf("an enrich plugin is already registered; unregister it first")
		}
		// Try to assert to EnrichPlugin
		enrich, ok := p.(EnrichPlugin)
		if !ok {
			return fmt.Errorf("plugin %q claims to be TierEnrich but does not implement EnrichPlugin", name)
		}
		r.enrich = enrich
	}

	// Register the plugin
	r.plugins[name] = p
	r.healthy[name] = true // Start healthy
	r.lastCheck[name] = time.Now()
	r.lastError[name] = ""
	// A successful registration supersedes any prior recorded init failure.
	delete(r.failed, name)

	return nil
}

// Unregister removes a plugin by name. Calls p.Close().
// Returns an error if the plugin is not found.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, exists := r.plugins[name]
	if !exists {
		return fmt.Errorf("plugin %q not found", name)
	}

	// Call Close on the plugin
	if err := p.Close(); err != nil {
		slog.Warn("plugin: close failed during unregister", "name", name, "err", err)
	}

	// Remove from registry
	delete(r.plugins, name)
	delete(r.healthy, name)
	delete(r.lastCheck, name)
	delete(r.lastError, name)
	delete(r.failed, name)

	// Clear embed or enrich reference if this was the active plugin
	tier := p.Tier()
	if tier == TierEmbed {
		r.embed = nil
	} else if tier == TierEnrich {
		r.enrich = nil
	}

	return nil
}

// GetEmbed returns the active embed plugin, or nil if none registered.
func (r *Registry) GetEmbed() EmbedPlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.embed
}

// GetEnrich returns the active enrich plugin, or nil if none registered.
func (r *Registry) GetEnrich() EnrichPlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enrich
}

// List returns all registered plugins with their current status.
func (r *Registry) List() []PluginStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]PluginStatus, 0, len(r.plugins)+len(r.failed))
	for name, p := range r.plugins {
		status := PluginStatus{
			Name:      name,
			Tier:      p.Tier(),
			Healthy:   r.healthy[name],
			LastCheck: r.lastCheck[name],
			Error:     r.lastError[name],
		}
		result = append(result, status)
	}
	// Include plugins that failed to initialize (no live instance) so the
	// status endpoint reports the failure instead of silently omitting them.
	for name, tier := range r.failed {
		if _, live := r.plugins[name]; live {
			continue // a live registration supersedes the recorded failure
		}
		result = append(result, PluginStatus{
			Name:      name,
			Tier:      tier,
			Healthy:   false,
			LastCheck: r.lastCheck[name],
			Error:     r.lastError[name],
		})
	}

	return result
}

// HasEmbed returns true if an embed plugin is registered and healthy.
// The activation engine checks this to decide whether to include the HNSW path.
func (r *Registry) HasEmbed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.embed == nil {
		return false
	}

	// Check if healthy
	return r.healthy[r.embed.Name()]
}

// HasEnrich returns true if an enrich plugin is registered and healthy.
func (r *Registry) HasEnrich() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.enrich == nil {
		return false
	}

	// Check if healthy
	return r.healthy[r.enrich.Name()]
}

// SetHealthy sets the health status of a plugin by name and records the check time.
func (r *Registry) SetHealthy(name string, healthy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthy[name] = healthy
	r.lastCheck[name] = time.Now()
	if healthy {
		r.lastError[name] = ""
	}
}

// SetUnhealthy marks a plugin as unhealthy, records the check time, and stores the error.
func (r *Registry) SetUnhealthy(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthy[name] = false
	r.lastCheck[name] = time.Now()
	if err != nil {
		r.lastError[name] = err.Error()
	}
}
