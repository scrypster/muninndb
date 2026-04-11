package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var datasets = map[string]string{
	"locomo10.json":                "https://raw.githubusercontent.com/snap-research/LocOMo/main/data/locomo10.json",
	"longmemeval_s_cleaned.json":   "https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json",
}

func ensureDatasets(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, url := range datasets {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // already downloaded
		}
		fmt.Fprintf(os.Stderr, "downloading %s...\n", name)
		if err := downloadFile(url, path); err != nil {
			return fmt.Errorf("download %s: %w", name, err)
		}
	}
	return nil
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, dest)
}
