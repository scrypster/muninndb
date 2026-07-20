package grpc

import (
	"context"

	"github.com/scrypster/muninndb/internal/engine"
	googlegrpc "google.golang.org/grpc"
)

// TestableAuthUnaryInterceptor exposes the unexported authUnaryInterceptor
// for use by external tests in package grpc_test.
func (s *Server) TestableAuthUnaryInterceptor(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
	return s.authUnaryInterceptor(ctx, req, info, handler)
}

// TestableAuthStreamInterceptor exposes the unexported authStreamInterceptor
// for use by external tests in package grpc_test.
func (s *Server) TestableAuthStreamInterceptor(srv any, ss googlegrpc.ServerStream, info *googlegrpc.StreamServerInfo, handler googlegrpc.StreamHandler) error {
	return s.authStreamInterceptor(srv, ss, info, handler)
}

// TestableNewEngineAdapter exposes the unexported grpcEngineAdapter constructor
// for use by external tests. A nil engine is safe for AdjustConfidence tests
// that exercise the ULID-parsing branches — the parser returns before the
// engine is dereferenced.
func TestableNewEngineAdapter(eng *engine.Engine) EngineAPI {
	return &grpcEngineAdapter{eng: eng}
}

// TestableMapAdjustConfidenceError exposes the unexported sentinel→gRPC-code
// mapper for direct table-driven testing.
func TestableMapAdjustConfidenceError(err error) error {
	return mapAdjustConfidenceError(err)
}

// TestableCallerFromContext exposes the unexported caller-extraction helper
// for direct table-driven testing. Pins the three branches: API key with
// Label (preferred), ID fallback when Label is empty, and "anonymous" for
// the public-vault path where the interceptor sets no key.
func TestableCallerFromContext(ctx context.Context) string {
	return callerFromContext(ctx)
}
