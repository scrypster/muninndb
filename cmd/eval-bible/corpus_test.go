package main

import (
	"encoding/json"
	"testing"
)

func TestParseKJV(t *testing.T) {
	raw := []kjvRecord{
		{B: 43, C: 3, V: 16, T: "For God so loved the world..."},
		{B: 1, C: 1, V: 1, T: "In the beginning..."},
	}
	data, _ := json.Marshal(raw)
	reqs, err := parseKJV(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("want 2 records, got %d", len(reqs))
	}
	john := reqs[0]
	if john.Concept != "John 3:16" {
		t.Errorf("want 'John 3:16', got %q", john.Concept)
	}
	if john.Content != "For God so loved the world..." {
		t.Errorf("unexpected content: %q", john.Content)
	}
	assertTag(t, john.Tags, "New Testament")
	assertTag(t, john.Tags, "John")
	assertTag(t, john.Tags, "gospel")
}

func TestParseKJV_NTOnly(t *testing.T) {
	raw := []kjvRecord{
		{B: 1, C: 1, V: 1, T: "In the beginning..."},
		{B: 40, C: 1, V: 1, T: "The book of..."},
	}
	data, _ := json.Marshal(raw)
	reqs, err := parseKJV(data, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 NT record, got %d", len(reqs))
	}
	if reqs[0].Concept != "Matthew 1:1" {
		t.Errorf("want 'Matthew 1:1', got %q", reqs[0].Concept)
	}
}

func TestVerseRef(t *testing.T) {
	if got := verseRef(43, 3, 16); got != "John 3:16" {
		t.Errorf("want 'John 3:16', got %q", got)
	}
	if got := verseRef(1, 1, 1); got != "Genesis 1:1" {
		t.Errorf("want 'Genesis 1:1', got %q", got)
	}
}

func assertTag(t *testing.T, tags []string, want string) {
	t.Helper()
	for _, tag := range tags {
		if tag == want {
			return
		}
	}
	t.Errorf("tag %q not found in %v", want, tags)
}
