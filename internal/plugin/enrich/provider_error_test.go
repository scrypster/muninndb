package enrich

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

func TestRetryableHTTPStatusPolicy(t *testing.T) {
	tests := []struct {
		status    int
		retryable bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusNotImplemented, false},
	}
	for _, tt := range tests {
		if got := retryableHTTPStatus(tt.status); got != tt.retryable {
			t.Errorf("status %d retryable = %v, want %v", tt.status, got, tt.retryable)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
		valid bool
	}{
		{name: "delta seconds", value: "7", want: 7 * time.Second, valid: true},
		{name: "http date", value: now.Add(12 * time.Second).Format(http.TimeFormat), want: 12 * time.Second, valid: true},
		{name: "zero is valid", value: "0", valid: true},
		{name: "invalid", value: "later", valid: false},
		{name: "negative", value: "-1", valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, valid := parseRetryAfter(tt.value, now)
			if got != tt.want || valid != tt.valid {
				t.Fatalf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tt.value, got, valid, tt.want, tt.valid)
			}
		})
	}
}

func TestParseRetryAfter_DeltaSecondsOverflow(t *testing.T) {
	now := time.Now()
	maxSafeSeconds := int64(math.MaxInt64) / int64(time.Second)
	got, valid := parseRetryAfter(strconv.FormatInt(maxSafeSeconds, 10), now)
	if !valid || got != time.Duration(maxSafeSeconds)*time.Second {
		t.Fatalf("largest safe Retry-After = (%v, %v)", got, valid)
	}
	if got, valid := parseRetryAfter(strconv.FormatInt(maxSafeSeconds+1, 10), now); valid || got != 0 {
		t.Fatalf("overflowing Retry-After = (%v, %v), want invalid", got, valid)
	}
}

func TestProviderTransportErrorClassification(t *testing.T) {
	cause := errors.New("dial failed")
	err := providerTransportError("fake", cause)
	var providerErr *plugin.ProviderError
	if !errors.As(err, &providerErr) || !providerErr.Retryable || providerErr.StatusCode != 0 {
		t.Fatalf("transport error classification = %#v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("transport cause was not preserved: %v", err)
	}

	cancelErr := providerTransportError("fake", context.Canceled)
	if !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("context cancellation was not preserved: %v", cancelErr)
	}
	if errors.As(cancelErr, &providerErr) {
		t.Fatalf("context cancellation must remain control flow, got ProviderError: %v", cancelErr)
	}
}
