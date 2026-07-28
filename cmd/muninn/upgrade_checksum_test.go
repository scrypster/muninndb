package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// buildTestArchive returns a tar.gz containing a file named binaryName followed by a
// large, incompressible padding entry.
//
// Both properties are load-bearing. The extractor stops reading as soon as it has the
// binary, so a hash taken at that point covers only the prefix it happened to consume —
// and an attacker could hide a payload in the tail. To prove the tail is really hashed,
// the padding must survive gzip (hence pseudo-random, not repeated bytes) and must be far
// larger than the reader's internal buffers (hence megabytes, not kilobytes). With small
// or compressible padding the whole body gets buffered incidentally and the test passes
// whether or not the code drains the stream, which would make it worthless.
func buildTestArchive(t *testing.T, binaryName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	write(binaryName, content)

	// Deterministic pseudo-random padding: fixed seed keeps the test reproducible,
	// randomness keeps gzip from shrinking it back into a single buffered read.
	padding := make([]byte, 8<<20)
	rng := rand.New(rand.NewSource(1))
	if _, err := rng.Read(padding); err != nil {
		t.Fatal(err)
	}
	write("padding.bin", padding)

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestParseChecksums(t *testing.T) {
	// Real sha256sum output format: "<hash>  <filename>", two spaces.
	in := strings.Join([]string{
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  muninn-linux-amd64",
		"da39a3ee5e6b4b0d3255bfef95601890afd80709  short-hash-ignored",
		"",
		"   ",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08  muninn_v0.9.0_darwin_arm64.tar.gz",
		"garbage-line-with-no-fields",
	}, "\n")

	got, err := parseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid entries, got %d: %v", len(got), got)
	}
	if got["muninn_v0.9.0_darwin_arm64.tar.gz"] != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Errorf("archive hash mismatch: %v", got)
	}
	if _, ok := got["short-hash-ignored"]; ok {
		t.Error("a non-SHA-256-length hash must not be accepted")
	}
}

func TestParseChecksums_Empty(t *testing.T) {
	if _, err := parseChecksums(strings.NewReader("")); err == nil {
		t.Fatal("an empty checksums file must be an error, not an empty map — " +
			"otherwise every lookup silently misses and verification is skipped")
	}
}

func TestFetchChecksums(t *testing.T) {
	body := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08  muninn_v1.0.0_linux_amd64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	sums, err := fetchChecksums(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(sums))
	}
}

func TestFetchChecksums_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchChecksums(srv.URL); err == nil {
		t.Fatal("a missing checksums.txt must fail closed, not return an empty map")
	}
}

// The download must hash every byte the server sent, including bytes after the
// extracted binary.
func TestDownloadAndExtractBinary_ReturnsFullArchiveHash(t *testing.T) {
	archive := buildTestArchive(t, "muninn", []byte("#!/bin/sh\necho fake"))
	want := sha256Hex(archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
		w.Write(archive)
	}))
	defer srv.Close()

	dest, gotSum, err := downloadAndExtractBinaryProgress(srv.URL, "muninn", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(dest)

	if gotSum != want {
		t.Errorf("hash covers only the consumed prefix, not the whole archive:\n got  %s\n want %s", gotSum, want)
	}
}

func TestVerifyChecksum_Match(t *testing.T) {
	sums := map[string]string{"muninn_v1.0.0_linux_amd64.tar.gz": "abc123"}
	if err := verifyChecksum("muninn_v1.0.0_linux_amd64.tar.gz", "abc123", sums); err != nil {
		t.Fatalf("matching checksum must pass: %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	sums := map[string]string{"muninn_v1.0.0_linux_amd64.tar.gz": "abc123"}
	err := verifyChecksum("muninn_v1.0.0_linux_amd64.tar.gz", "tampered", sums)
	if err == nil {
		t.Fatal("a tampered archive must be rejected")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should name the failure clearly, got: %v", err)
	}
}

// An asset absent from checksums.txt must fail closed. Treating "not listed" as
// "nothing to check" would let an attacker who controls the checksums file
// disable verification by simply omitting the entry.
func TestVerifyChecksum_AssetNotListed(t *testing.T) {
	sums := map[string]string{"some-other-asset.tar.gz": "abc123"}
	if err := verifyChecksum("muninn_v1.0.0_linux_amd64.tar.gz", "abc123", sums); err == nil {
		t.Fatal("an asset missing from checksums.txt must fail closed")
	}
}

func TestReleaseAssetName_MatchesURL(t *testing.T) {
	cases := []struct{ version, goos, goarch, want string }{
		{"v1.0.0", "linux", "amd64", "muninn_v1.0.0_linux_amd64.tar.gz"},
		{"v1.0.0", "darwin", "arm64", "muninn_v1.0.0_darwin_arm64.tar.gz"},
		{"v1.0.0", "windows", "amd64", "muninn_v1.0.0_windows_amd64.zip"},
	}
	for _, tc := range cases {
		got := releaseAssetName(tc.version, tc.goos, tc.goarch)
		if got != tc.want {
			t.Errorf("releaseAssetName(%q,%q,%q) = %q, want %q", tc.version, tc.goos, tc.goarch, got, tc.want)
		}
		// The name must be the tail of the URL, or checksum lookup silently misses.
		url := releaseAssetURL(tc.version, tc.goos, tc.goarch)
		if !strings.HasSuffix(url, "/"+got) {
			t.Errorf("asset name %q is not the tail of URL %q — lookup key would not match", got, url)
		}
	}
}

func TestChecksumsURL(t *testing.T) {
	got := checksumsURL("v1.2.3")
	want := "https://github.com/scrypster/muninndb/releases/download/v1.2.3/checksums.txt"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
