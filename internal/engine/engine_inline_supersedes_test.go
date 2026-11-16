package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// An inline `supersedes` on a write (the `relationships` field muninn_remember
// sends, or the `associations` field REST/gRPC/MBP send) declares the same
// thing Link(relation=supersedes) declares: "this memory replaces that one".
// Until COG-38 the write path stored the edge and nothing else — no fork guard,
// no validity stamp, no append-mode refusal — so the same statement retired
// the old memory when said with muninn_link and left it live when said inline.
// These pin that the two now agree.
//
// Fixtures are invented (a pottery studio).

type supersedesField string

const (
	viaRelationships supersedesField = "relationships"
	viaAssociations  supersedesField = "associations"
)

var bothSupersedesFields = []supersedesField{viaRelationships, viaAssociations}

// inlineSupersedesWeight is the weight every fixture here declares, on both the
// inline and the Link side, so a comparison is never a comparison of weights.
const inlineSupersedesWeight = float32(0.9)

// supersedingReq builds a write that declares, inline, that it supersedes each
// target — through whichever of the two inline edge fields the case names.
func supersedingReq(field supersedesField, concept, content string, targets ...string) *mbp.WriteRequest {
	req := &mbp.WriteRequest{Vault: "default", Concept: concept, Content: content}
	for _, target := range targets {
		switch field {
		case viaRelationships:
			req.Relationships = append(req.Relationships, mbp.InlineRelationship{
				TargetID: target, Relation: "supersedes", Weight: inlineSupersedesWeight,
			})
		case viaAssociations:
			req.Associations = append(req.Associations, mbp.Association{
				TargetID: target, RelType: uint16(storage.RelSupersedes),
				Weight: inlineSupersedesWeight, Confidence: 1.0, CreatedAt: time.Now().UnixNano(),
			})
		}
	}
	return req
}

func (h *versionHeadHarness) engram(id string) *storage.Engram {
	h.t.Helper()
	eng, err := h.eng.store.GetEngram(h.ctx, h.ws, h.mustParse(id))
	if err != nil || eng == nil {
		h.t.Fatalf("read engram %s: %v", id, err)
	}
	return eng
}

// successors is the uncapped, uncached store read of who supersedes id.
func (h *versionHeadHarness) successors(id string) []string {
	h.t.Helper()
	succ, err := h.eng.store.SupersedersOf(h.ctx, h.ws, h.mustParse(id))
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, len(succ))
	for i, s := range succ {
		out[i] = s.String()
	}
	return out
}

func (h *versionHeadHarness) countEngrams() int {
	h.t.Helper()
	n := 0
	if err := h.eng.store.ScanEngrams(h.ctx, h.ws, func(*storage.Engram) error { n++; return nil }); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// retired reports the Link(relation=supersedes) retirement signature: the
// validity window is closed and the memory is still StateActive (Link stamps,
// it does not soft-delete — that is evolve).
func retiredLikeLink(eng *storage.Engram) bool {
	return !eng.ValidUntil.IsZero() && eng.State == storage.StateActive
}

const (
	kilnConcept   = "kiln firing schedule"
	kilnOld       = "Stoneware bisque firing peaks at cone six in the electric kiln."
	kilnNew       = "Earthenware batches now mature at a lower setting inside the gas chamber."
	kilnOldQuery  = "stoneware bisque firing peaks at cone six in the electric kiln"
	glazeConcept  = "glaze mixing ratio"
	glazeOld      = "Celadon glaze is blended at three parts feldspar to one part whiting."
	glazeNew      = "Shino slip recipes switched to nepheline syenite with spodumene added."
	glazeThird    = "Tenmoku batches are thinned with rainwater before the second dip."
	wheelConcept  = "wheel maintenance"
	wheelOld      = "The kick wheel bearings are greased on the first Monday of each month."
	wheelNew      = "Wheel head runout is checked with a dial gauge every fortnight."
	shelfConcept  = "drying shelf rotation"
	shelfContent  = "Greenware on the upper shelves is rotated a quarter turn each morning."
	shelfContent2 = "Leather-hard pieces move to the lower rack before trimming."
)

// TestInlineSupersedes_RetiresTargetExactlyAsLink: the target of an inline
// supersedes ends in the same stored state as the target of a supersedes link —
// validity window closed at write time, still StateActive, exactly one
// successor.
//
// RED on develop: the inline target's ValidUntil stays open.
func TestInlineSupersedes_RetiresTargetExactlyAsLink(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()

			linkOld := h.write(kilnConcept, kilnOld)
			linkNew := h.write(kilnConcept, kilnNew)
			if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{Vault: "default", SourceID: linkNew, TargetID: linkOld,
				RelType: uint16(storage.RelSupersedes), Weight: inlineSupersedesWeight}); err != nil {
				t.Fatalf("link supersedes: %v", err)
			}

			inOld := h.write(glazeConcept, glazeOld)
			before := h.engram(inOld)
			resp, err := h.eng.Write(h.ctx, supersedingReq(field, glazeConcept, glazeNew, inOld))
			if err != nil {
				t.Fatalf("inline supersedes write: %v", err)
			}
			inNew := resp.ID

			viaLink, viaInline := h.engram(linkOld), h.engram(inOld)
			if !retiredLikeLink(viaLink) {
				t.Fatalf("fixture: the Link target is not retired (state %v, valid_until %v)", viaLink.State, viaLink.ValidUntil)
			}
			if !retiredLikeLink(viaInline) {
				t.Errorf("inline supersedes left the target LIVE: state=%v valid_until=%v — Link retires the same declaration (state=%v valid_until=%v)",
					viaInline.State, viaInline.ValidUntil, viaLink.State, viaLink.ValidUntil)
			}
			if viaInline.State != viaLink.State {
				t.Errorf("target state: inline %v, link %v", viaInline.State, viaLink.State)
			}
			// Link's stamp is a patch of the one field: it does not touch UpdatedAt.
			if !viaInline.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("inline supersedes moved the target's UpdatedAt (%v -> %v); Link's stamp leaves it alone",
					before.UpdatedAt, viaInline.UpdatedAt)
			}
			if got := h.successors(inOld); len(got) != 1 || got[0] != inNew {
				t.Errorf("inline target successors = %v, want exactly [%s]", got, inNew)
			}
			if got := h.successors(linkOld); len(got) != 1 || got[0] != linkNew {
				t.Errorf("link target successors = %v, want exactly [%s]", got, linkNew)
			}
			// The successor itself is an ordinary current memory.
			if s := h.engram(inNew); s.State != storage.StateActive || !s.ValidUntil.IsZero() {
				t.Errorf("the superseding memory is not current: state=%v valid_until=%v", s.State, s.ValidUntil)
			}

			// Read presents the two targets the same way.
			readOf := func(id string) *mbp.ReadResponse {
				r, err := h.eng.Read(h.ctx, &mbp.ReadRequest{Vault: "default", ID: id, ReadOnly: true})
				if err != nil {
					t.Fatalf("read %s: %v", id, err)
				}
				return r
			}
			rl, ri := readOf(linkOld), readOf(inOld)
			if rl.IsCurrent != ri.IsCurrent || (rl.ValidUntil != 0) != (ri.ValidUntil != 0) || rl.State != ri.State {
				t.Errorf("read of the target differs: link {is_current:%v valid_until_set:%v state:%d} inline {is_current:%v valid_until_set:%v state:%d}",
					rl.IsCurrent, rl.ValidUntil != 0, rl.State, ri.IsCurrent, ri.ValidUntil != 0, ri.State)
			}

			// And the audit trail records the retirement the way Link's does.
			h.eng.store.DrainProvenance()
			stamped := func(id string) int {
				entries, err := h.eng.GetProvenance(h.ctx, "default", id)
				if err != nil {
					t.Fatalf("provenance %s: %v", id, err)
				}
				n := 0
				for _, e := range entries {
					if e.Operation == "stamp-valid-until" {
						n++
					}
				}
				return n
			}
			if l, i := stamped(linkOld), stamped(inOld); l != i {
				t.Errorf("stamp-valid-until provenance entries on the target: link %d, inline %d", l, i)
			}
		})
	}
}

