package mbp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// dispatchCompressed runs one frame flagged compressed through dispatchFrame on
// a connection with the given mode ("" = none) and returns the error payload it
// was answered with, or nil if it was not answered with an error.
func dispatchCompressed(t *testing.T, mode string, frameType uint8, payload []byte) *ErrorPayload {
	t.Helper()
	s := newTestServer(t, &stubEngine{})
	ctx := context.Background()
	if mode != "" {
		ctx = context.WithValue(ctx, auth.ContextMode, mode)
	}
	writeCh := make(chan *Frame, 1)
	s.dispatchFrame(ctx, &Frame{Version: 0x01, Type: frameType, Flags: FlagCompressed, CorrelationID: 9, Payload: payload}, writeCh, nil)
	select {
	case f := <-writeCh:
		if f.Type != TypeError {
			return nil
		}
		var ep ErrorPayload
		if err := DecodeMsgpack(f.Payload, &ep); err != nil {
			t.Fatalf("decode error payload: %v", err)
		}
		return &ep
	default:
		t.Fatal("the frame was not answered")
		return nil
	}
}

// TestDispatch_ARefusedFrameIsNeverDecompressed pins the order SEC-20 and
// SEC-21 both rest on: a frame is admitted or refused on its type and its
// connection's mode BEFORE its payload is touched. A frame that is refused is
// answered with the refusal whatever its payload is — garbage flagged as
// compressed included — and nothing is allocated to decode it.
//
// If decompression is moved in front of the admission check, a refused frame
// with a garbage payload is answered "decompression failed" instead (which
// tells the sender its frame was looked at), and one with a valid payload
// costs what it decodes to: both halves below fail.
func TestDispatch_ARefusedFrameIsNeverDecompressed(t *testing.T) {
	garbage := []byte("flagged as compressed, and nothing of the kind")

	refused := []struct {
		name      string
		mode      string
		frameType uint8
		wantCode  ErrorCode
		wantMsg   string
	}{
		{"observe key, WRITE", auth.ModeObserve, TypeWrite, ErrAuthFailed, msgReadOnlyKey},
		{"observe key, LINK", auth.ModeObserve, TypeLink, ErrAuthFailed, msgReadOnlyKey},
		{"observe key, FORGET", auth.ModeObserve, TypeForget, ErrAuthFailed, msgReadOnlyKey},
		{"write key, READ", auth.ModeWrite, TypeRead, ErrAuthFailed, msgWriteOnlyKey},
		{"write key, ACTIVATE", auth.ModeWrite, TypeActivate, ErrAuthFailed, msgWriteOnlyKey},
		{"write key, STAT", auth.ModeWrite, TypeStat, ErrAuthFailed, msgWriteOnlyKey},
		{"no mode on the connection, PING", "", TypePing, ErrAuthFailed, msgUnrecognizedMode},
		{"unrecognized mode, PING", "admin", TypePing, ErrAuthFailed, msgUnrecognizedMode},
		{"full key, unrouted frame type", auth.ModeFull, 0x7E, ErrInternal, "unknown message type: 0x7E"},
		{"full key, cluster frame type", auth.ModeFull, TypeJoinRequest, ErrInternal, "unknown message type: 0x36"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			// Garbage: were it decoded first, the answer would be about that.
			ep := dispatchCompressed(t, tc.mode, tc.frameType, garbage)
			if ep == nil || ep.Code != tc.wantCode || ep.Message != tc.wantMsg {
				t.Errorf("garbage payload: answered %+v; want %d %q", ep, tc.wantCode, tc.wantMsg)
			}

			// A valid payload that decodes to 8 MiB: were it decoded at all,
			// 8 MiB would be allocated for it.
			valid := zstdDeclared(t, 8*testMiB)
			allocated := allocatedDuring(func() { ep = dispatchCompressed(t, tc.mode, tc.frameType, valid) })
			if ep == nil || ep.Code != tc.wantCode || ep.Message != tc.wantMsg {
				t.Errorf("valid payload: answered %+v; want %d %q", ep, tc.wantCode, tc.wantMsg)
			}
			if allocated > testMiB {
				t.Errorf("refusing a frame whose payload decodes to 8 MiB allocated %.2f MiB; want nothing decoded", float64(allocated)/testMiB)
			}
		})
	}

	// The control: an ADMITTED frame with the same garbage is decompressed, and
	// says so. Without this the cases above would pass on a server that never
	// decompressed anything.
	t.Run("an admitted frame is decompressed", func(t *testing.T) {
		ep := dispatchCompressed(t, auth.ModeFull, TypePing, garbage)
		if ep == nil || ep.Code != ErrInternal || !strings.HasPrefix(ep.Message, "decompression failed") {
			t.Errorf("answered %+v; want the decompression failure", ep)
		}
	})
}
