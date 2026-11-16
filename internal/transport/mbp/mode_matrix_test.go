package mbp_test

// Credential-mode enforcement over MBP, checked end to end: a real mbp.Server
// on a real TCP listener, a real auth.Store issuing the keys, and a real
// Engine behind it. The engine is wrapped only to record which of its methods a
// frame reached.
//
// Fixture domain: a fictional harbour pilot station.

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The four classes a client frame can have, as far as a credential's mode is
// concerned. They are spelled as the production table reports them.
const (
	// classControl returns nothing stored and changes nothing.
	classControl = "control"
	// classRead returns stored memory or vault state, or manages a
	// subscription to it.
	classRead = "read"
	// classMutate creates, changes or deletes stored memory.
	classMutate = "mutate"
	// classMutateAndRead does both, so it is refused to an observe key as a
	// mutation and to an ingest-only key as a read. No MBP frame has this
	// class today; gRPC's AdjustConfidence is the in-tree example.
	classMutateAndRead = "mutate+read"
)

// wantFrameClasses is the decision, recorded here by a person, of what every
// frame type the MBP server dispatches does. It is deliberately a second copy
// of the production table: TestEveryDispatchedFrameHasAModeClass fails when
// the two differ in either direction, so a frame type cannot be added, removed
// or reclassified without that decision being made twice.
//
// Each class is the one the same operation has on the other transports:
//
//	frame      REST route (guard)                         gRPC class   MCP bucket
//	WRITE      POST /api/engrams (ReadOnlyGuard)          mutate       muninn_remember: mutating
//	READ       GET /api/engrams/{id} (WriteOnlyGuard)     read         muninn_read: read-only
//	ACTIVATE   POST /api/activate (WriteOnlyGuard)        read         muninn_recall: read-only
//	SUBSCRIBE  GET /api/subscribe (WriteOnlyGuard)        read         (no tool)
//	UNSUB      (none: the SSE stream is closed)           (stream end) (no tool)
//	LINK       POST /api/link (ReadOnlyGuard)             mutate       muninn_link: mutating
//	FORGET     DELETE /api/engrams/{id} (ReadOnlyGuard)   mutate       muninn_forget: mutating
//	STAT       GET /api/stats (WriteOnlyGuard)            read         muninn_status: read-only
//	PING       (none; GET /api/health is public)          (keepalive)  (no tool)
//
// UNSUB has no counterpart elsewhere. It ends a subscription, which only a
// credential that may read can hold, so it takes the class of SUBSCRIBE.
var wantFrameClasses = map[uint8]string{
	mbp.TypeWrite:     classMutate,
	mbp.TypeRead:      classRead,
	mbp.TypeActivate:  classRead,
	mbp.TypeSubscribe: classRead,
	mbp.TypeUnsub:     classRead,
	mbp.TypeLink:      classMutate,
	mbp.TypeForget:    classMutate,
	mbp.TypeStat:      classRead,
	mbp.TypePing:      classControl,
}

// The refusal messages are the ones REST and gRPC use for the same refusal.
const (
	writeOnlyMsg = "write-only key cannot read"
	readOnlyMsg  = "read-only key cannot write"
	// appendMsg is the engine's refusal (engine.ErrAppendForbidden), not the
	// transport's: what an append key may not touch is an engine property
	// (SEC-15) and holds on every transport.
	appendMsg = "append-mode credential cannot modify or delete existing memories"
)

// ---------------------------------------------------------------------------
// A real engine, with the calls that reach it recorded.
// ---------------------------------------------------------------------------

// recordingEngine passes every call through to the real engine and records the
// method name, so a test can tell "refused before the engine" from "the engine
// was reached and answered".
type recordingEngine struct {
	mbp.EngineAPI
	mu    sync.Mutex
	calls []string

	// writeEntered and writeHold, when set, make Write announce itself and
	// then wait: a test uses them to keep a handler running.
	writeEntered chan struct{}
	writeHold    chan struct{}
}

func (r *recordingEngine) note(name string) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
}

// take returns the calls recorded since the last take, and clears them.
func (r *recordingEngine) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func (r *recordingEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	r.note("Write")
	if r.writeHold != nil {
		r.writeEntered <- struct{}{}
		<-r.writeHold
	}
	return r.EngineAPI.Write(ctx, req)
}

func (r *recordingEngine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	r.note("Read")
	return r.EngineAPI.Read(ctx, req)
}

