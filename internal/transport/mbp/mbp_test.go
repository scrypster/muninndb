package mbp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
)

// ---------------------------------------------------------------------------
// stubEngine implements EngineAPI for server integration tests.
// ---------------------------------------------------------------------------

type stubEngine struct {
	writeErr error
}

func (e *stubEngine) Hello(_ context.Context, req *HelloRequest) (*HelloResponse, error) {
	return BuildHelloResponse("sess-1", "default", req.Capabilities), nil
}

func (e *stubEngine) Write(_ context.Context, _ *WriteRequest) (*WriteResponse, error) {
	if e.writeErr != nil {
		return nil, e.writeErr
	}
	return &WriteResponse{ID: "e-1", CreatedAt: 1700000000}, nil
}

func (e *stubEngine) Read(_ context.Context, req *ReadRequest) (*ReadResponse, error) {
	return &ReadResponse{ID: req.ID, Concept: "c", Content: "body"}, nil
}

func (e *stubEngine) Activate(_ context.Context, _ *ActivateRequest) (*ActivateResponse, error) {
	return &ActivateResponse{QueryID: "q1"}, nil
}

func (e *stubEngine) Subscribe(_ context.Context, _ *SubscribeRequest) (*SubscribeResponse, error) {
	return &SubscribeResponse{SubID: "s1", Status: "active"}, nil
}

func (e *stubEngine) Unsubscribe(_ context.Context, _ string) error { return nil }

func (e *stubEngine) Link(_ context.Context, _ *LinkRequest) (*LinkResponse, error) {
	return &LinkResponse{OK: true}, nil
}

func (e *stubEngine) Forget(_ context.Context, _ *ForgetRequest) (*ForgetResponse, error) {
	return &ForgetResponse{OK: true}, nil
}

func (e *stubEngine) Stat(_ context.Context, _ *StatRequest) (*StatResponse, error) {
	return &StatResponse{EngramCount: 42, VaultCount: 1}, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestServer builds a server whose default vault is public, so the existing
// dispatch tests (which handshake with auth_method "none" and use the default
// vault) operate without an API key. Vault-isolation behaviour is covered
// separately in vault_scope_test.go.
func newTestServer(t *testing.T, eng EngineAPI) *Server {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		panic("open test auth db: " + err.Error())
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(db)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		panic("set default vault config: " + err.Error())
	}
	return &Server{engine: eng, authStore: store, shutdown: make(chan struct{})}
}

func startTestConn(t *testing.T, s *Server) (client net.Conn, wait func()) {
	t.Helper()
	c, srv := net.Pipe()
	dl := time.Now().Add(5 * time.Second)
	c.SetDeadline(dl)
	srv.SetDeadline(dl)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConnection(context.Background(), srv)
	}()
	return c, func() { c.Close(); <-done }
}

func doHandshake(t *testing.T, c net.Conn) {
	t.Helper()
	req := HelloRequest{Version: "1", AuthMethod: "none"}
	p, err := EncodeMsgpack(&req)
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := WriteFrame(c, &Frame{Version: 0x01, Type: TypeHello, Payload: p}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	f, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read hello_ok: %v", err)
	}
	if f.Type != TypeHelloOK {
		t.Fatalf("expected TypeHelloOK (0x%02x), got 0x%02x", TypeHelloOK, f.Type)
	}
}

func sendAndReceive(t *testing.T, c net.Conn, ftype uint8, corrID uint64, req interface{}) *Frame {
	t.Helper()
	p, err := EncodeMsgpack(req)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: ftype, CorrelationID: corrID, Payload: p,
	}); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	f, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return f
}

// ---------------------------------------------------------------------------
// codec.go
// ---------------------------------------------------------------------------

