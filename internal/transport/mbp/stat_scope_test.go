package mbp_test

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// statFixture is three vaults with memories in each: the locked vault the
// keyed principals are pinned to, a public vault, and a locked vault nobody in
// these tests is entitled to.
type statFixture struct {
	pinned, public, other string
}

func seedStatFixture(t *testing.T, env *matrixEnv) statFixture {
	t.Helper()
	fx := statFixture{pinned: "pilot-station", public: "harbour-notices", other: "customs-ledger"}
	for vault, n := range map[string]int{fx.pinned: 2, fx.public: 3, fx.other: 5} {
		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: vault == fx.public}); err != nil {
			t.Fatalf("configure %s: %v", vault, err)
		}
		for i := 0; i < n; i++ {
			_, err := env.eng.Write(fullCtx(), &mbp.WriteRequest{
				Vault: vault, Concept: "Log entry " + strconv.Itoa(i),
				Content: "Entry " + strconv.Itoa(i) + " in the " + vault + " log.",
			})
			if err != nil {
				t.Fatalf("seed %s: %v", vault, err)
			}
		}
	}
	return fx
}

// TestStatScopeMatrix pins what STAT tells each kind of session, over a real
// TCP connection to a real engine: only about the one vault the session is
// entitled to. A key pinned to one vault, and a session with no key on a
// public vault, learn nothing about any other vault — not its name, not its
// size, and no server-wide figure that would give its existence away.
//
// Before the scoping, STAT answered every session with a coherence entry for
// every vault on the server, keyed by vault name and carrying its engram
// total, plus the server's vault count and on-disk size.
func TestStatScopeMatrix(t *testing.T) {
	env := newMatrixEnv(t)
	fx := seedStatFixture(t, env)

	principals := []struct {
		name  string
		mode  string // "" = no key
		vault string
	}{
		{"full", auth.ModeFull, fx.pinned},
		{"append", auth.ModeAppend, fx.pinned},
		{"observe", auth.ModeObserve, fx.pinned},
		{"no key, public vault", "", fx.public},
	}
	for _, p := range principals {
		t.Run(p.name, func(t *testing.T) {
			token := ""
			if p.mode != "" {
				var err error
				if token, _, err = env.store.GenerateAPIKey(p.vault, "stat-"+p.mode, p.mode, nil); err != nil {
					t.Fatalf("GenerateAPIKey: %v", err)
				}
			}
			client := dialMBP(t, env.addr, token, p.vault)
			r := client.request(mbp.TypeStat, &mbp.StatRequest{Vault: p.vault})
			if r.frameType != mbp.TypeStatResp {
				t.Fatalf("STAT answered with frame 0x%02X %q", r.frameType, r.message)
			}

			// Nothing in the answer, in any field, names another vault.
			for _, vault := range []string{fx.pinned, fx.public, fx.other} {
				if vault != p.vault && bytes.Contains(r.payload, []byte(vault)) {
					t.Errorf("the answer names vault %q, which this session is not entitled to", vault)
				}
			}

			var st struct {
				EngramCount  int64                          `msgpack:"engram_count"`
				VaultCount   int                            `msgpack:"vault_count"`
				StorageBytes int64                          `msgpack:"storage_bytes"`
				Coherence    map[string]mbp.CoherenceResult `msgpack:"coherence"`
				Scope        string                         `msgpack:"scope"`
			}
			if err := mbp.DecodeMsgpack(r.payload, &st); err != nil {
				t.Fatalf("decode STAT: %v", err)
			}
			t.Logf("answer: scope=%q engram_count=%d vault_count=%d storage_bytes=%d coherence=%d entries",
				st.Scope, st.EngramCount, st.VaultCount, st.StorageBytes, len(st.Coherence))
			if st.Scope != "vault" {
				t.Errorf("scope = %q; want \"vault\"", st.Scope)
			}
			if st.VaultCount != 1 {
				t.Errorf("vault_count = %d; want 1, the one vault in scope (the server has 3)", st.VaultCount)
			}
			if st.StorageBytes != 0 {
				t.Errorf("storage_bytes = %d; want 0: the server's on-disk size covers every vault", st.StorageBytes)
			}
			if st.EngramCount <= 0 {
				t.Errorf("engram_count = %d; want the vault's own count", st.EngramCount)
			}
			for vault := range st.Coherence {
				if vault != p.vault {
					t.Errorf("coherence carries an entry for vault %q", vault)
				}
			}
			if _, ok := st.Coherence[p.vault]; !ok {
				t.Errorf("coherence has no entry for the session's own vault %q", p.vault)
			}
		})
	}
}
