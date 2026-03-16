package main

import (
	"testing"
)

// TestExtractOperation verifies that extractOperation correctly identifies the
// operation name by scanning for known operation tokens, regardless of what
// flags or flag values surround it.
func TestExtractOperation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOp  string
		wantLen int // expected length of returned rest slice
	}{
		{
			name:    "op only",
			args:    []string{"remember"},
			wantOp:  "remember",
			wantLen: 0,
		},
		{
			name:    "flag=value before op",
			args:    []string{"--vault=default", "remember"},
			wantOp:  "remember",
			wantLen: 1,
		},
		{
			name:    "flag value before op",
			args:    []string{"--vault", "default", "remember"},
			wantOp:  "remember",
			wantLen: 2,
		},
		{
			name:    "op followed by flags",
			args:    []string{"remember", "--concept", "c", "--content", "body"},
			wantOp:  "remember",
			wantLen: 4,
		},
		{
			// Known-ops scan is unaffected by any flags before the op name.
			name:    "unrecognized flag before value-flag before op",
			args:    []string{"--verbose", "--data-dir", "/tmp", "remember"},
			wantOp:  "remember",
			wantLen: 3,
		},
		{
			// Flag value starting with "-" used to fool the old heuristic.
			// Known-ops scan is immune to this entirely.
			name:    "flag value starting with dash before op",
			args:    []string{"--data-dir", "-custom-path", "remember"},
			wantOp:  "remember",
			wantLen: 2,
		},
		{
			name:    "all four ops recognized",
			args:    []string{"recall"},
			wantOp:  "recall",
			wantLen: 0,
		},
		{
			name:    "read op",
			args:    []string{"--vault", "myvault", "read", "--id", "abc"},
			wantOp:  "read",
			wantLen: 4,
		},
		{
			name:    "forget op",
			args:    []string{"forget", "--id", "abc"},
			wantOp:  "forget",
			wantLen: 2,
		},
		{
			name:    "flag at end no op",
			args:    []string{"--data-dir"},
			wantOp:  "",
			wantLen: 1,
		},
		{
			name:    "no args",
			args:    []string{},
			wantOp:  "",
			wantLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOp, gotRest := extractOperation(tt.args)
			if gotOp != tt.wantOp {
				t.Errorf("op: got %q, want %q (args: %v)", gotOp, tt.wantOp, tt.args)
			}
			if len(gotRest) != tt.wantLen {
				t.Errorf("rest len: got %d, want %d (rest: %v)", len(gotRest), tt.wantLen, gotRest)
			}
		})
	}
}
