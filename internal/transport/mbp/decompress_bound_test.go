package mbp

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const testMiB = 1 << 20

// zstdDeclared compresses n zero bytes as one frame whose header declares the
// decoded size — what a one-shot compress call produces.
func zstdDeclared(t testing.TB, n int) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatalf("zstd encoder: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(make([]byte, n), nil)
}

// zstdUndeclared compresses the given bytes as a streamed frame, whose header
// does not say how large the decoded content is. The encoder is flushed part
// way through so it has to commit to a header before it has seen everything.
// Content too short to split is returned as whatever the streaming encoder
// makes of it.
func zstdUndeclared(t testing.TB, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatalf("zstd encoder: %v", err)
	}
	half := len(content) / 2
	if _, err := enc.Write(content[:half]); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if half > 0 {
		if err := enc.Flush(); err != nil {
			t.Fatalf("zstd flush: %v", err)
		}
	}
	if _, err := enc.Write(content[half:]); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(buf.Bytes()); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if half > 0 && hdr.HasFCS {
		t.Fatalf("fixture: want a frame with no declared content size for %d bytes", len(content))
	}
	return buf.Bytes()
}

// zstdUndeclaredZeros is zstdUndeclared for n zero bytes, written in chunks so
// the fixture itself never holds n bytes.
func zstdUndeclaredZeros(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatalf("zstd encoder: %v", err)
	}
	chunk := make([]byte, testMiB)
	for written := 0; written < n; written += len(chunk) {
		if _, err := enc.Write(chunk[:min(len(chunk), n-written)]); err != nil {
			t.Fatalf("zstd write: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

// allocatedDuring returns the bytes the process allocated while f ran. It is
// cumulative (every allocation counts, freed or not), so it is an upper bound
// on what f could have held at once. f is run once beforehand so that one-time
// costs — building a decoder, the first scratch buffer — are not charged to it.
func allocatedDuring(f func()) uint64 {
	f()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestDecompressPayload_RefusesPastTheFrameLimitWithoutAllocatingIt is the
// decompression-bomb pin. A compressed payload of a few kilobytes that expands
// past what a frame may carry is refused, and the refusal costs memory in
// proportion to the LIMIT, not to what the payload claims or expands to.
//
// Before the bound, the size was checked only after the whole payload had been
// decoded: both fixtures below were decoded in full and returned as valid
// payloads, and a 0.16 MB frame could make the server allocate gigabytes.
//
// The fixtures are deliberately small (tens of MiB decoded) so the test is safe
// to run on unfixed code.
func TestDecompressPayload_RefusesPastTheFrameLimitWithoutAllocatingIt(t *testing.T) {
	const limit = MaxPayloadSize // 16 MiB

	t.Run("the frame header declares the size", func(t *testing.T) {
		bomb := zstdDeclared(t, limit+8*testMiB)
		var out []byte
		var err error
		allocated := allocatedDuring(func() { out, err = DecompressPayload(bomb) })
		t.Logf("%d compressed bytes declaring %d MiB: err=%v, %d bytes returned, %.2f MiB allocated",
			len(bomb), (limit+8*testMiB)/testMiB, err, len(out), float64(allocated)/testMiB)
		if err == nil {
			t.Errorf("a payload that decodes to %d bytes was accepted; the frame limit is %d", len(out), limit)
		}
		// The header says how large it is. Nothing needs to be allocated to refuse it.
		if allocated > testMiB {
			t.Errorf("refusing it allocated %.2f MiB; want under 1 MiB", float64(allocated)/testMiB)
		}
	})

	t.Run("the frame header declares nothing", func(t *testing.T) {
		bomb := zstdUndeclaredZeros(t, limit+24*testMiB)
		var out []byte
		var err error
		allocated := allocatedDuring(func() { out, err = DecompressPayload(bomb) })
		t.Logf("%d compressed bytes expanding to %d MiB: err=%v, %d bytes returned, %.2f MiB allocated",
			len(bomb), (limit+24*testMiB)/testMiB, err, len(out), float64(allocated)/testMiB)
		if err == nil {
			t.Errorf("a payload that decodes to %d bytes was accepted; the frame limit is %d", len(out), limit)
		}
		// The size is only known by decoding. The decoder is given a small
		// scratch buffer first and then, once, a buffer that holds the limit:
		// about the limit in all, never a multiple of what the payload expands
		// to.
		if bound := uint64(limit + limit/2); allocated > bound {
			t.Errorf("refusing it allocated %.2f MiB; want under %.0f MiB", float64(allocated)/testMiB, float64(bound)/testMiB)
		}
	})
}

// TestDecompressPayload_FrameLimitIsTheUncompressedLimit: a compressed frame
// may carry exactly what an uncompressed one may — MaxPayloadSize — and not a
// byte more, and decoding a payload at the limit allocates the payload, not a
// multiple of it.
func TestDecompressPayload_FrameLimitIsTheUncompressedLimit(t *testing.T) {
	atLimit := zstdDeclared(t, MaxPayloadSize)
	var out []byte
	var err error
	allocated := allocatedDuring(func() { out, err = DecompressPayload(atLimit) })
	if err != nil || len(out) != MaxPayloadSize {
		t.Fatalf("a payload of exactly MaxPayloadSize: %d bytes, err %v; want it decoded", len(out), err)
	}
	if bound := uint64(MaxPayloadSize + testMiB); allocated > bound {
		t.Errorf("decoding %d MiB allocated %.2f MiB; want about the payload", MaxPayloadSize/testMiB, float64(allocated)/testMiB)
	}
	out = nil

	if out, err := DecompressPayload(zstdDeclared(t, MaxPayloadSize+1)); err == nil {
		t.Errorf("a payload one byte over MaxPayloadSize was accepted (%d bytes)", len(out))
	}
}

// TestDecompressPayload_RoundTrips: the bound must not cost legitimate
// payloads anything — every shape of frame an encoder produces still decodes
// to the bytes that went in, and a small payload costs a small allocation.
func TestDecompressPayload_RoundTrips(t *testing.T) {
	pattern := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + i/251)
		}
		return b
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()

	for _, n := range []int{0, 1, 1023, 65535, 65536, 65537, 300_000, testMiB + 3} {
		content := pattern(n)
		for name, compressed := range map[string][]byte{
			"declared":   enc.EncodeAll(content, nil),
			"undeclared": zstdUndeclared(t, content),
		} {
			out, err := DecompressPayload(compressed)
			if err != nil {
				t.Errorf("%s, %d bytes: %v", name, n, err)
				continue
			}
			if !bytes.Equal(out, content) {
				t.Errorf("%s, %d bytes: decoded content differs (got %d bytes)", name, n, len(out))
			}
		}
	}

	// A streamed frame larger than the first scratch buffer still decodes.
	big := pattern(3*testMiB + 17)
	if out, err := DecompressPayload(zstdUndeclared(t, big)); err != nil || !bytes.Equal(out, big) {
		t.Errorf("undeclared, %d bytes: %d bytes, err %v; want it decoded", len(big), len(out), err)
	}

	// A small payload with no declared size must not cost a limit-sized buffer.
	small := zstdUndeclared(t, pattern(100_000))
	var out []byte
	allocated := allocatedDuring(func() { out, err = DecompressPayload(small) })
	if err != nil || len(out) != 100_000 {
		t.Fatalf("small undeclared payload: %d bytes, err %v", len(out), err)
	}
	if allocated > testMiB {
		t.Errorf("decoding 100 KB allocated %.2f MiB; want well under 1 MiB", float64(allocated)/testMiB)
	}

	// A streamed frame that is bad, not big, stops at the scratch buffer: it
	// is not handed a limit-sized buffer to be bad in.
	bad := zstdUndeclared(t, pattern(50_000))
	var badHdr zstd.Header
	if err := badHdr.Decode(bad); err != nil {
		t.Fatal(err)
	}
	bad[badHdr.HeaderSize+3+40] ^= 0xFF // inside the first block's body
	allocated = allocatedDuring(func() { out, err = DecompressPayload(bad) })
	if err == nil {
		t.Errorf("a corrupted frame was accepted (%d bytes)", len(out))
	}
	if allocated > testMiB {
		t.Errorf("refusing a corrupted 50 KB frame allocated %.2f MiB; want well under 1 MiB", float64(allocated)/testMiB)
	}

	for name, garbage := range map[string][]byte{
		"short":     {0x28, 0xB5},
		"not zstd":  []byte("this is not a zstd frame at all, it is a sentence"),
		"truncated": enc.EncodeAll(pattern(50_000), nil)[:200],
	} {
		if out, err := DecompressPayload(garbage); err == nil {
			t.Errorf("%s input was accepted (%d bytes)", name, len(out))
		}
	}
}

// TestDecompressPayload_OneFrameOnly: a compressed payload is exactly one zstd
// frame. zstd allows frames to be concatenated and the decoder would decode
// them all, which is its own attack: every frame costs the decoder a fixed
// setup whatever it holds. Before the bound, the 2,000 hundred-byte frames
// below — 200 KB decoded — made it allocate megabytes, and the cost grows with
// the number of frames, which a 16 MB payload can hold over a million of.
//
// Refusing them costs nothing: the frame's end is found by walking its block
// headers, without decoding.
func TestDecompressPayload_OneFrameOnly(t *testing.T) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	text := bytes.Repeat([]byte("berth 7 is closed. "), 400)

	t.Run("many small frames", func(t *testing.T) {
		one := enc.EncodeAll(text[:100], nil)
		var many []byte
		const frames = 2000
		for i := 0; i < frames; i++ {
			many = append(many, one...)
		}
		var out []byte
		var err error
		allocated := allocatedDuring(func() { out, err = DecompressPayload(many) })
		t.Logf("%d frames of 100 bytes (%d bytes compressed): err=%v, %d bytes returned, %.2f MiB allocated",
			frames, len(many), err, len(out), float64(allocated)/testMiB)
		if err == nil {
			t.Errorf("%d concatenated frames were accepted as one payload (%d bytes)", frames, len(out))
		}
		if allocated > testMiB/4 {
			t.Errorf("refusing them allocated %.2f MiB; want next to nothing", float64(allocated)/testMiB)
		}
	})

	// Every shape of second frame after every shape of first.
	declared := enc.EncodeAll(text[:5000], nil)
	undeclared := zstdUndeclared(t, text[:5000])
	for name, payload := range map[string][]byte{
		"declared then declared":     append(append([]byte{}, declared...), declared...),
		"declared then undeclared":   append(append([]byte{}, declared...), undeclared...),
		"undeclared then declared":   append(append([]byte{}, undeclared...), declared...),
		"undeclared then undeclared": append(append([]byte{}, undeclared...), undeclared...),
		"one frame then stray bytes": append(append([]byte{}, declared...), 0x00, 0x01, 0x02),
		"a skippable frame":          {0x50, 0x2A, 0x4D, 0x18, 0x04, 0x00, 0x00, 0x00, 1, 2, 3, 4},
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := DecompressPayload(payload); err == nil {
				t.Errorf("accepted as one payload (%d bytes decoded)", len(out))
			}
		})
	}

	// One frame of each shape is still fine, with and without a checksum.
	for name, payload := range map[string][]byte{"declared": declared, "undeclared": undeclared} {
		if out, err := DecompressPayload(payload); err != nil || !bytes.Equal(out, text[:5000]) {
			t.Errorf("a single %s frame: %d bytes, err %v; want it decoded", name, len(out), err)
		}
	}
	crcEnc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer crcEnc.Close()
	noCRC, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderCRC(false))
	if err != nil {
		t.Fatal(err)
	}
	defer noCRC.Close()
	for name, e := range map[string]*zstd.Encoder{"with a checksum": crcEnc, "without a checksum": noCRC} {
		if out, err := DecompressPayload(e.EncodeAll(text, nil)); err != nil || !bytes.Equal(out, text) {
			t.Errorf("a single frame %s: %d bytes, err %v; want it decoded", name, len(out), err)
		}
	}
}
