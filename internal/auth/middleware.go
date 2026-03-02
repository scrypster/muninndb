package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// VaultAuthMiddleware enforces vault-level API key auth.
// Vault is read from ?vault= query param (matches existing REST API convention).
// Public vaults allow unauthenticated access in observe mode.
// If a Bearer token is present, it is always validated regardless of vault visibility.
func (s *Store) VaultAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vault := r.URL.Query().Get("vault")
		if vault == "" {
			vault = "default"
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
		ctx = context.WithValue(ctx, ContextMode, "observe")
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
			ctx = context.WithValue(ctx, ContextMode, "write")
			next(w, r.WithContext(ctx))
			return
		}
		// Fall through to standard vault auth (Bearer token or public vault).
		s.VaultAuthMiddleware(next)(w, r)
	}
}

// ObserveFromContext returns true if the request is in observe mode.
// Engine activation handlers use this to skip cognitive state mutations.
func ObserveFromContext(ctx context.Context) bool {
	mode, _ := ctx.Value(ContextMode).(string)
	return mode == "observe"
}