func TestCodec_RoundTrip(t *testing.T) {
	type sample struct {
		Name  string `msgpack:"name"`
		Count int    `msgpack:"count"`
	}
	orig := sample{Name: "test", Count: 42}
	data, err := EncodeMsgpack(&orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got sample
	if err := DecodeMsgpack(data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != orig {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestDecodeMsgpack_InvalidData(t *testing.T) {
	var out struct{ X int }
	err := DecodeMsgpack([]byte{0xFF, 0xFE, 0xFD}, &out)
	if err == nil {
		t.Fatal("expected error for garbage input")
	}
	if !strings.Contains(err.Error(), "msgpack decode") {
		t.Errorf("error should wrap with 'msgpack decode': %v", err)
	}
}

// ---------------------------------------------------------------------------
// hello.go
// ---------------------------------------------------------------------------

func TestValidateHelloRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     HelloRequest
		wantErr string
	}{
		{"valid none", HelloRequest{Version: "1", AuthMethod: "none"}, ""},
		{"valid token", HelloRequest{Version: "1", AuthMethod: "token", Token: "s"}, ""},
		{"empty auth defaults none", HelloRequest{Version: "1"}, ""},
		{"bad version", HelloRequest{Version: "2"}, "invalid version"},
		{"empty version", HelloRequest{}, "invalid version"},
		{"bad auth method", HelloRequest{Version: "1", AuthMethod: "kerberos"}, "invalid auth_method"},
		{"token missing", HelloRequest{Version: "1", AuthMethod: "token"}, "token required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateHelloRequest(&tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got err=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNegotiateCapabilities(t *testing.T) {
	tests := []struct {
		name    string
		client  []string
		wantLen int
		wantAll []string
	}{
		{"full", []string{"compression", "streaming", "subscriptions"}, 3, []string{"compression", "streaming", "subscriptions"}},
		{"partial", []string{"compression"}, 1, []string{"compression"}},
		{"none matching", []string{"encryption"}, 0, nil},
		{"nil client", nil, 0, nil},
		{"empty slice", []string{}, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NegotiateCapabilities(tt.client)
			if len(got) != tt.wantLen {
				t.Fatalf("len: got %d, want %d (%v)", len(got), tt.wantLen, got)
			}
			for i, w := range tt.wantAll {
				if got[i] != w {
					t.Errorf("[%d]: got %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestBuildHelloResponse(t *testing.T) {
	resp := BuildHelloResponse("s1", "v1", []string{"compression"})
	if resp.ServerVersion != "1.0.0" {
		t.Errorf("version: %q", resp.ServerVersion)
	}
	if resp.SessionID != "s1" || resp.VaultID != "v1" {
		t.Errorf("ids: sess=%q vault=%q", resp.SessionID, resp.VaultID)
	}
	if len(resp.Capabilities) != 1 || resp.Capabilities[0] != "compression" {
		t.Errorf("caps: %v", resp.Capabilities)
	}
	if resp.Limits != ServerLimits {
		t.Errorf("limits mismatch")
	}
}

func TestBuildErrorPayload_Variants(t *testing.T) {
	ep1 := BuildErrorPayload(ErrStorageError, "disk full")
	if ep1.Code != ErrStorageError || ep1.Message != "disk full" || ep1.RequestID != "" {
		t.Errorf("basic: %+v", ep1)
	}

	ep2 := BuildErrorPayloadWithID(ErrEngramNotFound, "gone", "r1")
	if ep2.Code != ErrEngramNotFound || ep2.Message != "gone" || ep2.RequestID != "r1" {
		t.Errorf("withID: %+v", ep2)
	}

	ep3 := BuildErrorPayloadWithRetry(ErrRateLimited, "slow", 30)
	if ep3.Code != ErrRateLimited || ep3.Message != "slow" || ep3.RetryAfter != 30 {
		t.Errorf("withRetry: %+v", ep3)
	}
}

// ---------------------------------------------------------------------------
// errors.go
// ---------------------------------------------------------------------------

func TestErrorCodeMessage_AllCodes(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want string
	}{
		{ErrOK, "OK"},
		{ErrEngramNotFound, "engram not found"},
		{ErrVaultNotFound, "vault not found"},
		{ErrInvalidEngram, "invalid engram"},
		{ErrIdempotencyViolation, "idempotency violation"},
		{ErrInvalidAssociation, "invalid association"},
		{ErrSubscriptionNotFound, "subscription not found"},
		{ErrThresholdInvalid, "threshold invalid"},
		{ErrHopDepthExceeded, "hop depth exceeded"},
		{ErrWeightsInvalid, "weights invalid"},
		{ErrAuthFailed, "auth failed"},
		{ErrVaultForbidden, "vault forbidden"},
		{ErrRateLimited, "rate limited"},
		{ErrMaxResultsExceeded, "max results exceeded"},
		{ErrStorageError, "storage error"},
		{ErrIndexError, "index error"},
		{ErrEnrichmentError, "enrichment error"},
		{ErrShardUnavailable, "shard unavailable"},
		{ErrInternal, "internal error"},
		{ErrorCode(9999), "unknown error"},
	}
	for _, tc := range cases {
		got := ErrorCodeMessage(tc.code)
		if got != tc.want {
			t.Errorf("ErrorCodeMessage(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// compress.go
// ---------------------------------------------------------------------------

func TestCompressPayload_BelowThreshold(t *testing.T) {
	data := bytes.Repeat([]byte("x"), CompressionThreshold)
	out, compressed, err := CompressPayload(data)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if compressed {
		t.Error("should not compress at threshold")
	}
	if !bytes.Equal(out, data) {
		t.Error("data should be unchanged")
	}
}

func TestCompressPayload_AboveThreshold(t *testing.T) {
	data := bytes.Repeat([]byte("x"), CompressionThreshold+500)
	out, compressed, err := CompressPayload(data)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !compressed {
		t.Error("expected compression for repetitive data above threshold")
	}
	if len(out) >= len(data) {
		t.Errorf("compressed %d should be smaller than original %d", len(out), len(data))
	}
}

// ---------------------------------------------------------------------------
// frame.go – edge cases beyond existing frame_test.go
// ---------------------------------------------------------------------------

func TestReadFrame_OversizedPayloadHeader(t *testing.T) {
	var buf bytes.Buffer
	h := make([]byte, FramePrefixSize)
	h[0] = 0x01
	h[1] = TypeWrite
	binary.BigEndian.PutUint32(h[4:8], MaxPayloadSize+1)
	buf.Write(h)

	_, err := ReadFrame(&buf)
	if err != ErrPayloadTooLarge {
		t.Errorf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestReadFrame_TruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	h := make([]byte, FramePrefixSize)
	h[0] = 0x01
	h[1] = TypeWrite
	binary.BigEndian.PutUint32(h[4:8], 100)
	buf.Write(h)
	buf.Write([]byte("short"))

	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

func TestReadFrame_EmptyReader(t *testing.T) {
	_, err := ReadFrame(bytes.NewReader(nil))
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestWriteReadFrame_AllTypes(t *testing.T) {
	allTypes := []uint8{
		TypeHello, TypeHelloOK, TypeWrite, TypeWriteOK,
		TypeRead, TypeReadResp, TypeActivate, TypeActivateResp,
		TypeSubscribe, TypeSubOK, TypeActivation,
		TypeLink, TypeLinkOK, TypeForget, TypeForgetOK,
		TypeUnsub, TypeUnsubOK, TypeStat, TypeStatResp,
		TypePing, TypePong, TypeError,
		TypeReplEntry, TypeReplBatch, TypeReplAck, TypeReplNack,
		TypeSnapHeader, TypeSnapChunk, TypeSnapAck, TypeSnapComplete,
		TypeCogForward, TypeCogAck,
		TypeVoteRequest, TypeVoteResponse, TypeCortexClaim,
		TypeSDown, TypeODown, TypeGossip,
		TypeJoinRequest, TypeJoinResponse, TypeLeave,
		TypeHandoff, TypeHandoffAck,
		TypeCCSProbe, TypeCCSResponse,
		TypeReconProbe, TypeReconReply, TypeReconSync, TypeReconAck,
	}
	for _, ft := range allTypes {
		var buf bytes.Buffer
		f := &Frame{Version: 0x01, Type: ft, CorrelationID: uint64(ft), Payload: []byte{0xAA}}
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("type 0x%02x write: %v", ft, err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("type 0x%02x read: %v", ft, err)
		}
		if got.Type != ft {
			t.Errorf("type 0x%02x: got 0x%02x", ft, got.Type)
		}
		if got.CorrelationID != uint64(ft) {
			t.Errorf("type 0x%02x: corrID got %d", ft, got.CorrelationID)
		}
	}
}

func TestMultipleFrames_NetPipe(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	dl := time.Now().Add(5 * time.Second)
	c.SetDeadline(dl)
	s.SetDeadline(dl)

	frames := []*Frame{
		{Version: 0x01, Type: TypePing, CorrelationID: 1, Payload: []byte("a")},
		{Version: 0x01, Type: TypePong, CorrelationID: 2, Payload: []byte("bb")},
		{Version: 0x01, Type: TypeWrite, CorrelationID: 3, Payload: []byte("ccc")},
	}

	done := make(chan error, 1)
	go func() {
		for _, f := range frames {
			if err := WriteFrame(c, f); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for i, want := range frames {
		got, err := ReadFrame(s)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Type != want.Type || got.CorrelationID != want.CorrelationID {
			t.Errorf("frame %d: type/corrID mismatch", i)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("frame %d: payload mismatch", i)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("writer: %v", err)
	}
}

func TestFrame_FlagPreservation(t *testing.T) {
	flags := []uint16{
		FlagCompressed,
		FlagStreaming,
		FlagLastFrame,
		FlagVault,
		FlagUrgent,
		FlagCompressed | FlagStreaming | FlagLastFrame | FlagVault | FlagUrgent,
	}
	for _, fl := range flags {
		var buf bytes.Buffer
		f := &Frame{Version: 0x01, Type: TypePing, Flags: fl, Payload: []byte{}}
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("flags 0x%04x write: %v", fl, err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("flags 0x%04x read: %v", fl, err)
		}
		if got.Flags != fl {
			t.Errorf("flags: got 0x%04x, want 0x%04x", got.Flags, fl)
		}
	}
}

func TestWriteFrame_SetsPayloadLength(t *testing.T) {
	var buf bytes.Buffer
	f := &Frame{Version: 0x01, Type: TypePing, PayloadLength: 999, Payload: []byte("hello")}
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.PayloadLength != 5 {
		t.Errorf("PayloadLength: got %d, want 5", got.PayloadLength)
	}
}

// ---------------------------------------------------------------------------
// cluster_frames.go – types missing from cluster_frames_test.go
// ---------------------------------------------------------------------------

func TestRoundTrip_HandoffMessage(t *testing.T) {
	orig := HandoffMessage{TargetID: "n2", Epoch: 5, CortexSeq: 100}
	var got HandoffMessage
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_HandoffAck(t *testing.T) {
	orig := HandoffAck{TargetID: "n2", Epoch: 5, Success: true}
	var got HandoffAck
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_CCSProbeMsg(t *testing.T) {
	orig := CCSProbeMsg{
		RequestID:   "ccs-1",
		SampledKeys: [][16]byte{{1}, {2}, {3}},
	}
	var got CCSProbeMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.RequestID != orig.RequestID || len(got.SampledKeys) != len(orig.SampledKeys) {
		t.Errorf("got %+v, want %+v", got, orig)
	}
	for i := range orig.SampledKeys {
		if got.SampledKeys[i] != orig.SampledKeys[i] {
			t.Errorf("SampledKeys[%d] mismatch", i)
		}
	}
}

func TestRoundTrip_CCSResponseMsg(t *testing.T) {
	orig := CCSResponseMsg{
		RequestID: "ccs-1",
		NodeID:    "n3",
		Hash:      []byte{0xAB, 0xCD, 0xEF},
		KeyCount:  10,
	}
	var got CCSResponseMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.RequestID != orig.RequestID || got.NodeID != orig.NodeID || got.KeyCount != orig.KeyCount {
		t.Errorf("scalar mismatch: got %+v, want %+v", got, orig)
	}
	if !bytes.Equal(got.Hash, orig.Hash) {
		t.Errorf("Hash mismatch")
	}
}

func TestRoundTrip_ReconProbeMsg(t *testing.T) {
	orig := ReconProbeMsg{
		RequestID: "r-1",
		Keys:      [][16]byte{{1}, {2}},
		Weights:   []float64{0.5, 0.8},
	}
	var got ReconProbeMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.RequestID != orig.RequestID {
		t.Errorf("RequestID mismatch")
	}
	if len(got.Keys) != 2 || len(got.Weights) != 2 {
		t.Fatalf("lengths: keys=%d weights=%d", len(got.Keys), len(got.Weights))
	}
	for i := range orig.Keys {
		if got.Keys[i] != orig.Keys[i] {
			t.Errorf("Keys[%d] mismatch", i)
		}
	}
	for i := range orig.Weights {
		if got.Weights[i] != orig.Weights[i] {
			t.Errorf("Weights[%d] mismatch", i)
		}
	}
}

func TestRoundTrip_ReconReplyMsg(t *testing.T) {
	orig := ReconReplyMsg{RequestID: "r-1", NodeID: "n2", Weights: []float64{0.5, 0.0}}
	var got ReconReplyMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.RequestID != orig.RequestID || got.NodeID != orig.NodeID {
		t.Errorf("scalar mismatch")
	}
	if len(got.Weights) != 2 {
		t.Fatalf("weights len: %d", len(got.Weights))
	}
	for i := range orig.Weights {
		if got.Weights[i] != orig.Weights[i] {
			t.Errorf("Weights[%d]: got %f, want %f", i, got.Weights[i], orig.Weights[i])
		}
	}
}

func TestRoundTrip_ReconSyncMsg(t *testing.T) {
	orig := ReconSyncMsg{
		RequestID: "r-1",
		Keys:      [][16]byte{{1}},
		Weights:   []float64{0.9},
	}
	var got ReconSyncMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got.RequestID != orig.RequestID || len(got.Keys) != 1 || got.Weights[0] != 0.9 {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestRoundTrip_ReconAckMsg(t *testing.T) {
	orig := ReconAckMsg{RequestID: "r-1", NodeID: "n2", Applied: 5}
	var got ReconAckMsg
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestRoundTrip_ErrorPayload(t *testing.T) {
	orig := ErrorPayload{Code: ErrRateLimited, Message: "slow down", RequestID: "r1", RetryAfter: 10}
	var got ErrorPayload
	mustUnmarshal(t, mustMarshal(t, orig), &got)
	if got != orig {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

// ---------------------------------------------------------------------------
// protocol_version.go
// ---------------------------------------------------------------------------

func TestProtocolVersionConstants(t *testing.T) {
	if CurrentProtocolVersion == 0 {
		t.Error("CurrentProtocolVersion should be > 0")
	}
	if MinSupportedProtocolVersion > CurrentProtocolVersion {
		t.Error("Min > Current")
	}
	if DeprecatedProtocolVersion > MinSupportedProtocolVersion {
		t.Error("Deprecated > Min")
	}
}

// ---------------------------------------------------------------------------
// server.go – integration tests via net.Pipe()
// ---------------------------------------------------------------------------

func TestServer_DispatchAllHandlers(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	cases := []struct {
		name     string
		sendType uint8
		wantType uint8
		req      interface{}
	}{
		{"ping", TypePing, TypePong, &PingRequest{Data: "x"}},
		{"write", TypeWrite, TypeWriteOK, &WriteRequest{Concept: "c"}},
		{"read", TypeRead, TypeReadResp, &ReadRequest{ID: "e-1"}},
		{"activate", TypeActivate, TypeActivateResp, &ActivateRequest{Context: []string{"q"}}},
		{"subscribe", TypeSubscribe, TypeSubOK, &SubscribeRequest{Context: []string{"q"}}},
		{"unsub", TypeUnsub, TypeUnsubOK, &UnsubscribeRequest{SubID: "s-1"}},
		{"link", TypeLink, TypeLinkOK, &LinkRequest{SourceID: "a", TargetID: "b"}},
		{"forget", TypeForget, TypeForgetOK, &ForgetRequest{ID: "e-1"}},
		{"stat", TypeStat, TypeStatResp, &StatRequest{}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := sendAndReceive(t, c, tc.sendType, uint64(i+1), tc.req)
			if resp.Type != tc.wantType {
				t.Errorf("expected 0x%02x, got 0x%02x", tc.wantType, resp.Type)
			}
			if resp.CorrelationID != uint64(i+1) {
				t.Errorf("corrID: got %d, want %d", resp.CorrelationID, i+1)
			}
		})
	}
}

func TestServer_PingEchoData(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	resp := sendAndReceive(t, c, TypePing, 1, &PingRequest{Data: "echo-test"})
	if resp.Type != TypePong {
		t.Fatalf("expected TypePong, got 0x%02x", resp.Type)
	}
	var pong PongResponse
	if err := DecodeMsgpack(resp.Payload, &pong); err != nil {
		t.Fatalf("decode pong: %v", err)
	}
	if pong.Data != "echo-test" {
		t.Errorf("pong data: got %q, want %q", pong.Data, "echo-test")
	}
}

func TestServer_EngineError(t *testing.T) {
	s := newTestServer(t, &stubEngine{writeErr: fmt.Errorf("disk full")})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	resp := sendAndReceive(t, c, TypeWrite, 1, &WriteRequest{Concept: "c"})
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
	var ep ErrorPayload
	if err := DecodeMsgpack(resp.Payload, &ep); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if ep.Code != ErrStorageError {
		t.Errorf("code: got %d, want %d", ep.Code, ErrStorageError)
	}
	if !strings.Contains(ep.Message, "disk full") {
		t.Errorf("message: %q", ep.Message)
	}
}

func TestServer_InvalidPayload(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: TypeWrite, CorrelationID: 1, Payload: []byte{0xFF, 0xFE},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
}

func TestServer_UnknownFrameType(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: 0xEE, CorrelationID: 1, Payload: []byte{},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
	var ep ErrorPayload
	if err := DecodeMsgpack(resp.Payload, &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(ep.Message, "unknown message type") {
		t.Errorf("message: %q", ep.Message)
	}
}

func TestServer_NonHelloFirst(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()

	p, _ := EncodeMsgpack(&WriteRequest{Concept: "c"})
	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: TypeWrite, Payload: p,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
	var ep ErrorPayload
	if err := DecodeMsgpack(resp.Payload, &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(ep.Message, "expected HELLO") {
		t.Errorf("message: %q", ep.Message)
	}
}

func TestServer_InvalidHelloPayload(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()

	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: TypeHello, Payload: []byte{0xFF},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
}

func TestServer_HelloBadVersion(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()

	req := HelloRequest{Version: "99", AuthMethod: "none"}
	p, _ := EncodeMsgpack(&req)
	if err := WriteFrame(c, &Frame{
		Version: 0x01, Type: TypeHello, Payload: p,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := ReadFrame(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != TypeError {
		t.Fatalf("expected TypeError, got 0x%02x", resp.Type)
	}
	var ep ErrorPayload
	if err := DecodeMsgpack(resp.Payload, &ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.Code != ErrAuthFailed {
		t.Errorf("code: got %d, want %d", ep.Code, ErrAuthFailed)
	}
}

func TestServer_WriteResponsePayload(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	resp := sendAndReceive(t, c, TypeWrite, 1, &WriteRequest{Concept: "c", Content: "body"})
	if resp.Type != TypeWriteOK {
		t.Fatalf("expected TypeWriteOK, got 0x%02x", resp.Type)
	}
	var wr WriteResponse
	if err := DecodeMsgpack(resp.Payload, &wr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if wr.ID != "e-1" {
		t.Errorf("ID: got %q, want %q", wr.ID, "e-1")
	}
	if wr.CreatedAt != 1700000000 {
		t.Errorf("CreatedAt: got %d", wr.CreatedAt)
	}
}

func TestServer_StatResponsePayload(t *testing.T) {
	s := newTestServer(t, &stubEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)

	resp := sendAndReceive(t, c, TypeStat, 1, &StatRequest{})
	if resp.Type != TypeStatResp {
		t.Fatalf("expected TypeStatResp, got 0x%02x", resp.Type)
	}
	var sr StatResponse
	if err := DecodeMsgpack(resp.Payload, &sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sr.EngramCount != 42 {
		t.Errorf("EngramCount: got %d, want 42", sr.EngramCount)
	}
}

// ---------------------------------------------------------------------------
// Full frame + msgpack integration: typed payload through Frame wire format
// ---------------------------------------------------------------------------

func TestFrameWithMsgpackPayload_PingPong(t *testing.T) {
	orig := PingRequest{Data: "integration"}
	payload, err := EncodeMsgpack(&orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var buf bytes.Buffer
	f := &Frame{Version: 0x01, Type: TypePing, CorrelationID: 42, Payload: payload}
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != TypePing || got.CorrelationID != 42 {
		t.Fatalf("frame mismatch: type=0x%02x corrID=%d", got.Type, got.CorrelationID)
	}

	var decoded PingRequest
	if err := DecodeMsgpack(got.Payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Data != "integration" {
		t.Errorf("data: got %q", decoded.Data)
	}
}

func TestFrameWithMsgpackPayload_VoteRequest(t *testing.T) {
	orig := VoteRequest{Epoch: 10, CandidateID: "n1", LastSeq: 500, ConfigEpoch: 3}
	payload, err := EncodeMsgpack(&orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	dl := time.Now().Add(5 * time.Second)
	c.SetDeadline(dl)
	s.SetDeadline(dl)

	done := make(chan error, 1)
	go func() {
		done <- WriteFrame(c, &Frame{
			Version: 0x01, Type: TypeVoteRequest, CorrelationID: 7, Payload: payload,
		})
	}()

	got, err := ReadFrame(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}

	if got.Type != TypeVoteRequest {
		t.Fatalf("type: 0x%02x", got.Type)
	}
	var decoded VoteRequest
	if err := DecodeMsgpack(got.Payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded != orig {
		t.Errorf("got %+v, want %+v", decoded, orig)
	}
}

func TestFrameWithMsgpackPayload_HandoffRoundTrip(t *testing.T) {
	orig := HandoffMessage{TargetID: "n2", Epoch: 8, CortexSeq: 200}
	payload, err := EncodeMsgpack(&orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var buf bytes.Buffer
	f := &Frame{Version: 0x01, Type: TypeHandoff, CorrelationID: 99, Payload: payload}
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var decoded HandoffMessage
	if err := DecodeMsgpack(got.Payload, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded != orig {
		t.Errorf("got %+v, want %+v", decoded, orig)
	}
}
