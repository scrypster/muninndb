package mbp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/google/uuid"
	"github.com/scrypster/muninndb/internal/auth"
)

// EngineAPI is the interface the MBP server requires from the engine.
// Defined here (consumer-side), implemented by internal/engine or a mock.
type EngineAPI interface {
	Hello(ctx context.Context, req *HelloRequest) (*HelloResponse, error)
	Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error)
	Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error)
	Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error)
	Subscribe(ctx context.Context, req *SubscribeRequest) (*SubscribeResponse, error)
	Unsubscribe(ctx context.Context, subID string) error
	Link(ctx context.Context, req *LinkRequest) (*LinkResponse, error)
	Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error)
	Stat(ctx context.Context, req *StatRequest) (*StatResponse, error)
}

// Server listens on a TCP port and handles MBP connections.
type Server struct {
	addr      string
	engine    EngineAPI
	authStore *auth.Store
	listener  net.Listener
	shutdown  chan struct{}
	wg        sync.WaitGroup
	shutdownM sync.Mutex
	tlsConfig *tls.Config // nil = plain TCP

	// decodeBudget overrides connDecodeBudget when positive. Tests set it to
	// watch the budget work without megabyte payloads.
	decodeBudget int64
}

// NewServer creates a new MBP server.
// tlsConfig, if non-nil, wraps the TCP listener with TLS.
func NewServer(addr string, engine EngineAPI, authStore *auth.Store, tlsConfig *tls.Config) *Server {
	return &Server{
		addr:      addr,
		engine:    engine,
		authStore: authStore,
		shutdown:  make(chan struct{}),
		tlsConfig: tlsConfig,
	}
}

// Serve starts listening and blocks until context is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener serves on a listener the caller already holds, and blocks until
// Shutdown is called. It closes ln on return. A caller that binds port 0 itself
// knows the address before anything is served and knows the listener is its
// own, which a "pick a free port, release it, bind it again" sequence cannot
// promise.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	if s.tlsConfig != nil {
		ln = tls.NewListener(ln, s.tlsConfig)
		slog.Info("mbp: TLS enabled", "addr", ln.Addr().String())
	}
	slog.Info("mbp: listening", "addr", ln.Addr().String())
	s.listener = ln

	// Accept connections in a goroutine
	s.wg.Add(1)
	go s.acceptLoop(ctx)

	// Wait for shutdown signal
	<-s.shutdown
	ln.Close()
	s.wg.Wait()

	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownM.Lock()
	defer s.shutdownM.Unlock()

	select {
	case <-s.shutdown:
		return nil // Already shut down
	default:
	}

	close(s.shutdown)

	// Wait for goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acceptLoop accepts incoming connections.
func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()

	for {
		select {
		case <-s.shutdown:
			return
		default:
		}

		// Set accept deadline to allow checking shutdown.
		// Use a defensive type assertion: a TLS listener wraps the TCP listener
		// and does not implement *net.TCPListener directly, so we skip the
		// deadline if the underlying type is not accessible.
		if deadline, ok := ctx.Deadline(); ok {
			if tc, ok := s.listener.(*net.TCPListener); ok {
				tc.SetDeadline(deadline)
			}
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
			}
			// Log but continue
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConnection(ctx, conn)
		}()
	}
}

