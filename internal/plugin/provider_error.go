package plugin

import (
	"errors"
	"fmt"
	"time"
)

// ProviderError classifies failures returned by an external enrichment
// provider without retaining response bodies, request content, or credentials.
// StatusCode is zero for transport failures.
type ProviderError struct {
	Provider      string
	StatusCode    int
	Retryable     bool
	RetryAfter    time.Duration
	HasRetryAfter bool
	Err           error
}

func (e *ProviderError) Error() string {
	provider := e.Provider
	if provider == "" {
		provider = "enrichment"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s provider returned HTTP %d", provider, e.StatusCode)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s provider transport failed: %v", provider, e.Err)
	}
	return fmt.Sprintf("%s provider failed", provider)
}

// Unwrap preserves transport causes such as context cancellation.
func (e *ProviderError) Unwrap() error { return e.Err }

// IsRetryableProviderError reports whether err contains a retryable provider
// failure through any number of wrapping layers.
func IsRetryableProviderError(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && providerErr.Retryable
}

// IsProviderError reports whether err contains a provider failure, regardless
// of retryability. Provider failures are systemic; they are never evidence
// that an individual engram's content is permanently malformed.
func IsProviderError(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr)
}
