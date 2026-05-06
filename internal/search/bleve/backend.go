package bleve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	blevesearch "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// Backend implements search.Backend using Bleve for full-text search.
// FTS and vector indexes are stored separately so the vector index can be
// rebuilt on dimension changes without losing FTS data.
type Backend struct {
	cfg  Config
	mu   sync.RWMutex
	fts  map[[8]byte]blevesearch.Index // full-text search per vault
	vec  map[[8]byte]blevesearch.Index // vector search per vault
	docs map[string]document           // cached latest document state
}

var _ search.Backend = (*Backend)(nil)

// Open opens a Bleve backend rooted at cfg.Path.
// FTS indexes live under <path>/fts/<vault>/; vector indexes under <path>/vec/<vault>/.
func Open(cfg Config) (*Backend, error) {
	if cfg.Path == "" {
		return nil, errors.New("bleve search: path is required")
	}
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil {
		return nil, fmt.Errorf("bleve search: create root: %w", err)
	}
	return &Backend{
		cfg:  cfg,
		fts:  make(map[[8]byte]blevesearch.Index),
		vec:  make(map[[8]byte]blevesearch.Index),
		docs: make(map[string]document),
	}, nil
}

// -- path helpers ---------------------------------------------------------

func (b *Backend) ftsPath(ws [8]byte) string {
	return filepath.Join(b.cfg.Path, "fts", fmt.Sprintf("%x", ws))
}

func (b *Backend) vecPath(ws [8]byte) string {
	return filepath.Join(b.cfg.Path, "vec", fmt.Sprintf("%x", ws))
}

func (b *Backend) vecDimFile(ws [8]byte) string {
	return filepath.Join(b.vecPath(ws), ".vectordim")
}

// -- index open/create helpers --------------------------------------------

// ftsForVault returns the FTS-only index for a vault, creating it if necessary.
// This index never changes dimension — it stores text fields exclusively.
func (b *Backend) ftsForVault(ws [8]byte) (blevesearch.Index, error) {
	if b == nil {
		return nil, nil
	}
	b.mu.RLock()
	idx := b.fts[ws]
	b.mu.RUnlock()
	if idx != nil {
		return idx, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if idx = b.fts[ws]; idx != nil {
		return idx, nil
	}

	path := b.ftsPath(ws)
	if _, statErr := os.Stat(filepath.Join(path, "index_meta.json")); errors.Is(statErr, os.ErrNotExist) {
		idx, err := blevesearch.New(path, buildFTSMapping(b.cfg))
		if err != nil {
			return nil, fmt.Errorf("bleve search: create FTS index: %w", err)
		}
		b.fts[ws] = idx
		return idx, nil
	}

	existing, err := blevesearch.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bleve search: open FTS index: %w", err)
	}
	b.fts[ws] = existing
	return existing, nil
}

// vecForVault returns the vector index for a vault.
// If the stored vector dimension differs from the current config, the old
// index is discarded and a fresh one created — FTS is unaffected.
func (b *Backend) vecForVault(ws [8]byte) (blevesearch.Index, error) {
	if b == nil || b.cfg.VectorDim <= 0 {
		return nil, nil
	}
	b.mu.RLock()
	idx := b.vec[ws]
	b.mu.RUnlock()
	if idx != nil && hasMatchingVectorDim(idx, b.cfg.VectorDim) {
		return idx, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if idx = b.vec[ws]; idx != nil {
		if hasMatchingVectorDim(idx, b.cfg.VectorDim) {
			return idx, nil
		}
		_ = idx.Close()
		delete(b.vec, ws)
	}

	path := b.vecPath(ws)
	dimFile := b.vecDimFile(ws)

	if stored, err := readVectorDim(dimFile); err == nil && stored > 0 && stored != b.cfg.VectorDim {
		slog.Warn("bleve: vector dimension changed — rebuilding vector index (FTS is preserved). Vectors will be repopulated on next reembed or engram update.",
			"path", path, "stored_dim", stored, "new_dim", b.cfg.VectorDim)
		_ = os.RemoveAll(path)
	}

	idx, err := blevesearch.New(path, buildVecMapping(b.cfg))
	if err != nil {
		return nil, fmt.Errorf("bleve search: create vector index: %w", err)
	}
	_ = writeVectorDim(dimFile, b.cfg.VectorDim)
	b.vec[ws] = idx
	return idx, nil
}

// -- dimension helpers ----------------------------------------------------

func readVectorDim(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	d, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return d, nil
}

func writeVectorDim(path string, dim int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(dim)+"\n"), 0o644)
}

