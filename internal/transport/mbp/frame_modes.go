package mbp

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/auth"
)

// frameClass is what a client frame does, as far as a credential's mode is
// concerned. It is the MBP counterpart of REST's auth.ReadOnlyGuard /
// auth.WriteOnlyGuard, of MCP's tool buckets (SEC-6) and of gRPC's per-RPC
// classes (SEC-19).
type frameClass uint8

const (
	// frameUnclassified is the zero value, so a route written without a class
	// has this one. It is refused to every credential, a full one included: a
	// frame type nobody has classified is not served.
	frameUnclassified frameClass = iota
	// frameControl returns nothing stored and changes nothing (PING).
	frameControl
	// frameRead returns stored memory or vault state, or manages a
	// subscription to it. Refused to a write-mode (ingest-only) credential.
	frameRead
	// frameMutate creates, changes or deletes stored memory. Refused to an
	// observe-mode (read-only) credential.
	frameMutate
	// frameMutateAndRead does both, so it is refused to an observe credential
	// as a mutation and to an ingest-only one as a read. No frame has this
	// class today; it exists so that one that needs it (gRPC's
	// AdjustConfidence is the in-tree example) is not squeezed into one half.
	frameMutateAndRead
)

func (c frameClass) String() string {
	switch c {
	case frameControl:
		return "control"
	case frameRead:
		return "read"
	case frameMutate:
		return "mutate"
	case frameMutateAndRead:
		return "mutate+read"
	default:
		return "unclassified"
	}
}

func (c frameClass) reads() bool   { return c == frameRead || c == frameMutateAndRead }
func (c frameClass) mutates() bool { return c == frameMutate || c == frameMutateAndRead }

// frameHandler serves one frame type.
type frameHandler func(s *Server, ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string)

// frameRoute is one frame type the server serves after HELLO: what the frame
// does, and the handler that does it. The class lives beside the handler so
// that a frame type cannot be routed without one.
type frameRoute struct {
	class  frameClass
	handle frameHandler
}

// frameRoutes is every frame type dispatchFrame serves. A frame type that is
// not here is answered "unknown message type" and reaches nothing (see admit).
// That is what happens to a second HELLO, to the response opcodes, and to the
// cluster opcodes (0x20-0x41): those are served by the cluster listener
// (cmd/muninn handleClusterConn), never by this one.
//
// Adding a frame type means adding it here WITH its class, and recording the
// same decision in wantFrameClasses (mode_matrix_test.go).
// TestEveryDispatchedFrameHasAModeClass fails until both agree, and
// TestCredentialModeMatrix then checks every kind of session against it.
//
// Each class is the one the same operation has on REST, gRPC and MCP, so the
// four transports agree. UNSUB has no counterpart there. It takes the class of
// SUBSCRIBE: it can only end a subscription made on the same connection
// (SEC-26), and a connection that may not SUBSCRIBE has made none.
//
// What an append-mode credential may not do is not decided here. Changing or
// deleting an existing memory is refused by the engine (SEC-15) on every
// transport, and a WRITE can be either a create or, with upsert_mode, a
// rewrite, which only the engine can tell apart.
var frameRoutes = map[uint8]frameRoute{
	TypeWrite:     {frameMutate, (*Server).handleWrite},
	TypeRead:      {frameRead, (*Server).handleRead},
	TypeActivate:  {frameRead, (*Server).handleActivate},
	TypeSubscribe: {frameRead, (*Server).handleSubscribe},
	TypeUnsub:     {frameRead, (*Server).handleUnsub},
	TypeLink:      {frameMutate, (*Server).handleLink},
	TypeForget:    {frameMutate, (*Server).handleForget},
	TypeStat:      {frameRead, (*Server).handleStat},
	TypePing:      {frameControl, (*Server).handlePing},
}

// The refusal messages are the ones REST and gRPC give for the same refusal.
const (
	msgReadOnlyKey      = "read-only key cannot write"
	msgWriteOnlyKey     = "write-only key cannot read"
	msgUnrecognizedMode = "unrecognized key mode"
	// msgKeyNoLongerValid answers every frame on a connection whose key has
	// been revoked or has expired since HELLO (vault_scope.go sessionEnded).
	msgKeyNoLongerValid = "api key is no longer valid"
)

// modeRefusal returns why a session whose credential has the given mode may
// not send a frame of the given class, or "" when it may.
//
// The mode is the one HELLO stored on the connection: the key's own mode for a
// keyed session, auth.ModeFull for a credential-less session on a public vault
// (full access by design, as on REST and gRPC). It is fixed for the life of
// the connection — a second HELLO is not served — so checking it on each frame
// is checking the same value each time; it is done per frame anyway because
// that is the one place every frame passes.
//
// The switch names the modes that are allowed. Any other value, including a
// connection with no mode at all, is refused everything, as MCP does.
func modeRefusal(mode string, class frameClass) string {
	switch mode {
	case auth.ModeFull, auth.ModeAppend:
		return ""
	case auth.ModeObserve:
		if class.mutates() {
			return msgReadOnlyKey
		}
		return ""
	case auth.ModeWrite:
		if class.reads() {
			return msgWriteOnlyKey
		}
		return ""
	default:
		return msgUnrecognizedMode
	}
}

// admit decides whether a frame of the given type is served on a connection
// whose credential has the given mode. It returns the handler to run, or a nil
// handler and the error to answer with.
//
// A frame type with no route, or with a route nobody classified, is served to
// no one — it is answered exactly as an opcode the server has never heard of.
func admit(routes map[uint8]frameRoute, frameType uint8, mode string) (frameHandler, ErrorCode, string) {
	route, ok := routes[frameType]
	if !ok || route.class == frameUnclassified || route.handle == nil {
		return nil, ErrInternal, fmt.Sprintf("unknown message type: 0x%02X", frameType)
	}
	if refusal := modeRefusal(mode, route.class); refusal != "" {
		return nil, ErrAuthFailed, refusal
	}
	return route.handle, 0, ""
}
