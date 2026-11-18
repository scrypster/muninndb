//go:build localassets

package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/scrypster/muninndb/internal/embedlane"
	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
)

const (
	localModelDim   = 384 // bge-small-en-v1.5 output dimension
	localMaxTokens  = 256 // model max sequence length
	ortSentinelFile = ".ort_extracted"
	// localMaxBatch is the most texts one ORT inference call carries. Every
	// text is padded to localMaxTokens, so inference cost and its transient
	// memory scale linearly with the batch while throughput does not improve:
	// measured with the bundled model (512 texts, chunk 8/16/32/64) throughput
	// was flat at ~48-57 items/s while peak RSS rose ~670-790 MB / ~1.1 GB /
	// ~1.4 GB / ~2.2-2.6 GB. 8 keeps the batch lane's peak under 1 GB on a
	// 2 GB host and bounds how long one call holds the batch lane (~0.2 s).
	// BatchEmbedder and the retroactive processor both chunk by MaxBatchSize.
	localMaxBatch = 8
	// localQueryLaneMaxTexts caps what may take the query lane. The query lane
	// runs CONCURRENTLY with the batch lane on the shared session, so whatever
	// it admits adds to peak memory; a marked call larger than this (an
	// unusually long multi-phrase recall context) takes the batch lane instead
	// — slower, never unbounded.
	localQueryLaneMaxTexts = 4
)

// ortInitOnce guards the global ORT environment — there can only be one.
var (
	ortInitOnce sync.Once
	ortInitErr  error
)

// LocalProvider implements Provider using an in-process ONNX model: by default
// the bundled bge-small-en-v1.5 (assets embedded in the binary, extracted to
// DataDir on first Init), or a user-supplied model when
// ProviderHTTPConfig.LocalModelPath/LocalTokenizerPath are set (issue #583).
// No external process or network connection is required either way.
//
// Uses DynamicAdvancedSession so tensors are allocated per-call at the actual
// batch size, supporting variable-sized batches up to localMaxBatch.
//
// Concurrency: inference runs in two lanes, each admitting one call at a time
// (see embedLane). The QUERY lane serves calls the caller marked with
// embedlane.WithQuery — recall's query embed, a short text someone is waiting
// on. Everything else (write-path and retroactive embeds) takes the BATCH lane.
// A batch in progress therefore never delays a query embed, while there is
// still never more than one batch in flight — the batch lane is the memory cap
// that the old single mutex provided (one 64-text call took RSS from ~0.4 GB to
// ~1.4 GB; two concurrent ones to ~2.8 GB). Both lanes share one ORT session:
// OrtApi::Run is documented safe for concurrent calls on one session, the Go
// wrapper's DynamicAdvancedSession.Run keeps no per-call state on the session,
// and the tokenizer is read-only after load. A second session would cost
// ~85 MB for no measured latency gain.
//
// Bit-exactness is a property of the CONCURRENCY LEVEL, not of the design.
// At the 2-way concurrency these lanes permit, vectors are bit-identical to a
// serial run (pinned by TestLocalProvider_LanesProduceIdenticalVectors;
// review stress: 0 mismatches in 12,000 raw runs at 2-way, 16,000 at 8-way).
// It is NOT guaranteed under heavier concurrency: at 16-way, 3 of 136,000 runs
// on a shared session and 1 of 72,000 on separate sessions differed in every
// component (max diff ~0.005, cosine ~0.9995, never NaN) — ORT's CPU kernels
// under oversubscription, not session sharing. Do not raise a lane's capacity
// or add lanes without re-running that bit-equality stress.
//
// Independently of scheduling, the bundled model is dynamically quantized, so
// a text's vector depends on its batch-mates: alone vs inside a batch, cosine
// p50 0.9994 (min 0.9989); batch of 8 vs 64, min 0.9985. An exact own-text
// match therefore scores ~0.998, never 1.0 — nothing may assume self-cosine
// 1.0 — and re-embedding after the 64 -> 8 batch change moves a stored vector
// by at most that much.
type LocalProvider struct {
	// mu serialises Init and Close. Both also hold BOTH lanes while they run,
	// so the session is never created, probed or destroyed under an inference.
	mu sync.Mutex

	lanesOnce sync.Once
	queryLane *embedLane
	batchLane *embedLane
	// onLaneAcquired / onLaneReleased, when non-nil, are called with the lane
	// name as a call enters / leaves its lane. Test-only instrumentation: it is
	// how tests observe ordering and in-flight counts without sleeping.
	onLaneAcquired func(lane string)
	onLaneReleased func(lane string)

	session *ort.DynamicAdvancedSession
	tok     *tokenizer.Tokenizer
	dataDir string

	// Model parameters, set at Init. The bundled model uses the package
	// constants; a user-supplied model uses config values and a probed
	// dimension.
	dim        int
	maxTokens  int
	meanPool   bool
	inputNames []string // the model's declared inputs, fed in this order
}