func (r *recordingEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	r.note("Activate")
	return r.EngineAPI.Activate(ctx, req)
}

func (r *recordingEngine) Subscribe(ctx context.Context, req *mbp.SubscribeRequest) (*mbp.SubscribeResponse, error) {
	r.note("Subscribe")
	return r.EngineAPI.Subscribe(ctx, req)
}

func (r *recordingEngine) Unsubscribe(ctx context.Context, subID string) error {
	r.note("Unsubscribe")
	return r.EngineAPI.Unsubscribe(ctx, subID)
}

func (r *recordingEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	r.note("Link")
	return r.EngineAPI.Link(ctx, req)
}

func (r *recordingEngine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	r.note("Forget")
	return r.EngineAPI.Forget(ctx, req)
}

func (r *recordingEngine) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	r.note("Stat")
	return r.EngineAPI.Stat(ctx, req)
}

// matrixEnv is one running server with its engine and auth store.
type matrixEnv struct {
	eng   *engine.Engine
	rec   *recordingEngine
	store *auth.Store
	srv   *mbp.Server
	addr  string
}

// newMatrixEnv starts a real MBP server on an ephemeral loopback port, backed
// by a real engine and auth store on an in-memory filesystem.
func newMatrixEnv(t *testing.T) *matrixEnv {
	t.Helper()
	return newMatrixEnvWith(t, nil)
}

// newMatrixEnvWith is newMatrixEnv with the server handed to configure before
// it serves anything.
func newMatrixEnvWith(t *testing.T, configure func(*mbp.Server)) *matrixEnv {
	t.Helper()
	opts := storage.DefaultOptions()
	opts.FS = vfs.NewMem()
	db, err := storage.OpenPebble("mbp-mode-matrix", opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	authStore := auth.NewStore(db)
	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		AuthStore:        authStore,
		FTSIndex:         ftsIdx,
		ActivationEngine: activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder),
		TriggerSystem:    trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder),
		Embedder:         embedder,
	})
	t.Cleanup(func() {
		eng.Stop()
		store.Close()
	})

	// The test binds port 0 itself and hands the listener to the server, so the
	// address is known before anything is served and whatever answers on it is
	// this server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	rec := &recordingEngine{EngineAPI: eng}
	srv := mbp.NewServer(addr, rec, authStore, nil)
	if configure != nil {
		configure(srv)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeListener(context.Background(), ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-served:
		case <-ctx.Done():
		}
	})
	return &matrixEnv{eng: eng, rec: rec, store: authStore, srv: srv, addr: addr}
}

// ---------------------------------------------------------------------------
// A minimal MBP client.
// ---------------------------------------------------------------------------

type mbpClient struct {
	t    *testing.T
	conn net.Conn
	next uint64
}

// dialMBP connects and completes the HELLO handshake. An empty token is a
// credential-less session (auth_method "none").
func dialMBP(t *testing.T, addr, token, vault string) *mbpClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	hello := mbp.HelloRequest{Version: "1", AuthMethod: "none", Vault: vault}
	if token != "" {
		hello.AuthMethod = "token"
		hello.Token = token
	}
	payload, err := mbp.EncodeMsgpack(&hello)
	if err != nil {
		t.Fatalf("encode HELLO: %v", err)
	}
	if err := mbp.WriteFrame(conn, &mbp.Frame{Version: 0x01, Type: mbp.TypeHello, Payload: payload}); err != nil {
		t.Fatalf("write HELLO: %v", err)
	}
	f, err := mbp.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read HELLO response: %v", err)
	}
	if f.Type != mbp.TypeHelloOK {
		var ep mbp.ErrorPayload
		_ = mbp.DecodeMsgpack(f.Payload, &ep)
		t.Fatalf("HELLO refused: frame 0x%02x, %q", f.Type, ep.Message)
	}
	return &mbpClient{t: t, conn: conn}
}

// reply is one response frame, with the error payload decoded when it is one.
type reply struct {
	frameType uint8
	payload   []byte
	code      mbp.ErrorCode
	message   string
}

func (r reply) isError() bool { return r.frameType == mbp.TypeError }

// send writes one frame with a raw payload and reads the one frame answering it.
func (c *mbpClient) send(frameType uint8, payload []byte) reply {
	c.t.Helper()
	return c.sendFlags(frameType, 0, payload)
}

