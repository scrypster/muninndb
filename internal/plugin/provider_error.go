package plugin

import (
	"errors"
	"fmt"
	"net/http"
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
// of retryability.
func IsProviderError(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr)
}

// isPermanentContentStatus reports whether an HTTP status indicates the request
// itself is the problem — the engram's content is too large or malformed and
// will fail identically on every retry. These are per-engram permanent failures,
// distinct from systemic conditions (auth/config 401/403, throttling 429, or
// upstream 5xx) that affect the provider rather than this specific engram.
func isPermanentContentStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, // 400 (e.g. context_length_exceeded)
		http.StatusNotFound,              // 404 (e.g. unknown model/deployment)
		http.StatusRequestEntityTooLarge, // 413 (payload too large)
		http.StatusUnprocessableEntity:   // 422 (content rejected)
		return true
	default:
		return false
	}
}

// IsPermanentContent reports whether the failure is caused by this engram's
// content and will therefore fail identically on every retry. Such engrams must
// be latched with DigestEnrichFailed so the batch continues and followers drain,
// rather than breaking the pass and wedging every engram sorted after it (#587).
func (e *ProviderError) IsPermanentContent() bool {
	return !e.Retryable && isPermanentContentStatus(e.StatusCode)
}

// IsPermanentContentProviderError reports whether err contains a content-caused
// provider failure that is permanent for the originating engram.
func IsPermanentContentProviderError(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && providerErr.IsPermanentContent()
}
