package mbp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// modeRecEngine records the credential mode present in ctx when an op reaches
// the engine. Read is universally allowed for every mode, so it is the probe.
type modeRecEngine struct {
	stubEngine
	gotMode string
}

func (e *modeRecEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	e.gotMode, _ = ctx.Value(auth.ContextMode).(string)
	return e.stubEngine.Read(ctx, req)
}

// TestMBP_ContextMode_ThreadedFromCredential proves the MBP HELLO handler
// injects auth.ContextMode for the whole connection: a token session carries
// its key's real mode (so an observe key stays observe and can never satisfy
// the SEC-14 verified gate), and a credential-less "none" session is full.
// Before the fix MBP set no mode at all, which (a) over-blocked verified writes
// for legitimate full/write keys and (b) left observe reads reinforcing (COG-11).
func TestMBP_ContextMode_ThreadedFromCredential(t *testing.T) {
	cases := []struct {
		name       string
		authMethod string
		keyMode    string // "" when authMethod == none
		wantMode   string
	}{
		{"full-token", "token", "full", auth.ModeFull},
		{"observe-token", "token", "observe", auth.ModeObserve},
		{"write-token", "token", "write", auth.ModeWrite},
		{"none-open", "none", "", auth.ModeFull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &modeRecEngine{}
			store := newVaultAuthStore(t)
			hello := HelloRequest{Version: "1", AuthMethod: tc.authMethod}
			if tc.authMethod == "token" {
				token, _, err := store.GenerateAPIKey("default", "test", tc.keyMode, nil)
				if err != nil {
					t.Fatalf("GenerateAPIKey: %v", err)
				}
				hello.Token = token
			}
			s := newAuthedTestServer(eng, store)
			c, wait := startTestConn(t, s)
			defer wait()

			f := doHandshakeReq(t, c, hello)
			if f.Type != TypeHelloOK {
				t.Fatalf("%s: expected HELLO_OK, got 0x%02x", tc.name, f.Type)
			}
			resp := sendAndReceive(t, c, TypeRead, 1, &ReadRequest{ID: "e-1"})
			if resp.Type != TypeReadResp {
				t.Fatalf("%s: expected TypeReadResp, got 0x%02x", tc.name, resp.Type)
			}
			if eng.gotMode != tc.wantMode {
				t.Errorf("%s: engine saw ctx mode %q, want %q", tc.name, eng.gotMode, tc.wantMode)
			}
		})
	}
}
