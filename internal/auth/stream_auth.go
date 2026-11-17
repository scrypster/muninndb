package auth

import (
	"context"
	"errors"
)

// ErrVaultNoLongerPublic is returned by StreamStillAuthorized for a stream that
// was opened without a key on a public vault that is not public any more.
var ErrVaultNoLongerPublic = errors.New("vault requires an API key")

// StreamStillAuthorized reports whether what authorised a long-lived stream
// when it opened would authorise it now. A stream that pushes memory content
// for as long as it stays open asks before each thing it sends and on its
// keepalive tick: a stream that asked once, at open, would keep delivering to
// a key that has since been revoked or has expired, or to a caller with no key
// after the vault stopped being public (SEC-23).
//
// ctx is the context the transport's auth step produced for the request that
// opened the stream; vault is the vault the stream was opened on.
//
//   - Opened with an API key: the key must still validate (APIKeyStillValid).
//   - Opened with no key: vault must still be public.
//   - Opened under a verified admin session: nil. A session token is stateless
//     and carries its own expiry; nothing can revoke one, and this does not
//     end a stream when the token's lifetime runs out.
//
// The answer is read from the store every time, for the reason given at
// APIKeyStillValid: it is the only thing every path that changes a key or a
// vault's access goes through, a cluster follower's replicated batches
// included. BenchmarkStreamStillAuthorized measures the read.
func (s *Store) StreamStillAuthorized(ctx context.Context, vault string) error {
	if AdminSessionFromContext(ctx) {
		return nil
	}
	if key, ok := ctx.Value(ContextAPIKey).(*APIKey); ok && key != nil {
		return s.APIKeyStillValid(key)
	}
	cfg, err := s.GetVaultConfig(vault)
	if err != nil || !cfg.Public {
		return ErrVaultNoLongerPublic
	}
	return nil
}
