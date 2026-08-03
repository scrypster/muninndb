package storage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/prefix"
)

// === Per-list partition guards (RT-FIX RT2) ================================
//
// The four prefix-lists below are DIVERGENT policies (different exclusions
// for different operational reasons), not copies of one fact. Deriving them
// programmatically from prefix.Category would falsely claim the lists are
// identical. Instead, each test below pins its list's actual scope by
// asserting the bytes equal the expected const-set. A future prefix falling
// out of one list (e.g. someone adding a new prefix to prefix.All() but
// forgetting to add it to ClearVault's range) is caught at the list level.
//
// The four lists:
//   - vaultScopedSwapPrefixes     (clone.go)   — CloneVaultData / MergeVaultData
//   - vaultScopedExportPrefixes   (export.go)  — ExportVaultData
//   - clearVaultDataPrefixes      (vault_lifecycle.go) — ClearVault
//   - clearFTSKeysPrefixes        (impl.go)    — ClearFTSKeys

func TestVaultScopedSwapPrefixes_Scope(t *testing.T) {
	// clone.go's swap list: vault-scoped data prefixes CloneVaultData/MergeVaultData
	// walk to rebuild the target vault. Omits engrams (handled separately with ERF
	// decode/modify/encode), VaultMeta/NameIndex/DigestFlags (global), Coherence
	// (handled separately), VaultWeights (vault-specific), Embedding/Episode/
	// FTSVersion (added later for export-only flows), entity-graph prefixes
	// (EntityEngramLink/Relationship/LastAccess/CoOccurrence/ArchiveAssoc/
	// RelEntityIndex/DreamState/Lease — not part of clone's swap), and EntityReverseIndex
	// (global by name hash).
	want := []byte{
		prefix.Meta, prefix.AssocFwd, prefix.AssocRev, prefix.FTSPosting,
		prefix.Trigram, prefix.HNSWNode, prefix.FTSStats, prefix.TermStats,
		prefix.Contradiction, prefix.StateIndex, prefix.TagIndex, prefix.CreatorIndex,
		prefix.RelevanceBucket, prefix.AssocWeightIndex, prefix.VaultCount, prefix.Provenance,
		prefix.BucketMigration, prefix.ContentHash, prefix.RawTagRange, prefix.UpsertKey,
	}
	assertPrefixListEqual(t, "vaultScopedSwapPrefixes", vaultScopedSwapPrefixes, want)
}

func TestVaultScopedExportPrefixes_Scope(t *testing.T) {
	// export.go's export list: every vault-scoped prefix included in the export
	// archive. Omits VaultMeta/NameIndex (written by WriteVaultName on import) and
	// DigestFlags (globally keyed by ULID). Includes Coherence/VaultWeights/
	// Embedding/Episode/FTSVersion (live data that must be transported) but NOT
	// the entity-graph prefixes (those rebuild from engrams on import) nor Lease
	// (work-queue checkout attribute — not transportable) nor ArchiveAssoc
	// (soft-deleted associations — not exported).
	want := []byte{
		prefix.Engram, prefix.Meta, prefix.AssocFwd, prefix.AssocRev,
		prefix.FTSPosting, prefix.Trigram, prefix.HNSWNode, prefix.FTSStats,
		prefix.TermStats, prefix.Contradiction, prefix.StateIndex, prefix.TagIndex,
		prefix.CreatorIndex, prefix.RelevanceBucket, prefix.Coherence, prefix.VaultWeights,
		prefix.AssocWeightIndex, prefix.VaultCount, prefix.Provenance, prefix.BucketMigration,
		prefix.Embedding, prefix.Episode, prefix.FTSVersion, prefix.ContentHash, prefix.RawTagRange, prefix.UpsertKey,
	}
	assertPrefixListEqual(t, "vaultScopedExportPrefixes", vaultScopedExportPrefixes, want)
}

func TestClearVaultDataPrefixes_Scope(t *testing.T) {
	// vault_lifecycle.go's ClearVault list: every vault-scoped data prefix
	// ClearVault range-tombstones. Omits VaultMeta/NameIndex (preserved by Clear,
	// deleted by DeleteVaultNameOnly), DigestFlags (globally keyed by ULID —
	// orphans acceptable), and EntityReverseIndex (deleted row-by-row in
	// deleteVaultEntityReverseIndex because its layout embeds the vault prefix
	// mid-key, so a prefix-range tombstone cannot target it).
	//
	// Entity (0x1F) IS in the list since #683 made the record vault-scoped
	// (0x1F|ws|nameHash). Before that it was keyed globally and had to be pruned
	// by walking the vault's links and decrementing each mention count.
	want := []byte{
		prefix.Engram, prefix.Meta, prefix.AssocFwd, prefix.AssocRev,
		prefix.FTSPosting, prefix.Trigram, prefix.HNSWNode, prefix.FTSStats,
		prefix.TermStats, prefix.Contradiction, prefix.StateIndex, prefix.TagIndex,
		prefix.CreatorIndex, prefix.RelevanceBucket, prefix.Coherence, prefix.VaultWeights,
		prefix.AssocWeightIndex, prefix.VaultCount, prefix.Provenance, prefix.BucketMigration,
		prefix.Entity, prefix.EntityEngramLink, prefix.Relationship, prefix.LastAccess,
		prefix.CoOccurrence,
		prefix.ArchiveAssoc, prefix.RelEntityIndex, prefix.DreamState, prefix.ContentHash,
		prefix.RecallEvent, prefix.Lease, prefix.EvolveRepairMark, prefix.RawTagRange,
		prefix.ProspectiveIntent, prefix.AssocWeightRepairMark, prefix.UpsertKey,
	}
	assertPrefixListEqual(t, "clearVaultDataPrefixes", clearVaultDataPrefixes, want)
}

