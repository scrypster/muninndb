package mbp

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	CompressionThreshold = 1024 // Compress payloads > 1KB
)

// Decoding limits. A compressed frame may carry what an uncompressed frame may
// — MaxPayloadSize decoded bytes — and no more, and the bound has to hold
// WHILE decoding: the size of a hostile payload is not known until it has been
// decoded, and by then the memory has been spent.
const (
	// probeBufferSize is the scratch buffer first tried for a frame whose
	// header does not declare its decoded size. No legitimate request is near
	// it (an engram's content is at most 16 KB), so in practice it is the only
	// buffer such a frame ever needs.
	probeBufferSize = 384 * 1024

	// maxDecodedBlock is the most one zstd block decodes to. The decoder
	// compares its output with its limits after each block, so a buffer with
	// this much room past a limit is never outgrown before the limit is seen —
	// and an outgrown buffer is what the decoder reallocates.
	maxDecodedBlock = 128 * 1024
)

// ErrDecodedPayloadTooLarge is returned when a compressed payload decodes to
// more than a frame may carry.
var ErrDecodedPayloadTooLarge = errors.New("decompressed payload exceeds the frame limit")

// ErrNotOneZstdFrame is returned when a compressed payload is anything other
// than exactly one zstd frame.
var ErrNotOneZstdFrame = errors.New("compressed payload is not a single zstd frame")

// zstd encoder/decoder pools — lazily initialised on first use.
// Errors during pool creation are surfaced to the caller via error returns
// rather than panicking at process start, allowing graceful shutdown.
var (
	encoderPool sync.Pool
	decoderPool sync.Pool
	// probeBuffers are the scratch buffers a frame with no declared size is
	// first decoded into; what it decodes to is copied out at its own size.
	probeBuffers sync.Pool
	initOnce     sync.Once
	initErr      error
)

// initPools validates that zstd is operational on first use.
// Returns a non-nil error if the zstd library cannot create codecs.
func initPools() error {
	initOnce.Do(func() {
		encoderPool.New = func() any {
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
			if err != nil {
				// Return the error wrapped in a sentinel so callers can detect it.
				return fmt.Errorf("zstd encoder init: %w", err)
			}
			return enc
		}
		decoderPool.New = func() any {
			// Neither of the library's limits bounds ALLOCATION on its own; what
			// they do bound was measured, not read off their names
			// (TestDecompressPayload_RefusesPastTheFrameLimitWithoutAllocatingIt):
			//
			//   - WithDecoderMaxMemory refuses a frame whose HEADER declares more
			//     than the limit before allocating for it. For a frame that
			//     declares nothing it only compares the output after each block,
			//     and the output buffer has by then been appended to and
			//     reallocated — about six times the limit in all. It also caps
			//     the window a frame may declare at the same figure, so it cannot
			//     be set low to make a cheap first attempt.
			//   - WithDecodeAllCapLimit refuses output past the capacity of the
			//     buffer DecodeAll was handed, so the buffer decides the bound.
			//     When the frame declares nothing the refusal can be an untyped
			//     error, and can come after the buffer has been outgrown once.
			//
			// decompressBounded uses both and chooses every buffer itself.
			dec, err := zstd.NewReader(nil,
				zstd.WithDecoderMaxMemory(MaxPayloadSize),
				zstd.WithDecodeAllCapLimit(true))
			if err != nil {
				return fmt.Errorf("zstd decoder init: %w", err)
			}
			return dec
		}
		probeBuffers.New = func() any {
			b := make([]byte, 0, probeBufferSize)
			return &b
		}
		// Eagerly validate both codecs so failures surface at server start.
		enc := encoderPool.Get()
		if err, ok := enc.(error); ok {
			initErr = err
			return
		}
		encoderPool.Put(enc)
		dec := decoderPool.Get()
		if err, ok := dec.(error); ok {
			initErr = err
			return
		}
		decoderPool.Put(dec)
	})
	return initErr
}

// CompressPayload compresses data with zstd if it's worth it.
// Returns compressed data and true if compression was applied.
// Returns the original data unmodified if zstd is unavailable or compression
// doesn't reduce size.
func CompressPayload(data []byte) ([]byte, bool, error) {
	if len(data) <= CompressionThreshold {
		return data, false, nil
	}
	if err := initPools(); err != nil {
		return data, false, nil // degrade gracefully: send uncompressed
	}

	raw := encoderPool.Get()
	if err, ok := raw.(error); ok {
		return data, false, err
	}
	enc := raw.(*zstd.Encoder)
	defer encoderPool.Put(enc)

	compressed := enc.EncodeAll(data, nil)
	// Only use compression if it actually saves space.
	if len(compressed) < len(data) {
		return compressed, true, nil
	}
	return data, false, nil
}

// DecompressPayload decompresses a zstd-compressed frame payload of at most
// MaxPayloadSize decoded bytes. A payload that expands past that is refused
// with ErrDecodedPayloadTooLarge, having cost memory in proportion to the
// limit and not to what the payload claims or would expand to.
func DecompressPayload(data []byte) ([]byte, error) {
	return decompressBounded(data, noCharge{})
}

// decodeCharger is told how many decoded bytes a frame holds, before it holds
// them. It is how a connection's budget is charged (frameCharge).
type decodeCharger interface {
	// hold makes the charge exactly n bytes — not n more. An error from it
	// ends the decode.
	hold(n int) error
	// keep lowers the charge to n bytes, once the frame is known to need less
	// than it was charged for.
	keep(n int)
}

type noCharge struct{}

func (noCharge) hold(int) error { return nil }
func (noCharge) keep(int)       {}

