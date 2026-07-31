package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// appendAdditive: exported *Engine methods that only CREATE new state (allowed for append).
var appendAdditive = map[string]bool{
	"Write": true, "WriteBatch": true, "WriteIdempotency": true,
	"RememberTree": true, "AddChild": true, "RegisterVaultName": true,
	// Intend only CREATES: a new TypeGoal engram (via Write) plus new 0x2D
	// armed-intention keys. It never modifies or deletes existing state.
	"Intend": true,
}

// appendInfra: engine wiring/lifecycle/getters with no credential-reachable
// data-mutation path (workers, plugins, the store handle, lifecycle).
var appendInfra = map[string]bool{
	"ClearCognitiveWorkers": true, "SetCognitiveWorkers": true, "SetCoordinator": true,
	"SetEnrichPlugin": true, "SetLatencyTracker": true, "SetOnWrite": true,
	"SetReplayEnrichTimeout": true, "SetRetroactiveProcessors": true, "SetTransitionWorker": true,
	"Store": true, "ResetReplayFailCount": true, "Stop": true, "Checkpoint": true,
	"Observability": true, "LatencyTracker": true, "ActivityTracker": true,
}

// appendReadOnly: read/query methods, safe for append.
var appendReadOnly = map[string]bool{
	"Activate": true, "ActivateWithStructuredFilter": true, "ActivityCounts": true,
	"CheckIdempotency": true, "CountChildren": true, "CountEmbedded": true, "EmbedStats": true,
	"Explain": true, "ExportGraph": true, "ExportVault": true, "FindByEntity": true,
	"FindSimilarEntities": true, "GetAnnotations": true, "GetAssociations": true,
	"GetAssociationsBatch": true, "GetContradictions": true, "GetContradictionReport": true,
	"GetEngram":               true,
	"GetEnrichmentCandidates": true, "GetEnrichmentMode": true, "GetEntityAggregate": true,
	"GetEntityClusters": true, "GetEntityTimeline": true, "GetNoveltyDrops": true,
	"GetProcessorStats": true, "GetProvenance": true, "GetVaultEmbedDim": true, "GetVaultJob": true,
	"Hello": true, "ListDeleted": true, "ListEngrams": true, "ListEntities": true, "ListVaults": true,
	"Read": true, "RecallTree": true, "ReevaluatePushOnEmbed": true, "ResolveVaultPlasticity": true,
	"Session": true, "SessionPaged": true, "Stat": true, "Subscribe": true, "SubscribeWithDeliver": true,
	"Traverse": true, "Unsubscribe": true, "VaultNameExists": true, "WhereLeftOff": true, "WorkerStats": true,
	// NoticesFor* are read/query surfaces with ONE recorded residual write: on
	// delivery they bump the fired-marker (FiredCount/LastFiredAt; a one-shot
	// consumes its own 0x2D keys). That is delivery bookkeeping on the
	// intention's OWN advisory index — the same access-metadata residual class
	// as TouchAccess under SEC-15 (#682): append/read paths may strengthen or
	// consume delivery state, never overwrite/evolve/forget an engram. COG-11
	// readOnly (observe) suppresses even the marker.
	"NoticesForRecall": true, "NoticesForRemember": true,
}

