package trigger

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TriggerConfig controls trigger system behavior limits.
type TriggerConfig struct {
	MaxSubscriptionsPerVault int // 0 = unlimited; default 100
	MaxTotalSubscriptions    int // 0 = unlimited; default 1000
}

// Sentinel errors for subscription cap enforcement.
var (
	ErrVaultSubscriptionLimitReached  = errors.New("trigger: per-vault subscription limit reached")
	ErrGlobalSubscriptionLimitReached = errors.New("trigger: global subscription limit reached")
)

const (
	sweepInterval      = 30 * time.Second
	writeEventBufSize  = 1024
	cogEventBufSize    = 4096
	contraEventBufSize = 256
	embedEventBufSize  = 1024
	embedCacheTTL      = 5 * time.Minute
	embedCacheMax      = 1000
	sweepTopK          = 200
	deliverTimeout     = 100 * time.Millisecond
)

// TriggerType is the type of activation push.
type TriggerType string

const (
	TriggerNewWrite         TriggerType = "new_write"
	TriggerThresholdCrossed TriggerType = "threshold_crossed"
	TriggerContradiction    TriggerType = "contradiction_detected"
)

// DeliverFunc sends a push to a client connection.
type DeliverFunc func(ctx context.Context, push *ActivationPush) error

// ActivationPush is an unsolicited push from the DB to a subscribed client.
type ActivationPush struct {
	SubscriptionID string
	Engram         *storage.Engram
	Score          float64
	Trigger        TriggerType
	Why            string
	PushNumber     int
	At             time.Time
}

// Subscription is one active context subscription.
type Subscription struct {
	ID             string
	VaultID        uint32
	Context        []string
	Threshold      float64
	TTL            time.Duration
	RateLimit      int
	PushOnWrite    bool
	DeltaThreshold float64

	// Deliver is the function called to push activations to this subscriber.
	// Set this before calling TriggerSystem.Subscribe.
	Deliver DeliverFunc

	// T6: circuit-breaker counters; read by the REST SSE handler to decide
	// when to disconnect a slow subscriber.
	consecutiveDrops atomic.Int64
	droppedTotal     atomic.Int64

	embedding    []float32
	embeddingAt  time.Time
	pushedScores map[storage.ULID]float64
	pushCount    int
	expiresAt    time.Time
	rateLimiter  *tokenBucket
	mu           sync.Mutex
	createdAt    time.Time
}

// EngramEvent fires after every WRITE ACK.
type EngramEvent struct {
	VaultID uint32
	Engram  *storage.Engram
	IsNew   bool
}

// CognitiveEvent fires when a cognitive worker changes an engram's score.
type CognitiveEvent struct {
	VaultID  uint32
	EngramID storage.ULID
	Field    string
	OldValue float32
	NewValue float32
	Delta    float32
}

// ContradictEvent fires when a contradiction is detected.
type ContradictEvent struct {
	VaultID  uint32
	EngramA  storage.ULID
	EngramB  storage.ULID
	Severity float64
	Type     string
}

// EmbedEvent fires when an engram's embedding finishes computing asynchronously
// (the retroactive embed processor runs ~tens of ms after the write). It lets
// the worker re-evaluate PushOnWrite subscriptions with the now-available vector
// so a vector-scored match on a freshly-written engram is not missed (#512).
type EmbedEvent struct {
	VaultID   uint32
	Engram    *storage.Engram
	Embedding []float32
}

// ScoredID is an index search result.
type ScoredID struct {
	ID    storage.ULID
	Score float64
}

// TriggerStore is the storage interface for the trigger system.
type TriggerStore interface {
	GetMetadata(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error)
	GetEngrams(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]*storage.Engram, error)
	GetEmbedding(ctx context.Context, wsPrefix [8]byte, id storage.ULID) ([]float32, error)
	VaultPrefix(vault string) [8]byte
}

// FTSIndex for trigger scoring.
type FTSIndex interface {
	Search(ctx context.Context, ws [8]byte, query string, topK int) ([]ScoredID, error)
}

