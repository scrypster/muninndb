package replication

import (
	"context"
	"sync"
	"time"
)

// MemoryLeaseBackend is an in-memory LeaseBackend for testing and single-node use.
// It is not suitable for distributed deployments.
//
// In production, replace with an etcd or Consul backend via the LeaseBackend interface.
type MemoryLeaseBackend struct {
	mu      sync.Mutex
	holder  string            // current lease holder
	expires time.Time         // lease expiration time
	token   uint64            // fencing token (incremented on each lease change)
	genLock map[string]uint64 // generation lock per node (for optimistic concurrency)
}

// NewMemoryLeaseBackend creates a new in-memory lease backend.
func NewMemoryLeaseBackend() *MemoryLeaseBackend {
	return &MemoryLeaseBackend{
		holder:  "",
		expires: time.Time{},
		token:   0,
		genLock: make(map[string]uint64),
	}
}

// TryAcquire attempts to acquire a lease for nodeID with the given TTL.
// Returns (true, nil) if acquired; (false, nil) if already held by another node.
func (b *MemoryLeaseBackend) TryAcquire(ctx context.Context, nodeID string, ttl time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	// If lease expired or no holder, grant it
	if b.expires.Before(now) || b.holder == "" {
		b.holder = nodeID
		b.expires = now.Add(ttl)
		b.token++
		b.genLock[nodeID] = b.token
		return true, nil
	}

	// If held by the same node, renew it
	if b.holder == nodeID {
		b.expires = now.Add(ttl)
		return true, nil
	}

	// Held by another node
	return false, nil
}

// Renew renews an existing lease for nodeID.
// Returns error if nodeID doesn't hold the lease.
func (b *MemoryLeaseBackend) Renew(ctx context.Context, nodeID string, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.holder != nodeID {
		return ErrNotLeaseHolder
	}

	now := time.Now()
	b.expires = now.Add(ttl)
	return nil
}

// Release releases the lease held by nodeID.
func (b *MemoryLeaseBackend) Release(ctx context.Context, nodeID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.holder == nodeID {
		b.holder = ""
		b.expires = time.Time{}
	}

	return nil
}

// CurrentHolder returns the nodeID that currently holds the lease, or empty string if no holder.
func (b *MemoryLeaseBackend) CurrentHolder(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	// Check if lease has expired
	if b.expires.Before(now) {
		return "", nil
	}

	return b.holder, nil
}

// Token returns the current fencing token.
func (b *MemoryLeaseBackend) Token(ctx context.Context) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.token, nil
}

type leaseErr struct {
	msg string
}

// Error returns the error message
func (e *leaseErr) Error() string {
	return e.msg
}

var ErrNotLeaseHolder = &leaseErr{msg: "not lease holder"}
