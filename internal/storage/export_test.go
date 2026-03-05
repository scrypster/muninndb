package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestExportImportRoundtrip(t *testing.T) {
	src := openTestStore(t)
	dst := openTestStore(t)

	ctx := context.Background()

	ws := src.VaultPrefix("vault-a")
	if err := src.WriteVaultName(ws, "vault-a"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write a few engrams.
	for i := 0; i < 3; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content body",
			Tags:    []string{"tag1"},
		}
		if _, err := src.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	opts := ExportOpts{EmbedderModel: "all-MiniLM-L6-v2", Dimension: 384}

	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "vault-a", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 3 {
		t.Errorf("EngramCount: got %d, want 3", result.EngramCount)
	}
	if result.TotalKeys == 0 {
		t.Errorf("TotalKeys: expected > 0")
	}

	// Import into a new vault on the destination store.
	wsB := dst.VaultPrefix("vault-b")
	if err := dst.WriteVaultName(wsB, "vault-b"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iOpts := ImportOpts{}
	iResult, err := dst.ImportVaultData(ctx, wsB, "vault-b", iOpts, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData: %v", err)
	}
	if iResult.EngramCount != 3 {
		t.Errorf("ImportVaultData EngramCount: got %d, want 3", iResult.EngramCount)
	}
}

func TestImportDeduplication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("dedup-vault")
	if err := store.WriteVaultName(ws, "dedup-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write 2 engrams.
	for i := 0; i < 2; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content body",
			Tags:    []string{"tag1"},
		}
		if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	opts := ExportOpts{EmbedderModel: "all-MiniLM-L6-v2", Dimension: 384}

	var buf bytes.Buffer
	exportResult, err := store.ExportVaultData(ctx, ws, "dedup-vault", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if exportResult.EngramCount != 2 {
		t.Errorf("ExportVaultData EngramCount: got %d, want 2", exportResult.EngramCount)
	}

	exportedBytes := buf.Bytes()

	// First import: both engrams are new, count should be 2.
	iOpts := ImportOpts{SkipCompatCheck: true}
	firstResult, err := store.ImportVaultData(ctx, ws, "dedup-vault", iOpts, bytes.NewReader(exportedBytes))
	if err != nil {
		t.Fatalf("first ImportVaultData: %v", err)
	}
	if firstResult.EngramCount != 0 {
		t.Errorf("first import EngramCount: got %d, want 0 (all were already present from WriteEngram)", firstResult.EngramCount)
	}

	// Second import of the same archive: all engrams already exist, count must be 0.
	secondResult, err := store.ImportVaultData(ctx, ws, "dedup-vault", iOpts, bytes.NewReader(exportedBytes))
	if err != nil {
		t.Fatalf("second ImportVaultData: %v", err)
	}
	if secondResult.EngramCount != 0 {
		t.Errorf("second import EngramCount: got %d, want 0 (duplicates should be skipped)", secondResult.EngramCount)
	}
}

func TestExportEmptyVault(t *testing.T) {
	src := openTestStore(t)
	ctx := context.Background()

	ws := src.VaultPrefix("empty-vault")
	if err := src.WriteVaultName(ws, "empty-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	opts := ExportOpts{}
	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "empty-vault", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 0 {
		t.Errorf("expected 0 engrams, got %d", result.EngramCount)
	}

	// Should still be importable.
	dst := openTestStore(t)
	wsD := dst.VaultPrefix("dest-empty")
	if err := dst.WriteVaultName(wsD, "dest-empty"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iResult, err := dst.ImportVaultData(ctx, wsD, "dest-empty", ImportOpts{}, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData empty: %v", err)
	}
	if iResult.EngramCount != 0 {
		t.Errorf("imported engram count: got %d, want 0", iResult.EngramCount)
	}
}

// TestImport_CorruptChecksum verifies that importing an archive with a tampered
// checksum returns an error and does not commit any engrams to the store.
func TestImport_CorruptChecksum(t *testing.T) {
	ctx := context.Background()

	// 1. Export 2 engrams from "chk-src".
	src := openTestStore(t)
	wsSrc := src.VaultPrefix("chk-src")
	if err := src.WriteVaultName(wsSrc, "chk-src"); err != nil {
		t.Fatalf("WriteVaultName src: %v", err)
	}
	for i := 0; i < 2; i++ {
		eng := &Engram{Concept: "concept", Content: "body"}
		if _, err := src.WriteEngram(ctx, wsSrc, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	var exported bytes.Buffer
	_, err := src.ExportVaultData(ctx, wsSrc, "chk-src", ExportOpts{}, &exported)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}

	// 2. Tamper: decompress → re-tar with a bad checksum → recompress.
	tampered, err := tamperChecksum(exported.Bytes())
	if err != nil {
		t.Fatalf("tamperChecksum: %v", err)
	}

	// 3. Import into "chk-dst" — must fail with a checksum error.
	dst := openTestStore(t)
	wsDst := dst.VaultPrefix("chk-dst")
	if err := dst.WriteVaultName(wsDst, "chk-dst"); err != nil {
		t.Fatalf("WriteVaultName dst: %v", err)
	}
	_, importErr := dst.ImportVaultData(ctx, wsDst, "chk-dst", ImportOpts{SkipCompatCheck: true}, bytes.NewReader(tampered))
	if importErr == nil {
		t.Fatal("expected ImportVaultData to return an error for corrupt checksum, got nil")
	}
	if !strings.Contains(importErr.Error(), "checksum") {
		t.Errorf("expected error message to contain 'checksum', got: %v", importErr)
	}

	// 4. Scan "chk-dst" — expect 0 engrams (batch was not committed).
	engrams, err := dst.EngramsByCreatedSince(ctx, wsDst, time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince: %v", err)
	}
	if len(engrams) != 0 {
		t.Errorf("expected 0 engrams in chk-dst after corrupt import, got %d", len(engrams))
	}
}

// tamperChecksum decompresses the gzip+tar archive, replaces checksum.txt with
// a zeroed hash, and returns the re-compressed archive.
func tamperChecksum(archiveData []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// Collect all entries.
	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read entry %s: %w", hdr.Name, err)
		}
		entries = append(entries, entry{hdr: hdr, data: data})
	}

	// Re-build with a corrupted checksum.txt.
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		data := e.data
		if e.hdr.Name == "checksum.txt" {
			data = []byte("sha256:0000000000000000000000000000000000000000000000000000000000000000\n")
		}
		hdr := *e.hdr
		hdr.Size = int64(len(data))
		if err := tw.WriteHeader(&hdr); err != nil {
			return nil, fmt.Errorf("write header %s: %w", hdr.Name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write data %s: %w", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return out.Bytes(), nil
}

// TestImport_LegacyNoChecksum verifies that importing a legacy archive
// (no checksum.txt entry) succeeds and returns nil error.
func TestImport_LegacyNoChecksum(t *testing.T) {
	ctx := context.Background()

	// Build a tar archive manually without a checksum.txt entry.
	manifest := MuninnManifest{
		MuninnVersion: "1",
		SchemaVersion: MuninnSchemaVersion,
		Vault:         "legacy-src",
		EngramCount:   0,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Write manifest.json.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "manifest.json",
		Mode:     0644,
		Size:     int64(len(manifestBytes)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("manifest header: %v", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		t.Fatalf("manifest write: %v", err)
	}

	// No data.kvs and no checksum.txt — purely empty legacy archive.

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	// Import into "legacy-dst" — must succeed (nil error).
	dst := openTestStore(t)
	wsDst := dst.VaultPrefix("legacy-dst")
	if err := dst.WriteVaultName(wsDst, "legacy-dst"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	iResult, importErr := dst.ImportVaultData(ctx, wsDst, "legacy-dst", ImportOpts{SkipCompatCheck: true}, bytes.NewReader(buf.Bytes()))
	if importErr != nil {
		t.Fatalf("expected nil error for legacy archive without checksum.txt, got: %v", importErr)
	}
	if iResult.EngramCount != 0 {
		t.Errorf("expected 0 engrams from empty legacy archive, got %d", iResult.EngramCount)
	}
}