// handleConnection handles a single client connection.
func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Set up connection timeout
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Read the HELLO frame first
	helloFrame, err := ReadFrame(conn)
	if err != nil {
		s.writeErrorFrame(conn, 0, ErrAuthFailed, "failed to read HELLO")
		return
	}

	if helloFrame.Type != TypeHello {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, "expected HELLO as first message")
		return
	}

	// Decode HELLO request
	var helloReq HelloRequest
	if err := DecodeMsgpack(helloFrame.Payload, &helloReq); err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, "invalid HELLO payload")
		return
	}

	// Validate HELLO
	if err := ValidateHelloRequest(&helloReq); err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, err.Error())
		return
	}

	// Authenticate and establish the connection's vault scope. The scope is
	// enforced on every subsequent frame (see dispatchFrame handlers), closing
	// the gap where a "none" session could read/write any vault and a keyed
	// session could name a vault other than its key's.
	if s.authStore == nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, "server misconfigured: vault auth unavailable")
		return
	}
	scope := &vaultScope{}
	// Credential mode for the whole connection. A keyed session carries its
	// key's real mode (so an observe key stays observe); a credential-less
	// session (open/localhost MBP) is full access. This mirrors gRPC
	// (internal/transport/grpc/server.go:172,184) and REST
	// (internal/auth/middleware.go). Without it, ctx has no auth.ContextMode on
	// MBP, so (a) resolveTrust (SEC-14) over-blocks a legitimate full/write key's
	// trust=verified write, and (b) an observe key's reads still reinforce
	// (COG-11), because auth.ObserveFromContext returns false. Use the key's
	// actual mode — a blanket ModeFull here would let an observe MBP key stamp
	// verified, turning the over-block into a real SEC-14 under-block.
	//
	// The same value is what dispatchFrame checks every later frame against
	// (SEC-20, frame_modes.go). It is set once, here: the handshake is not
	// repeated on a connection, so the mode cannot change under it.
	connMode := auth.ModeFull
	if helloReq.AuthMethod == "token" {
		key, err := s.authStore.ValidateAPIKey(helloReq.Token)
		if err != nil {
			s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, "invalid token")
			return
		}
		scope.key = &key
		connMode = key.Mode
	}
	connCtx = withVaultScope(connCtx, scope)
	connCtx = context.WithValue(connCtx, auth.ContextMode, connMode)
	connCtx = withDecodeBudget(connCtx, s.connDecodeBudget())

	// Resolve and validate the HELLO vault under the new scope, then pin the
	// request to it so the engine registers (and the response advertises) the
	// authorized vault rather than a client-supplied one.
	resolvedVault, err := s.scopeVault(connCtx, helloReq.Vault)
	if err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, err.Error())
		return
	}
	helloReq.Vault = resolvedVault

	// Call engine's Hello handler
	helloResp, err := s.engine.Hello(connCtx, &helloReq)
	if err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, err.Error())
		return
	}

	// Write HELLO_OK
	helloOKFrame := &Frame{
		Version:       0x01,
		Type:          TypeHelloOK,
		Flags:         0,
		CorrelationID: helloFrame.CorrelationID,
	}

	payload, err := EncodeMsgpack(helloResp)
	if err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrInternal, "failed to encode HELLO_OK")
		return
	}
	helloOKFrame.Payload = payload
	helloOKFrame.PayloadLength = uint32(len(payload))

	if err := WriteFrame(conn, helloOKFrame); err != nil {
		return
	}

	// Start writer goroutine
	writeCh := make(chan *Frame, 256)
	writerErr := make(chan error, 1)
	go s.writerLoop(conn, writeCh, writerErr)

	// Start reader loop. When it returns, no handler is still running.
	s.readerLoop(connCtx, conn, writeCh, helloResp.Capabilities)

	close(writeCh)

	// Wait for writer goroutine
	<-writerErr
}

// maxConcurrentFrames caps the number of goroutines processing frames
// concurrently for a single connection. This prevents goroutine exhaustion
// from a client sending rapid-fire frames (Slowloris-style attack).
const maxConcurrentFrames = 64