// recallShape renders a recall response as roles and annotations, so two
// fixtures with different ids can be compared row for row.
func recallShape(resp *mbp.ActivateResponse, roles map[string]string) []string {
	role := func(id string) string {
		if id == "" {
			return "-"
		}
		if r, ok := roles[id]; ok {
			return r
		}
		return "other"
	}
	out := make([]string, 0, len(resp.Activations)+1)
	for _, a := range resp.Activations {
		out = append(out, fmt.Sprintf("%s superseded_by=%s current_version=%s substituted_for=%s expired=%v",
			role(a.ID), role(a.SupersededBy), role(a.CurrentVersion), role(a.SubstitutedFor), a.Expired))
	}
	if resp.Abstained {
		out = append(out, "abstained:"+resp.AbstainedReason)
	}
	return out
}

// TestInlineSupersedes_RecallPresentsItExactlyAsALink: two stores hold the same
// two memories; one declares the supersession with Link, the other inline on
// the write. Recall must present them identically — by default (the current
// memory answers for the old wording, the old one is gone) and in history mode
// (the old one is shown expired and annotated with what replaced it).
//
// RED on develop: the inline store's default recall still returns the old
// memory as a current fact.
func TestInlineSupersedes_RecallPresentsItExactlyAsALink(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			hl, cleanupL := newVersionHeadHarness(t)
			defer cleanupL()
			hi, cleanupI := newVersionHeadHarness(t)
			defer cleanupI()

			lOld := hl.write(kilnConcept, kilnOld)
			lNew := hl.write(kilnConcept, kilnNew)
			if _, err := hl.eng.Link(hl.ctx, &mbp.LinkRequest{Vault: "default", SourceID: lNew, TargetID: lOld,
				RelType: uint16(storage.RelSupersedes), Weight: inlineSupersedesWeight}); err != nil {
				t.Fatalf("link supersedes: %v", err)
			}
			lRoles := map[string]string{lOld: "old", lNew: "new"}

			iOld := hi.write(kilnConcept, kilnOld)
			resp, err := hi.eng.Write(hi.ctx, supersedingReq(field, kilnConcept, kilnNew, iOld))
			if err != nil {
				t.Fatalf("inline supersedes write: %v", err)
			}
			iRoles := map[string]string{iOld: "old", resp.ID: "new"}

			for _, view := range []struct {
				name   string
				mutate func(*mbp.ActivateRequest)
			}{
				{"default", func(*mbp.ActivateRequest) {}},
				{"include_invalid", func(r *mbp.ActivateRequest) { r.IncludeInvalid = true }},
			} {
				viaLink := recallShape(hl.recall(kilnOldQuery, view.mutate), lRoles)
				viaInline := recallShape(hi.recall(kilnOldQuery, view.mutate), iRoles)
				if strings.Join(viaLink, "\n") != strings.Join(viaInline, "\n") {
					t.Errorf("%s recall differs.\n after Link:\n   %s\n after inline supersedes:\n   %s",
						view.name, strings.Join(viaLink, "\n   "), strings.Join(viaInline, "\n   "))
				}
			}

			// Absolute, not only relative: the default view must not present
			// the superseded memory, and must present the new one.
			def := hi.recall(kilnOldQuery)
			if it := itemByID(def, iOld); it != nil {
				t.Errorf("default recall returned the superseded memory as a current fact (superseded_by=%q expired=%v)", it.SupersededBy, it.Expired)
			}
			if itemByID(def, resp.ID) == nil {
				t.Errorf("default recall did not return the superseding memory for the old wording: got %v", ids(def))
			}
			hist := hi.recall(kilnOldQuery, func(r *mbp.ActivateRequest) { r.IncludeInvalid = true })
			if it := itemByID(hist, iOld); it == nil {
				t.Errorf("include_invalid did not return the superseded memory: got %v", ids(hist))
			} else if !it.Expired || it.SupersededBy != resp.ID || it.CurrentVersion != resp.ID {
				t.Errorf("history row for the superseded memory: expired=%v superseded_by=%q current_version=%q, want expired and both naming %s",
					it.Expired, it.SupersededBy, it.CurrentVersion, resp.ID)
			}
		})
	}
}

// TestInlineSupersedes_AlreadySupersededTargetIsRefusedAndNamesHead: a memory
// that already has a successor cannot be given a second one by an inline
// supersedes — however it got the first. The refusal names the current head,
// and nothing is stored.
//
// RED on develop: every case writes the second successor (a fork), which is
// how 15 forks were found in a copy of production data.
func TestInlineSupersedes_AlreadySupersededTargetIsRefusedAndNamesHead(t *testing.T) {
	type firstSuccessor func(h *versionHeadHarness, target string) (head string)
	cases := map[string]firstSuccessor{
		"evolved": func(h *versionHeadHarness, target string) string {
			return h.evolve(target, glazeConcept, glazeNew)
		},
		"link-superseded": func(h *versionHeadHarness, target string) string {
			b := h.write(glazeConcept, glazeNew)
			h.linkSupersedes(b, target)
			return b
		},
		// The census's still-active case: two inline supersedes on one memory.
		"inline-superseded": func(h *versionHeadHarness, target string) string {
			resp, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, glazeConcept, glazeNew, target))
			if err != nil {
				h.t.Fatalf("first inline supersedes: %v", err)
			}
			return resp.ID
		},
	}
	for name, first := range cases {
		for _, field := range bothSupersedesFields {
			t.Run(name+"/"+string(field), func(t *testing.T) {
				h, cleanup := newVersionHeadHarness(t)
				defer cleanup()
				a := h.write(glazeConcept, glazeOld)
				head := first(h, a)
				engrams := h.countEngrams()
				stampedAt := h.engram(a).ValidUntil

				_, err := h.eng.Write(h.ctx, supersedingReq(field, glazeConcept, glazeThird, a))
				if err == nil {
					t.Fatalf("an inline supersedes onto already-superseded %s SUCCEEDED — that is a version fork (successors now %v)", a, h.successors(a))
				}
				if !errors.Is(err, ErrEvolveTargetSuperseded) {
					t.Errorf("err = %v, want ErrEvolveTargetSuperseded (the error Link and evolve return)", err)
				}
				if !strings.Contains(err.Error(), head) {
					t.Errorf("the refusal must name the current head %s so the caller can act on it: %v", head, err)
				}
				if got := h.successors(a); len(got) != 1 {
					t.Errorf("successors of %s = %v, want only the original one", a, got)
				}
				if got := h.countEngrams(); got != engrams {
					t.Errorf("a refused write stored something: %d engrams, was %d", got, engrams)
				}
				if got := h.engram(a).ValidUntil; !got.Equal(stampedAt) {
					t.Errorf("a refused write moved the target's valid_until (%v -> %v)", stampedAt, got)
				}
				// A refused write leaves no content-hash entry behind either: the
				// same text, without the declaration, is still a fresh memory.
				resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: glazeThird})
				if err != nil || resp.Hint == "duplicate_content" {
					t.Errorf("after the refusal the same content is not a fresh write: resp=%+v err=%v", resp, err)
				}
			})
		}
	}
}