func (p *LocalProvider) Name() string { return "local" }

func (p *LocalProvider) MaxBatchSize() int { return localMaxBatch }

// Init initializes the ORT session — for the bundled model (assets extracted
// to DataDir) or, when cfg.LocalModelPath/LocalTokenizerPath are set, for a
// user-supplied model. The bundled ORT shared library serves both cases, so
// embedded assets are extracted either way.
func (p *LocalProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	release, err := p.holdAllLanes(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	dataDir := cfg.DataDir
	if dataDir == "" {
		// Fallback: use a directory next to the binary.
		dataDir = "muninndb-data"
	}

	userModel := cfg.LocalModelPath != "" || cfg.LocalTokenizerPath != ""
	if userModel {
		// Validate the user configuration BEFORE any side effects (asset
		// extraction, ORT library load): a broken configuration is a
		// deterministic error and must not extract ~55 MB of assets or load
		// the ORT shared library first — on Windows a loaded DLL cannot be
		// deleted, which would also break temp-dir cleanup for any caller.
		if _, _, err := validateUserModelConfig(cfg); err != nil {
			return 0, err
		}
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

	p.dataDir = dataDir

	if userModel {
		return p.initUserModelLocked(cfg)
	}
	return p.initBundledLocked(modelDir)
}

// validateUserModelConfig is the single owner of the user-model configuration
// checks (issue #583): both paths present and readable, positive max tokens,
// known pooling value. It is deliberately side-effect free so Init can run it
// before extracting assets or loading the ORT library. Returns the resolved
// max tokens and pooling mode.
func validateUserModelConfig(cfg ProviderHTTPConfig) (maxTokens int, meanPool bool, err error) {
	modelPath, tokPath := cfg.LocalModelPath, cfg.LocalTokenizerPath
	if modelPath == "" || tokPath == "" {
		return 0, false, fmt.Errorf("local provider: embed_model_path and embed_tokenizer_path must both be set (got model=%q tokenizer=%q)", modelPath, tokPath)
	}
	if _, statErr := os.Stat(modelPath); statErr != nil {
		return 0, false, fmt.Errorf("local provider: user model: %w", statErr)
	}
	if _, statErr := os.Stat(tokPath); statErr != nil {
		return 0, false, fmt.Errorf("local provider: user tokenizer: %w", statErr)
	}

	maxTokens = cfg.LocalMaxTokens
	if maxTokens == 0 {
		maxTokens = localMaxTokens
	}
	if maxTokens < 1 {
		return 0, false, fmt.Errorf("local provider: embed_max_tokens must be positive, got %d", cfg.LocalMaxTokens)
	}

	switch cfg.LocalPooling {
	case "", "cls":
		meanPool = false
	case "mean":
		meanPool = true
	default:
		return 0, false, fmt.Errorf("local provider: unknown embed_pooling %q (want \"cls\" or \"mean\")", cfg.LocalPooling)
	}
	return maxTokens, meanPool, nil
}

// initBundledLocked sets up the bundled bge-small-en-v1.5 session.
// Caller holds p.mu.
func (p *LocalProvider) initBundledLocked(modelDir string) (int, error) {
	// Load tokenizer.
	tokPath := filepath.Join(modelDir, "tokenizer.json")
	tok, err := pretrained.FromFile(tokPath)
	if err != nil {
		return 0, fmt.Errorf("local provider: load tokenizer: %w", err)
	}
	p.tok = tok

	p.dim = localModelDim
	p.maxTokens = localMaxTokens
	p.meanPool = false // bge encodes sentence meaning into [CLS]
	p.inputNames = []string{"input_ids", "attention_mask", "token_type_ids"}

	modelPath := filepath.Join(modelDir, "model_int8.onnx")
	session, err := p.newSession(modelPath, p.inputNames, "last_hidden_state")
	if err != nil {
		return 0, err
	}
	p.session = session

	slog.Info("local embed provider initialized",
		"model", "bge-small-en-v1.5",
		"dimension", p.dim,
		"model_dir", modelDir,
	)

	return p.dim, nil
}

// initUserModelLocked sets up a user-supplied ONNX model (issue #583).
// Every failure is fatal to Init: an explicitly configured user model must
// never fall back to the bundled one — the same principle #582 established
// for explicitly configured network providers. Caller holds p.mu.
func (p *LocalProvider) initUserModelLocked(cfg ProviderHTTPConfig) (int, error) {
	modelPath, tokPath := cfg.LocalModelPath, cfg.LocalTokenizerPath
	maxTokens, meanPool, err := validateUserModelConfig(cfg)
	if err != nil {
		return 0, err
	}

	tok, err := pretrained.FromFile(tokPath)
	if err != nil {
		return 0, fmt.Errorf("local provider: load user tokenizer %s: %w", tokPath, err)
	}

	// Read the model's declared inputs/outputs and feed exactly what it
	// declares — XLM-R-family exports often omit token_type_ids.
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return 0, fmt.Errorf("local provider: read user model metadata %s: %w", modelPath, err)
	}
	inputNames, err := resolveModelInputs(inputs)
	if err != nil {
		return 0, fmt.Errorf("local provider: user model %s: %w", modelPath, err)
	}
	outputName, err := resolveModelOutput(outputs)
	if err != nil {
		return 0, fmt.Errorf("local provider: user model %s: %w", modelPath, err)
	}

	session, err := p.newSession(modelPath, inputNames, outputName)
	if err != nil {
		return 0, err
	}

	p.tok = tok
	p.maxTokens = maxTokens
	p.meanPool = meanPool
	p.inputNames = inputNames
	p.session = session

	// Probe: the model's true output dimension comes from running one real
	// inference — a user-declared number would be a second source of truth
	// that can disagree with reality (#583).
	dim, err := p.probeDimensionLocked()
	if err != nil {
		_ = session.Destroy()
		p.session = nil
		return 0, fmt.Errorf("local provider: user model probe failed for %s: %w", modelPath, err)
	}
	p.dim = dim

	pooling := "cls"
	if meanPool {
		pooling = "mean"
	}
	slog.Info("local embed provider initialized",
		"model", filepath.Base(modelPath),
		"model_path", modelPath,
		"dimension", p.dim,
		"pooling", pooling,
		"max_tokens", p.maxTokens,
		"inputs", p.inputNames,
	)

	return p.dim, nil
}

// newSession creates the ORT session. DynamicAdvancedSession does not bind
// tensors at init time — tensors are passed per Run() call at the actual
// batch size.
func (p *LocalProvider) newSession(modelPath string, inputNames []string, outputName string) (*ort.DynamicAdvancedSession, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("local provider: ORT session options: %w", err)
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

	session, err := ort.NewDynamicAdvancedSession(modelPath, inputNames, []string{outputName}, opts)
	if err != nil {
		return nil, fmt.Errorf("local provider: create ORT session: %w", err)
	}
	return session, nil
}

// resolveModelInputs returns the model's declared input names. A BERT-style
// text encoder must declare input_ids and attention_mask; token_type_ids is
// the only genuinely optional input (XLM-R-family exports omit it). Anything
// else is refused.
func resolveModelInputs(inputs []ort.InputOutputInfo) ([]string, error) {
	known := map[string]bool{"input_ids": true, "attention_mask": true, "token_type_ids": true}
	declared := make(map[string]bool, len(inputs))
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if !known[in.Name] {
			return nil, fmt.Errorf("model declares input %q, which this provider cannot produce (supported: input_ids, attention_mask, token_type_ids)", in.Name)
		}
		declared[in.Name] = true
		names = append(names, in.Name)
	}
	if !declared["input_ids"] || !declared["attention_mask"] {
		return nil, fmt.Errorf("model must declare input_ids and attention_mask inputs, got %v", names)
	}
	return names, nil
}

