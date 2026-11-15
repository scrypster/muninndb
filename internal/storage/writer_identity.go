package storage

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// WriterIdentity is the hashed identity of the caller that created an engram:
// the 16-hex-char keyed session and credential hashes the label pipeline uses
// to decide self-labelling ("was this memory written in the session that
// recalled it?"). Raw tokens and session ids never reach storage.
type WriterIdentity struct {
	SessionHash    string
	CredentialHash string
}

type writerIdentityKey struct{}

// ContextWithWriterIdentity returns ctx carrying the writer identity that the
// create and evolve provenance entries of writes under ctx will record.
func ContextWithWriterIdentity(ctx context.Context, w WriterIdentity) context.Context {
	return context.WithValue(ctx, writerIdentityKey{}, w)
}

// stampWriterIdentity returns d with the ctx's writer identity added (a copy;
// d is never mutated). Returns d unchanged when ctx carries none, so a write
// from a transport that carries no session records exactly what it did
// before.
func stampWriterIdentity(ctx context.Context, d *provenance.Details) *provenance.Details {
	w, ok := ctx.Value(writerIdentityKey{}).(WriterIdentity)
	if !ok || (w.SessionHash == "" && w.CredentialHash == "") {
		return d
	}
	var out provenance.Details
	if d != nil {
		out = *d
	}
	out.SessionHash = w.SessionHash
	out.CredentialHash = w.CredentialHash
	return &out
}

// DrainProvenance blocks until every queued provenance entry is written. For
// tests that read provenance right after a write.
func (ps *PebbleStore) DrainProvenance() {
	if ps.provWork != nil {
		ps.provWork.Drain()
	}
}

// FlushCounters synchronously writes every pending vault-count update the
// counter coalescer would otherwise write on its next 100ms tick. A test seam
// for byte-identical before/after store snapshots (the label pipeline's
// read-only-writes-nothing pins): without it, a seed write's coalesced count
// can land between the two snapshots and look like a write by the read under
// test.
func (ps *PebbleStore) FlushCounters() {
	if ps.counterFlush != nil {
		ps.counterFlush.flush()
	}
}

// GetEngramUncached reads an engram straight from Pebble and does NOT fill
// the L1 cache. For feedback's liveness checks, which run concurrently with
// feedback's own access bump: GetEngram's read-then-cache is not ordered
// against a concurrent writer, so a reader that decoded the pre-bump value
// can store it AFTER the writer's commit and leave a stale cache entry
// (measured: 2 of 40 concurrent runs served AccessCount 0 with 1 persisted).
// That fill race is general and pre-existing; this read simply does not take
// part in it.
func (ps *PebbleStore) GetEngramUncached(ctx context.Context, wsPrefix [8]byte, id ULID) (*Engram, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	val, err := Get(ps.db, keys.EngramKey(wsPrefix, [16]byte(id)))
	if err != nil {
		return nil, fmt.Errorf("get engram: %w", err)
	}
	if val == nil {
		return nil, fmt.Errorf("engram %w", ErrNotFound)
	}
	erfEng, err := erf.Decode(val)
	if err != nil {
		return nil, fmt.Errorf("decode engram: %w", err)
	}
	return fromERFEngram(erfEng), nil
}
