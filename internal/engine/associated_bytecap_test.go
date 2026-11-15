package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestAssociated_OversizedLoneItemIsShortenedNotDropped — the 600-byte cap is
// measured on the JSON-encoded array, and Go's encoder escapes <, > and & to
// six bytes each. One association-only memory whose concept and shared entity
// names are made of them encodes past the cap on its own. It must be
// shortened (on a UTF-8 boundary, marked truncated) to fit — never silently
// dropped so the response looks identical to "no association". RED before:
// the strongest item alone was over the cap, the drop-weakest loop emptied
// the whole block, and the response carried no associated key at all.
// Fixtures are invented (trail maintenance).
func TestAssociated_OversizedLoneItemIsShortenedNotDropped(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-oversize"
	f := cfBuild(c, vault)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	// Three shared entity names, each 40 runes of which 39 escape to six
	// bytes: the `why` alone is ~330 bytes, the 60-rune concept ~360.
	ents := []string{strings.Repeat("<", 39) + "N", strings.Repeat("<", 39) + "S", strings.Repeat("<", 39) + "R"}
	adv := c.write(vault, strings.Repeat("<>&", 25), "permits for the gate", 3*time.Hour, ents...)
	for _, e := range ents {
		if err := c.store.WriteEntityEngramLink(ctx, ws, f.strong[0], e); err != nil {
			t.Fatal(err)
		}
	}
	resp := c.activate(vault, cfQuery, 10, true, nil)
	b, _ := json.Marshal(resp.Associated)
	t.Logf("associated: %d items, %d bytes: %s", len(resp.Associated), len(b), b)
	if len(resp.Associated) == 0 {
		full, _ := json.Marshal(resp)
		t.Fatalf("an oversized association-only item emptied the whole block; the response says nothing: %s", full)
	}
	if len(b) > 600 {
		t.Errorf("associated array is %d bytes, cap is 600", len(b))
	}
	i := assocIndex(resp.Associated, adv)
	if i < 0 {
		t.Fatalf("the strongest association-only memory was dropped instead of shortened: %+v", resp.Associated)
	}
	ib, _ := json.Marshal(resp.Associated[i])
	if !strings.Contains(string(ib), `"truncated":true`) {
		t.Errorf("a shortened item must say so: %s", ib)
	}
	if !utf8.ValidString(resp.Associated[i].Concept) || !utf8.ValidString(resp.Associated[i].Why) {
		t.Errorf("shortening split a UTF-8 sequence: %q / %q", resp.Associated[i].Concept, resp.Associated[i].Why)
	}
	if !strings.Contains(resp.Associated[i].Why, f.strong[0].String()) {
		t.Errorf("shortening must keep the returned row the item came through: %q", resp.Associated[i].Why)
	}
	_ = storage.ULID{}
}

// TestAssociated_OmittedCountSaysMoreExisted — when association-only
// memories qualified but the item or byte cap kept them out, the response
// says how many, so "some were not shown" never looks like "there were no
// more". RED before: no count anywhere in the response.
func TestAssociated_OmittedCountSaysMoreExisted(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-omitted"
	f := cfBuild(c, vault)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	for i := 0; i < 6; i++ {
		place := "Wrenfold Notch " + string(rune('A'+i))
		c.write(vault, "saddle permit desk notes "+place, "permits issued at the saddle desk", time.Duration(i+1)*time.Minute, place)
		if err := c.store.WriteEntityEngramLink(ctx, ws, f.strong[0], place); err != nil {
			t.Fatal(err)
		}
	}
	resp := c.activate(vault, cfQuery, 10, true, nil)
	full, _ := json.Marshal(resp)
	var m map[string]any
	_ = json.Unmarshal(full, &m)
	if len(resp.Associated) == 0 || len(resp.Associated) > 3 {
		t.Fatalf("premise: seven association-only memories, a non-empty block of at most 3, got %d", len(resp.Associated))
	}
	n, ok := m["associated_omitted"].(float64)
	if !ok || int(n)+len(resp.Associated) < 7 {
		t.Errorf("seven qualified, %d shown: the response must count the rest, got associated_omitted=%v", len(resp.Associated), m["associated_omitted"])
	}
	none := c.activate(vault, "corridor brushing", 10, true, nil)
	nb, _ := json.Marshal(none)
	if strings.Contains(string(nb), "associated_omitted") {
		t.Errorf("no association at all must carry no omitted count: %s", nb)
	}
}
