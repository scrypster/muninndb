package trigger

import (
	"sync"
	"time"
)

// SubscriptionRegistry holds all active subscriptions.
type SubscriptionRegistry struct {
	mu         sync.RWMutex
	byID       map[string]*Subscription
	byVault    map[uint32][]*Subscription
	vaultCount map[uint32]int // O(1) per-vault counter (T3)
	totalCount int            // O(1) global counter (T3)
}

func newRegistry() *SubscriptionRegistry {
	return &SubscriptionRegistry{
		byID:       make(map[string]*Subscription),
		byVault:    make(map[uint32][]*Subscription),
		vaultCount: make(map[uint32]int),
	}
}

// CountForVault returns the number of active subscriptions for vaultID (O(1), T3).
func (r *SubscriptionRegistry) CountForVault(vaultID uint32) int {
	r.mu.RLock()
	n := r.vaultCount[vaultID]
	r.mu.RUnlock()
	return n
}

// CountTotal returns the total number of active subscriptions across all vaults (O(1), T3).
func (r *SubscriptionRegistry) CountTotal() int {
	r.mu.RLock()
	n := r.totalCount
	r.mu.RUnlock()
	return n
}

// Add registers a subscription, and reports whether it did. A subscription
// whose id is already registered is NOT added: replacing the entry under an id
// would leave the earlier subscription listed for its vault with nothing able
// to remove it, and would hand whoever chose the id the means to have the
// earlier owner's unsubscribe remove the wrong one (SEC-26).
func (r *SubscriptionRegistry) Add(sub *Subscription) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byID[sub.ID]; taken {
		return false
	}
	r.byID[sub.ID] = sub
	r.byVault[sub.VaultID] = append(r.byVault[sub.VaultID], sub)
	r.vaultCount[sub.VaultID]++
	r.totalCount++
	return true
}

func (r *SubscriptionRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	subs := r.byVault[sub.VaultID]
	for i, s := range subs {
		if s.ID == id {
			subs[i] = subs[len(subs)-1]
			subs[len(subs)-1] = nil
			r.byVault[sub.VaultID] = subs[:len(subs)-1]
			break
		}
	}
	r.vaultCount[sub.VaultID]--
	if r.vaultCount[sub.VaultID] == 0 {
		delete(r.vaultCount, sub.VaultID)
	}
	r.totalCount--
}

func (r *SubscriptionRegistry) Get(id string) (*Subscription, bool) {
	r.mu.RLock()
	sub, ok := r.byID[id]
	r.mu.RUnlock()
	return sub, ok
}

func (r *SubscriptionRegistry) ForVault(vaultID uint32) []*Subscription {
	r.mu.RLock()
	subs := r.byVault[vaultID]
	if len(subs) == 0 {
		r.mu.RUnlock()
		return nil
	}
	snapshot := make([]*Subscription, len(subs))
	copy(snapshot, subs)
	r.mu.RUnlock()
	return snapshot
}

func (r *SubscriptionRegistry) ActiveVaults() []uint32 {
	r.mu.RLock()
	vaults := make([]uint32, 0, len(r.byVault))
	for v := range r.byVault {
		vaults = append(vaults, v)
	}
	r.mu.RUnlock()
	return vaults
}

func (r *SubscriptionRegistry) PruneExpired() int {
	now := time.Now()
	var expired []string
	r.mu.RLock()
	for id, sub := range r.byID {
		if !sub.expiresAt.IsZero() && now.After(sub.expiresAt) {
			expired = append(expired, id)
		}
	}
	r.mu.RUnlock()
	for _, id := range expired {
		r.Remove(id)
	}
	return len(expired)
}