// resolveModelOutput picks the hidden-state output: "last_hidden_state" when
// present, otherwise the model's single output, otherwise "token_embeddings".
// A configurable output tensor name is deliberately deferred until a model
// actually needs it (#583).
func resolveModelOutput(outputs []ort.InputOutputInfo) (string, error) {
	for _, out := range outputs {
		if out.Name == "last_hidden_state" {
			return out.Name, nil
		}
	}
	if len(outputs) == 1 {
		return outputs[0].Name, nil
	}
	// A sentence-transformers ONNX export names the per-token tensor
	// "token_embeddings" and declares a pooled "sentence_embedding" beside
	// it. The per-token tensor has the same [batch, sequence, dim] shape as
	// last_hidden_state, so the configured pooling applies to it unchanged.
	// The pooled output is never picked: this provider pools for itself.
	for _, out := range outputs {
		if out.Name == "token_embeddings" {
			return out.Name, nil
		}
	}
	names := make([]string, len(outputs))
	for i, out := range outputs {
		names[i] = out.Name
	}
	return "", fmt.Errorf("cannot pick an output tensor: no \"last_hidden_state\" and %d outputs declared %v", len(outputs), names)
}

// probeDimensionLocked runs one real inference with an ORT-allocated output
// tensor and derives the model's output dimension from the returned shape
// [batch, sequence, dim]. It also validates that the probe vector is finite
// and non-zero. Caller holds p.mu; p.session, p.tok, p.maxTokens and
// p.inputNames must be set.
func (p *LocalProvider) probeDimensionLocked() (int, error) {
	inputs, _, cleanup, err := p.packBatchLocked([]string{"dimension probe"})
	if err != nil {
		return 0, err
	}
	defer cleanup()

	// A nil output makes ORT allocate it, so the true output shape can be
	// read back instead of being assumed.
	outputs := []ort.Value{nil}
	if err := p.session.Run(inputs, outputs); err != nil {
		return 0, fmt.Errorf("probe inference: %w", err)
	}
	if outputs[0] == nil {
		return 0, fmt.Errorf("probe inference returned no output")
	}
	defer outputs[0].Destroy()

	tensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return 0, fmt.Errorf("probe output is not a float32 tensor")
	}
	shape := tensor.GetShape()
	if len(shape) != 3 {
		return 0, fmt.Errorf("probe output has rank %d, want 3 ([batch, sequence, dim])", len(shape))
	}
	// EmbedBatch pre-allocates its output as [batch, maxTokens, dim]; a model
	// whose output sequence axis differs from the padded input length would
	// pass a dim-only probe and then fail every real embedding call — reject
	// it here, at init, instead (#583 fail-loud).
	if int(shape[1]) != p.maxTokens {
		return 0, fmt.Errorf("probe output sequence length %d does not match the padded input length %d — this model's output shape is unsupported", shape[1], p.maxTokens)
	}
	dim := int(shape[2])
	if dim <= 0 {
		return 0, fmt.Errorf("probe output reports non-positive dimension %d", dim)
	}

	// Validate the first token vector: all-zero or non-finite output means
	// the model ran but produced garbage — fail now, not at recall time.
	data := tensor.GetData()
	if len(data) < dim {
		return 0, fmt.Errorf("probe output holds %d floats, want at least %d", len(data), dim)
	}
	allZero := true
	for _, v := range data[:dim] {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return 0, fmt.Errorf("probe output contains non-finite values")
		}
		if v != 0 {
			allZero = false
		}
	}
	if allZero {
		return 0, fmt.Errorf("probe output is all zeros")
	}

	return dim, nil
}