// TestInlineSupersedes_MissingTargetIsRefused pins behaviour develop already
// has (STO-12, #817): a supersedes naming a memory that does not exist fails
// the write as a bad request. Not RED on develop — it is here so re-routing the
// supersedes edge cannot lose the refusal.
func TestInlineSupersedes_MissingTargetIsRefused(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()
			ghost := storage.NewULID().String()
			_, err := h.eng.Write(h.ctx, supersedingReq(field, glazeConcept, glazeNew, ghost))
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("err = %v, want ErrInvalidID for a supersedes target that does not exist", err)
			}
			if !strings.Contains(err.Error(), ghost) {
				t.Errorf("the refusal must name the missing target %s: %v", ghost, err)
			}
			if n := h.countEngrams(); n != 0 {
				t.Errorf("a refused write stored %d engrams", n)
			}
		})
	}
}

// TestInlineSupersedes_CrossVaultTargetIsRefused pins behaviour develop already
// has: a target id that lives in ANOTHER vault is not a legal target, and the
// memory in the other vault is not touched. Not RED on develop.
func TestInlineSupersedes_CrossVaultTargetIsRefused(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()
			other, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "glaze-lab", Concept: glazeConcept, Content: glazeOld})
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.eng.Write(h.ctx, supersedingReq(field, glazeConcept, glazeNew, other.ID))
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("err = %v, want ErrInvalidID for a supersedes target in another vault", err)
			}
			otherWS := h.eng.store.ResolveVaultPrefix("glaze-lab")
			eng, err := h.eng.store.GetEngram(h.ctx, otherWS, h.mustParse(other.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !eng.ValidUntil.IsZero() || eng.State != storage.StateActive {
				t.Errorf("the other vault's memory was modified: state=%v valid_until=%v", eng.State, eng.ValidUntil)
			}
			if succ, _ := h.eng.store.SupersedersOf(h.ctx, otherWS, h.mustParse(other.ID)); len(succ) != 0 {
				t.Errorf("the other vault's memory gained successors %v", succ)
			}
		})
	}
}

// TestInlineSupersedes_UnusableTargetIsRefusedLikeLink: Link refuses a
// soft-deleted endpoint. So must the inline form — superseding a forgotten
// memory would stamp a record the caller cannot see.
//
// RED on develop: the inline write succeeds and lands the edge.
func TestInlineSupersedes_UnusableTargetIsRefusedLikeLink(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()
			a := h.write(glazeConcept, glazeOld)
			b := h.write(glazeConcept, glazeNew)
			h.softDelete(a)

			_, linkErr := h.eng.Link(h.ctx, &mbp.LinkRequest{Vault: "default", SourceID: b, TargetID: a,
				RelType: uint16(storage.RelSupersedes), Weight: inlineSupersedesWeight})
			if !errors.Is(linkErr, ErrEngramSoftDeleted) {
				t.Fatalf("fixture: Link onto a soft-deleted target err = %v, want ErrEngramSoftDeleted", linkErr)
			}
			_, err := h.eng.Write(h.ctx, supersedingReq(field, glazeConcept, glazeThird, a))
			if !errors.Is(err, ErrEngramSoftDeleted) {
				t.Errorf("inline supersedes onto a soft-deleted target err = %v, want ErrEngramSoftDeleted (what Link returns)", err)
			}
			if got := h.successors(a); len(got) != 0 {
				t.Errorf("a refused write left successors %v on the forgotten memory", got)
			}
		})
	}
}

// supersedeOutcome is what one attempt to declare a supersession did.
type supersedeOutcome struct {
	refused   bool
	appendErr bool
	forkErr   bool
	retired   bool
	edges     int
}

func (o supersedeOutcome) String() string {
	switch {
	case o.appendErr:
		return fmt.Sprintf("REFUSED append-forbidden (retired=%v successors=%d)", o.retired, o.edges)
	case o.forkErr:
		return fmt.Sprintf("REFUSED already-superseded (retired=%v successors=%d)", o.retired, o.edges)
	case o.refused:
		return fmt.Sprintf("REFUSED other (retired=%v successors=%d)", o.retired, o.edges)
	}
	return fmt.Sprintf("accepted (retired=%v successors=%d)", o.retired, o.edges)
}

func (h *versionHeadHarness) outcome(target string, err error) supersedeOutcome {
	return supersedeOutcome{
		refused:   err != nil,
		appendErr: errors.Is(err, ErrAppendForbidden),
		forkErr:   errors.Is(err, ErrEvolveTargetSuperseded),
		retired:   retiredLikeLink(h.engram(target)),
		edges:     len(h.successors(target)),
	}
}

// TestInlineSupersedes_ModeTableAgreesWithLink is the per-credential-mode pin:
// for every mode, declaring a supersession inline on a write does exactly what
// declaring it with Link does — accepted or refused, for the same reason, with
// the same effect on the target. Two scenarios per mode: a live target (can
// this mode RETIRE another memory?) and an already-superseded one (can it FORK
// one?).
//
// The engine gates append (SEC-15, refuseAppend); observe is refused by every
// transport before the engine is reached, so at this layer it behaves like
// full — for Link and for the inline form alike, which is what is pinned.
//
// RED on develop: an append-mode credential's inline supersedes is accepted
// (Link refuses it), every mode's inline form leaves the target live, and every
// mode's inline form forks an already-superseded memory.
func TestInlineSupersedes_ModeTableAgreesWithLink(t *testing.T) {
	modes := []struct {
		name string
		ctx  context.Context
	}{
		{"full", withMode(auth.ModeFull)},
		{"write", withMode(auth.ModeWrite)},
		{"append", withMode(auth.ModeAppend)},
		{"observe", withMode(auth.ModeObserve)},
		{"none", context.Background()},
	}
	for _, field := range bothSupersedesFields {
		for _, m := range modes {
			t.Run(string(field)+"/"+m.name, func(t *testing.T) {
				h, cleanup := newVersionHeadHarness(t)
				defer cleanup()
				link := func(src, dst string) error {
					_, err := h.eng.Link(m.ctx, &mbp.LinkRequest{Vault: "default", SourceID: src, TargetID: dst,
						RelType: uint16(storage.RelSupersedes), Weight: inlineSupersedesWeight})
					return err
				}
				inline := func(concept, content, dst string) error {
					_, err := h.eng.Write(m.ctx, supersedingReq(field, concept, content, dst))
					return err
				}

				// Scenario 1 — a live target.
				lOld, lNew := h.write(kilnConcept, kilnOld), h.write(kilnConcept, kilnNew)
				viaLink := h.outcome(lOld, link(lNew, lOld))
				iOld := h.write(glazeConcept, glazeOld)
				viaInline := h.outcome(iOld, inline(glazeConcept, glazeNew, iOld))
				t.Logf("mode=%-7s field=%-13s live target      : link %v | inline %v", m.name, field, viaLink, viaInline)
				if viaLink != viaInline {
					t.Errorf("live target: Link and inline supersedes disagree under %s mode.\n   link:   %v\n   inline: %v", m.name, viaLink, viaInline)
				}

				// Scenario 2 — a target that already has a successor (declared
				// by a full-mode Link, so the fixture exists under every mode).
				fl, fi := h.write(wheelConcept, wheelOld), h.write(shelfConcept, shelfContent)
				h.linkSupersedes(h.write(wheelConcept, wheelNew), fl)
				h.linkSupersedes(h.write(shelfConcept, shelfContent2), fi)
				rival := h.write(wheelConcept, "The treadle linkage is oiled whenever it squeaks.")
				forkLink := h.outcome(fl, link(rival, fl))
				forkInline := h.outcome(fi, inline(shelfConcept, "Racks are wiped down with a damp sponge at closing.", fi))
				t.Logf("mode=%-7s field=%-13s superseded target: link %v | inline %v", m.name, field, forkLink, forkInline)
				if forkLink != forkInline {
					t.Errorf("already-superseded target: Link and inline supersedes disagree under %s mode.\n   link:   %v\n   inline: %v", m.name, forkLink, forkInline)
				}
				if forkInline.edges != 1 {
					t.Errorf("%s mode forked a memory with an inline supersedes: %d successors", m.name, forkInline.edges)
				}
			})
		}
	}
}

