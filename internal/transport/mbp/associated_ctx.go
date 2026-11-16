package mbp

import "context"

// The COG-35 `associated` block is part of what a recall SERVED: its ids are
// recorded with the recall (the label pipeline accepts labels for them). A
// surface whose response type has no place for the block — gRPC, the embedded
// Go library — must therefore never receive one, or ids the caller never saw
// would be recorded as served. The engine builds the block only when the
// request context says the response will carry it, so a surface that does not
// opt in cannot drift into recording undelivered ids: forgetting the marker
// loses the block, it never mislabels.
type associatedRenderedKey struct{}

// WithAssociatedRendered marks ctx: the caller returns ActivateResponse's
// Associated (and AssociatedOmitted) to its client verbatim. Set by MCP
// muninn_recall, REST /api/activate and the MBP ACTIVATE handler.
func WithAssociatedRendered(ctx context.Context) context.Context {
	return context.WithValue(ctx, associatedRenderedKey{}, true)
}

// AssociatedRendered reports whether ctx carries WithAssociatedRendered.
func AssociatedRendered(ctx context.Context) bool {
	v, _ := ctx.Value(associatedRenderedKey{}).(bool)
	return v
}
