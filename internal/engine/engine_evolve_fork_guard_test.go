package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// Evolve on a memory that has ALREADY been superseded used to mint a second
// successor beside the first — a version fork (one memory, two rival
// replacements) that recall then refuses to resolve. These pin the refusal:
// loud, naming the current head, and writing nothing.
//
// Fixtures are invented (beekeeping).

func supersedersOf(t *testing.T, h *versionHeadHarness, id string) []string {
	t.Helper()
	rev, err := h.eng.store.GetReverseAssociations(h.ctx, h.ws, h.mustParse(id), 256)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[storage.ULID]bool{}
	var out []string
	for _, a := range rev {
		if a.RelType == storage.RelSupersedes && !seen[a.TargetID] {
			seen[a.TargetID] = true
			out = append(out, a.TargetID.String())
		}
	}
	return out
}

func TestEvolve_SupersededTargetIsRefusedAndNamesTheHead(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("hive inspection cadence", "Hives are inspected every nine days in spring.")
	b := h.evolve(a, "hive inspection cadence", "Hives are inspected every seven days in spring.")
	c := h.evolve(b, "hive inspection cadence", "Hives are inspected every five days during swarm season.")

	for _, stale := range []string{a, b} {
		_, err := h.eng.Evolve(h.ctx, "default", stale, "Hives are inspected weekly.", "stale target", nil, "")
		if err == nil {
			t.Fatalf("evolve of already-superseded %s succeeded — that mints a fork", stale)
		}
		if !errors.Is(err, ErrEvolveTargetSuperseded) {
			t.Errorf("err = %v, want ErrEvolveTargetSuperseded", err)
		}
		if !strings.Contains(err.Error(), c) {
			t.Errorf("error must name the current head %s so the caller can evolve it: %v", c, err)
		}
		if got := supersedersOf(t, h, stale); len(got) != 1 {
			t.Errorf("%s now has superseders %v — a refused evolve must write nothing", stale, got)
		}
	}
	// The head itself still evolves normally.
	h.evolve(c, "hive inspection cadence", "Hives are inspected every four days during swarm season.")
}

func TestEvolve_ForgottenHeadIsAClearErrorNotAFork(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("varroa treatment", "Oxalic acid is applied in the broodless period.")
	b := h.evolve(a, "varroa treatment", "Oxalic acid vapour is applied in the broodless period.")
	h.softDelete(b) // plain forget of the successor: soft-deleted, ValidUntil open

	_, err := h.eng.Evolve(h.ctx, "default", a, "Formic acid strips are used in late summer.", "new method", nil, "")
	if err == nil {
		t.Fatal("evolve of a memory whose successor was forgotten succeeded — that forks the chain")
	}
	if !errors.Is(err, ErrEvolveTargetSuperseded) || !strings.Contains(err.Error(), b) || !strings.Contains(err.Error(), "forgotten") {
		t.Errorf("want a superseded-target error naming forgotten successor %s, got: %v", b, err)
	}
	if got := supersedersOf(t, h, a); len(got) != 1 {
		t.Errorf("%s superseders = %v, want only the original successor", a, got)
	}
}

// TestEvolve_ConcurrentEvolvesOfOneMemory_ExactlyOneWins: without a guard
// that is atomic with the commit, two simultaneous evolves both read the
// target as current and both write a successor.
func TestEvolve_ConcurrentEvolvesOfOneMemory_ExactlyOneWins(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write("queen marking colour", "Queens are marked white in years ending 1 or 6.")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = h.eng.Evolve(h.ctx, "default", a, "Queens are marked yellow in years ending 2 or 7.", "colour code", nil, "")
		}(i)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrEvolveTargetSuperseded):
			t.Errorf("losing evolve failed for the wrong reason: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d of %d concurrent evolves succeeded, want exactly 1", wins, n)
	}
	if got := supersedersOf(t, h, a); len(got) != 1 {
		t.Fatalf("target has %d superseders %v, want exactly 1 (a fork was written)", len(got), got)
	}
}
