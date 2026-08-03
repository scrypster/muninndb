package plugin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// ProviderHTTPError builds a ProviderError from a non-2xx HTTP response,
// draining and discarding the response body rather than interpolating it into
// the error. The request body embed providers send IS the memory text, and a
// provider that echoes the offending input back in a 400/413/422 error body
// (common for oversize or content-policy rejections) must not have that text
// retained, formatted into an error string, or reach a log line — this is the
// same class the enrich transport already holds via its own local
// providerHTTPError (internal/plugin/enrich/provider_error.go); this is the
// shared version any HTTP-based provider package can call directly. Retains
// only status, retryability, and Retry-After metadata — enough to diagnose,
// never enough to leak (#750's parse-error category, applied to embed's HTTP
// layer, #790).
func ProviderHTTPError(provider string, resp *http.Response) *ProviderError {
	// Drain a bounded amount so the standard transport can reuse the
	// connection, but never retain or log the provider's response body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	retryAfter, hasRetryAfter := parseProviderRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return &ProviderError{
		Provider:      provider,
		StatusCode:    resp.StatusCode,
		Retryable:     retryableProviderHTTPStatus(resp.StatusCode),
		RetryAfter:    retryAfter,
		HasRetryAfter: hasRetryAfter,
	}
}

func retryableProviderHTTPStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func parseProviderRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		const maxRetryAfterSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if seconds > maxRetryAfterSeconds {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if !when.After(now) {
		return 0, true
	}
	return when.Sub(now), true
}