// TestInlineSupersedes_AppendModeIsRefusedOnEveryWriter: an append credential
// may create; it may not modify existing state — and retiring another memory
// is exactly that. The refusal must hold on every writer that accepts an
// inline edge, and must not cost the credential its ordinary creates.
//
// RED on develop: every writer accepts the append-mode supersedes.
func TestInlineSupersedes_AppendModeIsRefusedOnEveryWriter(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	ap := withMode(auth.ModeAppend)
	target := h.write(glazeConcept, glazeOld)
	unchanged := func(when string) {
		t.Helper()
		if eng := h.engram(target); !eng.ValidUntil.IsZero() || len(h.successors(target)) != 0 {
			t.Fatalf("%s: an append credential changed an existing memory (valid_until=%v successors=%v)", when, eng.ValidUntil, h.successors(target))
		}
	}

	for _, field := range bothSupersedesFields {
		if _, err := h.eng.Write(ap, supersedingReq(field, glazeConcept, glazeNew+" "+string(field), target)); !errors.Is(err, ErrAppendForbidden) {
			t.Errorf("Write(%s) under append: err = %v, want ErrAppendForbidden", field, err)
		}
		unchanged("Write " + string(field))

		ok := &mbp.WriteRequest{Vault: "default", Concept: shelfConcept, Content: shelfContent + " " + string(field)}
		_, errs := h.eng.WriteBatch(ap, []*mbp.WriteRequest{
			ok, supersedingReq(field, glazeConcept, glazeThird+" "+string(field), target),
		})
		if errs[0] != nil {
			t.Errorf("WriteBatch(%s) under append: the ordinary item failed: %v", field, errs[0])
		}
		if !errors.Is(errs[1], ErrAppendForbidden) {
			t.Errorf("WriteBatch(%s) under append: superseding item err = %v, want ErrAppendForbidden", field, errs[1])
		}
		unchanged("WriteBatch " + string(field))

		up := supersedingReq(field, glazeConcept, "Ash glaze tests are logged on the kiln-room chalkboard. "+string(field), target)
		up.UpsertMode, up.IdempotentID = true, "glaze-doc-"+string(field)
		if _, err := h.eng.Write(ap, up); !errors.Is(err, ErrAppendForbidden) {
			t.Errorf("upsert Write(%s) under append: err = %v, want ErrAppendForbidden", field, err)
		}
		unchanged("upsert " + string(field))
	}

	// Other inline relations stay additive: append may still declare them.
	rel := &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: "Celadon tiles are test-fired beside every production load.",
		Relationships: []mbp.InlineRelationship{{TargetID: target, Relation: "supports", Weight: 0.9}}}
	if _, err := h.eng.Write(ap, rel); err != nil {
		t.Errorf("append mode must still create a memory with a non-supersedes relationship: %v", err)
	}
	unchanged("supports relationship")
}

// TestInlineSupersedes_BatchRefusesOnlyTheBadItem: in a batch, an item whose
// supersedes cannot be honoured fails alone, with the same error a single
// write gets; its neighbours — including another superseding item — land.
//
// RED on develop: no item errors; the bad one forks its target and the good
// one leaves its target live.
func TestInlineSupersedes_BatchRefusesOnlyTheBadItem(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()
			taken := h.write(glazeConcept, glazeOld)
			head := h.evolve(taken, glazeConcept, glazeNew)
			free := h.write(wheelConcept, wheelOld)

			reqs := []*mbp.WriteRequest{
				{Vault: "default", Concept: shelfConcept, Content: shelfContent},
				supersedingReq(field, glazeConcept, glazeThird, taken), // bad: already superseded
				supersedingReq(field, wheelConcept, wheelNew, free),    // good
				{Vault: "default", Concept: shelfConcept, Content: shelfContent2},
			}
			resps, errs := h.eng.WriteBatch(h.ctx, reqs)

			for _, i := range []int{0, 2, 3} {
				if errs[i] != nil || resps[i] == nil || resps[i].ID == "" {
					t.Errorf("item %d should have been stored: resp=%+v err=%v", i, resps[i], errs[i])
				}
			}
			if errs[1] == nil {
				t.Fatalf("the item superseding an already-superseded memory was stored (successors of %s now %v)", taken, h.successors(taken))
			}
			if !errors.Is(errs[1], ErrEvolveTargetSuperseded) || !strings.Contains(errs[1].Error(), head) {
				t.Errorf("item 1 err = %v, want ErrEvolveTargetSuperseded naming head %s", errs[1], head)
			}
			if resps[1] != nil {
				t.Errorf("a refused item carries a response: %+v", resps[1])
			}
			if got := h.successors(taken); len(got) != 1 {
				t.Errorf("successors of the already-superseded memory = %v, want 1", got)
			}
			if !retiredLikeLink(h.engram(free)) {
				t.Errorf("the good superseding item did not retire its target")
			}
			if resps[2] != nil {
				if got := h.successors(free); len(got) != 1 || got[0] != resps[2].ID {
					t.Errorf("successors of the good target = %v, want [%s]", got, resps[2].ID)
				}
			}
			// 2 fixtures + 1 evolve successor + 3 stored items.
			if got := h.countEngrams(); got != 6 {
				t.Errorf("vault holds %d engrams, want 6 (the refused item must store nothing)", got)
			}
		})
	}
}

