package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// countingHandler counts the log records it is handed, by message prefix.
type countingHandler struct {
	mu       sync.Mutex
	records  int
	loud     int // records above DEBUG
	longest  int // longest "vault" attribute seen, in bytes
	messages map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records++
	if r.Level > slog.LevelDebug {
		h.loud++
	}
	if h.messages == nil {
		h.messages = make(map[string]int)
	}
	h.messages[r.Message]++
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "vault" {
			if n := len(a.Value.String()); n > h.longest {
				h.longest = n
			}
		}
		return true
	})
	return nil
}

// TestUnconfiguredVaultNamesAreNotRememberedWithoutLimit pins that asking about
// vaults that have no configuration costs the server a bounded amount, however
// many names are asked about and however long they are.
//
// GetVaultConfig notes once in the log each vault it finds unconfigured, and to
// do it once it has to remember the name. It is reached with a name the CALLER chose,
// before the caller has shown any credential: an unauthenticated request on any
// transport names a vault, and the name goes here to find out whether the vault
// is public. Remembering every such name, and logging each, handed any caller a
// way to make the server's memory and its log grow without limit.
func TestUnconfiguredVaultNamesAreNotRememberedWithoutLimit(t *testing.T) {
	store := openKeyStore(t)
	resetUnconfiguredVaultWarnings()
	t.Cleanup(resetUnconfiguredVaultWarnings)

	logged := &countingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logged))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const asked = 5000
	for i := 0; i < asked; i++ {
		name := fmt.Sprintf("tug-roster-%05d", i)
		if i%10 == 0 {
			// Every tenth name is far longer than any vault name may be.
			name += strings.Repeat("v", 32<<10)
		}
		cfg, err := store.GetVaultConfig(name)
		if err != nil || cfg.Public {
			t.Fatalf("GetVaultConfig(%d) = %+v, %v; an unconfigured vault is locked", i, cfg, err)
		}
	}
	// Asking again about names already asked about adds nothing.
	for i := 1; i < 10; i++ {
		_, _ = store.GetVaultConfig(fmt.Sprintf("tug-roster-%05d", i))
	}

	names, bytes := unconfiguredVaultWarningsHeld()
	if names > maxWarnedUnconfiguredVaults {
		t.Errorf("%d names are remembered after %d were asked about; the limit is %d", names, asked, maxWarnedUnconfiguredVaults)
	}
	if limit := maxWarnedUnconfiguredVaults * (maxWarnedVaultNameBytes + len("…")); bytes > limit {
		t.Errorf("the remembered names hold %d bytes; the limit is %d", bytes, limit)
	}
	if limit := maxWarnedUnconfiguredVaults + 1; logged.records > limit {
		t.Errorf("%d log lines were written for %d names; at most %d (one per remembered name, and one saying no more will be)", logged.records, asked, limit)
	}
	if logged.longest > maxWarnedVaultNameBytes+len("…") {
		t.Errorf("a logged vault name was %d bytes long; names are cut to %d", logged.longest, maxWarnedVaultNameBytes)
	}
	if logged.messages[msgUnconfiguredVaultLimit] != 1 {
		t.Errorf("the notice that no more names will be logged was written %d times; want once", logged.messages[msgUnconfiguredVaultLimit])
	}
	// Every one of these lines can be caused by a caller with no key. None of
	// them is above DEBUG, the notice that the set is full included.
	if logged.loud != 0 {
		t.Errorf("%d lines were logged above DEBUG; anyone can cause them, so they are not warnings", logged.loud)
	}
}

// TestUnconfiguredVaultIsStillNotedOnce pins what the line is for: a vault that
// has no explicit configuration is named in the log, once, at DEBUG.
func TestUnconfiguredVaultIsStillNotedOnce(t *testing.T) {
	store := openKeyStore(t)
	resetUnconfiguredVaultWarnings()
	t.Cleanup(resetUnconfiguredVaultWarnings)

	logged := &countingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(logged))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for i := 0; i < 5; i++ {
		if _, err := store.GetVaultConfig("pilot-station"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetVaultConfig(VaultConfig{Name: "harbour-notices", Public: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetVaultConfig("harbour-notices"); err != nil {
		t.Fatal(err)
	}
	if logged.records != 1 || logged.loud != 0 {
		t.Errorf("%d log lines, %d above DEBUG; want exactly one, at DEBUG, for the one unconfigured vault", logged.records, logged.loud)
	}
}
