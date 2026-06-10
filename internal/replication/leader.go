package replication

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// LeaderElector manages leader election using a LeaseBackend.
// It implements lease-based leader election with automatic renewal and failover.
type LeaderElector struct {
	NodeID     string
	Backend    LeaseBackend
	LeaseTTL   time.Duration
	RenewEvery time.Duration

	// Callbacks
	OnPromote func() // called when this node becomes primary
	OnDemote  func() // called when this node loses the lease

	isLeader atomic.Bool
	token    atomic.Uint64

	mu     sync.Mutex
	runCtx context.Context
	cancel context.CancelFunc
}

// NewLeaderElector creates a new leader elector.
// Default LeaseTTL is 10s, RenewEvery is 3s.
func NewLeaderElector(nodeID string, backend LeaseBackend) *LeaderElector {
	return &LeaderElector{
		NodeID:     nodeID,
		Backend:    backend,
		LeaseTTL:   10 * time.Second,
		RenewEvery: 3 * time.Second,
		OnPromote:  func() {},
		OnDemote:   func() {},
	}
}

// Run starts the election loop. Blocks until ctx is cancelled.
// Acquires the lease, renews it periodically, and handles promotion/demotion.
func (e *LeaderElector) Run(ctx context.Context) error {
	e.mu.Lock()

	if e.runCtx != nil {
		e.mu.Unlock()
		return ErrAlreadyRunning
	}

	runCtx, cancel := context.WithCancel(ctx)
	e.runCtx = runCtx
	e.cancel = cancel
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.runCtx = nil
		e.cancel = nil
		e.mu.Unlock()

		// Release lease if held
		_ = e.Backend.Release(context.Background(), e.NodeID)
		e.isLeader.Store(false)
	}()

	ticker := time.NewTicker(e.RenewEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// tick attempts to acquire/renew the lease and handles state transitions.
func (e *LeaderElector) tick(ctx context.Context) {
	wasLeader := e.isLeader.Load()

	// Try to acquire or renew the lease
	acquired, err := e.Backend.TryAcquire(ctx, e.NodeID, e.LeaseTTL)
	if err != nil {
		// Backend error — maintain current state
		return
	}

	// Update fencing token
	token, err := e.Backend.Token(ctx)
	if err == nil {
		e.token.Store(token)
	}

	if acquired && !wasLeader {
		// Promoted to primary
		e.isLeader.Store(true)
		e.OnPromote()
	} else if !acquired && wasLeader {
		// Demoted from primary
		e.isLeader.Store(false)
		e.OnDemote()
	}
}

// IsLeader returns true if this node currently holds the lease.
func (e *LeaderElector) IsLeader() bool {
	return e.isLeader.Load()
}

// FencingToken returns the current fencing token.
// Increments every time the lease changes hands.
func (e *LeaderElector) FencingToken() uint64 {
	return e.token.Load()
}

type electorErr struct {
	msg string
}

// Error returns the error message
func (e *electorErr) Error() string {
	return e.msg
}

var ErrAlreadyRunning = &electorErr{msg: "leader elector already running"}
