package grpc

import (
	"context"
	"errors"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcEngineAdapter adapts *engine.Engine to grpcpkg.EngineAPI, translating
// between pb.* (Protocol Buffers) types used by gRPC and mbp.* types used
// by the engine. One struct, zero duplication.
type grpcEngineAdapter struct {
	eng *engine.Engine
}

// NewEngineAdapter returns an EngineAPI that delegates to the given engine.
func NewEngineAdapter(eng *engine.Engine) EngineAPI {
	return &grpcEngineAdapter{eng: eng}
}

func (a *grpcEngineAdapter) Hello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloResponse, error) {
	resp, err := a.eng.Hello(ctx, &mbp.HelloRequest{
		Version: req.Version, AuthMethod: req.AuthMethod, Token: req.Token,
		Vault: req.Vault, Client: req.Client, Capabilities: req.Capabilities,
	})
	if err != nil {
		return nil, err
	}
	return &pb.HelloResponse{
		ServerVersion: resp.ServerVersion,
		SessionID:     resp.SessionID,
		VaultID:       resp.VaultID,
		Capabilities:  resp.Capabilities,
	}, nil
}

func (a *grpcEngineAdapter) Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	mbpAssocs := make([]mbp.Association, len(req.Associations))
	for i, assoc := range req.Associations {
		mbpAssocs[i] = mbp.Association{
			TargetID: assoc.TargetID, RelType: uint16(assoc.RelType),
			Weight: assoc.Weight, Confidence: assoc.Confidence,
			CreatedAt: assoc.CreatedAt, LastActivated: assoc.LastActivated,
		}
	}
	// NOTE: Inline enrichment fields (Summary, Entities, Relationships) are not yet
	// in the proto schema. They pass through as zero values until the proto is updated.
	resp, err := a.eng.Write(ctx, &mbp.WriteRequest{
		Concept: req.Concept, Content: req.Content, Tags: req.Tags,
		Confidence: req.Confidence, Stability: req.Stability, Vault: req.Vault,
		IdempotentID: req.IdempotentID, Associations: mbpAssocs, Embedding: req.Embedding,
		MemoryType: uint8(req.MemoryType), TypeLabel: req.TypeLabel,
	})
	if err != nil {
		return nil, err
	}
	return &pb.WriteResponse{ID: resp.ID, CreatedAt: resp.CreatedAt}, nil
}

func (a *grpcEngineAdapter) BatchWrite(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
	mbpReqs := make([]*mbp.WriteRequest, len(req.Requests))
	for i, r := range req.Requests {
		mbpAssocs := make([]mbp.Association, len(r.Associations))
		for j, assoc := range r.Associations {
			mbpAssocs[j] = mbp.Association{
				TargetID: assoc.TargetID, RelType: uint16(assoc.RelType),
				Weight: assoc.Weight, Confidence: assoc.Confidence,
				CreatedAt: assoc.CreatedAt, LastActivated: assoc.LastActivated,
			}
		}
		// NOTE: Inline enrichment fields not yet in proto (see single Write).
		mbpReqs[i] = &mbp.WriteRequest{
			Concept: r.Concept, Content: r.Content, Tags: r.Tags,
			Confidence: r.Confidence, Stability: r.Stability, Vault: r.Vault,
			IdempotentID: r.IdempotentID, Associations: mbpAssocs, Embedding: r.Embedding,
			MemoryType: uint8(r.MemoryType), TypeLabel: r.TypeLabel,
		}
	}
	responses, errs := a.eng.WriteBatch(ctx, mbpReqs)
	results := make([]*pb.BatchWriteItemResult, len(mbpReqs))
	for i := range mbpReqs {
		result := &pb.BatchWriteItemResult{Index: int32(i)}
		if errs[i] != nil {
			result.Error = errs[i].Error()
		} else if responses[i] != nil {
			result.Id = responses[i].ID
		}
		results[i] = result
	}
	return &pb.BatchWriteResponse{Results: results}, nil
}