// TestInlineSupersedes_TwoBatchItemsSupersedingOneMemory: the second item sees
// the first one's commit — one wins, the other is refused naming it.
//
// RED on develop: both land and the target is forked.
func TestInlineSupersedes_TwoBatchItemsSupersedingOneMemory(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write(glazeConcept, glazeOld)
	resps, errs := h.eng.WriteBatch(h.ctx, []*mbp.WriteRequest{
		supersedingReq(viaRelationships, glazeConcept, glazeNew, a),
		supersedingReq(viaRelationships, glazeConcept, glazeThird, a),
	})
	if errs[0] != nil || resps[0] == nil {
		t.Fatalf("first superseding item: resp=%+v err=%v", resps[0], errs[0])
	}
	if !errors.Is(errs[1], ErrEvolveTargetSuperseded) || !strings.Contains(errs[1].Error(), resps[0].ID) {
		t.Errorf("second superseding item err = %v, want ErrEvolveTargetSuperseded naming %s", errs[1], resps[0].ID)
	}
	if got := h.successors(a); len(got) != 1 {
		t.Errorf("successors = %v, want exactly one", got)
	}
}

// TestInlineSupersedes_RetryNeitherDoubleRetiresNorErrors: the same write sent
// twice — with an op id (the receipt path) or without one (content-hash dedup)
// — answers with the first write's id, writes no second edge, and leaves the
// target's retirement instant where the first write put it.
//
// RED on develop for the retirement itself (the target is never retired); the
// no-error and same-id halves already hold there and are pinned so the new
// guard cannot break them.
func TestInlineSupersedes_RetryNeitherDoubleRetiresNorErrors(t *testing.T) {
	for _, field := range bothSupersedesFields {
		for _, opID := range []string{"glaze-update-0042", ""} {
			name := string(field) + "/content-dedup"
			if opID != "" {
				name = string(field) + "/op-id"
			}
			t.Run(name, func(t *testing.T) {
				h, cleanup := newVersionHeadHarness(t)
				defer cleanup()
				a := h.write(glazeConcept, glazeOld)
				send := func() (*mbp.WriteResponse, error) {
					req := supersedingReq(field, glazeConcept, glazeNew, a)
					req.IdempotentID = opID
					return h.eng.Write(h.ctx, req)
				}

				first, err := send()
				if err != nil {
					t.Fatalf("first write: %v", err)
				}
				stampedAt := h.engram(a).ValidUntil
				if stampedAt.IsZero() {
					t.Errorf("the first write did not retire the target")
				}
				engrams := h.countEngrams()

				for attempt := 2; attempt <= 3; attempt++ {
					again, err := send()
					if err != nil {
						t.Fatalf("attempt %d errored: %v — a retry of an accepted write must not fail", attempt, err)
					}
					if again.ID != first.ID {
						t.Errorf("attempt %d returned %s, want the first write's %s", attempt, again.ID, first.ID)
					}
					if again.Hint == "" {
						t.Errorf("attempt %d carries no hint; a replay must say it stored nothing new", attempt)
					}
				}
				if got := h.engram(a).ValidUntil; !got.Equal(stampedAt) {
					t.Errorf("the retry moved the retirement instant: %v -> %v", stampedAt, got)
				}
				if got := h.successors(a); len(got) != 1 || got[0] != first.ID {
					t.Errorf("successors after retries = %v, want [%s]", got, first.ID)
				}
				if got := h.countEngrams(); got != engrams {
					t.Errorf("the retries stored %d extra engrams", got-engrams)
				}
			})
		}
	}
}

// TestInlineSupersedes_FailedWriteLeavesTargetLive: when the write fails for an
// unrelated reason AFTER the supersedes target passed every check, the target
// must be exactly as it was — no stamp, no edge. Retirement and the new memory
// commit together or not at all.
//
// Not RED-able on develop (develop never retires, so there is nothing to leave
// half-done); it pins the ordering of the new code.
func TestInlineSupersedes_FailedWriteLeavesTargetLive(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write(glazeConcept, glazeOld)
	ghost := storage.NewULID().String()

	req := supersedingReq(viaRelationships, glazeConcept, glazeNew, a)
	req.Relationships = append(req.Relationships, mbp.InlineRelationship{TargetID: ghost, Relation: "supports", Weight: 0.7})
	if _, err := h.eng.Write(h.ctx, req); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID for the dangling supports target", err)
	}
	if eng := h.engram(a); !eng.ValidUntil.IsZero() {
		t.Errorf("the write failed but the supersedes target was retired anyway: valid_until=%v", eng.ValidUntil)
	}
	if got := h.successors(a); len(got) != 0 {
		t.Errorf("the write failed but left a supersedes edge: %v", got)
	}
	if n := h.countEngrams(); n != 1 {
		t.Errorf("the write failed but %d engrams exist, want 1", n)
	}
}

// TestInlineSupersedes_SameWeightSecondRelationIsRefused: an association key
// has no relation in it (STO-15), so two inline edges to one target at one
// weight are ONE key — the later would silently erase the supersedes edge, or
// be erased by it. Link refuses that collision; so does the write.
//
// RED on develop: the write succeeds and one of the two declarations is gone.
func TestInlineSupersedes_SameWeightSecondRelationIsRefused(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write(glazeConcept, glazeOld)

	req := supersedingReq(viaRelationships, glazeConcept, glazeNew, a)
	req.Relationships = append(req.Relationships, mbp.InlineRelationship{TargetID: a, Relation: "contradicts", Weight: inlineSupersedesWeight})
	if _, err := h.eng.Write(h.ctx, req); !errors.Is(err, storage.ErrAssocRelTypeCollision) {
		t.Fatalf("err = %v, want ErrAssocRelTypeCollision: supersedes and contradicts at one weight share a key", err)
	}
	if eng := h.engram(a); !eng.ValidUntil.IsZero() || len(h.successors(a)) != 0 {
		t.Errorf("a refused write changed the target (valid_until=%v successors=%v)", eng.ValidUntil, h.successors(a))
	}

	// At DIFFERENT weights both edges fit, and the write is honoured in full.
	ok := supersedingReq(viaRelationships, glazeConcept, glazeNew, a)
	ok.Relationships = append(ok.Relationships, mbp.InlineRelationship{TargetID: a, Relation: "contradicts", Weight: 0.6})
	resp, err := h.eng.Write(h.ctx, ok)
	if err != nil {
		t.Fatalf("distinct weights: %v", err)
	}
	fwd, err := h.eng.store.GetAssociations(h.ctx, h.ws, []storage.ULID{h.mustParse(resp.ID)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rels := map[storage.RelType]bool{}
	for _, e := range fwd[h.mustParse(resp.ID)] {
		rels[e.RelType] = true
	}
	if !rels[storage.RelSupersedes] || !rels[storage.RelContradicts] {
		t.Errorf("edges stored = %v, want both supersedes and contradicts", rels)
	}
	if !retiredLikeLink(h.engram(a)) {
		t.Errorf("the target was not retired")
	}
}

// TestInlineSupersedes_SeveralTargetsInOneWrite: one memory may replace several
// (Link allows it edge by edge). All are retired together, or — if any one is
// not a legal target — none is and nothing is stored.
//
// RED on develop: nothing is retired, and the mixed write forks.
func TestInlineSupersedes_SeveralTargetsInOneWrite(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a, b := h.write(glazeConcept, glazeOld), h.write(wheelConcept, wheelOld)
	resp, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, glazeConcept, glazeNew, a, b))
	if err != nil {
		t.Fatalf("write superseding two memories: %v", err)
	}
	for _, id := range []string{a, b} {
		if !retiredLikeLink(h.engram(id)) {
			t.Errorf("%s was not retired", id)
		}
		if got := h.successors(id); len(got) != 1 || got[0] != resp.ID {
			t.Errorf("successors of %s = %v, want [%s]", id, got, resp.ID)
		}
	}

	c := h.write(shelfConcept, shelfContent)
	n := h.countEngrams()
	if _, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, shelfConcept, shelfContent2, c, a)); !errors.Is(err, ErrEvolveTargetSuperseded) {
		t.Fatalf("mixed targets: err = %v, want ErrEvolveTargetSuperseded (one target is taken)", err)
	}
	if eng := h.engram(c); !eng.ValidUntil.IsZero() || len(h.successors(c)) != 0 {
		t.Errorf("the legal target was retired although the write was refused (valid_until=%v successors=%v)", eng.ValidUntil, h.successors(c))
	}
	if got := h.countEngrams(); got != n {
		t.Errorf("a refused write stored %d engrams", got-n)
	}
}