// sendFlags is send with the frame's flag bits set by the caller.
func (c *mbpClient) sendFlags(frameType uint8, flags uint16, payload []byte) reply {
	c.t.Helper()
	c.next++
	if err := mbp.WriteFrame(c.conn, &mbp.Frame{Version: 0x01, Type: frameType, Flags: flags, CorrelationID: c.next, Payload: payload}); err != nil {
		c.t.Fatalf("write frame 0x%02x: %v", frameType, err)
	}
	f, err := mbp.ReadFrame(c.conn)
	if err != nil {
		c.t.Fatalf("read the answer to frame 0x%02x: %v", frameType, err)
	}
	if f.CorrelationID != c.next {
		c.t.Fatalf("answer to frame 0x%02x carries correlation id %d, want %d", frameType, f.CorrelationID, c.next)
	}
	out := reply{frameType: f.Type, payload: f.Payload}
	if f.Type == mbp.TypeError {
		var ep mbp.ErrorPayload
		if err := mbp.DecodeMsgpack(f.Payload, &ep); err != nil {
			c.t.Fatalf("decode error payload: %v", err)
		}
		out.code, out.message = ep.Code, ep.Message
	}
	return out
}

func (c *mbpClient) request(frameType uint8, req any) reply {
	c.t.Helper()
	payload, err := mbp.EncodeMsgpack(req)
	if err != nil {
		c.t.Fatalf("encode request for frame 0x%02x: %v", frameType, err)
	}
	return c.send(frameType, payload)
}

// ---------------------------------------------------------------------------
// The operations of the matrix.
// ---------------------------------------------------------------------------

// fullCtx is what a full-mode credential's request context looks like to the
// engine. The matrix seeds and inspects state with it, directly on the engine,
// so no cell depends on another cell's frame having worked.
func fullCtx() context.Context {
	return context.WithValue(context.Background(), auth.ContextMode, auth.ModeFull)
}

// cell is the state one (principal, operation) cell works on.
type cell struct {
	t     *testing.T
	env   *matrixEnv
	vault string
	n     int // unique per cell, so no two cells store identical content

	memory, other string // ids of memories seeded for this cell
	content       string // what `memory` holds
	subID         string
}

var cellSeq struct {
	sync.Mutex
	n int
}

func newCell(t *testing.T, env *matrixEnv, vault string) *cell {
	cellSeq.Lock()
	cellSeq.n++
	n := cellSeq.n
	cellSeq.Unlock()
	return &cell{t: t, env: env, vault: vault, n: n}
}

// seed stores one memory directly on the engine and returns its id.
func (c *cell) seed(req *mbp.WriteRequest) string {
	c.t.Helper()
	req.Vault = c.vault
	resp, err := c.env.eng.Write(fullCtx(), req)
	if err != nil {
		c.t.Fatalf("seed a memory: %v", err)
	}
	return resp.ID
}

func (c *cell) seedMemory() {
	c.t.Helper()
	c.content = "Berth " + strconv.Itoa(c.n) + " is closed while the fender piles are replaced."
	c.memory = c.seed(&mbp.WriteRequest{Concept: "Berth closure " + strconv.Itoa(c.n), Content: c.content})
}

func (c *cell) seedPair() {
	c.t.Helper()
	c.seedMemory()
	c.other = c.seed(&mbp.WriteRequest{
		Concept: "Tug roster " + strconv.Itoa(c.n),
		Content: "Two tugs are rostered for every movement at berth " + strconv.Itoa(c.n) + ".",
	})
}

func (c *cell) upsertKey() string { return "standing-order/" + strconv.Itoa(c.n) }

// read loads a memory directly from the engine.
func (c *cell) read(id string) (*mbp.ReadResponse, error) {
	return c.env.eng.Read(fullCtx(), &mbp.ReadRequest{Vault: c.vault, ID: id, ReadOnly: true})
}

// matrixOp is one row of the matrix: a frame type with a concrete request.
type matrixOp struct {
	name       string
	frame      uint8
	okFrame    uint8  // the frame type that answers it on success
	engineCall string // the EngineAPI method that serves it; "" when none does
	// modifiesExisting marks a request that changes or deletes a memory that
	// already exists. The engine refuses those to an append credential
	// (SEC-15); the transport does not look inside the payload to tell.
	modifiesExisting bool
	// request seeds what the operation needs and returns the request payload.
	request func(c *cell) any
	// check inspects the answer and the stored state. went is true when the
	// operation was expected to be carried out.
	check func(c *cell, r reply, went bool)
}