// readerLoop reads frames and dispatches them to handlers.
// A semaphore limits per-connection goroutine concurrency to maxConcurrentFrames.
//
// It returns only when every handler it started has finished. The caller
// closes writeCh next, and a handler that went to answer after that would be
// sending on a closed channel — a panic, in a goroutine nothing recovers, that
// ends the whole server process. Any client could cause it by disconnecting
// with a request still being handled (SEC-22). The handlers are not cut short:
// what the client asked for before it left is carried out, and the answers
// nobody is there to read are dropped by the writer.
func (s *Server) readerLoop(ctx context.Context, conn net.Conn, writeCh chan *Frame, negotiatedCapabilities []string) {
	sem := make(chan struct{}, maxConcurrentFrames)
	// Taking every slot is waiting for every handler: each holds one until it
	// returns.
	defer func() {
		for i := 0; i < cap(sem); i++ {
			sem <- struct{}{}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := ReadFrame(conn)
		if err != nil {
			return // Connection closed or read error
		}

		// Acquire semaphore slot before spawning; blocks if maxConcurrentFrames
		// goroutines are already active for this connection.
		sem <- struct{}{}

		// Launch handler goroutine
		go func(f *Frame) {
			defer func() { <-sem }()
			s.dispatchFrame(ctx, f, writeCh, negotiatedCapabilities)
		}(frame)
	}
}

// writerLoop serializes frames to the connection.
func (s *Server) writerLoop(conn net.Conn, writeCh chan *Frame, errCh chan error) {
	bw := bufio.NewWriter(conn)
	defer bw.Flush()

	for frame := range writeCh {
		if err := WriteFrame(bw, frame); err != nil {
			errCh <- err
			return
		}
		bw.Flush()
	}

	errCh <- nil
}

// dispatchFrame routes a frame to its handler. It is the one place every frame
// after HELLO passes, so it is where the credential's mode is enforced
// (SEC-20): admit decides, and no handler is reached by a frame the
// connection's mode may not send.
func (s *Server) dispatchFrame(ctx context.Context, frame *Frame, writeCh chan *Frame, negotiatedCapabilities []string) {
	// Before the payload is decompressed or decoded and before the engine is
	// called: the answer depends on the frame type and the connection alone, so
	// it is the same whether or not the request is well formed or its target
	// exists.
	mode, _ := ctx.Value(auth.ContextMode).(string)
	handle, code, refusal := admit(frameRoutes, frame.Type, mode)
	if handle == nil {
		s.queueErrorFrame(writeCh, frame.CorrelationID, code, refusal)
		return
	}

	// Decompress only now, for a frame that was admitted, into at most what a
	// frame may carry, and charged to this connection's budget until the
	// handler is done with it (compress.go, decode_budget.go).
	var payload []byte = frame.Payload
	if (frame.Flags & FlagCompressed) != 0 {
		decompressed, release, err := decodeCharged(ctx, frame.Payload)
		if err != nil {
			if ctx.Err() == nil {
				s.queueErrorFrame(writeCh, frame.CorrelationID, ErrInternal, decompressionFailure(err))
			}
			return
		}
		defer release()
		payload = decompressed
	}

	handle(s, ctx, frame.CorrelationID, payload, writeCh, negotiatedCapabilities)
}

// Handler methods

func (s *Server) handleWrite(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req WriteRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid write request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Write(ctx, &req)
	if err != nil {
		s.queueEngineError(writeCh, corrID, err, ErrStorageError)
		return
	}

	s.queueFrame(writeCh, TypeWriteOK, corrID, resp, caps)
}

func (s *Server) handleRead(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ReadRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid read request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Read(ctx, &req)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrEngramNotFound, err.Error())
		return
	}

	s.queueFrame(writeCh, TypeReadResp, corrID, resp, caps)
}

func (s *Server) handleActivate(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ActivateRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid activate request")
		return
	}
	// Clamp a negative threshold: the in-process negative-means-bypass contract
	// (used by Explain) is deliberately not exposed on the wire.
	if req.Threshold < 0 {
		req.Threshold = 0
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	// The response frame is the ActivateResponse verbatim, so it carries the
	// COG-35 associated block: the engine may build it and record its ids.
	resp, err := s.engine.Activate(WithAssociatedRendered(ctx), &req)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrIndexError, err.Error())
		return
	}

	s.queueActivateResponse(writeCh, corrID, resp, caps)
}

func (s *Server) handleSubscribe(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req SubscribeRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid subscribe request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Subscribe(ctx, &req)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrSubscriptionNotFound, err.Error())
		return
	}

	s.queueFrame(writeCh, TypeSubOK, corrID, resp, caps)
}

func (s *Server) handleUnsub(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req UnsubscribeRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid unsub request")
		return
	}

	err := s.engine.Unsubscribe(ctx, req.SubID)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrSubscriptionNotFound, err.Error())
		return
	}

	s.queueFrame(writeCh, TypeUnsubOK, corrID, &UnsubscribeResponse{OK: true}, caps)
}

