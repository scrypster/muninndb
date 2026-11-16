package mbp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

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

	// maxConns is the most connections the listener holds at once
	// (connection_cap.go). Zero until the server serves, when it is resolved
	// from the memory limit or the override.
	maxConns    int
	refusals    chan net.Conn // refused connections waiting for a courtesy answer
	openConns   atomic.Int64
	capRefused  atomic.Int64 // refused since the last warning
	capLastWarn atomic.Int64 // UnixNano of the last warning
	capWarnings atomic.Int64 // warnings logged
	// handshakeTimeout overrides helloTimeout when positive. Tests set it so
	// they do not wait fifteen seconds for a silent connection to be closed.
	handshakeTimeout time.Duration
	// decodeBudget overrides connDecodeBudget when positive. Tests set it to
	// watch the budget work without megabyte payloads.
	decodeBudget int64
	// decodedInFlight is the decompressed payload charged to every
	// connection's budget right now, in bytes. It returns to zero when no
	// compressed frame is being handled.
	decodedInFlight atomic.Int64
	// replyStall overrides replyStallTimeout when positive. Tests set it so
	// they do not wait fifteen seconds for a stalled connection to be closed.
	replyStall time.Duration
	// repliesUnwritten is the reply payload queued on every connection and not
	// yet written to its socket, in bytes (reply_flow.go).
	repliesUnwritten atomic.Int64
	// stalledClosed counts connections closed because they stopped taking
	// their replies; stallLastWarn rate-limits the log line about it.
	stalledClosed atomic.Int64
	stallLastWarn atomic.Int64
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
	if s.maxConns <= 0 {
		s.maxConns = resolveMaxConnections()
	}
	slog.Info("mbp: listening", "addr", ln.Addr().String(), "max_connections", s.maxConns, "set_with", maxConnectionsEnv)
	s.listener = ln
	s.startRefusers()

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

		// A place for it, or a refusal: the listener holds a bounded number of
		// connections (SEC-28).
		if !s.admitConnection() {
			s.refuseAtCap(conn)
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.releaseConnection()
			s.handleConnection(ctx, conn)
		}()
	}
}

// Limits on a connection that has not authenticated yet (SEC-27). Until HELLO
// succeeds the peer has shown no credential, so what it can make the server
// hold has to be small and has to end.
const (
	// helloTimeout is how long a connection has to complete the handshake:
	// from accept to HELLO_OK written. It is one deadline for the whole
	// exchange, not one per read, so a peer that sends a byte now and then does
	// not keep it open. The value is the REST server's ReadTimeout, which is
	// the same bound on the same thing there: the time a client has to deliver
	// a request. Like that one it is fixed, not configurable.
	helloTimeout = 15 * time.Second

	// maxHelloPayload is the largest HELLO payload read. A HELLO holds a
	// version, a key (46 bytes), a vault name (at most 64), a client name and
	// a few capability names; 64 KiB is several hundred times that. A frame
	// header that declares more is refused before anything is allocated for
	// it. Without this, the 16 bytes of a header declaring MaxPayloadSize made
	// the server allocate 16 MiB for a peer that had not authenticated.
	maxHelloPayload = 64 * 1024
)

func (s *Server) helloDeadline() time.Time {
	if s.handshakeTimeout > 0 {
		return time.Now().Add(s.handshakeTimeout)
	}
	return time.Now().Add(helloTimeout)
}

