package mbp

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frame represents a single MBP wire frame.
type Frame struct {
	Version       uint8
	Type          uint8
	Flags         uint16
	PayloadLength uint32
	CorrelationID uint64
	Payload       []byte
}

// Frame constants
const (
	FramePrefixSize = 16               // version(1) + type(1) + flags(2) + payloadLen(4) + correlationID(8)
	MaxPayloadSize  = 16 * 1024 * 1024 // 16 MB
)

// Flag constants (bits in the Flags uint16)
const (
	FlagCompressed uint16 = 1 << 0 // payload is zstd compressed
	FlagStreaming  uint16 = 1 << 1 // part of a multi-frame response
	FlagLastFrame  uint16 = 1 << 2 // final frame in streaming response
	FlagVault      uint16 = 1 << 3 // vault override in payload
	FlagUrgent     uint16 = 1 << 4 // process before queued commands
)

// Message type constants
const (
	TypeHello        uint8 = 0x01
	TypeHelloOK      uint8 = 0x02
	TypeWrite        uint8 = 0x03
	TypeWriteOK      uint8 = 0x04
	TypeRead         uint8 = 0x05
	TypeReadResp     uint8 = 0x06
	TypeActivate     uint8 = 0x07
	TypeActivateResp uint8 = 0x08
	TypeSubscribe    uint8 = 0x09
	TypeSubOK        uint8 = 0x0A
	TypeActivation   uint8 = 0x0B
	TypeLink         uint8 = 0x0C
	TypeLinkOK       uint8 = 0x0D
	TypeForget       uint8 = 0x0E
	TypeForgetOK     uint8 = 0x0F
	TypeUnsub        uint8 = 0x10
	TypeUnsubOK      uint8 = 0x11
	TypeStat         uint8 = 0x12
	TypeStatResp     uint8 = 0x13
	TypePing         uint8 = 0x14
	TypePong         uint8 = 0x15
	TypeError        uint8 = 0xFF

	// Cluster: replication stream
	TypeReplEntry uint8 = 0x20 // single replication log entry
	TypeReplBatch uint8 = 0x21 // batch of replication entries
	TypeReplAck   uint8 = 0x22 // replica acknowledges seq
	TypeReplNack  uint8 = 0x23 // replica requests resend from seq

	// Cluster: snapshot transfer
	TypeSnapHeader   uint8 = 0x24 // snapshot header (seq, total keys)
	TypeSnapChunk    uint8 = 0x25 // snapshot data chunk
	TypeSnapAck      uint8 = 0x26 // chunk acknowledged
	TypeSnapComplete uint8 = 0x27 // snapshot transfer complete

	// Cluster: cognitive side-effect forwarding
	TypeCogForward uint8 = 0x28 // lobe forwards cognitive side effects to cortex
	TypeCogAck     uint8 = 0x29 // cortex acknowledges cognitive forward

	// Cluster: MSP protocol
	TypeVoteRequest  uint8 = 0x30 // election vote request
	TypeVoteResponse uint8 = 0x31 // election vote response
	TypeCortexClaim  uint8 = 0x32 // winner claims cortex role
	TypeSDown        uint8 = 0x33 // subjective down notification
	TypeODown        uint8 = 0x34 // objective down (quorum agreed)
	TypeGossip       uint8 = 0x35 // cluster membership gossip
	TypeJoinRequest  uint8 = 0x36 // new node join request
	TypeJoinResponse uint8 = 0x37 // join response with cluster state
	TypeLeave        uint8 = 0x38 // graceful node departure

	// Cluster: graceful handoff
	TypeHandoff    uint8 = 0x39 // cortex sends handoff to target
	TypeHandoffAck uint8 = 0x3A // target acknowledges handoff

	// Cluster: cognitive consistency score (CCS)
	TypeCCSProbe    uint8 = 0x3B // cortex sends sampled engram keys to lobes
	TypeCCSResponse uint8 = 0x3C // lobe responds with hash of sampled keys

	// Cluster: post-partition cognitive reconciliation
	TypeReconProbe uint8 = 0x3D // cortex sends keys + weights to lobes
	TypeReconReply uint8 = 0x3E // lobe replies with its weights for those keys
	TypeReconSync  uint8 = 0x3F // cortex sends corrected weights to divergent lobe
	TypeReconAck   uint8 = 0x40 // lobe confirms weights applied

	// Cluster: peer discovery (#522 Step 4)
	TypePeerHello uint8 = 0x41 // authenticated peer-discovery handshake
)

// Errors
var (
	ErrPayloadTooLarge = errors.New("payload exceeds 16 MB limit")
	ErrVersionMismatch = errors.New("unsupported frame version")
)

// ReadFrame reads exactly one frame from the reader.
// Returns io.EOF if the connection is closed cleanly.
func ReadFrame(r io.Reader) (*Frame, error) {
	frame := &Frame{}

	// Read 16-byte prefix
	prefixBuf := make([]byte, FramePrefixSize)
	_, err := io.ReadFull(r, prefixBuf)
	if err != nil {
		return nil, err
	}

	// Parse prefix
	frame.Version = prefixBuf[0]
	frame.Type = prefixBuf[1]
	frame.Flags = binary.BigEndian.Uint16(prefixBuf[2:4])
	frame.PayloadLength = binary.BigEndian.Uint32(prefixBuf[4:8])
	frame.CorrelationID = binary.BigEndian.Uint64(prefixBuf[8:16])

	// Validate version
	if frame.Version != 0x01 {
		return nil, ErrVersionMismatch
	}

	// Validate payload size
	if frame.PayloadLength > MaxPayloadSize {
		return nil, ErrPayloadTooLarge
	}

	// Read payload
	if frame.PayloadLength == 0 {
		frame.Payload = []byte{}
	} else {
		payload := make([]byte, frame.PayloadLength)
		_, err := io.ReadFull(r, payload)
		if err != nil {
			return nil, err
		}
		frame.Payload = payload
	}

	return frame, nil
}

// WriteFrame writes a single frame to the writer.
// If the payload exceeds 16MB, returns ErrPayloadTooLarge.
// The entire frame (header + payload) is written atomically.
func WriteFrame(w io.Writer, f *Frame) error {
	if len(f.Payload) > MaxPayloadSize {
		return ErrPayloadTooLarge
	}

	// Build 16-byte prefix
	prefixBuf := make([]byte, FramePrefixSize)
	prefixBuf[0] = f.Version
	prefixBuf[1] = f.Type
	binary.BigEndian.PutUint16(prefixBuf[2:4], f.Flags)
	binary.BigEndian.PutUint32(prefixBuf[4:8], uint32(len(f.Payload)))
	binary.BigEndian.PutUint64(prefixBuf[8:16], f.CorrelationID)

	// Combine prefix and payload into a single buffer for atomic write
	buf := make([]byte, FramePrefixSize+len(f.Payload))
	copy(buf[:FramePrefixSize], prefixBuf)
	copy(buf[FramePrefixSize:], f.Payload)

	_, err := w.Write(buf)
	return err
}
