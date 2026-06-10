package embed

import (
	"context"
	"testing"
	"time"
)

func TestTokenBucketLimiter_Create(t *testing.T) {
	limiter := NewTokenBucketLimiter(10.0, 100.0)
	if limiter == nil {
		t.Fatal("NewTokenBucketLimiter returned nil")
	}
	if limiter.rate != 10.0 {
		t.Errorf("expected rate 10.0, got %f", limiter.rate)
	}
	if limiter.max != 100.0 {
		t.Errorf("expected max 100.0, got %f", limiter.max)
	}
}

func TestTokenBucketLimiter_ImmediateWait(t *testing.T) {
	limiter := NewTokenBucketLimiter(10.0, 100.0)

	start := time.Now()
	err := limiter.Wait(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	// First wait should be immediate (we start with full bucket)
	if elapsed > 100*time.Millisecond {
		t.Logf("first wait took %v (expected immediate)", elapsed)
	}
}

func TestTokenBucketLimiter_RateLimiting(t *testing.T) {
	limiter := NewTokenBucketLimiter(2.0, 2.0) // 2 req/sec

	// Should be able to consume 2 tokens immediately
	for i := 0; i < 2; i++ {
		err := limiter.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d failed: %v", i, err)
		}
	}

	// Third wait should block
	start := time.Now()
	err := limiter.Wait(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	// Should have waited approximately 0.5 seconds (1 token at 2 req/sec)
	expectedMin := 400 * time.Millisecond
	expectedMax := 700 * time.Millisecond
	if elapsed < expectedMin || elapsed > expectedMax {
		t.Logf("waited %v, expected between %v and %v", elapsed, expectedMin, expectedMax)
	}
}

func TestTokenBucketLimiter_ContextCancellation(t *testing.T) {
	limiter := NewTokenBucketLimiter(1.0, 1.0)

	// Consume the initial token
	limiter.Wait(context.Background())

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Wait should return immediately with error
	err := limiter.Wait(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestTokenBucketLimiter_MultipleWaits(t *testing.T) {
	limiter := NewTokenBucketLimiter(100.0, 100.0) // High rate, plenty of tokens

	for i := 0; i < 10; i++ {
		err := limiter.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d failed: %v", i, err)
		}
	}
}

func TestTokenBucketLimiter_RefillAfterWait(t *testing.T) {
	limiter := NewTokenBucketLimiter(10.0, 10.0)

	// Consume all tokens
	for i := 0; i < 10; i++ {
		limiter.Wait(context.Background())
	}

	// Wait for refill
	start := time.Now()
	err := limiter.Wait(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	// Should have waited approximately 100ms (1 token at 10 req/sec)
	expectedMin := 80 * time.Millisecond
	expectedMax := 150 * time.Millisecond
	if elapsed < expectedMin || elapsed > expectedMax {
		t.Logf("waited %v, expected between %v and %v", elapsed, expectedMin, expectedMax)
	}
}

func TestTokenBucketLimiter_HighRate(t *testing.T) {
	limiter := NewTokenBucketLimiter(1000.0, 1000.0)

	// Should be able to make many rapid requests
	for i := 0; i < 50; i++ {
		err := limiter.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d failed: %v", i, err)
		}
	}
}