// TestInlineSupersedes_ConcurrentWritersNeverFork: N writers declare, at once,
// that their new memory supersedes the same one. Exactly one may win — the
// guard and the commit are one step under the target's evolve lock — and the
// same holds when the rival is an evolve.
//
// RED on develop: every writer wins.
func TestInlineSupersedes_ConcurrentWritersNeverFork(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	t.Run("inline-vs-inline", func(t *testing.T) {
		a := h.write(glazeConcept, glazeOld)
		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = h.eng.Write(h.ctx, supersedingReq(bothSupersedesFields[i%2], glazeConcept,
					fmt.Sprintf("Glaze batch %d is sieved twice through an eighty mesh screen.", i), a))
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
				t.Errorf("a losing writer failed for the wrong reason: %v", err)
			}
		}
		if wins != 1 || len(h.successors(a)) != 1 {
			t.Fatalf("%d of %d concurrent inline supersedes succeeded; successors=%v — want exactly one", wins, n, h.successors(a))
		}
	})

	t.Run("inline-vs-evolve", func(t *testing.T) {
		for trial := 0; trial < 20; trial++ {
			a := h.write(wheelConcept, fmt.Sprintf("Splash pan %d is emptied after every throwing session.", trial))
			var wg sync.WaitGroup
			var evErr, inErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, evErr = h.eng.Evolve(h.ctx, "default", a, fmt.Sprintf("Splash pan %d is emptied at the end of the day.", trial), "race", nil, "")
			}()
			go func() {
				defer wg.Done()
				<-start
				_, inErr = h.eng.Write(h.ctx, supersedingReq(viaRelationships, wheelConcept,
					fmt.Sprintf("Splash pan %d drains into the settling bucket.", trial), a))
			}()
			close(start)
			wg.Wait()
			if got := h.successors(a); len(got) != 1 {
				t.Fatalf("trial %d: %d successors (evolve err %v, inline err %v) — a fork", trial, len(got), evErr, inErr)
			}
			if (evErr == nil) == (inErr == nil) {
				t.Fatalf("trial %d: want exactly one winner, got evolve=%v inline=%v", trial, evErr, inErr)
			}
		}
	})
}

// TestInlineSupersedes_OtherInlineRelationsAreUnchanged pins what this change
// deliberately leaves alone: every other inline relation — contradicts
// included — is still a plain edge. No guard, no stamp, and it may point at a
// memory that already has a successor. GREEN on develop and after.
func TestInlineSupersedes_OtherInlineRelationsAreUnchanged(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write(glazeConcept, glazeOld)
	head := h.write(glazeConcept, glazeNew)
	h.linkSupersedes(head, a) // a already has a successor
	stampedAt := h.engram(a).ValidUntil
	live := h.write(wheelConcept, wheelOld)

	for i, rel := range []string{"contradicts", "supports", "relates_to", "refines", "depends_on"} {
		req := &mbp.WriteRequest{Vault: "default", Concept: shelfConcept,
			Content: fmt.Sprintf("Shelf note %d: bats are scraped clean before reuse.", i),
			Relationships: []mbp.InlineRelationship{
				{TargetID: a, Relation: rel, Weight: 0.5 + float32(i)*0.05},
				{TargetID: live, Relation: rel, Weight: 0.5 + float32(i)*0.05},
			}}
		if _, err := h.eng.Write(h.ctx, req); err != nil {
			t.Errorf("inline %s: %v — only supersedes is gated", rel, err)
		}
	}
	if got := h.engram(a).ValidUntil; !got.Equal(stampedAt) {
		t.Errorf("a non-supersedes relation moved a validity window: %v -> %v", stampedAt, got)
	}
	if eng := h.engram(live); !eng.ValidUntil.IsZero() {
		t.Errorf("a non-supersedes relation retired its target: valid_until=%v", eng.ValidUntil)
	}
	if got := h.successors(a); len(got) != 1 {
		t.Errorf("successors of %s = %v, want the one declared link", a, got)
	}
}

// TestInlineSupersedes_BackgroundOnlyVaultRefusesRatherThanDrops: a vault whose
// inline_enrichment is background_only ignores caller-supplied relationships.
// For any other relation that stays as it was. For supersedes it would be the
// exact outcome this invariant removes — an explicit declaration dropped
// without a word — so the write is refused, saying why and what to do instead.
// The `associations` field is not an enrichment field and is honoured there.
//
// RED on develop: the relationships write succeeds and the declaration is gone.
func TestInlineSupersedes_BackgroundOnlyVaultRefusesRatherThanDrops(t *testing.T) {
	eng, cleanup := testEnvWithInlineMode(t, "background_only")
	defer cleanup()
	ctx := context.Background()
	const vault = "test-vault"
	ws := eng.store.ResolveVaultPrefix(vault)
	write := func(content string) string {
		t.Helper()
		resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: glazeConcept, Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return resp.ID
	}
	state := func(id string) (*storage.Engram, []storage.ULID) {
		t.Helper()
		u, _ := storage.ParseULID(id)
		e, err := eng.store.GetEngram(ctx, ws, u)
		if err != nil {
			t.Fatal(err)
		}
		succ, err := eng.store.SupersedersOf(ctx, ws, u)
		if err != nil {
			t.Fatal(err)
		}
		return e, succ
	}

	old := write(glazeOld)
	req := supersedingReq(viaRelationships, glazeConcept, glazeNew, old)
	req.Vault = vault
	_, err := eng.Write(ctx, req)
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "background_only") {
		t.Fatalf("err = %v, want ErrInvalidRequest naming background_only — the declaration must not be silently dropped", err)
	}
	if e, succ := state(old); !e.ValidUntil.IsZero() || len(succ) != 0 {
		t.Errorf("a refused write changed the target (valid_until=%v successors=%v)", e.ValidUntil, succ)
	}
	// Refused means nothing stored: the same text is still a fresh write.
	if resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: glazeConcept, Content: glazeNew}); err != nil || resp.Hint != "" {
		t.Errorf("after the refusal the same content is not a fresh write: resp=%+v err=%v", resp, err)
	}
	// Other relations in relationships[] are ignored there exactly as before.
	other := &mbp.WriteRequest{Vault: vault, Concept: glazeConcept, Content: glazeThird,
		Relationships: []mbp.InlineRelationship{{TargetID: old, Relation: "supports", Weight: 0.9}}}
	if _, err := eng.Write(ctx, other); err != nil {
		t.Errorf("a non-supersedes relationship on a background_only vault must still be accepted (and ignored): %v", err)
	}

	// associations[] is not an enrichment field: honoured, and it retires.
	viaAssoc := supersedingReq(viaAssociations, wheelConcept, wheelNew, old)
	viaAssoc.Vault = vault
	resp, err := eng.Write(ctx, viaAssoc)
	if err != nil {
		t.Fatalf("associations supersedes on a background_only vault: %v", err)
	}
	if e, succ := state(old); !retiredLikeLink(e) || len(succ) != 1 || succ[0].String() != resp.ID {
		t.Errorf("associations supersedes did not retire the target (valid_until=%v successors=%v)", e.ValidUntil, succ)
	}
}

