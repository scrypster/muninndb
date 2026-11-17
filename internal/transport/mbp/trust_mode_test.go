package mbp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// modeRecEngine records the credential mode present in ctx when an op reaches
// the engine. No single frame type is open to every mode (an ingest-only key
// may not read, an observe key may not write — SEC-20), so it records on both
// Read and Write and each case sends the one its mode may send.
type modeRecEngine struct {
	stubEngine
	gotMode string
}

func (e *modeRecEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	e.gotMode, _ = ctx.Value(auth.ContextMode).(string)
	return e.stubEngine.Read(ctx, req)
}

func (e *modeRecEngine) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	e.gotMode, _ = ctx.Value(auth.ContextMode).(string)
	return e.stubEngine.Write(ctx, req)
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
		write      bool // probe with WRITE instead of READ
	}{
		{"full-token", "token", "full", auth.ModeFull, false},
		{"observe-token", "token", "observe", auth.ModeObserve, false},
		{"write-token", "token", "write", auth.ModeWrite, true},
		{"append-token", "token", "append", auth.ModeAppend, false},
		{"none-open", "none", "", auth.ModeFull, false},
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
			if tc.write {
				resp := sendAndReceive(t, c, TypeWrite, 1, &WriteRequest{Concept: "c", Content: "x"})
				if resp.Type != TypeWriteOK {
					t.Fatalf("%s: expected TypeWriteOK, got 0x%02x", tc.name, resp.Type)
				}
			} else {
				resp := sendAndReceive(t, c, TypeRead, 1, &ReadRequest{ID: "e-1"})
				if resp.Type != TypeReadResp {
					t.Fatalf("%s: expected TypeReadResp, got 0x%02x", tc.name, resp.Type)
				}
			}
			if eng.gotMode != tc.wantMode {
				t.Errorf("%s: engine saw ctx mode %q, want %q", tc.name, eng.gotMode, tc.wantMode)
			}
		})
	}
}