// TestAppendMode_MethodCensus is the ANTI-ROT pin the #687 reviews demanded: it
// reflects EVERY exported *Engine method and requires each to be classified as
// guarded-destructive (in the guardedOps table below), additive, infra, or
// read-only. A newly-added method — even an innocuously-named one like Link or a
// hidden destructive one like Restore/PruneVault (both of which slipped past the
// hand-maintained table) — is UNCLASSIFIED and fails here, forcing a human to
// decide its bucket (and gate it if destructive). This is real completeness, not
// a checklist someone must remember to update (principle #6; cf. SEC-6).
func TestAppendMode_MethodCensus(t *testing.T) {
	guarded := guardedOpNames()
	rt := reflect.TypeOf(&Engine{})
	for i := 0; i < rt.NumMethod(); i++ {
		name := rt.Method(i).Name
		buckets := 0
		if guarded[name] {
			buckets++
		}
		if appendAdditive[name] {
			buckets++
		}
		if appendInfra[name] {
			buckets++
		}
		if appendReadOnly[name] {
			buckets++
		}
		if buckets == 0 {
			t.Errorf("UNCLASSIFIED *Engine method %q: classify it. If it modifies/deletes an EXISTING engram/entity/lease/vault, add a refuseAppend guard and put it in guardedOps; otherwise add it to appendAdditive / appendInfra / appendReadOnly.", name)
		}
		if buckets > 1 {
			t.Errorf("*Engine method %q is in more than one append classification set", name)
		}
	}
}

// TestAppendMode_RefusesEveryDestructiveOp is the completeness pin (both #687
// reviews): the append guarantee must hold at the ENGINE layer for every method
// that modifies or deletes an existing engram/entity/lease/enrichment, so it is
// transport-agnostic (the MCP dispatch gate alone left REST/gRPC/MBP open — the
// confirmed break). Each closure calls one such method under an append-mode ctx
// and must get ErrAppendForbidden. A newly-added destructive method that forgets
// the refuseAppend guard is caught by adding it here — and if it is destructive
// and NOT added here, that is the omission this test exists to make visible.
// guardedOps returns one closure per engine method that modifies/deletes an
// EXISTING engram/entity/lease/vault and must refuse append. The closures are
// invoked by TestAppendMode_RefusesEveryDestructiveOp; their KEYS are the
// "guarded-destructive" bucket the census (TestAppendMode_MethodCensus) checks.
// eng/ap may be nil when only the key set is needed (the closures are not called).
func guardedOps(eng *Engine, ap context.Context) map[string]func() error {
	var z storage.ULID
	const v, id = "append-cov-vault", "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	return map[string]func() error{
		"UpdateTags":           func() error { return eng.UpdateTags(ap, v, z, []string{"t"}) },
		"AdjustConfidence":     func() error { _, e := eng.AdjustConfidence(ap, v, z, 0.1, z, false, "r", "c"); return e },
		"UpdateLifecycleState": func() error { return eng.UpdateLifecycleState(ap, v, id, "archived") },
		"SetTrust":             func() error { return eng.SetTrust(ap, v, id, "verified") },
		"Consolidate":          func() error { _, e := eng.Consolidate(ap, v, []string{id}, "m"); return e },
		"Decide":               func() error { _, e := eng.Decide(ap, v, "d", "r", nil, nil); return e },
		"RecordFeedback":       func() error { return eng.RecordFeedback(ap, v, id, true) },
		"RecordAccess":         func() error { return eng.RecordAccess(ap, v, id) },
		"SetEntityState":       func() error { return eng.SetEntityState(ap, "E", "merged", "", "") },
		"SetEntityStateBatch":  func() error { errs := eng.SetEntityStateBatch(ap, []EntityStateOp{{}}); return errs[0] },
		"CompareAndSet":        func() error { _, e := eng.CompareAndSet(ap, v, id, nil, nil); return e },
		"Claim":                func() error { _, e := eng.Claim(ap, v, id, "o", 60); return e },
		"Release":              func() error { _, e := eng.Release(ap, v, id, "o"); return e },
		"MergeEntity":          func() error { _, e := eng.MergeEntity(ap, v, "a", "b", false); return e },
		"ReplayEnrichment":     func() error { _, e := eng.ReplayEnrichment(ap, v, nil, 10, false); return e },
		"ApplyEnrichment":      func() error { _, e := eng.ApplyEnrichment(ap, v, &EnrichmentApplyRequest{}); return e },
		"Evolve":               func() error { _, e := eng.Evolve(ap, v, id, "new", "r", nil, ""); return e },
		"EvolveAt":             func() error { _, e := eng.EvolveAt(ap, v, id, "new", "r", nil, "", nil, nil, time.Time{}); return e },
		"Forget":               func() error { _, e := eng.Forget(ap, &mbp.ForgetRequest{Vault: v, ID: id}); return e },
		"Restore":              func() error { _, e := eng.Restore(ap, v, id); return e },
		"Link":                 func() error { _, e := eng.Link(ap, &mbp.LinkRequest{Vault: v, SourceID: id, TargetID: id}); return e },
		"PruneVault":           func() error { _, e := eng.PruneVault(ap, v); return e },
		"ClearVault":           func() error { return eng.ClearVault(ap, v) },
		"DeleteVault":          func() error { return eng.DeleteVault(ap, v) },
		"RenameVault":          func() error { return eng.RenameVault(ap, v, "v2") },
		"ResolveContradiction": func() error { return eng.ResolveContradiction(ap, v, id, id) },
		"StartMerge":           func() error { _, e := eng.StartMerge(ap, v, "t", false); return e },
		"StartClone":           func() error { _, e := eng.StartClone(ap, v, "n"); return e },
		"StartImport":          func() error { _, e := eng.StartImport(ap, v, "", 0, false, nil); return e },
		"StartReembedVault":    func() error { _, e := eng.StartReembedVault(ap, v, "m"); return e },
		"ReindexFTSVault":      func() error { _, e := eng.ReindexFTSVault(ap, v); return e },
	}
}

