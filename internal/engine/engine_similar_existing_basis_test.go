package engine

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// F5 (712-currency fix round): the response-wide basis allowlist missed
// activation.BasisNoContentChannel, and filterSimilarExisting indexed
// resp.Activations[0] unconditionally — a ZERO-ROW response (a genuine
// shape on an uncalibrated or degraded vault, not just "nothing similar")
// silently skipped the calibration-gate check entirely, presenting a loud
// "couldn't measure" condition as a quiet, confident absence.
// ---------------------------------------------------------------------------

// TestFilterSimilarExisting_ZeroRow_SemanticDegraded_YieldsLoudBasis is the
// unit-level RED: a hand-built zero-Activations response with
// SemanticDegraded set must still produce a non-empty OmittedBasis, never
// silence indistinguishable from "genuinely nothing similar".
func TestFilterSimilarExisting_ZeroRow_SemanticDegraded_YieldsLoudBasis(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	newID := storage.NewULID()
	probeEng := &storage.Engram{
		Concept: "zero row uncalibrated probe", Content: "no candidates exist yet",
		CreatedAt: time.Now(),
	}

	resp := &mbp.ActivateResponse{
		Activations:      nil, // genuinely zero rows
		SemanticDegraded: true,
	}

	adv := eng.filterSimilarExisting(resp, newID, probeEng)
	if len(adv.Items) != 0 {
		t.Fatalf("unexpected items on a zero-row response: %+v", adv.Items)
	}
	if adv.OmittedBasis == "" {
		t.Fatal("F5 violated: a zero-row, semantically-degraded self-query response " +
			"produced an empty OmittedBasis — silence indistinguishable from " +
			"\"genuinely nothing similar\"")
	}
}

// TestSimilarExistingResponseWideBases_IncludesNoContentChannel is the
// direct, cheap pin for F5's first sub-item: the allowlist must include
// every response-wide basis engine_relevance.go can produce, and
// no_content_channel was missing.
func TestSimilarExistingResponseWideBases_IncludesNoContentChannel(t *testing.T) {
	if !similarExistingResponseWideBases["no_content_channel"] {
		t.Fatal("F5 violated: similarExistingResponseWideBases is missing no_content_channel")
	}
}