// packBatchLocked tokenizes texts and packs them into one input tensor per
// input the model declares, in p.inputNames order. It also returns the flat
// attention-mask buffer ([len(texts) * p.maxTokens], shared with the
// attention_mask tensor — valid until cleanup runs) for pooling, and a
// cleanup that destroys the tensors. Caller holds p.mu.
func (p *LocalProvider) packBatchLocked(texts []string) ([]ort.Value, []int64, func(), error) {
	batchSize := len(texts)
	inShape := ort.NewShape(int64(batchSize), int64(p.maxTokens))

	values := make([]ort.Value, 0, len(p.inputNames))
	cleanup := func() {
		for _, v := range values {
			_ = v.Destroy()
		}
	}

	// resolveModelInputs guarantees input_ids and attention_mask are declared;
	// only token_type_ids is optional (typeBuf stays nil when absent).
	var idsBuf, maskBuf, typeBuf []int64
	for _, name := range p.inputNames {
		t, err := ort.NewEmptyTensor[int64](inShape)
		if err != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("local provider: alloc %s: %w", name, err)
		}
		values = append(values, t)
		buf := t.GetData()
		// Explicitly zero — the ORT allocator does not guarantee zeroed memory.
		for i := range buf {
			buf[i] = 0
		}
		switch name {
		case "input_ids":
			idsBuf = buf
		case "attention_mask":
			maskBuf = buf
		case "token_type_ids":
			typeBuf = buf
		}
	}

	// Tokenize and pack each text into the batch tensors.
	for i, text := range texts {
		enc, encErr := p.tok.EncodeSingle(text, true)
		if encErr != nil {
			cleanup()
			return nil, nil, nil, fmt.Errorf("local provider: tokenize text[%d]: %w", i, encErr)
		}
		ids := enc.GetIds()
		mask := enc.GetAttentionMask()
		typeIDs := enc.GetTypeIds()

		seqLen := len(ids)
		if seqLen > p.maxTokens {
			seqLen = p.maxTokens
		}
		offset := i * p.maxTokens
		for j := 0; j < seqLen; j++ {
			idsBuf[offset+j] = int64(ids[j])
			maskBuf[offset+j] = int64(mask[j])
			if typeBuf != nil {
				typeBuf[offset+j] = int64(typeIDs[j])
			}
		}
	}

	return values, maskBuf, cleanup, nil
}