// handleConnection handles a single client connection.
func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Set up connection timeout
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The handshake is on a clock: everything up to HELLO_OK, reads and writes
	// both. It is taken off once the session exists (below).
	_ = conn.SetDeadline(s.helloDeadline())

	// Read the HELLO frame first
	helloFrame, err := readFrameLimited(conn, maxHelloPayload)
	if err != nil {
		if errors.Is(err, ErrPayloadTooLarge) {
			s.writeErrorFrame(conn, 0, ErrAuthFailed, "HELLO payload too large")
			return
		}
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
	// Ending the session stops the reader; the frames already being handled
	// finish and are answered (the refusal that ended it among them), and then
	// the connection is closed (readerLoop, SEC-22).
	// flow is this connection's account of the replies it has not read and the
	// requests being handled for it (reply_flow.go, SEC-30).
	flow := newConnFlow(&s.repliesUnwritten)
	var endOnce sync.Once
	scope.end = func() {
		endOnce.Do(func() {
			_ = conn.SetReadDeadline(time.Now())
			flow.stop() // the reader may be waiting on the account, not on the socket
		})
	}
	connCtx = withVaultScope(connCtx, scope)
	connCtx = context.WithValue(connCtx, auth.ContextMode, connMode)
	connCtx = withDecodeBudget(connCtx, s.connDecodeBudget(), &s.decodedInFlight)
	connCtx = withConnFlow(connCtx, flow)
	// A reader waiting on the account is released when the connection's
	// context ends, as one waiting on the socket is by the socket closing.
	defer context.AfterFunc(connCtx, flow.abandon)()

	// Resolve and validate the HELLO vault under the new scope, then pin the
	// request to it so the engine registers (and the response advertises) the
	// authorized vault rather than a client-supplied one.
	resolvedVault, err := s.scopeVault(connCtx, helloReq.Vault)
	if err != nil {
		s.writeErrorFrame(conn, helloFrame.CorrelationID, ErrAuthFailed, err.Error())
		return
	}
	helloReq.Vault = resolvedVault
	scope.openedOn = resolvedVault

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

	// The session exists. From here the connection is not on the handshake's
	// clock: a client may hold it open and idle between requests.
	_ = conn.SetDeadline(time.Time{})

	// Start writer goroutine
	writeCh := make(chan *Frame, replyQueueLen)
	writerErr := make(chan error, 1)
	go s.writerLoop(conn, writeCh, writerErr, flow)

	// Start reader loop. When it returns, no handler is still running.
	s.readerLoop(connCtx, conn, writeCh, helloResp.Capabilities)

	// The connection is over, and so are the subscriptions it made. No handler
	// is running, so none can be added after this.
	for _, id := range scope.subs.drain() {
		_ = s.engine.Unsubscribe(connCtx, id)
	}

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
	flow := flowOf(ctx)
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

		// Not while this connection has replies backed up or enough being
		// handled already: a connection that is not reading its answers stops
		// being read from (reply_flow.go). The reader holds nothing while it
		// waits here.
		if flow != nil && !flow.waitToRead() {
			return
		}

		frame, err := ReadFrame(conn)
		if err != nil {
			return // Connection closed or read error
		}
		held := len(frame.Payload)
		if flow != nil {
			flow.requestRead(held)
		}

		// Acquire semaphore slot before spawning; blocks if maxConcurrentFrames
		// goroutines are already active for this connection.
		sem <- struct{}{}

		// Launch handler goroutine
		go func(f *Frame) {
			defer func() { <-sem }()
			if flow != nil {
				defer flow.requestDone(held)
			}
			s.dispatchFrame(ctx, f, writeCh, negotiatedCapabilities)
		}(frame)
	}
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
	// Is there still a session? What authorised it at HELLO — a key, or a
	// public vault — is checked again for every frame, of every type, so a key
	// revoked or expired while the connection is open stops working on the
	// next frame rather than when the client chooses to reconnect (SEC-23).
	if ended := s.sessionEnded(ctx); ended != "" {
		s.queueErrorFrame(ctx, writeCh, frame.CorrelationID, ErrAuthFailed, ended)
		endSession(ctx)
		return
	}

	mode, _ := ctx.Value(auth.ContextMode).(string)
	handle, code, refusal := admit(frameRoutes, frame.Type, mode)
	if handle == nil {
		s.queueErrorFrame(ctx, writeCh, frame.CorrelationID, code, refusal)
		return
	}

	// Decompress only now, for a frame that was admitted, into at most what a
	// frame may carry, and charged to this connection's budget until the
	// handler is done with it (compress.go, decode_budget.go). A frame that
	// does not fit the budget is answered, not held: nothing here waits.
	var payload []byte = frame.Payload
	if (frame.Flags & FlagCompressed) != 0 {
		decompressed, release, err := decodeCharged(ctx, frame.Payload)
		if err != nil {
			code, message := decompressionFailure(err)
			s.queueErrorFrame(ctx, writeCh, frame.CorrelationID, code, message)
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
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid write request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Write(ctx, &req)
	if err != nil {
		s.queueEngineError(ctx, writeCh, corrID, err, ErrStorageError)
		return
	}

	s.queueFrame(ctx, writeCh, TypeWriteOK, corrID, resp, caps)
}

func (s *Server) handleRead(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ReadRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid read request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	// Room for the reply, before the work: see reply_flow.go.
	ctx, giveBack, ok := s.reserveReply(ctx, writeCh, corrID, readReplyBound)
	if !ok {
		return
	}
	defer giveBack()

	resp, err := s.engine.Read(ctx, &req)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrEngramNotFound, err.Error())
		return
	}

	s.queueFrame(ctx, writeCh, TypeReadResp, corrID, resp, caps)
}

