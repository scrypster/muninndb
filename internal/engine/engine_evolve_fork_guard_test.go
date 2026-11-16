package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Evolve on a memory that has ALREADY been superseded used to mint a second
// successor beside the first — a version fork (one memory, two rival
// replacements) that recall then refuses to resolve. These pin the refusal:
// loud, naming the current head, and writing nothing.
//
// Fixtures are invented (beekeeping).

func supersedersOf(t *testing.T, h *versionHeadHarness, id string) []string {
	t.Helper()
	rev, err := h.eng.store.GetReverseAssociations(h.ctx, h.ws, h.mustParse(id), 0) // 0 = uncapped
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

// TestEvolve_GuardIsNotStarvedByOtherIncomingLinks: the guard used to read at
// most 256 incoming links ordered by weight, so on a memory with >= 256
// max-weight links of another type the supersedes link fell past the cap and
// a second evolve forked.
func TestEvolve_GuardIsNotStarvedByOtherIncomingLinks(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write("apiary site", "The apiary sits on the south-facing slope behind the orchard.")
	for i := 0; i < 260; i++ {
		src := h.write(fmt.Sprintf("hive record %d", i), fmt.Sprintf("Hive %d is kept at the apiary site.", i))
		if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{Vault: "default", SourceID: src, TargetID: a,
			RelType: uint16(storage.RelRelatesTo), Weight: 1.0}); err != nil {
			t.Fatal(err)
		}
	}
	h.evolve(a, "apiary site", "The apiary moved to the east hedge line.")
	_, err := h.eng.Evolve(h.ctx, "default", a, "The apiary moved to the north meadow.", "second", nil, "")
	if !errors.Is(err, ErrEvolveTargetSuperseded) {
		t.Fatalf("second evolve err = %v, want ErrEvolveTargetSuperseded", err)
	}
	if got := supersedersOf(t, h, a); len(got) != 1 {
		t.Fatalf("superseders = %d, want 1", len(got))
	}
}

// TestEvolve_ForkRepairAdviceWorks follows the fork error's own advice end to
// end. A SOFT forget does not repair a fork: the forgotten branch keeps its
// supersedes link, and recall's head walk still sees two successors and
// abstains as ambiguous — so the guard keeps refusing, consistently with
// recall, and the advice says hard-forget. A hard forget removes the branch
// and its link; then the guard names the surviving head and recall resolves.
func TestEvolve_ForkRepairAdviceWorks(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	root := h.write("smoker fuel", "Pine needles are used as smoker fuel.")
	b1 := h.evolve(root, "smoker fuel", "Hessian sacking is used as smoker fuel.")
	// A legacy fork: a second successor written the way a pre-guard store has it.
	b2 := h.write("smoker fuel", "Cardboard is used as smoker fuel.")
	if err := h.eng.store.WriteAssociation(h.ctx, h.ws, h.mustParse(b2), h.mustParse(root), &storage.Association{
		TargetID: h.mustParse(root), RelType: storage.RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	evolveRoot := func() error {
		_, err := h.eng.Evolve(h.ctx, "default", root, "Wood shavings are used as smoker fuel.", "x", nil, "")
		return err
	}
	err := evolveRoot()
	if err == nil || !strings.Contains(err.Error(), "forked") || !strings.Contains(err.Error(), "hard=true") {
		t.Fatalf("want the fork error with hard-forget advice, got %v", err)
	}
	// Soft forget: not a repair (recall would still abstain), so still refused.
	if _, err := h.eng.Forget(h.ctx, &mbp.ForgetRequest{Vault: "default", ID: b2}); err != nil {
		t.Fatal(err)
	}
	if err := evolveRoot(); err == nil || !strings.Contains(err.Error(), "forked") {
		t.Fatalf("after a soft forget the fork still exists for recall; want the fork error, got %v", err)
	}
	// The advice: hard-forget one branch.
	if _, err := h.eng.Forget(h.ctx, &mbp.ForgetRequest{Vault: "default", ID: b2, Hard: true}); err != nil {
		t.Fatal(err)
	}
	err = evolveRoot()
	if !errors.Is(err, ErrEvolveTargetSuperseded) || strings.Contains(err.Error(), "forked") || !strings.Contains(err.Error(), b1) {
		t.Fatalf("after hard-forgetting one branch: err = %v, want the ordinary superseded error naming %s", err, b1)
	}
	gate := newVisibilityGate(&activation.ActivateRequest{}, time.Now())
	walk := h.eng.resolveSupersessionHead(h.ctx, h.ws, h.mustParse(root), gate, func(*storage.Engram) bool { return true })
	if walk.Head == nil || walk.Head.ID.String() != b1 {
		t.Fatalf("recall head walk after repair = %+v, want head %s", walk, b1)
	}
	h.evolve(b1, "smoker fuel", "Wood shavings are used as smoker fuel.")
}

// Declaring a supersedes link is the other way to give a memory a successor,
// so it must pass the same guard under the same lock.
func TestLinkSupersedes_SecondSuccessorIsRefused(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write("frame spacing", "Frames are spaced at 38mm.")
	b := h.write("frame spacing", "Frames are spaced at 35mm.")
	c := h.write("frame spacing", "Frames are spaced at 32mm.")
	h.linkSupersedes(b, a)
	// Re-declaring the same edge is idempotent, not a fork.
	h.linkSupersedes(b, a)
	_, err := h.eng.Link(h.ctx, &mbp.LinkRequest{Vault: "default", SourceID: c, TargetID: a, RelType: uint16(storage.RelSupersedes), Weight: 1})
	if !errors.Is(err, ErrEvolveTargetSuperseded) || !strings.Contains(err.Error(), b) {
		t.Fatalf("second supersedes link err = %v, want ErrEvolveTargetSuperseded naming %s", err, b)
	}
	if got := supersedersOf(t, h, a); len(got) != 1 {
		t.Fatalf("superseders = %v, want only %s", got, b)
	}
}

func TestLinkSupersedes_RacingAnEvolveNeverForks(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	for trial := 0; trial < 30; trial++ {
		a := h.write(fmt.Sprintf("swarm trap %d", trial), fmt.Sprintf("Swarm trap %d hangs at two metres.", trial))
		c := h.write(fmt.Sprintf("swarm trap %d", trial), fmt.Sprintf("Swarm trap %d hangs at three metres.", trial))
		var wg sync.WaitGroup
		var evErr, linkErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, evErr = h.eng.Evolve(h.ctx, "default", a, fmt.Sprintf("Swarm trap %d hangs at four metres.", trial), "race", nil, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, linkErr = h.eng.Link(h.ctx, &mbp.LinkRequest{Vault: "default", SourceID: c, TargetID: a, RelType: uint16(storage.RelSupersedes), Weight: 1})
		}()
		close(start)
		wg.Wait()
		if got := supersedersOf(t, h, a); len(got) != 1 {
			t.Fatalf("trial %d: %d superseders (evolve err %v, link err %v) — a fork", trial, len(got), evErr, linkErr)
		}
		if (evErr == nil) == (linkErr == nil) {
			t.Fatalf("trial %d: want exactly one winner, got evolve=%v link=%v", trial, evErr, linkErr)
		}
	}
}
