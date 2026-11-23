package plugin

import "context"

// ProcessBatch runs one processor pass synchronously. It exists for the
// external test package (retroactive_enrich_service_test.go), which drives a
// real enrich.EnrichService and therefore cannot be package plugin: the enrich
// package imports this one.
func (rp *RetroactiveProcessor) ProcessBatch(ctx context.Context) bool {
	return rp.processBatch(ctx)
}
