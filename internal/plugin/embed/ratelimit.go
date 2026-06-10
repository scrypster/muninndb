package embed

import (
	"context"
	"sync"
	"time"
)

// TokenBucketLimiter implements token bucket rate limiting.
type TokenBucketLimiter struct {
	rate     float64   // requests per second
	bucket   float64   // current tokens
	max      float64   // max burst
	lastTick time.Time // last refill time
	mu       sync.Mutex
}

// NewTokenBucketLimiter creates a new token bucket limiter.
func NewTokenBucketLimiter(ratePerSec float64, maxBurst float64) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		rate:     ratePerSec,
		bucket:   maxBurst,
		max:      maxBurst,
		lastTick: time.Now(),
	}
}

// Wait blocks until a request token is available or ctx is cancelled.
func (l *TokenBucketLimiter) Wait(ctx context.Context) error {
	for {
		// Check context cancellation before acquiring the lock.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		l.mu.Lock()

		// Refill tokens based on elapsed time
		now := time.Now()
		elapsed := now.Sub(l.lastTick).Seconds()
		l.bucket = min(l.max, l.bucket+elapsed*l.rate)
		l.lastTick = now

		if l.bucket >= 1.0 {
			l.bucket -= 1.0
			l.mu.Unlock()
			return nil
		}

		// Release the mutex before sleeping so other goroutines are not blocked
		// for the entire wait duration while making their own rate decisions.
		waitDuration := time.Duration((1.0 - l.bucket) / l.rate * float64(time.Second))
		l.mu.Unlock()
		timer := time.NewTimer(waitDuration)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}

		// Re-loop to re-check and consume token after the sleep.
		continue
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
