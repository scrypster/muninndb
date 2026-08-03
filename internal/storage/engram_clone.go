package storage

// Clone returns a private, fully writable copy of e.
//
// # Why this exists
//
// GetEngram, GetEngrams and L1Cache.Get all return the SHARED L1-cache pointer.
// That is deliberate — the cache exists so that a hot read costs a map lookup
// and not a decode, and recall reads hundreds of engrams per query — and
// L1Cache.Get documents the returned struct as read-only. Clone is the
// sanctioned way for a WRITER to obtain something it may modify:
//
//	eng, err := ps.GetEngram(ctx, ws, id)
//	...
//	eng = eng.Clone()   // from here on the struct is ours
//	eng.State = StateSoftDeleted
//
// Mutating a cache-returned pointer in place is a data race against every
// concurrent recall, and it publishes UNCOMMITTED state to readers the instant
// the assignment runs — before validation, let alone before the batch commits.
// Both halves of that have been live defects (#492 dedup, #858 mutateEngram),
// and `TestCachedEngramMutationCensus` now walks the module's AST to keep the
// class closed rather than trusting the doc comment on L1Cache.Get, which was
// added by #492 and did not prevent #858 or the six other sites #858's census
// found.
//
// # What is copied
//
// The struct, plus every slice-valued field: Tags, KeyPoints, Associations and
// Embedding. String fields are shared, which is safe — Go strings are
// immutable. Association is a flat value type with no pointer or slice fields,
// so copying the outer slice is a full copy of the edges.
//
// If a slice-valued field is added to Engram, add it here: the census sees
// element mutation (`eng.Tags[0] = x`) as a sink, but it cannot see a NEW field
// this method forgot to copy.
//
// # Cost, measured
//
// BenchmarkGetEngramWarm on an L1 hit, Apple Silicon: the bare read is
// 113-132 ns/op, 80 B/op, 2 allocs. Adding this clone makes it 247-273 ns/op,
// 560 B/op, 5 allocs — ~2.1x. That number is why the fix is copy-on-MUTATE and not
// copy-on-READ: seven call sites pay it, instead of every cached read in the
// product paying it on recall's hottest path.
//
// Clone on a nil receiver returns nil, so a caller need not special-case a
// missing engram before copying.
func (e *Engram) Clone() *Engram {
	if e == nil {
		return nil
	}
	c := *e
	if e.Tags != nil {
		c.Tags = append([]string(nil), e.Tags...)
	}
	if e.KeyPoints != nil {
		c.KeyPoints = append([]string(nil), e.KeyPoints...)
	}
	if e.Associations != nil {
		c.Associations = append([]Association(nil), e.Associations...)
	}
	if e.Embedding != nil {
		c.Embedding = append([]float32(nil), e.Embedding...)
	}
	return &c
}
