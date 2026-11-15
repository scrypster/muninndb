// Package embedlane carries the caller's explicit embed-lane choice through a
// context.Context, from the code that knows WHY it is embedding (recall's query
// embed) to the provider that schedules the work (the bundled local ONNX
// embedder).
//
// It is a leaf package on purpose: the recall pipeline (internal/engine/
// activation) sets the marker and the embed providers (internal/plugin/embed)
// read it, and the embed package already imports activation, so the marker
// cannot live in either without an import cycle.
//
// The signal is EXPLICIT. A provider must never infer the lane from the shape
// of the call (for example "one text means a query"): a single-text write-path
// embed would then compete with recall for the query lane, and a multi-phrase
// recall would queue behind the retroactive backfill. Anything not marked is
// background work and takes the batch lane.
package embedlane

import "context"

type queryKey struct{}

// WithQuery marks ctx as a recall query embed: a short text a caller is
// actively waiting on, which must not queue behind background batch embeds.
// Scope it to the embed call alone — a context derived from it carries the
// mark too, so do not hand a marked context to unrelated work.
func WithQuery(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryKey{}, true)
}

// IsQuery reports whether ctx was marked by WithQuery.
func IsQuery(ctx context.Context) bool {
	v, _ := ctx.Value(queryKey{}).(bool)
	return v
}
