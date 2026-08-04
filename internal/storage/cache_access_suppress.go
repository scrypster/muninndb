package storage

import "context"

// ctxKeyNoAccessCacheStamp marks a ctx whose GetEngram/GetEngrams calls must
// not stamp the L1 cache's recency timestamp.
//
// The cache's lastAccess is not purely a cache-eviction detail — activation
// reads it back via EngramLastAccessNs as a real recency SCORING input for a
// LATER, unrelated Activate() call (engine/activation/engine.go, phase6). A
// scoring pass that merely SCORES a candidate (whether or not it emits it)
// is not a user access; without this, any ReadOnly recall could make an
// aged, genuinely-never-touched memory look "just accessed" to whatever
// real recall runs next, purely by having glanced at it.
type ctxKeyNoAccessCacheStamp struct{}

// ContextWithNoAccessCacheStamp returns a ctx that suppresses L1 cache
// recency stamping for the GetEngram/GetEngrams calls made with it.
func ContextWithNoAccessCacheStamp(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyNoAccessCacheStamp{}, true)
}

func noAccessCacheStampFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyNoAccessCacheStamp{}).(bool)
	return v
}