func hasMatchingVectorDim(idx blevesearch.Index, dim int) bool {
	if dim <= 0 {
		return true
	}
	im, ok := idx.Mapping().(*mapping.IndexMappingImpl)
	if !ok || im == nil || im.DefaultMapping == nil {
		return false
	}
	for _, fm := range im.DefaultMapping.Fields {
		if fm == nil || fm.Dims <= 0 {
			continue
		}
		return fm.Dims == dim
	}
	return true
}

// -- index mappings -------------------------------------------------------

// buildFTSMapping creates a text-only index mapping (no vector field).
func buildFTSMapping(cfg Config) *mapping.IndexMappingImpl {
	im := blevesearch.NewIndexMapping()
	im.DefaultAnalyzer = cfg.analyzer()
	doc := blevesearch.NewDocumentMapping()

	idField := blevesearch.NewTextFieldMapping()
	idField.Store = true
	idField.Index = false
	doc.AddFieldMappingsAt("id", idField)

	conceptField := blevesearch.NewTextFieldMapping()
	conceptField.Analyzer = cfg.analyzer()
	doc.AddFieldMappingsAt("concept", conceptField)

	contentField := blevesearch.NewTextFieldMapping()
	contentField.Analyzer = cfg.analyzer()
	doc.AddFieldMappingsAt("content", contentField)

	tagsField := blevesearch.NewTextFieldMapping()
	tagsField.Analyzer = cfg.analyzer()
	doc.AddFieldMappingsAt("tags", tagsField)

	createdByField := blevesearch.NewTextFieldMapping()
	createdByField.Analyzer = cfg.analyzer()
	doc.AddFieldMappingsAt("created_by", createdByField)

	im.DefaultMapping = doc
	return im
}

// buildVecMapping creates a vector-first index mapping.
// Filter fields (created_by, tags as keyword; created_at as numeric) are
// included so that AddKNNWithFilter pre-filtering works.
func buildVecMapping(cfg Config) *mapping.IndexMappingImpl {
	im := blevesearch.NewIndexMapping()
	doc := blevesearch.NewDocumentMapping()

	idField := blevesearch.NewTextFieldMapping()
	idField.Store = true
	idField.Index = false
	doc.AddFieldMappingsAt("id", idField)

	vecField := mapping.NewVectorFieldMapping()
	vecField.Dims = cfg.VectorDim
	vecField.Similarity = cfg.similarity()
	vecField.VectorIndexOptimizedFor = cfg.VectorOptimizedFor
	doc.AddFieldMappingsAt("embedding", vecField)

	kw := blevesearch.NewTextFieldMapping()
	kw.Analyzer = "keyword"
	doc.AddFieldMappingsAt("created_by", kw)
	doc.AddFieldMappingsAt("tags", kw)

	createdAtField := blevesearch.NewNumericFieldMapping()
	doc.AddFieldMappingsAt("created_at", createdAtField)

	im.DefaultMapping = doc
	return im
}

// -- document helpers -----------------------------------------------------

func docID(ws [8]byte, id [16]byte) string {
	return fmt.Sprintf("%x:%s", ws, storage.ULID(id).String())
}

