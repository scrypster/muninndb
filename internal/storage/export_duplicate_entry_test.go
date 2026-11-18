package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

// rewriteArchive re-emits an export archive, repeating every entry whose name
// is in dup immediately after the original.
func rewriteArchive(t *testing.T, src []byte, dup map[string]bool) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gr)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		times := 1
		if dup[hdr.Name] {
			times = 2
		}
		for i := 0; i < times; i++ {
			h := *hdr
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatalf("tar header: %v", err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("tar write: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestImportVaultData_RejectsDuplicateEntries pins that an archive repeating
// data.kvs or checksum.txt is refused. A second checksum.txt used to commit
// and close the held batch a second time, after the first close had returned
// it to Pebble's pool: a nil-pointer panic when nothing else held it, and a
// commit of another writer's batch when something did. A second data.kvs
// after checksum.txt was held and then committed as a legacy archive, with
// no checksum check.
func TestImportVaultData_RejectsDuplicateEntries(t *testing.T) {
	src := openTestStore(t)
	ctx := context.Background()
	ws := src.VaultPrefix("trail-logs")
	if err := src.WriteVaultName(ws, "trail-logs"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := src.WriteEngram(ctx, ws, &Engram{Concept: "water bar", Content: "set every thirty meters"}); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}
	var buf bytes.Buffer
	if _, err := src.ExportVaultData(ctx, ws, "trail-logs", ExportOpts{EmbedderModel: "m", Dimension: 384}, &buf); err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}

	for _, name := range []string{"checksum.txt", "data.kvs"} {
		t.Run(name, func(t *testing.T) {
			archive := rewriteArchive(t, buf.Bytes(), map[string]bool{name: true})
			dst := openTestStore(t)
			wsB := dst.VaultPrefix("imported")
			if err := dst.WriteVaultName(wsB, "imported"); err != nil {
				t.Fatalf("WriteVaultName: %v", err)
			}
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("import panicked on a repeated %s: %v", name, r)
					}
				}()
				_, err = dst.ImportVaultData(ctx, wsB, "imported", ImportOpts{}, bytes.NewReader(archive))
			}()
			if err == nil || !strings.Contains(err.Error(), "duplicate "+name) {
				t.Fatalf("import of an archive repeating %s: err = %v, want a duplicate-entry error", name, err)
			}
		})
	}
}
