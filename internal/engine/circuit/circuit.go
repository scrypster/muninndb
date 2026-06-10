// Package circuit provides a simple circuit breaker for LLM calls.
// When consecutive failures exceed the threshold, the circuit opens
// and subsequent calls return ErrOpen immediately until the reset
// interval elapses. No external dependencies.
package circuit

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned when the circuit breaker is open.
var ErrOpen = errors.New("circuit breaker open")

// State represents the circuit state.
type State int

const (
	StateClosed   State = iota // normal operation
	StateHalfOpen              // one probe request allowed
	StateOpen                  // fast-fail, no calls forwarded
)

// StateChangeEvent carries metadata about a circuit state transition.
// It is passed to the OnStateChange callback (if set) on every transition.
// The callback is invoked while the breaker's lock is NOT held, so it is
// safe to call registry methods or emit logs without risk of deadlock.
type StateChangeEvent struct {
	From           State
	To             State
	FailureCount   int
	OutageDuration time.Duration // non-zero only when recovering (Open→Closed or HalfOpen→Closed)
}

// Breaker is a simple three-state circuit breaker.
// It is safe for concurrent use.
type Breaker struct {
	mu               sync.Mutex
	state            State
	consecutiveFails int
	lastFailTime     time.Time
	openedAt         time.Time // set when transitioning to Open; used to compute outage duration on recovery
	halfOpenUsed     bool

	// Configuration
	maxFails   int           // consecutive failures before opening
	resetAfter time.Duration // time open before half-open probe

	// OnStateChange is called (outside the lock) whenever the circuit transitions
	// between states. It may be nil. Set it once at construction time via
	// NewWithOptions or by direct assignment before any concurrent use.
	OnStateChange func(ev StateChangeEvent)
}

// New creates a Breaker with the given thresholds.
// maxFails: number of consecutive failures before opening (default recommendation: 5).
// resetAfter: duration to stay open before allowing a probe (default: 30s).
func New(maxFails int, resetAfter time.Duration) *Breaker {
	if maxFails <= 0 {
		maxFails = 5
	}
	if resetAfter <= 0 {
		resetAfter = 30 * time.Second
	}
	return &Breaker{
		maxFails:   maxFails,
		resetAfter: resetAfter,
	}
}

// Allow returns nil if the call should proceed, or ErrOpen if it should be rejected.
// Must be paired with a call to RecordSuccess or RecordFailure.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(b.lastFailTime) >= b.resetAfter {
			// Transition to half-open: allow exactly one probe.
			// Mark halfOpenUsed=true immediately so concurrent callers are rejected.
			b.state = StateHalfOpen
			b.halfOpenUsed = true
			return nil
		}
		return ErrOpen
	case StateHalfOpen:
		if b.halfOpenUsed {
			return ErrOpen
		}
		b.halfOpenUsed = true
		return nil
	}
	return nil
}

// RecordSuccess records a successful call. Closes the circuit if it was half-open.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	prev := b.state
	fails := b.consecutiveFails
	var outage time.Duration
	if prev == StateOpen || prev == StateHalfOpen {
		outage = time.Since(b.openedAt)
	}
	b.consecutiveFails = 0
	b.state = StateClosed
	b.halfOpenUsed = false
	cb := b.OnStateChange
	b.mu.Unlock()

	if cb != nil && prev != StateClosed {
		cb(StateChangeEvent{
			From:           prev,
			To:             StateClosed,
			FailureCount:   fails,
			OutageDuration: outage,
		})
	}
}

// RecordFailure records a failed call. Opens the circuit if failures exceed maxFails.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	prev := b.state
	b.consecutiveFails++
	b.lastFailTime = time.Now()
	var ev StateChangeEvent
	var cb func(StateChangeEvent)
	transitioned := false
	if b.state == StateHalfOpen || b.consecutiveFails >= b.maxFails {
		// Capture failure count before the half-open reset so the event carries
		// the actual count that triggered the transition, not the post-reset zero.
		failCount := b.consecutiveFails
		// Reset consecutiveFails when transitioning back from half-open to open so
		// the next probe cycle starts from a clean slate rather than an already-
		// elevated counter that would cause the circuit to re-open faster than intended.
		if b.state == StateHalfOpen {
			b.consecutiveFails = 0
		}
		if b.state != StateOpen {
			b.openedAt = time.Now()
		}
		b.state = StateOpen
		b.halfOpenUsed = false
		if prev != StateOpen {
			transitioned = true
			ev = StateChangeEvent{
				From:         prev,
				To:           StateOpen,
				FailureCount: failCount,
			}
			cb = b.OnStateChange
		}
	}
	b.mu.Unlock()

	if transitioned && cb != nil {
		cb(ev)
	}
}

// State returns the current circuit state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// StateString returns a human-readable circuit state.
func (b *Breaker) StateString() string {
	switch b.State() {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	case StateOpen:
		return "open"
	default:
		return "unknown"
	}
}

// Do executes fn if the circuit allows it; otherwise returns ErrOpen.
// Automatically records success/failure based on whether fn returns an error.
func (b *Breaker) Do(fn func() error) error {
	if err := b.Allow(); err != nil {
		return err
	}
	err := fn()
	if err != nil {
		b.RecordFailure()
	} else {
		b.RecordSuccess()
	}
	return err
}
