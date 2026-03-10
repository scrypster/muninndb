package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// VaultAuthMiddleware enforces vault-level API key auth.
// Vault is resolved from ?vault= query param first, then from JSON request bodies
// on body-based routes, and finally defaults to "default" when no explicit
// vault is provided.
// Public vaults allow unauthenticated access in observe mode.
// If a Bearer token is present, it is always validated regardless of vault visibility.
func (s *Store) VaultAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vault, resolveErr := resolveRequestVault(r, "default")
		if resolveErr != nil {
			writeVaultRequestError(w, http.StatusBadRequest, resolveErr)
			return
		}

		authHeader := r.Header.Get("Authorization")

		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			key, err := s.ValidateAPIKey(token)
			if err != nil {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			// Enforce vault scoping: the key must be issued for the requested vault.
			if key.Vault != vault {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				errMsg, _ := json.Marshal(map[string]string{
					"error": fmt.Sprintf("api key is not authorized for vault %q", vault),
					"code":  "VAULT_KEY_MISMATCH",
				})
				w.Write(errMsg)
				return
			}
			ctx := context.WithValue(r.Context(), ContextVault, key.Vault)
			ctx = context.WithValue(ctx, ContextMode, key.Mode)
			ctx = context.WithValue(ctx, ContextAPIKey, &key)
			next(w, r.WithContext(ctx))
			return
		}
		// No key — check if vault is public
		cfg, err := s.GetVaultConfig(vault)
		if err != nil || !cfg.Public {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			errMsg, _ := json.Marshal(map[string]string{
				"error": fmt.Sprintf("vault %q requires an API key", vault),
				"code":  "VAULT_LOCKED",
			})
			w.Write(errMsg)
			return
		}

		ctx := context.WithValue(r.Context(), ContextVault, vault)
		ctx = context.WithValue(ctx, ContextMode, ModeObserve)
		next(w, r.WithContext(ctx))
	}
}

// AdminSessionMiddleware checks for a valid admin session cookie.
// Redirects to /login on failure — suitable for browser-facing UI routes.
func AdminSessionMiddleware(secret []byte, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("muninn_session")
		if err != nil || !validateSessionToken(cookie.Value, secret) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// AdminAPIMiddleware checks for a valid admin session cookie.
// Returns JSON 401 on failure — suitable for REST API admin routes.
func (s *Store) AdminAPIMiddleware(secret []byte, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("muninn_session")
		if err != nil || !validateSessionToken(cookie.Value, secret) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"AUTH_FAILED","message":"admin session required"}}`))
			return
		}
		next(w, r)
	}
}

// VaultAuthWithAdminBypass combines vault-level API key auth with an admin
// session bypass. A valid admin session cookie (muninn_session) grants full
// write-mode access to any vault — the Web UI admin console uses this path.
// External API clients continue to authenticate with Bearer tokens as before.
func (s *Store) VaultAuthWithAdminBypass(secret []byte, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Admin session bypass — authenticated Web UI gets full access to any vault.
		cookie, err := r.Cookie("muninn_session")
		if err == nil && validateSessionToken(cookie.Value, secret) {
			vault := r.URL.Query().Get("vault")
			if vault == "" {
				vault = "default"
			}
			ctx := context.WithValue(r.Context(), ContextVault, vault)
			ctx = context.WithValue(ctx, ContextMode, ModeFull)
			next(w, r.WithContext(ctx))
			return
		}
		// Fall through to standard vault auth (Bearer token or public vault).
		s.VaultAuthMiddleware(next)(w, r)
	}
}

// Mode enforcement uses two layers — documented here for future reference:
//
//   "observe" mode — engine-layer enforcement: reads are allowed but cognitive
//   mutations (Hebbian associations, predictive activation) are suppressed via
//   ObserveFromContext. The engine decides what to skip.
//
//   "write" mode (ingest-only) — middleware-layer enforcement: read endpoints
//   return 403 before the engine is called at all. WriteOnlyGuard is applied at
//   route registration in transport/rest/server.go.

// ObserveFromContext returns true if the request is in observe (read-only) mode.
// Engine activation handlers use this to skip cognitive state mutations.
func ObserveFromContext(ctx context.Context) bool {
	mode, _ := ctx.Value(ContextMode).(string)
	return mode == ModeObserve
}

// WriteOnlyFromContext returns true if the request is in write-only (ingest) mode.
// Write-only keys may call mutation endpoints but not read endpoints.
func WriteOnlyFromContext(ctx context.Context) bool {
	mode, _ := ctx.Value(ContextMode).(string)
	return mode == ModeWrite
}

// WriteOnlyGuard is HTTP middleware that returns 403 for write-only mode requests.
// Apply it at route registration for every read endpoint:
//
//	mux.HandleFunc("GET /api/engrams/{id}", s.withMiddleware(auth.WriteOnlyGuard(s.handleGetEngram)))
//
// Scope: this guard applies to the REST API only. The MCP server uses a separate
// static-token auth model; write-only API keys cannot authenticate to MCP at all.
func WriteOnlyGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if WriteOnlyFromContext(r.Context()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"write-only key cannot read"}}`))
			return
		}
		next(w, r)
	}
}

func resolveRequestVault(r *http.Request, defaultVault string) (string, error) {
	queryVault := strings.TrimSpace(r.URL.Query().Get("vault"))
	bodyVault, err := extractVaultFromRequestBody(r)
	if err != nil {
		return "", err
	}
	if queryVault != "" {
		if bodyVault != "" && bodyVault != queryVault {
			return "", fmt.Errorf("vault in request body must match query parameter")
		}
		return queryVault, nil
	}
	if bodyVault != "" {
		return bodyVault, nil
	}
	return defaultVault, nil
}

func extractVaultFromRequestBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return "", nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read request body for vault routing")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	trimmedBody := bytes.TrimSpace(body)
	if len(trimmedBody) == 0 {
		return "", nil
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") && !looksLikeJSONObject(trimmedBody) {
		return "", nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmedBody, &envelope); err != nil {
		return "", fmt.Errorf("invalid request body")
	}

	var resolved string
	if raw, ok := envelope["vault"]; ok {
		vault, err := decodeOptionalVault(raw)
		if err != nil {
			return "", err
		}
		resolved = vault
	}

	rawEngrams, ok := envelope["engrams"]
	if !ok {
		return resolved, nil
	}

	var items []struct {
		Vault string `json:"vault"`
	}
	if err := json.Unmarshal(rawEngrams, &items); err != nil {
		return "", fmt.Errorf("invalid request body")
	}

	for _, item := range items {
		vault := strings.TrimSpace(item.Vault)
		if vault == "" {
			continue
		}
		if resolved == "" {
			resolved = vault
			continue
		}
		if resolved != vault {
			return "", fmt.Errorf("request body references multiple vaults")
		}
	}
	return resolved, nil
}

func looksLikeJSONObject(body []byte) bool {
	return len(body) > 0 && body[0] == '{'
}

func decodeOptionalVault(raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var vault string
	if err := json.Unmarshal(raw, &vault); err != nil {
		return "", fmt.Errorf("invalid request body")
	}
	return strings.TrimSpace(vault), nil
}

func writeVaultRequestError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]string{
		"error": err.Error(),
		"code":  "INVALID_VAULT_REQUEST",
	})
	w.Write(payload)
}