func guardedOpNames() map[string]bool {
	out := map[string]bool{}
	for k := range guardedOps(nil, context.Background()) {
		out[k] = true
	}
	return out
}

func TestAppendMode_RefusesEveryDestructiveOp(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ap := withMode(auth.ModeAppend)
	for name, fn := range guardedOps(eng, ap) {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrAppendForbidden) {
				t.Errorf("%s under append: err = %v, want ErrAppendForbidden", name, err)
			}
		})
	}
}

// TestAppendMode_RefusesEvolveAndForget proves the engine-layer backstop: an
// append-mode credential (auth.ModeAppend, threaded via ctx by every transport)
// cannot Evolve or Forget an existing memory, but can still Write (create) and
// the target memory survives the refused destructive calls. This holds
// regardless of transport, so a leaked/misconfigured append key cannot destroy.
func TestAppendMode_RefusesEvolveAndForget(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "append-vault"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "original memory to protect"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id := resp.ID

	// Evolve under append -> refused.
	if _, err := eng.Evolve(withMode(auth.ModeAppend), vault, id, "rewritten content", "reason", nil, ""); !errors.Is(err, ErrAppendForbidden) {
		t.Errorf("Evolve under append mode: err = %v, want ErrAppendForbidden", err)
	}
	// Forget under append -> refused.
	if _, err := eng.Forget(withMode(auth.ModeAppend), &mbp.ForgetRequest{Vault: vault, ID: id}); !errors.Is(err, ErrAppendForbidden) {
		t.Errorf("Forget under append mode: err = %v, want ErrAppendForbidden", err)
	}

	// The protected memory must be intact and unchanged.
	got, err := eng.Read(withMode(auth.ModeAppend), &mbp.ReadRequest{Vault: vault, ID: id, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read after refused destructive ops: %v", err)
	}
	if got.Content != "original memory to protect" {
		t.Errorf("content changed under append mode: %q", got.Content)
	}

	// Append CAN create a new memory (additive is allowed).
	if _, err := eng.Write(withMode(auth.ModeAppend), &mbp.WriteRequest{Vault: vault, Content: "a newly appended memory"}); err != nil {
		t.Errorf("append mode must allow Write (create): %v", err)
	}

	// Full mode is NOT append-forbidden (the gate is specific to append).
	if _, err := eng.Evolve(withMode(auth.ModeFull), vault, id, "full-mode rewrite", "reason", nil, ""); errors.Is(err, ErrAppendForbidden) {
		t.Errorf("full mode must not be append-forbidden")
	}
}
