package main

import (
	"strings"
	"testing"
)

// `muninn start` accepted and SILENTLY DISCARDED every flag it was given.
//
//	muninn start --data /tmp/scratch --mcp-addr 127.0.0.1:9260
//
// looks like it launches an isolated instance. It does not. runStart calls
// defaultDataDir() unconditionally (lifecycle.go), so the daemon comes up on
// $MUNINNDB_DATA or ~/.muninn/data — the operator's REAL vault — and binds the
// default ports, while the flags the caller typed are dropped on the floor with
// no error, no warning, and no mention in the startup banner.
//
// This is the project's first principle ("explicit config is never silently
// substituted") violated on the one argument that decides WHICH DATABASE the
// process opens, and the substituted value is production data. It was found the
// hard way: an agent building an isolated sandbox for evaluation used exactly
// this invocation, and its daemon bound the default ports for ~40s against the
// default data dir before it noticed. Nothing was written, but only by luck —
// the same command with a write would have gone to the live vault.
//
// The fix is fail-closed: `start` refuses any argument it cannot honour and
// names the invocation that does support them. It deliberately does NOT try to
// implement --data for `start`, because guessing at which flags to wire up is
// how this class of bug is created; refusing is unambiguous and cannot silently
// do the wrong thing.
func TestStartArgs_RefusesFlagsItCannotHonour(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		mustSay []string
	}{
		{
			// THE BUG: the exact invocation that silently opened the real vault.
			name:    "data and mcp-addr are refused, not swallowed",
			args:    []string{"--data", "/tmp/scratch", "--mcp-addr", "127.0.0.1:9260"},
			wantErr: true,
			mustSay: []string{"--data", "muninn --daemon"},
		},
		{
			name:    "single unsupported flag is refused",
			args:    []string{"--data", "/tmp/scratch"},
			wantErr: true,
			mustSay: []string{"--data"},
		},
		{
			name:    "equals form is refused too",
			args:    []string{"--data=/tmp/scratch"},
			wantErr: true,
			mustSay: []string{"--data"},
		},
		{
			// A bare `muninn start` is the supported form and must keep working.
			name:    "no args is accepted",
			args:    nil,
			wantErr: false,
		},
		{
			// The dispatcher passes the subcommand word through in some paths;
			// it must not be mistaken for an unsupported flag.
			name:    "the subcommand word itself is not an error",
			args:    []string{"start"},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStartArgs(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("checkStartArgs(%q) = nil, want an error — these flags are silently "+
					"discarded today, and --data silently redirects the daemon to the REAL vault", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkStartArgs(%q) = %v, want nil", tc.args, err)
			}
			if err != nil {
				for _, want := range tc.mustSay {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error must mention %q so the caller knows what was rejected and "+
							"what to run instead; got: %v", want, err)
					}
				}
			}
		})
	}
}