type document struct {
	ID        string    `json:"id"`
	Concept   string    `json:"concept"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	CreatedBy string    `json:"created_by"`
	CreatedAt int64     `json:"created_at"`
	Embedding []float32 `json:"embedding,omitempty"`
}

func buildDocument(eng *storage.Engram) document {
	id := [16]byte(eng.ID)
	return document{
		ID:        storage.ULID(id).String(),
		Concept:   eng.Concept,
		Content:   eng.Content,
		Tags:      eng.Tags,
		CreatedBy: eng.CreatedBy,
		CreatedAt: eng.CreatedAt.Unix(),
		Embedding: eng.Embedding,
	}
}

// -- TextIndexer ----------------------------------------------------------

// IndexText writes text fields into the FTS index. Embedding is not indexed here.
func (b *Backend) IndexText(_ context.Context, ws [8]byte, eng *storage.Engram) error {
	if b == nil || eng == nil {
		return nil
	}
	idx, err := b.ftsForVault(ws)
	if err != nil || idx == nil {
		return err
	}
	key := docID(ws, [16]byte(eng.ID))
	doc := buildDocument(eng)
	// Preserve any existing vector that arrived before text.
	b.mu.Lock()
	if prev, ok := b.docs[key]; ok && len(prev.Embedding) > 0 && len(doc.Embedding) == 0 {
		doc.Embedding = prev.Embedding
	}
	b.docs[key] = doc
	b.mu.Unlock()
	return idx.Index(key, doc)
}

// DeleteText removes the document from both FTS and vector indexes.
func (b *Backend) DeleteText(_ context.Context, ws [8]byte, id [16]byte) error {
	key := docID(ws, id)
	b.mu.Lock()
	delete(b.docs, key)
	b.mu.Unlock()

	// Best-effort delete from both indexes.
	if ftsIdx, _ := b.ftsForVault(ws); ftsIdx != nil {
		_ = ftsIdx.Delete(key)
	}
	if vecIdx, _ := b.vecForVault(ws); vecIdx != nil {
		_ = vecIdx.Delete(key)
	}
	return nil
}

// SearchText searches the FTS index and returns scored IDs.
func (b *Backend) SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]search.Hit, error) {
	if topK <= 0 {
		return nil, nil
	}
	idx, err := b.ftsForVault(ws)
	if err != nil || idx == nil {
		return nil, err
	}
	q := blevesearch.NewMatchQuery(query)
	req := blevesearch.NewSearchRequest(q)
	req.Size = topK
	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bleve search text: %w", err)
	}
	out := make([]search.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		_, idText, ok := strings.Cut(h.ID, ":")
		if !ok {
			continue
		}
		id, err := storage.ParseULID(idText)
		if err == nil {
			out = append(out, search.Hit{ID: [16]byte(id), Score: h.Score})
		}
	}
	return out, nil
}

// -- VectorIndexer / vector helpers ---------------------------------------

// indexVector writes the embedding and filter fields into the vector index.
func (b *Backend) indexVector(_ context.Context, ws [8]byte, id [16]byte, vec []float32) error {
	if b == nil || len(vec) == 0 {
		return nil
	}
	idx, err := b.vecForVault(ws)
	if err != nil || idx == nil {
		return err
	}
	key := docID(ws, id)
	b.mu.Lock()
	doc := b.docs[key]
	doc.ID = storage.ULID(id).String()
	doc.Embedding = vec
	b.docs[key] = doc
	b.mu.Unlock()
	return idx.Index(key, doc)
}

// DeleteVector is a no-op — vectors are removed together with text via DeleteText.
func (b *Backend) DeleteVector(context.Context, [8]byte, [16]byte) error { return nil }

// -- VaultLifecycle -------------------------------------------------------

// ResetVault removes all data for a vault from both indexes.
func (b *Backend) ResetVault(_ context.Context, ws [8]byte) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close and remove FTS index.
	if idx := b.fts[ws]; idx != nil {
		_ = idx.Close()
		delete(b.fts, ws)
	}
	_ = os.RemoveAll(b.ftsPath(ws))

	// Close and remove vector index.
	if idx := b.vec[ws]; idx != nil {
		_ = idx.Close()
		delete(b.vec, ws)
	}
	_ = os.RemoveAll(b.vecPath(ws))

	// Clean document cache for this vault.
	prefix := fmt.Sprintf("%x:", ws)
	for k := range b.docs {
		if strings.HasPrefix(k, prefix) {
			delete(b.docs, k)
		}
	}
	return nil
}

// ReindexVault rebuilds FTS (text) and vector data for a vault by scanning engrams.
func (b *Backend) ReindexVault(ctx context.Context, ws [8]byte, scan func(func(*storage.Engram) error) error) error {
	if scan == nil {
		return nil
	}
	return scan(func(eng *storage.Engram) error {
		if err := b.IndexText(ctx, ws, eng); err != nil {
			return err
		}
		if len(eng.Embedding) > 0 {
			return b.indexVector(ctx, ws, [16]byte(eng.ID), eng.Embedding)
		}
		return nil
	})
}

// -- io.Closer ------------------------------------------------------------

func (b *Backend) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	fts := b.fts
	vec := b.vec
	b.fts = nil
	b.vec = nil
	b.mu.Unlock()

	var err error
	for _, idx := range fts {
		if e := idx.Close(); e != nil && err == nil {
			err = e
		}
	}
	for _, idx := range vec {
		if e := idx.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
