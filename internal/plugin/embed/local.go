//go:build localassets

package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	localModelDim   = 384  // bge-small-en-v1.5 output dimension
	localMaxTokens  = 256  // model max sequence length
	localMaxBatch   = 64   // texts per ORT inference call (DynamicAdvancedSession)
	ortSentinelFile = ".ort_extracted"
)

// ortInitOnce guards the global ORT environment — there can only be one.
var (
	ortInitOnce sync.Once
	ortInitErr  error
)

// LocalProvider implements Provider using the bundled bge-small-en-v1.5 ONNX model.
// No external process or network connection is required; all assets are embedded
// in the binary and extracted to DataDir on first Init.
//
// Uses DynamicAdvancedSession so tensors are allocated per-call at the actual
// batch size, supporting variable-sized batches up to localMaxBatch.
type LocalProvider struct {
	// mu serialises ORT session calls; DynamicAdvancedSession is not
	// guaranteed thread-safe from the Go wrapper's perspective.
	mu sync.Mutex

	session *ort.DynamicAdvancedSession
	tok     *tokenizer.Tokenizer
	dataDir string
}

func (p *LocalProvider) Name() string { return "local" }

func (p *LocalProvider) MaxBatchSize() int { return localMaxBatch }

// Init extracts embedded assets to DataDir and initializes the ORT session.
func (p *LocalProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	dataDir := cfg.DataDir
	if dataDir == "" {
		// Fallback: use a directory next to the binary.
		dataDir = "muninndb-data"
	}

	modelDir := filepath.Join(dataDir, "models", "bge-small")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return 0, fmt.Errorf("local provider: cannot create model dir %s: %w", modelDir, err)
	}

	// Extract embedded assets if not already present (checked via SHA256 sentinel).
	if err := ensureExtracted(ctx, modelDir); err != nil {
		return 0, fmt.Errorf("local provider: asset extraction failed: %w", err)
	}

	// Initialize ORT global environment (once per process).
	ortLibPath := filepath.Join(modelDir, nativeLibFilename)
	ortInitOnce.Do(func() {
		ort.SetSharedLibraryPath(ortLibPath)
		ortInitErr = ort.InitializeEnvironment()
	})
	if ortInitErr != nil {
		hint := ""
		if runtime.GOOS == "windows" {
			hint = " (if onnxruntime.dll fails to load, install the Visual C++ 2019 Redistributable: https://aka.ms/vs/17/release/vc_redist.x64.exe)"
		}
		return 0, fmt.Errorf("local provider: ORT environment init: %w%s", ortInitErr, hint)
	}

	// Load tokenizer.
	tokPath := filepath.Join(modelDir, "tokenizer.json")
	tok, err := pretrained.FromFile(tokPath)
	if err != nil {
		return 0, fmt.Errorf("local provider: load tokenizer: %w", err)
	}
	p.tok = tok
	p.dataDir = dataDir

	// Create the ORT session. DynamicAdvancedSession does not bind tensors at
	// init time — tensors are passed per Run() call at the actual batch size.
	modelPath := filepath.Join(modelDir, "model_int8.onnx")
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return 0, fmt.Errorf("local provider: ORT session options: %w", err)
	}
	defer opts.Destroy()

	// Allow ORT to use up to half the logical CPUs (min 1, max 4) for intra-op
	// parallelism. This helps batch inference on multi-core hardware without
	// over-subscribing when multiple goroutines share the process.
	numThreads := runtime.NumCPU() / 2
	if numThreads < 1 {
		numThreads = 1
	}
	if numThreads > 4 {
		numThreads = 4
	}
	opts.SetIntraOpNumThreads(numThreads) //nolint:errcheck

	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		opts,
	)
	if err != nil {
		return 0, fmt.Errorf("local provider: create ORT session: %w", err)
	}
	p.session = session

	slog.Info("local embed provider initialized",
		"model", "bge-small-en-v1.5",
		"dimension", localModelDim,
		"model_dir", modelDir,
	)

	return localModelDim, nil
}

