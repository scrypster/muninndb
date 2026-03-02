package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNewerVersionAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.1", "v1.0.0", false},
		{"v1.2.0", "v2.0.0", true},
		{"dev", "v1.0.0", false},
		{"", "v1.0.0", false},
		{"v1.0.0", "", false},
	}
	for _, tc := range cases {
		got := newerVersionAvailable(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("newerVersionAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestIsHomebrewInstall(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Cellar/muninn/1.0.0/bin/muninn", true},
		{"/usr/local/opt/muninn/bin/muninn", true},
		{"/opt/homebrew/bin/muninn", true},
		{"/usr/local/Cellar/muninn/1.0.0/bin/muninn", true},
		{"/home/user/.local/bin/muninn", false},
		{"/usr/local/bin/muninn", false},
		{"/tmp/muninn", false},
	}
	for _, tc := range cases {
		got := isHomebrewInstallPath(tc.path)
		if got != tc.want {
			t.Errorf("isHomebrewInstallPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestReleaseAssetURL(t *testing.T) {
	url := releaseAssetURL("v1.2.3", "darwin", "arm64")
	want := "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_darwin_arm64.tar.gz"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}

	url = releaseAssetURL("v1.2.3", "linux", "amd64")
	want = "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_linux_amd64.tar.gz"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}

	url = releaseAssetURL("v1.2.3", "windows", "amd64")
	want = "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_windows_amd64.zip"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}
}

func TestDownloadAndExtractBinary(t *testing.T) {
	// Build a minimal tar.gz containing a fake "muninn" binary
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("#!/bin/sh\necho fake-binary")
	hdr := &tar.Header{
		Name: "muninn",
		Mode: 0755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dest, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(dest)

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("cannot read extracted file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
}

func TestVerifyBinary(t *testing.T) {
	// Use the current test binary — it's always a real executable
	err := verifyBinary(os.Args[0], "")
	if err != nil {
		t.Errorf("verifyBinary with real binary: %v", err)
	}
}

func TestVerifyBinary_NotExecutable(t *testing.T) {
	f, err := os.CreateTemp("", "muninn-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not a binary")
	f.Close()
	defer os.Remove(f.Name())

	// Make it non-executable
	os.Chmod(f.Name(), 0600)

	err = verifyBinary(f.Name(), "")
	if err == nil {
		t.Error("expected error for non-executable file, got nil")
	}
}

func TestDownloadAndExtractBinary_Progress(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := make([]byte, 1024) // 1KB fake binary
	hdr := &tar.Header{Name: "muninn", Mode: 0755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	var lastReported int64
	dest, err := downloadAndExtractBinaryProgress(srv.URL, "muninn", func(downloaded, total int64) {
		lastReported = downloaded
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(dest)
	if lastReported == 0 {
		t.Error("progress callback was never called")
	}
}