// TestInlineSupersedes_BatchOpIDReplayStaysAReplay: two items of one batch
// share an op id. The first is the write; the second is its replay and stores
// nothing — whichever of them carries the declaration.
//
// RED on develop for the retirement (the target stays live).
func TestInlineSupersedes_BatchOpIDReplayStaysAReplay(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	old := h.write(glazeConcept, glazeOld)
	first := supersedingReq(viaRelationships, glazeConcept, glazeNew, old)
	first.IdempotentID = "glaze-batch-0007"
	replay := &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: glazeThird, IdempotentID: "glaze-batch-0007"}

	resps, errs := h.eng.WriteBatch(h.ctx, []*mbp.WriteRequest{first, replay})
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errs = %v", errs)
	}
	if resps[1].ID != resps[0].ID || resps[1].Hint != "idempotent" {
		t.Errorf("the in-batch replay = %+v, want id %s with hint idempotent", resps[1], resps[0].ID)
	}
	if got := h.successors(old); len(got) != 1 || got[0] != resps[0].ID || !retiredLikeLink(h.engram(old)) {
		t.Errorf("the write was not honoured: successors=%v valid_until=%v", got, h.engram(old).ValidUntil)
	}
	if got := h.countEngrams(); got != 2 {
		t.Errorf("vault holds %d engrams, want 2 (the replay stores nothing)", got)
	}
	// And across calls: the receipt answers, nothing is re-applied.
	stampedAt := h.engram(old).ValidUntil
	again, errs2 := h.eng.WriteBatch(h.ctx, []*mbp.WriteRequest{first})
	if errs2[0] != nil || again[0].ID != resps[0].ID || again[0].Hint != "idempotent" {
		t.Errorf("a later batch replaying the op id: resp=%+v err=%v", again[0], errs2[0])
	}
	if got := h.engram(old).ValidUntil; !got.Equal(stampedAt) {
		t.Errorf("the replay moved the retirement instant")
	}
}

// TestInlineSupersedes_LegacyBareEdgeIsNotRetiredRetroactively pins what this
// change does NOT do, and the way out it documents. A store written before
// COG-38 holds inline supersedes edges whose targets were never retired. They
// are left as they are — nothing rewrites history on upgrade, and a replay of
// the old write changes nothing — but the declaration can be completed by
// declaring it again with Link, which retires the target; and a NEW inline
// supersedes onto such a target is refused as the fork it would be.
//
// GREEN on develop for the Link half (it is Link's existing behaviour); the
// fork refusal is RED there.
func TestInlineSupersedes_LegacyBareEdgeIsNotRetiredRetroactively(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	old := h.write(glazeConcept, glazeOld)
	// The pre-COG-38 shape, written the way the old write path wrote it: the
	// engram with its supersedes edge inline, and no stamp on the target.
	legacy := &storage.Engram{Concept: glazeConcept, Content: glazeNew, Associations: []storage.Association{{
		TargetID: h.mustParse(old), RelType: storage.RelSupersedes, Weight: inlineSupersedesWeight, Confidence: 1, CreatedAt: time.Now(),
	}}}
	legacyID, err := h.eng.store.WriteEngram(h.ctx, h.ws, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.store.PutContentHash(h.ctx, h.ws, storage.ContentHash(glazeNew), legacyID); err != nil {
		t.Fatal(err)
	}
	if eng := h.engram(old); !eng.ValidUntil.IsZero() {
		t.Fatalf("fixture: the legacy target is already retired")
	}

	// Replaying the old write resolves to the legacy memory and writes nothing.
	resp, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, glazeConcept, glazeNew, old))
	if err != nil || resp.ID != legacyID.String() {
		t.Fatalf("replay of the legacy write: resp=%+v err=%v, want %s", resp, err, legacyID)
	}
	if eng := h.engram(old); !eng.ValidUntil.IsZero() {
		t.Errorf("a replay retired a legacy target retroactively (valid_until=%v); that is not this change's to do", eng.ValidUntil)
	}

	// A new inline supersedes onto it is a fork, and is refused naming the head.
	if _, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, glazeConcept, glazeThird, old)); !errors.Is(err, ErrEvolveTargetSuperseded) || !strings.Contains(err.Error(), legacyID.String()) {
		t.Errorf("a second inline supersedes onto a legacy-superseded memory: err = %v, want ErrEvolveTargetSuperseded naming %s", err, legacyID)
	}

	// The documented way to complete the old declaration: say it with Link.
	h.linkSupersedes(legacyID.String(), old)
	if !retiredLikeLink(h.engram(old)) {
		t.Errorf("re-declaring the supersession with Link did not retire the legacy target")
	}
	if got := h.successors(old); len(got) != 1 || got[0] != legacyID.String() {
		t.Errorf("successors = %v, want only %s", got, legacyID)
	}
}

// TestInlineSupersedes_SaturatedWeightsShareAKey: "the same weight" has to mean
// the same POSITION in the association key, not the same float. The key stores
// a saturating complement — every weight >= 1 is one position and every weight
// <= 0 another — so contradicts@1 with supersedes@2 are one key although
// 1 != 2, and the later edge silently replaces the earlier.
//
// RED on the first build of this change (float comparison): each pair was
// accepted and only the supersedes edge was stored.
func TestInlineSupersedes_SaturatedWeightsShareAKey(t *testing.T) {
	for _, w := range [][2]float32{{1.0, 2.0}, {0, -0.5}, {1.5, 1.0}} {
		for _, order := range []string{"other-first", "supersedes-first"} {
			t.Run(fmt.Sprintf("other@%v_supersedes@%v/%s", w[0], w[1], order), func(t *testing.T) {
				h, cleanup := newVersionHeadHarness(t)
				defer cleanup()
				a := h.write(glazeConcept, glazeOld)
				other := mbp.InlineRelationship{TargetID: a, Relation: "contradicts", Weight: w[0]}
				sup := mbp.InlineRelationship{TargetID: a, Relation: "supersedes", Weight: w[1]}
				req := &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: glazeNew,
					Relationships: []mbp.InlineRelationship{other, sup}}
				if order == "supersedes-first" {
					req.Relationships = []mbp.InlineRelationship{sup, other}
				}
				resp, err := h.eng.Write(h.ctx, req)
				if !errors.Is(err, storage.ErrAssocRelTypeCollision) || !errors.Is(err, ErrInvalidRequest) {
					var stored []string
					if resp != nil {
						fwd, _ := h.eng.store.GetAssociations(h.ctx, h.ws, []storage.ULID{h.mustParse(resp.ID)}, 0)
						for _, e := range fwd[h.mustParse(resp.ID)] {
							stored = append(stored, fmt.Sprintf("%v@%v", e.RelType, e.Weight))
						}
					}
					t.Fatalf("contradicts@%v + supersedes@%v: err = %v, want ErrAssocRelTypeCollision (one key position); edges stored = %v — a declaration was silently lost",
						w[0], w[1], err, stored)
				}
				// The refusal names the other relation as the caller wrote it.
				if !strings.Contains(err.Error(), "and contradicts (weight") {
					t.Errorf("the refusal should name the colliding relation by name: %v", err)
				}
				if eng := h.engram(a); !eng.ValidUntil.IsZero() || len(h.successors(a)) != 0 {
					t.Errorf("a refused write changed the target (valid_until=%v successors=%v)", eng.ValidUntil, h.successors(a))
				}
				if n := h.countEngrams(); n != 1 {
					t.Errorf("a refused write stored %d engrams", n-1)
				}
			})
		}
	}

	// Two supersedes declarations of one target at weights that are one key
	// position are the same declaration said twice, not a conflict.
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()
	a := h.write(glazeConcept, glazeOld)
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: glazeNew,
		Relationships: []mbp.InlineRelationship{
			{TargetID: a, Relation: "supersedes", Weight: 1},
			{TargetID: a, Relation: "supersedes", Weight: 2},
		}})
	if err != nil {
		t.Fatalf("supersedes@1 + supersedes@2 of one target: %v — one key position, one declaration", err)
	}
	if got := h.successors(a); len(got) != 1 || got[0] != resp.ID || !retiredLikeLink(h.engram(a)) {
		t.Errorf("successors = %v, retired = %v", got, retiredLikeLink(h.engram(a)))
	}
}

