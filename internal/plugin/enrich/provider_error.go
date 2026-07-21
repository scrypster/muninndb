package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

func providerHTTPError(provider string, resp *http.Response) error {
	// Drain a bounded amount so the standard transport can reuse the
	// connection, but never retain or log the provider's response body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return &plugin.ProviderError{
		Provider:      provider,
		StatusCode:    resp.StatusCode,
		Retryable:     retryableHTTPStatus(resp.StatusCode),
		RetryAfter:    retryAfter,
		HasRetryAfter: hasRetryAfter,
	}
}

func providerTransportError(provider string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s provider request: %w", provider, err)
	}
	return &plugin.ProviderError{Provider: provider, Retryable: true, Err: err}
}

func retryableHTTPStatus(status int) bool {
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

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
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
