package contradictext

import "testing"

// PRIVACY: every string in this file is INVENTED. None of it is derived from
// any real vault, memory, tag, entity, or vault name. The fixtures are generic
// software-config assertions chosen only to exercise the detectors.

func TestConflict(t *testing.T) {
	cases := []struct {
		name     string
		a        string
		b        string
		wantHit  bool
		wantKind string
	}{
		// --- contradictions that MUST be detected -----------------------------
		{
			name:     "numeric mismatch: rate limit value swap",
			a:        "Rate limit is 100 req/s",
			b:        "Rate limit is 250 req/s",
			wantHit:  true,
			wantKind: KindNumeric,
		},
		{
			name:     "numeric mismatch: percentage swap across tense",
			a:        "Error rate was 50%",
			b:        "Error rate is 2%",
			wantHit:  true,
			wantKind: KindNumeric,
		},
		{
			name:     "boolean flip: true vs false",
			a:        "Payload validation is true",
			b:        "Payload validation is false",
			wantHit:  true,
			wantKind: KindNumeric,
		},
		{
			name:     "polarity flip: negation over shared predicate",
			a:        "Auth tokens never expire",
			b:        "Auth tokens expire after 24 hours",
			wantHit:  true,
			wantKind: KindPolarity,
		},
		{
			name:     "polarity flip: antonym enabled vs disabled",
			a:        "Feature flag beta is enabled",
			b:        "Feature flag beta is disabled",
			wantHit:  true,
			wantKind: KindPolarity,
		},
		{
			name:     "polarity flip: antonym allowed vs denied",
			a:        "Anonymous access is allowed",
			b:        "Anonymous access is denied",
			wantHit:  true,
			wantKind: KindPolarity,
		},

		// --- paraphrases that MUST NOT be flagged (the decisive property) -----
		// Embedding-similar, semantically equivalent, zero conflict signal.
		{
			name:    "paraphrase: 200-on-success vs responds-OK",
			a:       "returns 200 on success",
			b:       "responds OK when it succeeds",
			wantHit: false,
		},
		{
			name:    "paraphrase: deploy-on-merge reworded",
			a:       "deploys on merge to main",
			b:       "merging to main triggers a deploy",
			wantHit: false,
		},

		// --- unrelated pairs: no shared subject, never flagged ---------------
		{
			name:    "unrelated: cache ttl vs upload size (both have numbers)",
			a:       "Cache TTL is 60 seconds",
			b:       "Max upload size is 10 megabytes",
			wantHit: false,
		},
		{
			name:    "unrelated: no shared subject, opposite-sounding words",
			a:       "The scheduler is active",
			b:       "The mailbox is inactive",
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, kind := Conflict(tc.a, tc.b)
			if got != tc.wantHit {
				t.Fatalf("Conflict(%q, %q) hit=%v want %v (reason=%q kind=%q)",
					tc.a, tc.b, got, tc.wantHit, reason, kind)
			}
			if tc.wantHit && kind != tc.wantKind {
				t.Fatalf("Conflict(%q, %q) kind=%q want %q (reason=%q)",
					tc.a, tc.b, kind, tc.wantKind, reason)
			}
			if got && reason == "" {
				t.Fatalf("Conflict(%q, %q) reported a hit with an empty reason", tc.a, tc.b)
			}
			if !got && (reason != "" || kind != KindNone) {
				t.Fatalf("Conflict(%q, %q) no hit but reason=%q kind=%q", tc.a, tc.b, reason, kind)
			}
		})
	}
}

// TestKnownLimits pins the honest failure modes the proven miner documented, so
// a future change that alters them is a deliberate, visible decision rather than
// silent drift. These assert CURRENT behaviour, not desired behaviour.
func TestKnownLimits(t *testing.T) {
	// KNOWN LIMIT 1 — existential present-vs-absent MISS (a false negative,
	// consistent with the miner's recall 0.93). "was removed" carries no lexical
	// conflict signal (no negation cue, no antonym, no value swap), so a genuine
	// semantic contradiction is not detected. Model-free is the trade.
	if hit, _, _ := Conflict(
		"The service exposes a metrics endpoint",
		"The metrics endpoint was removed last quarter",
	); hit {
		t.Fatalf("known-limit existential present/absent unexpectedly flagged; the miss is expected")
	}

	// KNOWN LIMIT 2 — mixed historical state FALSE POSITIVE. Both statements can
	// be simultaneously true when separated in time ("was 50%" during an
	// incident, "is 2%" now), but the detector is tense-blind: it sees a value
	// swap over a shared subject and flags it. A time/tense-aware caller must
	// resolve this; the text detector cannot.
	if hit, _, kind := Conflict(
		"Error rate was 50%",
		"Error rate is 2%",
	); !hit || kind != KindNumeric {
		t.Fatalf("known-limit mixed-historical: want numeric hit (tense-blind), got hit=%v kind=%q", hit, kind)
	}
}

// TestNumericDetectorIsLoadBearing is a RED-sanity check: disable the numeric
// detector and the pure-numeric case must stop being flagged. If this still
// passed with EnableNumeric=false, the numeric detector would be dead code and
// something else would be (wrongly) catching the case.
func TestNumericDetectorIsLoadBearing(t *testing.T) {
	a, b := "Rate limit is 100 req/s", "Rate limit is 250 req/s"

	d := New()
	if hit, _, kind := d.Conflict(a, b); !hit || kind != KindNumeric {
		t.Fatalf("with numeric enabled: want numeric hit, got hit=%v kind=%q", hit, kind)
	}

	d.EnableNumeric = false
	if hit, _, _ := d.Conflict(a, b); hit {
		t.Fatalf("numeric detector disabled but numeric case still flagged — detector is not load-bearing")
	}
}

// TestPolarityDetectorIsLoadBearing is the same RED-sanity check for polarity.
func TestPolarityDetectorIsLoadBearing(t *testing.T) {
	a, b := "Feature flag beta is enabled", "Feature flag beta is disabled"

	d := New()
	if hit, _, kind := d.Conflict(a, b); !hit || kind != KindPolarity {
		t.Fatalf("with polarity enabled: want polarity hit, got hit=%v kind=%q", hit, kind)
	}

	d.EnablePolarity = false
	if hit, _, _ := d.Conflict(a, b); hit {
		t.Fatalf("polarity detector disabled but polarity case still flagged — detector is not load-bearing")
	}
}

// TestDeterministic guards the cheap-and-repeatable contract: identical inputs
// yield identical verdicts across runs (maps are read-only during detection).
func TestDeterministic(t *testing.T) {
	a, b := "Auth tokens never expire", "Auth tokens expire after 24 hours"
	firstHit, firstReason, firstKind := Conflict(a, b)
	for i := 0; i < 50; i++ {
		hit, reason, kind := Conflict(a, b)
		if hit != firstHit || reason != firstReason || kind != firstKind {
			t.Fatalf("non-deterministic verdict on run %d: (%v,%q,%q) != (%v,%q,%q)",
				i, hit, reason, kind, firstHit, firstReason, firstKind)
		}
	}
}