// TestInlineSupersedes_AppendBatchUpsertItemIsRefused: WriteBatch hands an
// upsert item straight to the upsert path, never through Write — so the append
// gate at the top of Write does not see it and the upsert path carries its own.
// This is that gate's only test: with it removed every other test here stays
// green, because every other append case enters through Write.
//
// RED with the gate in writeUpsert removed: the hit on an existing key answers
// err=nil.
func TestInlineSupersedes_AppendBatchUpsertItemIsRefused(t *testing.T) {
	for _, field := range bothSupersedesFields {
		t.Run(string(field), func(t *testing.T) {
			h, cleanup := newVersionHeadHarness(t)
			defer cleanup()
			target := h.write(glazeConcept, glazeOld)
			const doc = "Handbook: the studio fires a glaze kiln every second Friday."
			// The key already exists, written by a full credential with no
			// declaration: the append item below is a HIT with identical content.
			if _, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: glazeConcept, Content: doc,
				UpsertMode: true, IdempotentID: "studio-handbook/firing"}); err != nil {
				t.Fatal(err)
			}
			item := func(key, content string) *mbp.WriteRequest {
				req := supersedingReq(field, glazeConcept, content, target)
				req.UpsertMode, req.IdempotentID = true, key
				return req
			}
			ap := withMode(auth.ModeAppend)
			_, errs := h.eng.WriteBatch(ap, []*mbp.WriteRequest{
				item("studio-handbook/firing", doc),                                         // hit, identical content
				item("studio-handbook/firing", doc+" Cone ten."),                            // hit, changed content
				item("studio-handbook/never-seen", "Handbook: a page no key has held yet."), // miss
			})
			for i, kind := range []string{"hit/identical", "hit/changed", "miss"} {
				if !errors.Is(errs[i], ErrAppendForbidden) {
					t.Errorf("append batch upsert item (%s): err = %v, want ErrAppendForbidden", kind, errs[i])
				}
			}
			if eng := h.engram(target); !eng.ValidUntil.IsZero() || len(h.successors(target)) != 0 {
				t.Errorf("an append credential changed an existing memory through a batch upsert item (valid_until=%v successors=%v)",
					eng.ValidUntil, h.successors(target))
			}
			if n := h.countEngrams(); n != 2 {
				t.Errorf("vault holds %d engrams, want 2: a refused item stored or evolved something", n)
			}
		})
	}
}

// TestInlineSupersedes_ExistingMemoryPathsAreUnchangedInThisIncrement pins the
// boundary of this increment. COG-38 here covers a write that CREATES a memory.
// A write that resolves to a memory that already exists — identical content, or
// an upsert_mode key — does exactly what it did before, so that this increment
// cannot make a retried write or a re-synced document fail on its own earlier
// success. The next increment gives those paths their own rule and replaces
// this test. GREEN before and after this increment, by design.
func TestInlineSupersedes_ExistingMemoryPathsAreUnchangedInThisIncrement(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// Content-hash dedup hit: the existing id, `duplicate_content`, and the
	// declaration is not applied.
	old := h.write(glazeConcept, glazeOld)
	existing := h.write(glazeConcept, glazeNew)
	resp, err := h.eng.Write(h.ctx, supersedingReq(viaRelationships, glazeConcept, glazeNew, old))
	if err != nil || resp.ID != existing || resp.Hint != "duplicate_content" {
		t.Fatalf("dedup hit: resp=%+v err=%v, want %s duplicate_content", resp, err, existing)
	}
	if eng := h.engram(old); !eng.ValidUntil.IsZero() || len(h.successors(old)) != 0 {
		t.Errorf("dedup hit applied the declaration (valid_until=%v successors=%v); that belongs to the next increment", eng.ValidUntil, h.successors(old))
	}

	// upsert_mode create: the edge is written inline with the engram — no
	// guard, no stamp.
	replaced := h.write(wheelConcept, wheelOld)
	sync := func(content string) (*mbp.WriteResponse, error) {
		req := supersedingReq(viaRelationships, wheelConcept, content, replaced)
		req.UpsertMode, req.IdempotentID = true, "studio-handbook/wheels"
		return h.eng.Write(h.ctx, req)
	}
	v1, err := sync(wheelNew)
	if err != nil || v1.Hint != "upsert-created" {
		t.Fatalf("upsert create: resp=%+v err=%v", v1, err)
	}
	if got := h.successors(replaced); len(got) != 1 || got[0] != v1.ID {
		t.Errorf("upsert create: successors = %v, want the inline edge from %s", got, v1.ID)
	}
	if eng := h.engram(replaced); !eng.ValidUntil.IsZero() {
		t.Errorf("upsert create stamped its target (valid_until=%v); that belongs to the next increment", eng.ValidUntil)
	}
	// upsert_mode hits — unchanged and changed content — are not refused.
	if v2, err := sync(wheelNew); err != nil || v2.Hint != "upsert-identical" {
		t.Errorf("upsert re-sync, unchanged: resp=%+v err=%v", v2, err)
	}
	if v3, err := sync(wheelNew + " The belt is checked at the same time."); err != nil || v3.Hint != "upsert-evolved" {
		t.Errorf("upsert re-sync, changed: resp=%+v err=%v", v3, err)
	}
	// And a re-sync after someone else evolved the key's memory still creates
	// (the stale-pointer rule), as it always has.
	v4, err := sync("Wheel head runout is checked weekly.")
	if err != nil {
		t.Fatal(err)
	}
	h.evolve(v4.ID, wheelConcept, "Wheel head runout is checked weekly, by whoever opens the studio.")
	if v5, err := sync("Wheel head runout is checked weekly."); err != nil || v5.Hint != "upsert-created" {
		t.Errorf("upsert re-sync after an external evolve: resp=%+v err=%v, want upsert-created", v5, err)
	}
}