func (a *grpcEngineAdapter) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	resp, err := a.eng.Read(ctx, &mbp.ReadRequest{ID: req.ID, Vault: req.Vault})
	if err != nil {
		return nil, err
	}
	return &pb.ReadResponse{
		ID: resp.ID, Concept: resp.Concept, Content: resp.Content,
		Confidence: resp.Confidence, Relevance: resp.Relevance, Tags: resp.Tags,
		State: uint32(resp.State), CreatedAt: resp.CreatedAt, UpdatedAt: resp.UpdatedAt,
		LastAccess: resp.LastAccess, AccessCount: uint32(resp.AccessCount), Stability: resp.Stability,
		MemoryType: uint32(resp.MemoryType), TypeLabel: resp.TypeLabel,
	}, nil
}

func (a *grpcEngineAdapter) Activate(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
	resp, err := a.eng.Activate(ctx, &mbp.ActivateRequest{
		Context: req.Context, Threshold: req.Threshold, MaxResults: int(req.MaxResults),
		MaxHops: int(req.MaxHops), IncludeWhy: req.IncludeWhy, Vault: req.Vault, Embedding: req.Embedding,
	})
	if err != nil {
		return nil, err
	}
	items := make([]pb.ActivationItem, len(resp.Activations))
	for i, item := range resp.Activations {
		items[i] = pb.ActivationItem{
			ID: item.ID, Concept: item.Concept, Content: item.Content,
			Score: item.Score, Why: item.Why,
		}
	}
	return &pb.ActivateResponse{
		QueryID: resp.QueryID, TotalFound: int32(resp.TotalFound),
		Activations: items, LatencyMs: resp.LatencyMs,
	}, nil
}

func (a *grpcEngineAdapter) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
	resp, err := a.eng.Link(ctx, &mbp.LinkRequest{
		SourceID: req.SourceID, TargetID: req.TargetID,
		RelType: uint16(req.RelType), Weight: req.Weight, Vault: req.Vault,
	})
	if err != nil {
		return nil, err
	}
	return &pb.LinkResponse{OK: resp.OK}, nil
}

func (a *grpcEngineAdapter) Forget(ctx context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
	resp, err := a.eng.Forget(ctx, &mbp.ForgetRequest{ID: req.ID, Hard: req.Hard, Vault: req.Vault})
	if err != nil {
		return nil, err
	}
	return &pb.ForgetResponse{OK: resp.OK}, nil
}

func (a *grpcEngineAdapter) BatchForget(ctx context.Context, req *pb.BatchForgetRequest) (*pb.BatchForgetResponse, error) {
	results := make([]*pb.BatchForgetItemResult, len(req.Requests))
	for i, r := range req.Requests {
		result := &pb.BatchForgetItemResult{Index: int32(i)}
		resp, err := a.eng.Forget(ctx, &mbp.ForgetRequest{ID: r.ID, Hard: r.Hard, Vault: r.Vault})
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Ok = resp.OK
		}
		results[i] = result
	}
	return &pb.BatchForgetResponse{Results: results}, nil
}

func (a *grpcEngineAdapter) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	resp, err := a.eng.Stat(ctx, &mbp.StatRequest{Vault: req.Vault})
	if err != nil {
		return nil, err
	}
	return &pb.StatResponse{
		EngramCount: resp.EngramCount, StorageBytes: resp.StorageBytes,
		VaultCount: int32(resp.VaultCount), IndexSize: resp.IndexSize,
	}, nil
}