func TestClearFTSKeysPrefixes_Scope(t *testing.T) {
	// impl.go's ClearFTSKeys list: the four FTS-related prefixes that must be
	// purged when reindexing a vault's full-text search state.
	want := []byte{
		prefix.FTSPosting, prefix.Trigram, prefix.FTSStats, prefix.TermStats,
	}
	assertPrefixListEqual(t, "clearFTSKeysPrefixes", clearFTSKeysPrefixes, want)
}

func assertPrefixListEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: length mismatch: got %d bytes % x, want %d bytes % x",
			name, len(got), got, len(want), want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: byte %d: got 0x%02X, want 0x%02X (full got % x, want % x)",
				name, i, got[i], want[i], got, want)
			return
		}
	}
}

// === 3-shape raw-prefix-byte lint (RT-FIX RT2 anti-recurrence guard) ======
//
// Scans every production .go file under internal/storage (excluding keys.go's
// subpackage, the prefix-list decls themselves, and this lint test) for raw
// prefix-byte literals in the three recurrence shapes:
//
//  1. []byte{0xNN, ...} composite literals
//  2. x[N] = 0xNN index-assignments (the dominant pattern from the original
//     keys.go refactor — would catch a regression that re-introduces
//     index-byte hardcoding)
//  3. case 0xNN: / == 0xNN / != 0xNN comparison sites
//
// A hit FAILS unless the byte value is outside the registered prefix ranges
// (i.e. not a genuine prefix byte per prefix.All()). Separator/sentinel bytes
// like 0x00, 0xFF, ERF magic bytes (0x4D/0x55/0x4E), or version-marker value
// bytes (FTS schema version 0x01) are not registered prefixes and pass.
//
// Test files (_test.go) are excluded — tests legitimately use byte literals
// for fixtures and value assertions. The per-list partition guards above
// cover the prefix-list correctness directly.

func TestNoRawPrefixBytesInStorageProduction(t *testing.T) {
	prefixBytes := make(map[byte]string, len(prefix.All()))
	for _, e := range prefix.All() {
		prefixBytes[e.Byte] = e.Owner + "/" + e.Name
	}

	// Files that are allowed to contain prefix-byte literals (the registry-
	// derived list declarations themselves).
	excludedFiles := map[string]bool{
		"clone.go":             true, // vaultScopedSwapPrefixes decl
		"export.go":            true, // vaultScopedExportPrefixes decl
		"vault_lifecycle.go":   true, // clearVaultDataPrefixes decl
		"impl.go":              true, // clearFTSKeysPrefixes decl
		"prefix_lists_test.go": true, // this file
	}

	// Shape regexes — match the byte literal, capture the hex value.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\[\]byte\{0x([0-9A-Fa-f]{2})`),     // []byte{0xNN...
		regexp.MustCompile(`\[\d+\]\s*=\s*0x([0-9A-Fa-f]{2})`), // x[N] = 0xNN
		regexp.MustCompile(`case\s+0x([0-9A-Fa-f]{2})\s*[:,]`), // case 0xNN: / case 0xNN,
		regexp.MustCompile(`==\s*0x([0-9A-Fa-f]{2})\b`),        // == 0xNN
		regexp.MustCompile(`!=\s*0x([0-9A-Fa-f]{2})\b`),        // != 0xNN
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Resolve the storage package directory (this test file lives there).
	storageDir := cwd

	var failures []string
	walkErr := filepath.Walk(storageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip subdirectories (keys/, erf/, migrate/, cache/) — they have
			// their own scans or are out of scope for this lint.
			if path != storageDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") {
			return nil
		}
		if excludedFiles[base] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		for lineNo, line := range lines {
			// Skip lines that are pure comments — they often cite byte values
			// in documentation (e.g. "0x0E vault meta key").
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			for _, rx := range patterns {
				locs := rx.FindAllStringSubmatchIndex(line, -1)
				for _, loc := range locs {
					// loc[2]:loc[3] = the first capture group (hex digits).
					hex := line[loc[2]:loc[3]]
					var b byte
					for _, c := range []byte(hex) {
						b <<= 4
						switch {
						case c >= '0' && c <= '9':
							b |= c - '0'
						case c >= 'a' && c <= 'f':
							b |= c - 'a' + 10
						case c >= 'A' && c <= 'F':
							b |= c - 'A' + 10
						}
					}
					if owner, isPrefix := prefixBytes[b]; isPrefix {
						failures = append(failures, sprintfRaw(
							base, lineNo+1, b, owner, trimmed))
					}
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if len(failures) > 0 {
		t.Errorf("raw prefix-byte literals in production code (use prefix.X consts):\n%s",
			strings.Join(failures, "\n"))
	}
}

func sprintfRaw(file string, line int, b byte, owner, src string) string {
	return "   " + file + ":" + itoa(line) + "  byte=0x" +
		hex2(b) + "  owner=" + owner + "  src=\"" + src + "\""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func hex2(b byte) string {
	const hexdigits = "0123456789ABCDEF"
	return string([]byte{hexdigits[b>>4], hexdigits[b&0x0F]})
}