func (s *Server) handleActivate(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ActivateRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid activate request")
		return
	}
	// Clamp a negative threshold: the in-process negative-means-bypass contract
	// (used by Explain) is deliberately not exposed on the wire.
	if req.Threshold < 0 {
		req.Threshold = 0
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	// The response frame is the ActivateResponse verbatim, so it carries the
	// COG-35 associated block: the engine may build it and record its ids.
	// Room for the reply, before the work: see reply_flow.go.
	ctx, giveBack, ok := s.reserveReply(ctx, writeCh, corrID, activateReplyBound(req.MaxResults))
	if !ok {
		return
	}
	defer giveBack()

	resp, err := s.engine.Activate(WithAssociatedRendered(ctx), &req)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrIndexError, err.Error())
		return
	}

	s.queueActivateResponse(ctx, writeCh, corrID, resp, caps)
}

func (s *Server) handleSubscribe(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req SubscribeRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid subscribe request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	// The subscription belongs to this connection: it counts against the
	// connection's limit, only this connection can end it, and it ends when the
	// connection does (SEC-26).
	sc, _ := ctx.Value(vaultScopeKey).(*vaultScope)
	if sc == nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, "vault access denied: connection is not authorized")
		return
	}
	if !sc.subs.reserve() {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrRateLimited, "subscription limit reached for this connection")
		return
	}

	resp, err := s.engine.Subscribe(ctx, &req)
	if err != nil {
		sc.subs.settle("")
		s.queueErrorFrame(ctx, writeCh, corrID, ErrSubscriptionNotFound, err.Error())
		return
	}
	sc.subs.settle(resp.SubID)

	s.queueFrame(ctx, writeCh, TypeSubOK, corrID, resp, caps)
}

func (s *Server) handleUnsub(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req UnsubscribeRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid unsub request")
		return
	}

	// Only a subscription made on this connection. Any other id — another
	// connection's, another vault's, one that does not exist — is left alone,
	// and answered exactly as an id nobody holds always has been, so the answer
	// says nothing about whether it exists (SEC-26).
	if sc, _ := ctx.Value(vaultScopeKey).(*vaultScope); sc != nil && sc.subs.release(req.SubID) {
		if err := s.engine.Unsubscribe(ctx, req.SubID); err != nil {
			s.queueErrorFrame(ctx, writeCh, corrID, ErrSubscriptionNotFound, err.Error())
			return
		}
	}

	s.queueFrame(ctx, writeCh, TypeUnsubOK, corrID, &UnsubscribeResponse{OK: true}, caps)
}