// EmbedBatch tokenizes up to localMaxBatch texts, runs a single ORT inference
// call for the whole batch, and returns the concatenated embeddings
// (len(texts) * dim floats). The caller (BatchEmbedder) ensures
// len(texts) <= localMaxBatch.
//
// The call first waits for its lane (query or batch — see LocalProvider) and
// gives up with ctx's error the moment ctx ends while it is queued, so a
// caller's deadline is honoured while waiting. An inference that has STARTED
// cannot be interrupted (ORT offers no cancellation here), so a deadline
// shorter than one inference — roughly 20-40 ms for a short query on the
// bundled model, ~0.2 s for a full batch — can still be overrun by up to that
// much.
func (p *LocalProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lane := p.laneFor(ctx, len(texts))
	if err := lane.acquire(ctx); err != nil {
		return nil, err
	}
	if p.onLaneAcquired != nil {
		p.onLaneAcquired(lane.name)
	}
	defer func() {
		if p.onLaneReleased != nil {
			p.onLaneReleased(lane.name)
		}
		lane.release()
	}()

	if p.session == nil {
		return nil, fmt.Errorf("local provider not initialized")
	}

	batchSize := len(texts)
	inputs, poolMask, cleanup, err := p.packBatchLocked(texts)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	outShape := ort.NewShape(int64(batchSize), int64(p.maxTokens), int64(p.dim))
	outputTensor, err := ort.NewEmptyTensor[float32](outShape)
	if err != nil {
		return nil, fmt.Errorf("local provider: alloc output: %w", err)
	}
	defer outputTensor.Destroy()

	// Single ORT inference call for the entire batch.
	if err := p.session.Run(inputs, []ort.Value{outputTensor}); err != nil {
		return nil, fmt.Errorf("local provider: ORT run: %w", err)
	}

	// Unpack output shape [batchSize, maxTokens, dim] and pool each sequence:
	// the [CLS] token (position 0) for bge-family models, masked mean for
	// mean-pooled families (e.g. e5); then L2-normalise.
	hidden := outputTensor.GetData()
	result := make([]float32, 0, batchSize*p.dim)
	seqStride := p.maxTokens * p.dim
	for i := 0; i < batchSize; i++ {
		seqHidden := hidden[i*seqStride : (i+1)*seqStride]
		var vec []float32
		if p.meanPool {
			vec = meanPool(seqHidden, poolMask[i*p.maxTokens:(i+1)*p.maxTokens], p.maxTokens, p.dim)
		} else {
			vec = clsPool(seqHidden, p.dim)
		}
		l2Normalize(vec)
		result = append(result, vec...)
	}

	return result, nil
}