var matrixOps = []matrixOp{
	{
		name: "PING", frame: mbp.TypePing, okFrame: mbp.TypePong,
		request: func(c *cell) any { return &mbp.PingRequest{Data: "watch " + strconv.Itoa(c.n)} },
		check: func(c *cell, r reply, went bool) {
			var pong mbp.PongResponse
			if went && (mbp.DecodeMsgpack(r.payload, &pong) != nil || pong.Data != "watch "+strconv.Itoa(c.n)) {
				c.t.Errorf("PONG did not echo the ping data: %+v", pong)
			}
		},
	},
	{
		name: "WRITE (create)", frame: mbp.TypeWrite, okFrame: mbp.TypeWriteOK, engineCall: "Write",
		request: func(c *cell) any {
			return &mbp.WriteRequest{
				Vault: c.vault, Concept: "Fog signal " + strconv.Itoa(c.n),
				Content: "The fog signal on the north mole sounds every " + strconv.Itoa(c.n+20) + " seconds.",
			}
		},
		check: func(c *cell, r reply, went bool) {
			if !went {
				return
			}
			var resp mbp.WriteResponse
			if err := mbp.DecodeMsgpack(r.payload, &resp); err != nil || resp.ID == "" {
				c.t.Errorf("WRITE_OK carries no id (err %v)", err)
				return
			}
			if _, err := c.read(resp.ID); err != nil {
				c.t.Errorf("the memory WRITE reported is not stored: %v", err)
			}
		},
	},
	{
		// The same frame type as a create, but the request rewrites an existing
		// memory: upsert under a stable key whose content has changed.
		name: "WRITE (upsert changes an existing memory)", frame: mbp.TypeWrite, okFrame: mbp.TypeWriteOK,
		engineCall: "Write", modifiesExisting: true,
		request: func(c *cell) any {
			c.content = "Standing order " + strconv.Itoa(c.n) + ": vessels over 180 m take two pilots."
			c.memory = c.seed(&mbp.WriteRequest{
				Concept: "Standing order " + strconv.Itoa(c.n), Content: c.content,
				IdempotentID: c.upsertKey(), UpsertMode: true,
			})
			return &mbp.WriteRequest{
				Vault:        c.vault,
				Content:      "Standing order " + strconv.Itoa(c.n) + ": no pilot is required for any vessel.",
				IdempotentID: c.upsertKey(), UpsertMode: true,
			}
		},
		check: func(c *cell, r reply, went bool) {
			var resp mbp.WriteResponse
			if went {
				if err := mbp.DecodeMsgpack(r.payload, &resp); err != nil {
					c.t.Errorf("decode WRITE_OK: %v", err)
				}
				if resp.ID == c.memory {
					c.t.Errorf("the upsert returned the old memory's id; want a successor")
				}
				return
			}
			// Refused: a second upsert with the original content must find the
			// original memory still at the head of its key.
			again, err := c.env.eng.Write(fullCtx(), &mbp.WriteRequest{
				Vault: c.vault, Content: c.content, IdempotentID: c.upsertKey(), UpsertMode: true,
			})
			if err != nil {
				c.t.Errorf("re-read the upsert key: %v", err)
				return
			}
			if again.ID != c.memory {
				c.t.Errorf("the refused upsert still replaced the memory: key now points at %s, was %s", again.ID, c.memory)
			}
		},
	},
	{
		name: "READ", frame: mbp.TypeRead, okFrame: mbp.TypeReadResp, engineCall: "Read",
		request: func(c *cell) any {
			c.seedMemory()
			return &mbp.ReadRequest{Vault: c.vault, ID: c.memory}
		},
		check: func(c *cell, r reply, went bool) {
			if went {
				var resp mbp.ReadResponse
				if err := mbp.DecodeMsgpack(r.payload, &resp); err != nil || resp.Content != c.content {
					c.t.Errorf("READ returned %q (err %v); want the stored content", resp.Content, err)
				}
				return
			}
			if strings.Contains(string(r.payload), "fender piles") {
				c.t.Errorf("a refused READ still carried the memory's content")
			}
		},
	},
	{
		name: "ACTIVATE", frame: mbp.TypeActivate, okFrame: mbp.TypeActivateResp, engineCall: "Activate",
		request: func(c *cell) any {
			c.seedMemory()
			return &mbp.ActivateRequest{Vault: c.vault, Context: []string{"berth closed fender piles"}, MaxResults: 5}
		},
		check: func(c *cell, r reply, went bool) {
			if !went && strings.Contains(string(r.payload), "fender piles") {
				c.t.Errorf("a refused ACTIVATE still carried stored content")
			}
		},
	},
	{
		name: "SUBSCRIBE", frame: mbp.TypeSubscribe, okFrame: mbp.TypeSubOK, engineCall: "Subscribe",
		request: func(c *cell) any {
			return &mbp.SubscribeRequest{Vault: c.vault, Context: []string{"berth closures"}, TTL: 60}
		},
		check: func(c *cell, r reply, went bool) {
			if !went {
				return
			}
			var resp mbp.SubscribeResponse
			if err := mbp.DecodeMsgpack(r.payload, &resp); err != nil || resp.SubID == "" {
				c.t.Errorf("SUB_OK carries no subscription id (err %v)", err)
				return
			}
			_ = c.env.eng.Unsubscribe(fullCtx(), resp.SubID)
		},
	},
	{
		name: "UNSUB", frame: mbp.TypeUnsub, okFrame: mbp.TypeUnsubOK, engineCall: "Unsubscribe",
		request: func(c *cell) any {
			sub, err := c.env.eng.Subscribe(fullCtx(), &mbp.SubscribeRequest{Vault: c.vault, Context: []string{"tug roster"}, TTL: 60})
			if err != nil {
				c.t.Fatalf("seed a subscription: %v", err)
			}
			c.subID = sub.SubID
			c.t.Cleanup(func() { _ = c.env.eng.Unsubscribe(fullCtx(), sub.SubID) })
			return &mbp.UnsubscribeRequest{SubID: c.subID}
		},
		check: func(*cell, reply, bool) {},
	},
	{
		name: "LINK", frame: mbp.TypeLink, okFrame: mbp.TypeLinkOK, engineCall: "Link", modifiesExisting: true,
		request: func(c *cell) any {
			c.seedPair()
			return &mbp.LinkRequest{
				Vault: c.vault, SourceID: c.memory, TargetID: c.other,
				RelType: uint16(storage.RelContradicts), Weight: 1,
			}
		},
		check: func(c *cell, r reply, went bool) {
			assocs, err := c.env.eng.GetAssociations(fullCtx(), c.vault, c.memory, 50)
			if err != nil {
				c.t.Errorf("list associations: %v", err)
				return
			}
			linked := false
			for _, a := range assocs {
				if a.TargetID.String() == c.other && a.RelType == storage.RelContradicts {
					linked = true
				}
			}
			if linked != went {
				c.t.Errorf("contradicts link stored = %v; want %v", linked, went)
			}
		},
	},
	{
		name: "FORGET (soft)", frame: mbp.TypeForget, okFrame: mbp.TypeForgetOK, engineCall: "Forget", modifiesExisting: true,
		request: func(c *cell) any {
			c.seedMemory()
			return &mbp.ForgetRequest{Vault: c.vault, ID: c.memory}
		},
		check: func(c *cell, r reply, went bool) {
			got, err := c.read(c.memory)
			deleted := err != nil || got.State == uint8(storage.StateSoftDeleted)
			if deleted != went {
				c.t.Errorf("memory soft-deleted = %v (read err %v); want %v", deleted, err, went)
			}
		},
	},
	{
		name: "FORGET (hard)", frame: mbp.TypeForget, okFrame: mbp.TypeForgetOK, engineCall: "Forget", modifiesExisting: true,
		request: func(c *cell) any {
			c.seedMemory()
			return &mbp.ForgetRequest{Vault: c.vault, ID: c.memory, Hard: true}
		},
		check: func(c *cell, r reply, went bool) {
			_, err := c.read(c.memory)
			gone := err != nil
			if gone != went {
				c.t.Errorf("memory erased = %v; want %v", gone, went)
			}
		},
	},
	{
		name: "STAT", frame: mbp.TypeStat, okFrame: mbp.TypeStatResp, engineCall: "Stat",
		request: func(c *cell) any { return &mbp.StatRequest{Vault: c.vault} },
		check:   func(*cell, reply, bool) {},
	},
}