// HNSWIndex for trigger scoring.
type HNSWIndex interface {
	Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]ScoredID, error)
}

// Embedder for subscription context.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([]float32, error)
}

// tokenBucket implements per-subscription rate limiting.
// Tokens are stored as milliunits (1 token = 1000 units) to allow sub-token
// precision during refill without floating-point math.
// Refill rate is computed lazily in refill() as:
//
//	add = elapsed_nanoseconds * maxTokens_milliunits / nanoseconds_per_second
//
// This avoids the integer-truncation bug where storing refillRate as a field
// would yield 0 for any maxPerSecond < 1,000,001.
type tokenBucket struct {
	tokens     atomic.Int64
	maxTokens  int64
	lastRefill atomic.Int64
}

func newTokenBucket(maxPerSecond int) *tokenBucket {
	max := int64(maxPerSecond) * 1000
	tb := &tokenBucket{
		maxTokens: max,
	}
	tb.tokens.Store(max)
	tb.lastRefill.Store(time.Now().UnixNano())
	return tb
}

func (tb *tokenBucket) TryConsume() bool {
	tb.refill()
	for {
		current := tb.tokens.Load()
		if current < 1000 {
			return false
		}
		if tb.tokens.CompareAndSwap(current, current-1000) {
			return true
		}
	}
}

// TryConsumeOrBurst tries to consume 1 token. If the bucket is empty,
// it allows up to overdraft additional tokens of deficit before refusing.
// Used for high-priority events (contradictions) that warrant burst delivery (T5).
func (tb *tokenBucket) TryConsumeOrBurst(overdraft int) bool {
	tb.refill()
	minAllowed := -int64(overdraft) * 1000 // milliunits
	for {
		cur := tb.tokens.Load()
		if cur <= minAllowed {
			return false // overdraft exhausted
		}
		if tb.tokens.CompareAndSwap(cur, cur-1000) {
			return true
		}
	}
}

func (tb *tokenBucket) refill() {
	now := time.Now().UnixNano()
	last := tb.lastRefill.Load()
	elapsed := now - last
	if elapsed <= 0 {
		return
	}
	if !tb.lastRefill.CompareAndSwap(last, now) {
		return
	}
	// Compute tokens to add: elapsed_ns * maxTokens_milliunits / ns_per_second.
	// This avoids integer-truncation: storing refillRate = maxTokens/1e9 would
	// yield 0 for any maxPerSecond < 1,000,001.
	add := elapsed * tb.maxTokens / int64(time.Second)
	if add <= 0 {
		return // elapsed too small to add even 1 milliunit
	}
	for {
		current := tb.tokens.Load()
		next := current + add
		if next > tb.maxTokens {
			next = tb.maxTokens
		}
		if tb.tokens.CompareAndSwap(current, next) {
			break
		}
	}
}

// EmbedCache caches embeddings by context fingerprint.
type EmbedCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*embedEntry
}

type embedEntry struct {
	vec        []float32
	computedAt time.Time
	lastUsed   time.Time
}

// NewEmbedCache creates a new EmbedCache for testing and external use.
func NewEmbedCache() *EmbedCache { return newEmbedCache() }

func newEmbedCache() *EmbedCache {
	return &EmbedCache{entries: make(map[[32]byte]*embedEntry, 64)}
}

