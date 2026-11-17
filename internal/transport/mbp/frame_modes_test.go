package mbp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestModeRefusal_TruthTable pins the mode gate itself: every credential mode
// against every frame class, including the two inputs that must fail closed —
// a mode the server does not recognize, and a connection that carries none.
func TestModeRefusal_TruthTable(t *testing.T) {
	classes := []frameClass{frameControl, frameRead, frameMutate, frameMutateAndRead}
	const unknown = msgUnrecognizedMode
	cases := []struct {
		mode string
		want [4]string // the refusal for each entry of classes, in order
	}{
		{auth.ModeFull, [4]string{"", "", "", ""}},
		// Append is not restricted here; the engine refuses what it may not
		// modify (SEC-15).
		{auth.ModeAppend, [4]string{"", "", "", ""}},
		{auth.ModeObserve, [4]string{"", "", msgReadOnlyKey, msgReadOnlyKey}},
		{auth.ModeWrite, [4]string{"", msgWriteOnlyKey, "", msgWriteOnlyKey}},
		{"", [4]string{unknown, unknown, unknown, unknown}},
		{"admin", [4]string{unknown, unknown, unknown, unknown}},
	}
	for _, tc := range cases {
		for i, class := range classes {
			if got := modeRefusal(tc.mode, class); got != tc.want[i] {
				t.Errorf("modeRefusal(%q, %s) = %q; want %q", tc.mode, class, got, tc.want[i])
			}
		}
	}
}

func noopFrameHandler(*Server, context.Context, uint64, []byte, chan *Frame, []string) {}

// TestAdmit_UnclassifiedRouteIsServedToNoOne proves the fail-closed half of
// the route table: a frame type that has a handler but no class is refused to
// every credential, a full one included, and is answered like an opcode the
// server has never heard of. That is what a frame type added without deciding
// its class looks like. A route with a class and no handler is the same.
func TestAdmit_UnclassifiedRouteIsServedToNoOne(t *testing.T) {
	const noClass, noHandler, unrouted uint8 = 0x7D, 0x7E, 0x7F
	routes := map[uint8]frameRoute{
		noClass:   {handle: noopFrameHandler}, // class left at its zero value
		noHandler: {class: frameControl},
	}
	for _, mode := range []string{auth.ModeFull, auth.ModeAppend, auth.ModeObserve, auth.ModeWrite, ""} {
		for _, frameType := range []uint8{noClass, noHandler, unrouted} {
			handle, code, msg := admit(routes, frameType, mode)
			if handle != nil {
				t.Errorf("mode %q, frame 0x%02X: admitted; want it served to no one", mode, frameType)
			}
			if code != ErrInternal || msg == "" {
				t.Errorf("mode %q, frame 0x%02X: error = %d %q; want the unknown-message-type error", mode, frameType, code, msg)
			}
		}
	}
}

// TestAdmit_ModeDecidesBeforeAnyHandler runs every mode against a route of
// every class and checks admit hands back a handler exactly when modeRefusal
// allows it, and the protocol's auth-failure code when it does not.
func TestAdmit_ModeDecidesBeforeAnyHandler(t *testing.T) {
	const frameType uint8 = 0x7D
	for _, class := range []frameClass{frameControl, frameRead, frameMutate, frameMutateAndRead} {
		routes := map[uint8]frameRoute{frameType: {class: class, handle: noopFrameHandler}}
		for _, mode := range []string{auth.ModeFull, auth.ModeAppend, auth.ModeObserve, auth.ModeWrite, "", "admin"} {
			want := modeRefusal(mode, class)
			handle, code, msg := admit(routes, frameType, mode)
			if want == "" {
				if handle == nil {
					t.Errorf("%s frame, mode %q: refused with %d %q; want it admitted", class, mode, code, msg)
				}
				continue
			}
			if handle != nil {
				t.Errorf("%s frame, mode %q: admitted; want %q", class, mode, want)
			}
			if code != ErrAuthFailed || msg != want {
				t.Errorf("%s frame, mode %q: error = %d %q; want %d %q", class, mode, code, msg, ErrAuthFailed, want)
			}
		}
	}
}