func (s *Server) handleLink(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req LinkRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidAssociation, "invalid link request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Link(ctx, &req)
	if err != nil {
		s.queueEngineError(ctx, writeCh, corrID, err, ErrInvalidAssociation)
		return
	}

	s.queueFrame(ctx, writeCh, TypeLinkOK, corrID, resp, caps)
}

func (s *Server) handleForget(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req ForgetRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid forget request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Forget(ctx, &req)
	if err != nil {
		s.queueEngineError(ctx, writeCh, corrID, err, ErrStorageError)
		return
	}

	s.queueFrame(ctx, writeCh, TypeForgetOK, corrID, resp, caps)
}

func (s *Server) handleStat(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	var req StatRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "invalid stat request")
		return
	}

	resolved, err := s.scopeVault(ctx, req.Vault)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, err.Error())
		return
	}
	req.Vault = resolved

	resp, err := s.engine.Stat(ctx, &req)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrStorageError, err.Error())
		return
	}

	s.queueFrame(ctx, writeCh, TypeStatResp, corrID, resp, caps)
}

func (s *Server) handlePing(ctx context.Context, corrID uint64, payload []byte, writeCh chan *Frame, caps []string) {
	// PONG repeats what PING carried, so what PING may carry is small
	// (maxPingPayload).
	if len(payload) > maxPingPayload {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInvalidEngram, "ping payload too large: at most 1024 bytes are echoed")
		return
	}
	var req PingRequest
	if err := DecodeMsgpack(payload, &req); err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInternal, "invalid ping request")
		return
	}

	s.queueFrame(ctx, writeCh, TypePong, corrID, &PongResponse{Data: req.Data}, caps)
}

// Utility methods

func (s *Server) queueFrame(ctx context.Context, writeCh chan *Frame, frameType uint8, corrID uint64, payload interface{}, caps []string) {
	data, err := EncodeMsgpack(payload)
	if err != nil {
		s.queueErrorFrame(ctx, writeCh, corrID, ErrInternal, "failed to encode response")
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

	s.enqueue(ctx, writeCh, &Frame{
		Version:       0x01,
		Type:          frameType,
		Flags:         flags,
		PayloadLength: uint32(len(data)),
		CorrelationID: corrID,
		Payload:       data,
	})
}

func (s *Server) queueActivateResponse(ctx context.Context, writeCh chan *Frame, corrID uint64, resp *ActivateResponse, caps []string) {
	// For now, send as a single frame
	// TODO: implement multi-frame streaming for large result sets
	s.queueFrame(ctx, writeCh, TypeActivateResp, corrID, resp, caps)
}

// queueEngineError classifies an engine error before framing it. A write that
// reached a non-Cortex node gets ErrNotCortex (4015) with the leader hint in the
// message, never the generic storage/association code — a client cannot retry
// against the right node if the wire says "storage error" (#596).
func (s *Server) queueEngineError(ctx context.Context, writeCh chan *Frame, corrID uint64, err error, fallback ErrorCode) {
	code := fallback
	if errors.Is(err, ErrNotLeader) {
		code = ErrNotCortex
	}
	s.queueErrorFrame(ctx, writeCh, corrID, code, err.Error())
}

func (s *Server) queueErrorFrame(ctx context.Context, writeCh chan *Frame, corrID uint64, code ErrorCode, message string) {
	frame, ok := errorFrame(corrID, code, message)
	if !ok {
		// Can't even encode the error, give up
		return
	}
	s.enqueue(ctx, writeCh, frame)
}

// errorFrame builds an error frame. It reports false if the payload could not
// be encoded.
func errorFrame(corrID uint64, code ErrorCode, message string) (*Frame, bool) {
	data, err := EncodeMsgpack(&ErrorPayload{
		Code:      code,
		Message:   message,
		RequestID: uuid.New().String()[:8],
	})
	if err != nil {
		return nil, false
	}
	return &Frame{
		Version:       0x01,
		Type:          TypeError,
		Flags:         0,
		PayloadLength: uint32(len(data)),
		CorrelationID: corrID,
		Payload:       data,
	}, true
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