// Close releases the ORT session.
func (p *LocalProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Wait for any in-flight inference: destroying the session under a Run
	// would be a use-after-free in ORT.
	release, err := p.holdAllLanes(context.Background())
	if err != nil {
		return err
	}
	defer release()

	if p.session != nil {
		_ = p.session.Destroy()
		p.session = nil
	}
	return nil
}

// lanes returns the provider's two lanes, creating them on first use so the
// zero-value LocalProvider{} (how NewEmbedService and tests construct it)
// works.
func (p *LocalProvider) lanes() (query, batch *embedLane) {
	p.lanesOnce.Do(func() {
		p.queryLane = newEmbedLane("query")
		p.batchLane = newEmbedLane("batch")
	})
	return p.queryLane, p.batchLane
}

// HoldLaneForTest occupies the query lane (query=true) or the batch lane
// exactly as an in-flight inference would, and returns its release. It is a
// test seam for packages that cannot reach the lanes directly — recall's
// degrade path models "the embedder is busy" with it deterministically instead
// of racing a real multi-second batch. Production code never calls it.
func (p *LocalProvider) HoldLaneForTest(query bool) (release func()) {
	q, b := p.lanes()
	l := b
	if query {
		l = q
	}
	_ = l.acquire(context.Background()) // Background never ends: cannot fail
	return l.release
}

// laneFor picks the lane from the caller's explicit signal — never from the
// call's shape: a single-text write embed is still background work.
func (p *LocalProvider) laneFor(ctx context.Context, n int) *embedLane {
	query, batch := p.lanes()
	if embedlane.IsQuery(ctx) && n <= localQueryLaneMaxTexts {
		return query
	}
	return batch
}

// holdAllLanes acquires both lanes (batch first, then query — the only code
// path that takes two, so the order cannot deadlock) for Init and Close.
func (p *LocalProvider) holdAllLanes(ctx context.Context) (func(), error) {
	query, batch := p.lanes()
	if err := batch.acquire(ctx); err != nil {
		return nil, err
	}
	if err := query.acquire(ctx); err != nil {
		batch.release()
		return nil, err
	}
	return func() {
		query.release()
		batch.release()
	}, nil
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
		"model_int8.onnx": embeddedModel,
		"tokenizer.json":  embeddedTokenizer,
		nativeLibFilename: embeddedNativeLib,
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