// decompressBounded is DecompressPayload with the memory it is about to hold
// announced first.
//
// The decoder may not grow the buffer it is handed, so what a call allocates is
// decided here:
//
//   - The frame header declares the decoded size, as a one-shot compress call
//     does: more than MaxPayloadSize is refused before anything is allocated;
//     otherwise one buffer of exactly that size.
//   - It declares nothing (a streamed frame): decode into a pooled scratch
//     buffer and copy the result out at its own size. Only when the scratch
//     buffer was filled does the frame get a buffer that holds MaxPayloadSize,
//     once. A frame that fails for any other reason stops at the scratch
//     buffer.
//
// The payload must be exactly one zstd frame (requireOneFrame), which is
// checked before anything is decoded.
//
// charge.hold is called before each buffer with the TOTAL the frame then
// holds — the scratch buffer has gone back to its pool before the large one is
// asked for, so the frame never holds one charge while asking for another —
// and charge.keep once the frame turns out to need less.
func decompressBounded(data []byte, charge decodeCharger) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil // nothing to decode, as before
	}
	if err := initPools(); err != nil {
		return nil, fmt.Errorf("zstd unavailable: %w", err)
	}

	var hdr zstd.Header
	if err := hdr.Decode(data); err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	if err := requireOneFrame(data, &hdr); err != nil {
		return nil, err
	}

	if hdr.HasFCS {
		if hdr.FrameContentSize > MaxPayloadSize {
			return nil, ErrDecodedPayloadTooLarge
		}
		size := int(hdr.FrameContentSize)
		if err := charge.hold(size); err != nil {
			return nil, err
		}
		decoded, err := decodeInto(data, make([]byte, 0, size))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}

	if err := charge.hold(probeBufferSize); err != nil {
		return nil, err
	}
	scratch := probeBuffers.Get().(*[]byte)
	decoded, err := decodeInto(data, (*scratch)[:0])
	if err == nil {
		out := make([]byte, len(decoded))
		copy(out, decoded)
		probeBuffers.Put(scratch)
		charge.keep(len(out))
		return out, nil
	}
	// Did it run out of room, or is the frame bad? Out of room means the
	// decoder got to within one block of the end of the buffer; a bad frame
	// stops wherever it is bad, and does not get a larger buffer to be bad in.
	filled := errors.Is(err, ErrDecodedPayloadTooLarge) || len(decoded)+maxDecodedBlock >= probeBufferSize
	probeBuffers.Put(scratch)
	if !filled {
		return nil, err
	}

	// One buffer that holds the frame limit, with room for the block that takes
	// the output past it. The scratch buffer is back in its pool, and hold
	// gives the scratch charge back before it takes this one.
	if err := charge.hold(MaxPayloadSize); err != nil {
		return nil, err
	}
	decoded, err = decodeInto(data, make([]byte, 0, MaxPayloadSize+maxDecodedBlock+1024))
	if err != nil {
		return nil, err
	}
	if len(decoded) < cap(decoded)/2 {
		// Do not keep a limit-sized buffer alive behind a small payload.
		out := make([]byte, len(decoded))
		copy(out, decoded)
		charge.keep(len(out))
		return out, nil
	}
	return decoded, nil
}

// decodeInto decodes data into dst, which the decoder may fill but not grow.
// On an error it still returns what was decoded before it, so the caller can
// tell a full buffer from a bad frame; the decoder's own "too much output"
// comes back as ErrDecodedPayloadTooLarge.
func decodeInto(data, dst []byte) ([]byte, error) {
	raw := decoderPool.Get()
	if err, ok := raw.(error); ok {
		return nil, err
	}
	dec := raw.(*zstd.Decoder)
	defer decoderPool.Put(dec)

	decoded, err := dec.DecodeAll(data, dst)
	switch {
	case err == nil:
		return decoded, nil
	case errors.Is(err, zstd.ErrDecoderSizeExceeded):
		return decoded, ErrDecodedPayloadTooLarge
	default:
		return decoded, fmt.Errorf("zstd decompress: %w", err)
	}
}

// requireOneFrame refuses a payload that is not exactly one zstd frame, by
// walking the frame's block headers to find where it ends. Nothing is decoded.
//
// zstd lets frames be concatenated, and the decoder would decode them all. No
// client needs that for one request, and it is its own attack: each frame
// costs the decoder a fixed setup of a few kilobytes whatever it holds, so a
// payload of a million nine-byte frames costs gigabytes of allocation and
// seconds of CPU to decode a few megabytes. One frame, one setup.
func requireOneFrame(data []byte, hdr *zstd.Header) error {
	if hdr.Skippable {
		return ErrNotOneZstdFrame
	}
	off := hdr.HeaderSize
	for {
		if off+3 > len(data) {
			return fmt.Errorf("zstd decompress: %w", io.ErrUnexpectedEOF)
		}
		// Block header: bit 0 = last block, bits 1-2 = type, bits 3-23 = size.
		bh := uint32(data[off]) | uint32(data[off+1])<<8 | uint32(data[off+2])<<16
		off += 3
		size := int(bh >> 3)
		if (bh>>1)&3 == 1 {
			size = 1 // an RLE block is one byte on the wire, repeated `size` times
		}
		off += size
		if off > len(data) {
			return fmt.Errorf("zstd decompress: %w", io.ErrUnexpectedEOF)
		}
		if bh&1 != 0 {
			break
		}
	}
	if hdr.HasCheckSum {
		off += 4
	}
	switch {
	case off > len(data):
		return fmt.Errorf("zstd decompress: %w", io.ErrUnexpectedEOF)
	case off < len(data):
		return ErrNotOneZstdFrame
	}
	return nil
}