func (s *Server) handleLink(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req LinkRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidAssociation, "invalid link request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Link(ctx, &req)
	if err != nil {
		s.queueEngineError(writeCh, corrID, err, ErrInvalidAssociation)
		return
	}

	s.queueFrame(writeCh, TypeLinkOK, corrID, resp, caps)
}

func (s *Server) handleForget(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ForgetRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid forget request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Forget(ctx, &req)
	if err != nil {
		s.queueEngineError(writeCh, corrID, err, ErrStorageError)
		return
	}

	s.queueFrame(writeCh, TypeForgetOK, corrID, resp, caps)
}

func (s *Server) handleStat(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req StatRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInvalidEngram, "invalid stat request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Stat(ctx, &req)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrStorageError, err.Error())
		return
	}

	s.queueFrame(writeCh, TypeStatResp, corrID, resp, caps)
}

func (s *Server) handlePing(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req PingRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInternal, "invalid ping request")
		return
	}

	s.queueFrame(writeCh, TypePong, corrID, &PongResponse{Data: req.Data}, caps)
}

// Utility methods

func (s *Server) queueFrame(writeCh chan *Frame, frameType uint8, corrID uint64, payload interface{}, caps []string) {
	data, err := EncodeMsgpack(payload)
	if err != nil {
		s.queueErrorFrame(writeCh, corrID, ErrInternal, "failed to encode response")
		return
	}

	// Check if compression is negotiated and payload is large
	compressed := false
	if len(data) > CompressionThreshold {
		for _, cap := range caps {
			if cap == "compression" {
				if compData, isCompressed, err := CompressPayload(data); err == nil {
					data = compData
					compressed = isCompressed
				}
				break
			}
		}
	}

	flags := uint16(0)
	if compressed {
		flags |= FlagCompressed
	}

	frame := &Frame{
		Version:       0x01,
		Type:          frameType,
		Flags:         flags,
		PayloadLength: uint32(len(data)),
		CorrelationID: corrID,
		Payload:       data,
	}

	select {
	case writeCh <- frame:
	default:
		// Channel full, drop frame (should not happen with buffered channel)
	}
}

func (s *Server) queueActivateResponse(writeCh chan *Frame, corrID uint64, resp *ActivateResponse, caps []string) {
	// For now, send as a single frame
	// TODO: implement multi-frame streaming for large result sets
	s.queueFrame(writeCh, TypeActivateResp, corrID, resp, caps)
}

// queueEngineError classifies an engine error before framing it. A write that
// reached a non-Cortex node gets ErrNotCortex (4015) with the leader hint in the
// message, never the generic storage/association code — a client cannot retry
// against the right node if the wire says "storage error" (#596).
func (s *Server) queueEngineError(writeCh chan *Frame, corrID uint64, err error, fallback ErrorCode) {
	code := fallback
	if errors.Is(err, ErrNotLeader) {
		code = ErrNotCortex
	}
	s.queueErrorFrame(writeCh, corrID, code, err.Error())
}

func (s *Server) queueErrorFrame(writeCh chan *Frame, corrID uint64, code ErrorCode, message string) {
	errPayload := &ErrorPayload{
		Code:      code,
		Message:   message,
		RequestID: uuid.New().String()[:8],
	}

	data, err := EncodeMsgpack(errPayload)
	if err != nil {
		// Can't even encode the error, give up
		return
	}

	frame := &Frame{
		Version:       0x01,
		Type:          TypeError,
		Flags:         0,
		PayloadLength: uint32(len(data)),
		CorrelationID: corrID,
		Payload:       data,
	}

	select {
	case writeCh <- frame:
	default:
	}
}

func (s *Server) writeErrorFrame(conn net.Conn, corrID uint64, code ErrorCode, message string) {
	errPayload := &ErrorPayload{
		Code:      code,
		Message:   message,
		RequestID: uuid.New().String()[:8],
	}

	data, err := EncodeMsgpack(errPayload)
	if err != nil {
		return
	}

	frame := &Frame{
		Version:       0x01,
		Type:          TypeError,
		Flags:         0,
		PayloadLength: uint32(len(data)),
		CorrelationID: corrID,
		Payload:       data,
	}

	WriteFrame(conn, frame)
}