func contextFingerprint(ctx []string) [32]byte {
	h := sha256.New()
	for _, s := range ctx {
		h.Write([]byte(s))
		h.Write([]byte{0x00})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (c *EmbedCache) Get(ctx []string) ([]float32, bool) {
	fp := contextFingerprint(ctx)
	c.mu.Lock()
	entry, ok := c.entries[fp]
	if ok {
		if time.Since(entry.computedAt) > embedCacheTTL {
			delete(c.entries, fp)
			c.mu.Unlock()
			return nil, false
		}
		entry.lastUsed = time.Now()
		vec := entry.vec
		c.mu.Unlock()
		return vec, true
	}
	c.mu.Unlock()
	return nil, false
}

func (c *EmbedCache) Set(ctx []string, vec []float32) {
	fp := contextFingerprint(ctx)
	now := time.Now()
	c.mu.Lock()
	if len(c.entries) >= embedCacheMax {
		var oldestKey [32]byte
		var oldestTime time.Time
		first := true
		for k, e := range c.entries {
			if first || e.lastUsed.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.lastUsed
				first = false
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[fp] = &embedEntry{vec: vec, computedAt: now, lastUsed: now}
	c.mu.Unlock()
}

// TriggerScore computes a fast relevance score for a single engram against a subscription.
func TriggerScore(sub *Subscription, meta *storage.EngramMeta, vectorScore, ftsScore float64) (float64, bool) {
	const (
		wSemantic = 0.35
		wFTS      = 0.25
		wDecay    = 0.20
		wAccess   = 0.10
		wRecency  = 0.10
	)

	decayFactor := float64(meta.Relevance)
	count := meta.AccessCount
	if count < 0 {
		count = 0
	}
	accessFreq := math.Log1p(float64(count)) / math.Log1p(100.0)
	if accessFreq > 1.0 {
		accessFreq = 1.0
	}

	daysSince := time.Since(meta.LastAccess).Hours() / 24.0
	recency := math.Exp(-daysSince * math.Log(2) / 7.0)

	raw := wSemantic*vectorScore + wFTS*ftsScore + wDecay*decayFactor + wAccess*accessFreq + wRecency*recency
	if raw > 1.0 {
		raw = 1.0
	}

	final := raw * float64(meta.Confidence)
	return final, final >= sub.Threshold
}

// cosineSimilarity for trigger use.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float64(dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb)))))
}

// engramToMeta converts a storage.Engram to storage.EngramMeta for trigger scoring.
func engramToMeta(eng *storage.Engram) *storage.EngramMeta {
	return &storage.EngramMeta{
		ID:          eng.ID,
		CreatedAt:   eng.CreatedAt,
		UpdatedAt:   eng.UpdatedAt,
		LastAccess:  eng.LastAccess,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		State:       eng.State,
	}
}

// TriggerSystem is the top-level trigger system.
type TriggerSystem struct {
	registry   *SubscriptionRegistry
	worker     *TriggerWorker
	embedCache *EmbedCache
	store      TriggerStore
	embedder   Embedder
	config     TriggerConfig

	WriteEvents      chan *EngramEvent
	CognitiveEvents  chan CognitiveEvent
	ContradictEvents chan ContradictEvent
	EmbedEvents      chan *EmbedEvent
}

// New creates a TriggerSystem with optional config. Pass zero-value TriggerConfig
// to use defaults (100 per-vault, 1000 global).
func New(store TriggerStore, fts FTSIndex, hnsw HNSWIndex, embedder Embedder, cfg ...TriggerConfig) *TriggerSystem {
	var config TriggerConfig
	if len(cfg) > 0 {
		config = cfg[0]
	}
	if config.MaxSubscriptionsPerVault == 0 {
		config.MaxSubscriptionsPerVault = 100
	}
	if config.MaxTotalSubscriptions == 0 {
		config.MaxTotalSubscriptions = 1000
	}
	registry := newRegistry()
	writeEvents := make(chan *EngramEvent, writeEventBufSize)
	cogEvents := make(chan CognitiveEvent, cogEventBufSize)
	contraEvents := make(chan ContradictEvent, contraEventBufSize)
	embedEvents := make(chan *EmbedEvent, embedEventBufSize)
	deliver := &DeliveryRouter{registry: registry}

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		fts:          fts,
		hnsw:         hnsw,
		embedder:     embedder,
		deliver:      deliver,
		writeEvents:  writeEvents,
		cogEvents:    cogEvents,
		contraEvents: contraEvents,
		embedEvents:  embedEvents,
	}

	return &TriggerSystem{
		registry:         registry,
		worker:           worker,
		embedCache:       newEmbedCache(),
		store:            store,
		embedder:         embedder,
		config:           config,
		WriteEvents:      writeEvents,
		CognitiveEvents:  cogEvents,
		ContradictEvents: contraEvents,
		EmbedEvents:      embedEvents,
	}
}

// Start launches the trigger worker goroutine.
func (ts *TriggerSystem) Start(ctx context.Context) {
	go ts.worker.Run(ctx)
}

// Subscribe adds a new subscription.
func (ts *TriggerSystem) Subscribe(sub *Subscription) error {
	// T3: enforce subscription caps before registering.
	if ts.config.MaxTotalSubscriptions > 0 && ts.registry.CountTotal() >= ts.config.MaxTotalSubscriptions {
		return ErrGlobalSubscriptionLimitReached
	}
	if ts.config.MaxSubscriptionsPerVault > 0 && ts.registry.CountForVault(sub.VaultID) >= ts.config.MaxSubscriptionsPerVault {
		return ErrVaultSubscriptionLimitReached
	}

	if sub.RateLimit <= 0 {
		sub.RateLimit = 10
	}
	sub.rateLimiter = newTokenBucket(sub.RateLimit)
	sub.pushedScores = make(map[storage.ULID]float64)
	sub.createdAt = time.Now()
	if sub.TTL > 0 {
		sub.expiresAt = sub.createdAt.Add(sub.TTL)
	}

	if ts.embedder != nil && len(sub.Context) > 0 {
		if vec, ok := ts.embedCache.Get(sub.Context); ok {
			sub.embedding = vec
		} else {
			embedCtx, embedCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer embedCancel()
			if vec, err := ts.embedder.Embed(embedCtx, sub.Context); err == nil {
				sub.embedding = vec
				ts.embedCache.Set(sub.Context, vec)
			}
		}
	}

	ts.registry.Add(sub)
	return nil
}

// Unsubscribe removes a subscription.
func (ts *TriggerSystem) Unsubscribe(id string) {
	ts.registry.Remove(id)
}

// NotifyWrite sends a write event to the trigger system.
func (ts *TriggerSystem) NotifyWrite(vaultID uint32, eng *storage.Engram, isNew bool) {
	select {
	case ts.WriteEvents <- &EngramEvent{VaultID: vaultID, Engram: eng, IsNew: isNew}:
	default:
		// Drop — buffer full, sweep will catch it
	}
}

// NotifyEmbed re-evaluates PushOnWrite subscriptions for an engram whose
// embedding just finished computing asynchronously (#512). Non-blocking; drops
// on a full buffer (the periodic sweep is the backstop).
func (ts *TriggerSystem) NotifyEmbed(vaultID uint32, eng *storage.Engram, vec []float32) {
	if eng == nil || len(vec) == 0 {
		return
	}
	select {
	case ts.EmbedEvents <- &EmbedEvent{VaultID: vaultID, Engram: eng, Embedding: vec}:
	default:
		// Drop — buffer full, sweep will catch it
	}
}

// NotifyCognitive sends a cognitive change event.
func (ts *TriggerSystem) NotifyCognitive(vaultID uint32, id storage.ULID, field string, old, new float32) {
	delta := new - old
	if delta < 0 {
		delta = -delta
	}
	if delta < 0.001 {
		return
	}
	select {
	case ts.CognitiveEvents <- CognitiveEvent{VaultID: vaultID, EngramID: id, Field: field, OldValue: old, NewValue: new, Delta: delta}:
	default:
	}
}

// ForVault returns all active subscriptions for a given vault ID.
// Exposed for testing.
func (ts *TriggerSystem) ForVault(vaultID uint32) []*Subscription {
	return ts.registry.ForVault(vaultID)
}

// PruneExpired removes TTL-expired subscriptions and returns the count removed.
// Exposed for testing; the worker also calls this automatically every 30s.
func (ts *TriggerSystem) PruneExpired() int {
	return ts.registry.PruneExpired()
}

// NotifyContradiction sends a contradiction event.
func (ts *TriggerSystem) NotifyContradiction(vaultID uint32, a, b storage.ULID, severity float64, typ string) {
	select {
	case ts.ContradictEvents <- ContradictEvent{VaultID: vaultID, EngramA: a, EngramB: b, Severity: severity, Type: typ}:
	default:
	}
}