// reads and mutates report what a class does, from the test's own table.
func classReads(class string) bool   { return class == classRead || class == classMutateAndRead }
func classMutates(class string) bool { return class == classMutate || class == classMutateAndRead }

// TestCredentialModeMatrix pins what each kind of MBP session may do, for every
// frame type the server dispatches, over a real TCP connection to a real
// engine:
//
//   - observe: every mutating frame is refused. Before this gate an observe
//     key could store a memory, rewrite one through an upsert, link two as
//     contradicting, and soft- or hard-delete one.
//   - write (ingest-only): every reading frame is refused. Before this gate an
//     ingest-only key could read a memory by id, run recall, read vault
//     statistics, and open or close subscriptions.
//   - append: the transport refuses nothing; the engine refuses every request
//     that changes or deletes an existing memory (SEC-15), and creating one
//     goes through.
//   - full, and a session with no key on a public vault (which is full access
//     by design): nothing is refused.
//
// A transport refusal is the protocol's auth-failure error frame, the engine
// is never called, and the answer is the same whether or not the target
// exists: the gate runs before the payload is decoded.
func TestCredentialModeMatrix(t *testing.T) {
	env := newMatrixEnv(t)

	// The keyed principals share one locked vault; the credential-less one
	// uses a public vault, the only kind it can reach.
	const lockedVault, publicVault = "pilot-station", "harbour-notices"
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: lockedVault, Public: false}); err != nil {
		t.Fatalf("configure %s: %v", lockedVault, err)
	}
	if err := env.store.SetVaultConfig(auth.VaultConfig{Name: publicVault, Public: true}); err != nil {
		t.Fatalf("configure %s: %v", publicVault, err)
	}

	type principal struct {
		name  string
		mode  string // "" = no key
		vault string
	}
	principals := []principal{
		{"full", auth.ModeFull, lockedVault},
		{"write", auth.ModeWrite, lockedVault},
		{"append", auth.ModeAppend, lockedVault},
		{"observe", auth.ModeObserve, lockedVault},
		{"no key, public vault", "", publicVault},
	}

	// Every classified frame type must have at least one row.
	covered := map[uint8]bool{}
	for _, op := range matrixOps {
		covered[op.frame] = true
	}
	for frame := range wantFrameClasses {
		if !covered[frame] {
			t.Errorf("frame 0x%02x has a class but no row in matrixOps: add one", frame)
		}
	}

	for _, p := range principals {
		t.Run(p.name, func(t *testing.T) {
			token := ""
			if p.mode != "" {
				var err error
				token, _, err = env.store.GenerateAPIKey(p.vault, "matrix-"+p.mode, p.mode, nil)
				if err != nil {
					t.Fatalf("GenerateAPIKey(%s): %v", p.mode, err)
				}
			}
			client := dialMBP(t, env.addr, token, p.vault)

			for _, op := range matrixOps {
				t.Run(op.name, func(t *testing.T) {
					class, ok := wantFrameClasses[op.frame]
					if !ok {
						t.Fatalf("frame 0x%02x has a row but no class in wantFrameClasses", op.frame)
					}
					c := newCell(t, env, p.vault)
					req := op.request(c)

					// What should happen.
					wantTransportRefusal := ""
					switch {
					case p.mode == auth.ModeWrite && classReads(class):
						wantTransportRefusal = writeOnlyMsg
					case p.mode == auth.ModeObserve && classMutates(class):
						wantTransportRefusal = readOnlyMsg
					}
					wantEngineRefusal := wantTransportRefusal == "" && p.mode == auth.ModeAppend && op.modifiesExisting
					went := wantTransportRefusal == "" && !wantEngineRefusal

					env.rec.take()
					r := client.request(op.frame, req)
					reached := env.rec.take()
					if r.isError() {
						t.Logf("answer: error %d %q; engine calls %v", r.code, r.message, reached)
					} else {
						t.Logf("answer: frame 0x%02X; engine calls %v", r.frameType, reached)
					}

					switch {
					case wantTransportRefusal != "":
						if len(reached) != 0 {
							t.Errorf("the frame reached the engine (%v); want it refused before the engine with %q", reached, wantTransportRefusal)
						}
						if !r.isError() {
							t.Errorf("answered with frame 0x%02x; want an error frame", r.frameType)
						} else if r.code != mbp.ErrAuthFailed || r.message != wantTransportRefusal {
							t.Errorf("error = %d %q; want %d %q", r.code, r.message, mbp.ErrAuthFailed, wantTransportRefusal)
						}
						// The refusal depends on the frame type and the session
						// alone. The same frame type with no payload at all gets
						// the identical answer, so a refusal says nothing about
						// the request or whether its target exists.
						bare := client.send(op.frame, nil)
						if bare.code != r.code || bare.message != r.message {
							t.Errorf("with an empty payload the answer is %d %q; want the same refusal, %d %q", bare.code, bare.message, r.code, r.message)
						}
						if again := env.rec.take(); len(again) != 0 {
							t.Errorf("the empty-payload frame reached the engine (%v)", again)
						}
					case wantEngineRefusal:
						if !r.isError() || !strings.Contains(r.message, appendMsg) {
							t.Errorf("answered with frame 0x%02x %q; want the engine's append refusal", r.frameType, r.message)
						}
						if strings.Join(reached, ",") != op.engineCall {
							t.Errorf("engine calls = %v; want the transport to pass the frame on to %s, which refuses it", reached, op.engineCall)
						}
					default:
						if r.isError() {
							t.Errorf("refused with %d %q; want frame 0x%02x", r.code, r.message, op.okFrame)
						} else if r.frameType != op.okFrame {
							t.Errorf("answered with frame 0x%02x; want 0x%02x", r.frameType, op.okFrame)
						}
						want := []string{}
						if op.engineCall != "" {
							want = []string{op.engineCall}
						}
						if strings.Join(reached, ",") != strings.Join(want, ",") {
							t.Errorf("engine calls = %v; want %v", reached, want)
						}
					}
					op.check(c, r, went)
				})
			}
		})
	}
}

