package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
)

func f32(v float32) *float32 { return &v }

// captureWarn installs a slog handler for the duration of the test and returns
// the accumulated output.
func captureWarn(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// decayConfigEngine builds the minimum Engine decayAllVaults needs — store,
// auth store, stop context, and an already-open #759 repair gate — WITHOUT
// NewEngine's background repair goroutine.
//
// Two reasons, both load-bearing: that goroutine closes assocWeightRepairDone
// itself on a clean pass, so a test that also closes it is a double-close
// panic; and waiting for it instead costs the 5s startup delay per test, which
// the CI budget does not buy for a config assertion.
func decayConfigEngine(t *testing.T, store *storage.PebbleStore, as *auth.Store) *Engine {
	t.Helper()
	e := &Engine{store: store, authStore: as, assocWeightRepairDone: make(chan struct{})}
	e.stopCtx, e.stopCancel = context.WithCancel(context.Background())
	t.Cleanup(e.stopCancel)
	close(e.assocWeightRepairDone) // what a clean repair pass does
	return e
}

// TestDecayAllVaults_LegacyFactorWarnsOncePerVault covers the loud half of the
// #762 config migration.
//
// assoc_decay_factor was documented as a rate ("multiplier per pass"). It is now
// only an enable/disable switch, and an operator who set it explicitly gets the
// number reinterpreted per-day. Silently substituting a different meaning for a
// value someone configured is the failure class principle #1 exists to prevent,
// so it degrades LOUDLY: one WARN naming the vault, the old factor, and the
// derived half-life.
//
// Once per vault, not once per pass — the sweep runs every ~60s and a flood is
// functionally silent.
func TestDecayAllVaults_LegacyFactorWarnsOncePerVault(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	var e *Engine

	const vaultName = "legacy-factor-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocDecayFactor: f32(0.95)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	// The half-life must be the conversion of the legacy factor, not the preset.
	resolved := auth.ResolvePlasticity(&auth.PlasticityConfig{AssocDecayFactor: f32(0.95)})
	if got := resolved.AssocHalfLifeDays; got < 13.51 || got > 13.52 {
		t.Errorf("legacy factor 0.95 → half-life %v days, want ~13.51", got)
	}

	buf := captureWarn(t)
	e = decayConfigEngine(t, store, as)
	calls := 0
	e.decayAssocWeightsFn = func(_ context.Context, _ [8]byte, halfLife time.Duration, _ float32, _ float64) (int, error) {
		calls++
		if want := 13.5134 * float64(24*time.Hour); halfLife < time.Duration(want*0.999) || halfLife > time.Duration(want*1.001) {
			t.Errorf("decay called with halfLife %v, want ~13.51 days", halfLife)
		}
		return 0, nil
	}

	for i := 0; i < 3; i++ {
		e.decayAllVaults(e.stopCtx, []string{vaultName})
	}
	if calls != 3 {
		t.Fatalf("decay should run on every pass: got %d calls, want 3", calls)
	}

	got := buf.String()
	if n := strings.Count(got, "legacy assoc_decay_factor reinterpreted"); n != 1 {
		t.Errorf("legacy WARN fired %d times over 3 passes, want exactly 1\n%s", n, got)
	}
	for _, want := range []string{vaultName, "derived_half_life_days", "assoc_half_life_days"} {
		if !strings.Contains(got, want) {
			t.Errorf("WARN must name %q so the operator can act on it; got:\n%s", want, got)
		}
	}
}

// A vault that states its half-life explicitly reinterprets nothing, so it must
// not be warned at — otherwise the WARN is noise an operator learns to ignore.
func TestDecayAllVaults_ExplicitHalfLifeDoesNotWarn(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	var e *Engine

	const vaultName = "explicit-halflife-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocDecayFactor: f32(0.95), AssocHalfLifeDays: f32(60)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	buf := captureWarn(t)
	e = decayConfigEngine(t, store, as)
	var gotHalfLife time.Duration
	e.decayAssocWeightsFn = func(_ context.Context, _ [8]byte, halfLife time.Duration, _ float32, _ float64) (int, error) {
		gotHalfLife = halfLife
		return 0, nil
	}
	e.decayAllVaults(e.stopCtx, []string{vaultName})

	if gotHalfLife != 60*24*time.Hour {
		t.Errorf("explicit half-life not honoured: got %v, want 1440h", gotHalfLife)
	}
	if strings.Contains(buf.String(), "legacy assoc_decay_factor") {
		t.Errorf("warned about a vault that configured its half-life explicitly:\n%s", buf.String())
	}
}

// TestDecayAllVaults_LegacyFactorAtOne_SkipsWithoutSubstitutedRate pins the
// refute finding on assoc_decay_factor >= 1: an operator's explicit 1.0 —
// documented per-pass semantics "on, but weights never move" — used to fall
// through to the preset's 30-day half-life. Decay then RAN at a rate the
// operator explicitly declined, and the legacy WARN claimed
// derived_half_life_days=30, a value that was NOT derived from the factor.
// factor >= 1 must resolve half-life 0 and hit the skip-with-WARN path.
func TestDecayAllVaults_LegacyFactorAtOne_SkipsWithoutSubstitutedRate(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "legacy-factor-one-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	// Default preset (Hebbian on, half-life 30 in the preset table) + an
	// explicit legacy factor of exactly 1.0. The preset's rate must NOT leak in.
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocDecayFactor: f32(1.0)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	buf := captureWarn(t)
	e := decayConfigEngine(t, store, as)
	calls := 0
	e.decayAssocWeightsFn = func(_ context.Context, _ [8]byte, halfLife time.Duration, _ float32, _ float64) (int, error) {
		calls++
		t.Errorf("decay ran with halfLife %v for an explicit factor of 1.0 — a rate was silently substituted", halfLife)
		return 0, nil
	}
	for i := 0; i < 3; i++ {
		e.decayAllVaults(e.stopCtx, []string{vaultName})
	}

	if calls != 0 {
		t.Errorf("decay ran %d times for factor 1.0, want 0 (skip)", calls)
	}
	got := buf.String()
	// Must NOT claim a reinterpretation: nothing was derived from factor 1.0.
	if strings.Contains(got, "reinterpreted") || strings.Contains(got, "derived_half_life_days") {
		t.Errorf("WARN claims a derivation that never happened:\n%s", got)
	}
	// Must WARN once, truthfully: switch on, no rate, decay skipped.
	if n := strings.Count(got, "no half-life resolved"); n != 1 {
		t.Errorf("skip WARN fired %d times over 3 passes, want exactly 1\n%s", n, got)
	}
	if !strings.Contains(got, "carries no rate") {
		t.Errorf("skip WARN must name the factor >= 1 cause so the operator can act on it; got:\n%s", got)
	}
}

// Decay switched on with no resolvable rate must SKIP and WARN, never
// substitute a default half-life. An unconfigured rate is not a licence to pick
// one (principle #1) — the operator gets told, not guessed at.
//
// Reaching this state takes work, which is the point: scratchpad (no half-life)
// with Hebbian forced on and a factor of exactly 1.0, which flips the switch on
// but carries no rate to convert.
func TestDecayAllVaults_NoHalfLifeSkipsAndWarns(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	var e *Engine

	const vaultName = "no-halflife-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	yes := true
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   vaultName,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			Preset:           "scratchpad",
			HebbianEnabled:   &yes,
			AssocDecayFactor: f32(1.0),
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: "scratchpad", HebbianEnabled: &yes, AssocDecayFactor: f32(1.0)})
	if r.AssocDecayFactor <= 0 || r.AssocHalfLifeDays != 0 {
		t.Fatalf("test setup: want switch on + rate absent, got factor=%v halfLife=%v",
			r.AssocDecayFactor, r.AssocHalfLifeDays)
	}

	buf := captureWarn(t)
	e = decayConfigEngine(t, store, as)
	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
		calls++
		return 0, nil
	}
	for i := 0; i < 3; i++ {
		e.decayAllVaults(e.stopCtx, []string{vaultName})
	}

	if calls != 0 {
		t.Errorf("decay ran with no configured rate: %d calls — a default was substituted", calls)
	}
	if n := strings.Count(buf.String(), "no half-life resolved"); n != 1 {
		t.Errorf("skip WARN fired %d times over 3 passes, want exactly 1\n%s", n, buf.String())
	}
}