func (a *grpcEngineAdapter) ListVaults(ctx context.Context, _ *pb.ListVaultsRequest) (*pb.ListVaultsResponse, error) {
	vaults, err := a.eng.ListVaults(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.ListVaultsResponse{Vaults: vaults}, nil
}

func (a *grpcEngineAdapter) Subscribe(ctx context.Context, req *pb.SubscribeRequest) (*pb.SubscribeResponse, error) {
	resp, err := a.eng.Subscribe(ctx, &mbp.SubscribeRequest{
		SubscriptionID: req.SubscriptionID, Context: req.Context,
		Threshold: req.Threshold, Vault: req.Vault, TTL: int(req.TTL),
		RateLimit: int(req.RateLimit), PushOnWrite: req.PushOnWrite, DeltaThreshold: req.DeltaThreshold,
	})
	if err != nil {
		return nil, err
	}
	return &pb.SubscribeResponse{SubID: resp.SubID, Status: resp.Status}, nil
}

func (a *grpcEngineAdapter) SubscribeWithDeliver(ctx context.Context, req *pb.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	return a.eng.SubscribeWithDeliver(ctx, &mbp.SubscribeRequest{
		SubscriptionID: req.SubscriptionID, Context: req.Context,
		Threshold: req.Threshold, Vault: req.Vault, TTL: int(req.TTL),
		RateLimit: int(req.RateLimit), PushOnWrite: req.PushOnWrite, DeltaThreshold: req.DeltaThreshold,
	}, deliver)
}

func (a *grpcEngineAdapter) Unsubscribe(ctx context.Context, subID string) error {
	return a.eng.Unsubscribe(ctx, subID)
}

// AdjustConfidence adapts the AdjustConfidence RPC to engine.Engine.AdjustConfidence.
// ULID parsing failures return codes.InvalidArgument without reaching the engine;
// engine sentinel errors (ErrInvalidArgument, ErrSelfContradiction, ErrEngramNotFound)
// are mapped to the codes the engine contract documents (400 / INVALID_ARGUMENT and
// 404 / NOT_FOUND). All other errors surface as the engine returned them.
func (a *grpcEngineAdapter) AdjustConfidence(ctx context.Context, req *pb.AdjustConfidenceRequest) (*pb.AdjustConfidenceResponse, error) {
	id, err := storage.ParseULID(req.EngramId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "engram_id %q: %v", req.EngramId, err)
	}
	hasContra := req.ContradictedById != ""
	var other storage.ULID
	if hasContra {
		other, err = storage.ParseULID(req.ContradictedById)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "contradicted_by_id %q: %v", req.ContradictedById, err)
		}
	} else {
		other = id
	}
	newConf, err := a.eng.AdjustConfidence(ctx, req.Vault, id, req.Delta, other, hasContra, req.Reason, callerFromContext(ctx))
	if err != nil {
		return nil, mapAdjustConfidenceError(err)
	}
	return &pb.AdjustConfidenceResponse{NewConfidence: newConf}, nil
}

// mapAdjustConfidenceError maps the engine sentinel errors that AdjustConfidence
// documents (engine_vault.go: "REST/gRPC handlers map it to 400/INVALID_ARGUMENT")
// onto the corresponding gRPC codes. It is local to this RPC because the existing
// Link/Forget adapter surface does not currently perform sentinel→code mapping
// (those errors surface as codes.Unknown); AdjustConfidence was specified to do so.
func mapAdjustConfidenceError(err error) error {
	switch {
	case errors.Is(err, engine.ErrInvalidArgument), errors.Is(err, engine.ErrSelfContradiction):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, engine.ErrEngramNotFound), errors.Is(err, storage.ErrNotFound):
		return status.Errorf(codes.NotFound, "%v", err)
	default:
		return err
	}
}

// callerFromContext extracts a human-readable caller identifier from the
// authenticated principal in ctx. authUnaryInterceptor sets auth.ContextAPIKey
// to the validated *auth.APIKey on the mk_ path; the public-vault path leaves
// it unset. Returns the key Label (operator-assigned, stable, human-readable),
// falling back to the key ID when Label is empty, and "anonymous" when no key
// is present — so the audit log always carries a non-empty caller field.
//
// Mirrors the MCP transport's AuthContext.IsAPIKey extraction (context.go),
// read from the same context value the gRPC interceptor already populates.
func callerFromContext(ctx context.Context) string {
	if key, ok := ctx.Value(auth.ContextAPIKey).(*auth.APIKey); ok && key != nil {
		if key.Label != "" {
			return key.Label
		}
		return key.ID
	}
	return "anonymous"
}