// sortedFrames returns the keys of a frame table in opcode order.
func sortedFrames(m map[uint8]string) []uint8 {
	out := make([]uint8, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestEveryDispatchedFrameHasAModeClass pins the server's route table to
// wantFrameClasses in both directions, so a frame type cannot be added to the
// server without deciding which credential modes may send it, and cannot be
// reclassified without changing the decision recorded here.
func TestEveryDispatchedFrameHasAModeClass(t *testing.T) {
	dispatched := mbp.DispatchedFrameClasses()
	if len(dispatched) == 0 {
		t.Fatal("the server reports no dispatched frame types; this test is looking in the wrong place")
	}
	for _, frame := range sortedFrames(dispatched) {
		got := dispatched[frame]
		want, ok := wantFrameClasses[frame]
		switch {
		case !ok:
			t.Errorf("frame 0x%02X is dispatched (as %q) but has no entry in wantFrameClasses: decide whether it is control, a read or a mutation", frame, got)
		case got != want:
			t.Errorf("frame 0x%02X is dispatched as %q; the recorded decision is %q", frame, got, want)
		}
	}
	for _, frame := range sortedFrames(wantFrameClasses) {
		if _, ok := dispatched[frame]; !ok {
			t.Errorf("wantFrameClasses names frame 0x%02X, which the server no longer dispatches", frame)
		}
	}
}

// TestUnroutedFrameTypesReachNothing sends every one-byte opcode the server
// does not route — a second HELLO, the response opcodes, the whole cluster
// range 0x20-0x41, and everything unassigned — on a full-access connection,
// and checks each is answered with an error and none reaches the engine. The
// cluster opcodes share MBP's framing but are served by the cluster listener,
// behind its own join handshake; the client listener must not act on them.
func TestUnroutedFrameTypesReachNothing(t *testing.T) {
	env := newMatrixEnv(t)
	const vault = "pilot-station"
	token, _, err := env.store.GenerateAPIKey(vault, "sweep", auth.ModeFull, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	client := dialMBP(t, env.addr, token, vault)
	env.rec.take()

	sent := 0
	for op := 0; op <= 0xFF; op++ {
		frame := uint8(op)
		if _, routed := wantFrameClasses[frame]; routed {
			continue
		}
		sent++
		r := client.send(frame, nil)
		if !r.isError() {
			t.Errorf("frame 0x%02X was answered with frame 0x%02X; want an error frame", frame, r.frameType)
		} else if !strings.Contains(r.message, "unknown message type") {
			t.Errorf("frame 0x%02X: error %d %q; want the unknown-message-type error", frame, r.code, r.message)
		}
	}
	if reached := env.rec.take(); len(reached) != 0 {
		t.Errorf("unrouted frame types reached the engine: %v", reached)
	}
	if want := 256 - len(wantFrameClasses); sent != want {
		t.Fatalf("swept %d opcodes; want %d", sent, want)
	}
}