// EmbedBatch tokenizes up to localMaxBatch texts, runs a single ORT inference
// call for the whole batch, and returns the concatenated 384-dim embeddings.
// The caller (BatchEmbedder) ensures len(texts) <= localMaxBatch.
func (p *LocalProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.session == nil {
		return nil, fmt.Errorf("local provider not initialized")
	}

	batchSize := len(texts)

	// Allocate input tensors at actual batch size. Zeroed on allocation.
	inShape := ort.NewShape(int64(batchSize), int64(localMaxTokens))
	inputIDs, err := ort.NewEmptyTensor[int64](inShape)
	if err != nil {
		return nil, fmt.Errorf("local provider: alloc input_ids: %w", err)
	}
	defer inputIDs.Destroy()

	attentionMask, err := ort.NewEmptyTensor[int64](inShape)
	if err != nil {
		return nil, fmt.Errorf("local provider: alloc attention_mask: %w", err)
	}
	defer attentionMask.Destroy()

	tokenTypeIDs, err := ort.NewEmptyTensor[int64](inShape)
	if err != nil {
		return nil, fmt.Errorf("local provider: alloc token_type_ids: %w", err)
	}
	defer tokenTypeIDs.Destroy()

	outShape := ort.NewShape(int64(batchSize), int64(localMaxTokens), int64(localModelDim))
	outputTensor, err := ort.NewEmptyTensor[float32](outShape)
	if err != nil {
		return nil, fmt.Errorf("local provider: alloc output: %w", err)
	}
	defer outputTensor.Destroy()

	inputBuf := inputIDs.GetData()
	maskBuf := attentionMask.GetData()
	typeBuf := tokenTypeIDs.GetData()

	// Explicitly zero buffers — ORT allocator does not guarantee zeroed memory.
	for i := range inputBuf {
		inputBuf[i] = 0
		maskBuf[i] = 0
		typeBuf[i] = 0
	}

	// Tokenize and pack each text into the batch tensor.
	for i, text := range texts {
		enc, encErr := p.tok.EncodeSingle(text, true)
		if encErr != nil {
			return nil, fmt.Errorf("local provider: tokenize text[%d]: %w", i, encErr)
		}
		ids := enc.GetIds()
		mask := enc.GetAttentionMask()
		typeIDs := enc.GetTypeIds()

		seqLen := len(ids)
		if seqLen > localMaxTokens {
			seqLen = localMaxTokens
		}
		offset := i * localMaxTokens
		for j := 0; j < seqLen; j++ {
			inputBuf[offset+j] = int64(ids[j])
			maskBuf[offset+j] = int64(mask[j])
			typeBuf[offset+j] = int64(typeIDs[j])
		}
	}

	// Single ORT inference call for the entire batch.
	if err := p.session.Run(
		[]ort.Value{inputIDs, attentionMask, tokenTypeIDs},
		[]ort.Value{outputTensor},
	); err != nil {
		return nil, fmt.Errorf("local provider: ORT run: %w", err)
	}

	// Unpack output shape [batchSize, localMaxTokens, localModelDim].
	// For each sequence: extract the [CLS] token embedding (position 0), then L2-normalise.
	// bge-small-en-v1.5 encodes sentence meaning into the [CLS] token, not mean-pooled tokens.
	hidden := outputTensor.GetData()
	result := make([]float32, 0, batchSize*localModelDim)
	seqStride := localMaxTokens * localModelDim
	for i := 0; i < batchSize; i++ {
		seqHidden := hidden[i*seqStride : (i+1)*seqStride]
		vec := clsPool(seqHidden, localModelDim)
		l2Normalize(vec)
		result = append(result, vec...)
	}

	return result, nil
}

// clsPool extracts the [CLS] token embedding at position 0.
// Close releases the ORT session.
func (p *LocalProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.session != nil {
		_ = p.session.Destroy()
		p.session = nil
	}
	return nil
}

// ensureExtracted writes embedded assets to modelDir if not already present.
// Uses a SHA256 sentinel file to avoid redundant extraction.
func ensureExtracted(ctx context.Context, modelDir string) error {
	sentinelPath := filepath.Join(modelDir, ortSentinelFile)
	if _, err := os.Stat(sentinelPath); err == nil {
		// Already extracted.
		return nil
	}

	slog.Info("extracting bundled local embed assets", "dir", modelDir)

	files := map[string][]byte{
		"model_int8.onnx":      embeddedModel,
		"tokenizer.json":       embeddedTokenizer,
		nativeLibFilename:      embeddedNativeLib,
	}

	var sentinelHash string
	for name, data := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(data) == 0 {
			return fmt.Errorf("embedded asset %q is empty — run `make fetch-assets` and rebuild", name)
		}

		dest := filepath.Join(modelDir, name)
		if err := atomicWrite(dest, data); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}

		// Accumulate SHA256 for sentinel.
		h := sha256.Sum256(data)
		sentinelHash += hex.EncodeToString(h[:]) + "\n"
	}

	// Write sentinel only after all files succeed.
	if err := atomicWrite(sentinelPath, []byte(sentinelHash)); err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}

	// Make the native lib executable (required on unix, no-op on Windows).
	if runtime.GOOS != "windows" {
		libPath := filepath.Join(modelDir, nativeLibFilename)
		if err := os.Chmod(libPath, 0o755); err != nil {
			return fmt.Errorf("chmod native lib: %w", err)
		}
	}

	slog.Info("local embed assets extracted", "dir", modelDir)
	return nil
}

// atomicWrite writes data to dest via a temp file + rename to prevent corruption.
func atomicWrite(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName)
		return writeErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}

	// Atomic replace.
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Convenience reader that works with either io.Reader or raw bytes.
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
