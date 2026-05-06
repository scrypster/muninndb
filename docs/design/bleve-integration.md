# MuninnDB × Bleve 集成设计方案

> **版本**: v1.0  
> **状态**: 待评审  
> **关联**: `internal/index/fts/`, `internal/index/hnsw/`, `internal/engine/activation/`

---

## 目录

1. [动机与目标](#1-动机与目标)
2. [现状分析](#2-现状分析)
3. [Bleve 能力映射](#3-bleve-能力映射)
4. [架构设计](#4-架构设计)
5. [配置集成](#5-配置集成)
6. [中文分析器支持](#6-中文分析器支持)
7. [动态向量维度处理](#7-动态向量维度处理)
8. [Bleve Document Mapping](#8-bleve-document-mapping)
9. [索引与搜索流程](#9-索引与搜索流程)
10. [实施路线图](#10-实施路线图)
11. [风险与缓解](#11-风险与缓解)
12. [接口定义参考](#12-接口定义参考)

---

## 1. 动机与目标

### 现状痛点

| 问题 | 描述 |
|------|------|
| **查询类型单一** | 当前 FTS 仅支持 keyword token 匹配，不支持 phrase/prefix/fuzzy/wildcard/regexp 查询 |
| **文本分析简陋** | 仅有英文字符规范化 + Porter2 词干化，无中文分词（CJK）和多语言支持 |
| **无中文搜索** | `tokenizeRaw` 只保留 `[a-zA-Z0-9 ]`，中文完全被 strip，无法建立倒排索引 |
| **自建 HNSW 内存开销大** | 全量向量在内存中维护图结构，无磁盘化向量索引选项 |
| **混合融合重复造轮** | RRF 融合逻辑手动实现，缺少评分解释（explain）和诊断能力 |
| **Faiss 能力未利用** | Faiss IVF 提供更低的磁盘/内存比，支持 SQ8 量化等压缩选项 |

### 目标

1. **文本搜索增强**：支持中文分词（CJK bigram）、多语言、phrase/fuzzy/prefix/wildcard 等查询类型
2. **向量搜索选项**：提供 Faiss BIVF 作为 HNSW 的替代/补充，降低内存占用
3. **混合搜索原生化**：利用 bleve 原生 RRF/RSF 融合，避免手动维护
4. **可观测性**：搜索结果 explain、facets、highlight 等诊断能力
5. **平滑迁移**：通过 feature flag 和 adapter 模式，零风险渐进切换

---

## 2. 现状分析

### 2.1 当前索引架构

```
┌──────────────────────────────────────────────────────────────────┐
│                          Engine                                  │
│  ┌────────────┐  ┌────────────────┐  ┌───────────────────────┐  │
│  │ fts.Index   │  │ hnsw.Registry │  │ activation.Engine     │  │
│  │ (Pebble KV) │  │ (In-Memory)   │  │ Phase1→6 scoring     │  │
│  └────────────┘  └────────────────┘  └───────────────────────┘  │
│          ▲                ▲                     ▲                │
│          │                │                     │                │
│  ┌───────┴────────┐  ┌───┴──────────┐  ┌───────┴───────────┐   │
│  │ FTS Adapter    │  │ HNSW Adapter │  │ FTSIndex/HNSWIndex│   │
│  │ (activation)   │  │ (activation) │  │ interfaces        │   │
│  └────────────────┘  └──────────────┘  └───────────────────┘   │
└──────────────────────────────────────────────────────────────────┘
```

### 2.2 当前 FTS 分析管线

```
文本输入 → strings.ToLower → 字符过滤(仅保留[a-zA-Z0-9 ]) → 分词 → 停用词过滤 → Porter2 词干化 → BM25 搜索
```

**关键问题 — 中文不兼容**：
```go
// internal/index/fts/fts.go:93-99
for _, r := range text {
    if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
        b.WriteRune(r)       // ⚠️ 中文字符 unicode.IsLetter(r) 返回 true
    } else {
        b.WriteRune(' ')     // 但这里不会进入 else 分支
    }
}
// 实际上中文会被保留，但 Porter2 对中文无意义
// 下一个问题是：单个中文字符被当作 "token"，与搜索查询逐字匹配效果差
```
`Tokenize("内存数据库")` → `["内存数据库"]` (Porter2 对中文不生效，整个词作为一个 token)

### 2.3 当前 HNSW 特性

| 参数 | 值 | 说明 |
|------|-----|------|
| M | 16 | 每层最大连接数 |
| M0 | 32 | 第0层最大连接数 |
| EfConstruction | 200 | 构建时 beam width |
| EfSearch | 50 | 查询时 beam width |
| 存储 | 全内存 + Pebble 持久化 | 启动时 LoadFromPebble |
| 相似度 | Cosine | 4路展开点积 |
| 多租户 | 每 vault 一个 Index | Registry 懒创建 |

---

## 3. Bleve 能力映射

### 3.1 FTS 能力对比

| 能力 | MuninnDB (当前) | Bleve | 集成后收益 |
|------|----------------|-------|-----------|
| 评分算法 | BM25 | BM25 + TF-IDF | 可配置，无退化 |
| 分词器 | 自定义 (英文) | standard / unicode / CJK | **中文支持** |
| 停用词 | 硬编码 35 词 | 多语言停用词表 | 更准确 |
| 词干化 | Porter2 | Snowball + Stempel | 多语言 |
| 短语查询 | ❌ | ✅ MatchPhraseQuery | 新能力 |
| 模糊查询 | ❌ | ✅ FuzzyQuery | 新能力 |
| 前缀/通配符 | ❌ | ✅ Prefix/Wildcard/Regexp | 新能力 |
| 同义词 | ❌ | ✅ SynonymQuery | 新能力 |
| 范围查询 | ❌ | ✅ RangeQuery | 新能力 |
| Facets | ❌ | ✅ | 未来 UI |
| 搜索结果解释 | ❌ | ✅ Explain | 可观测性 |

### 3.2 向量/KNN 能力对比

| 能力 | MuninnDB HNSW (当前) | Bleve KNN (Faiss) | 集成后 |
|------|---------------------|-------------------|--------|
| 算法 | HNSW (纯内存) | IVF-flat / IVF-SQ8 | 磁盘友好 |
| 内存模型 | 全量向量 + 图 | IVF 聚类中心 + 可选量化 | 更低内存 |
| 相似度 | Cosine | L2 / Cosine / Dot Product | 更多选择 |
| 预过滤 | ❌ | ✅ KNN + FilterQuery | 新能力 |
| 构建参数 | EfConstruction/M | ivf_nprobe_pct / max_codes_pct | 可调优 |
| 多向量字段 | ❌ | ✅ multi-KNN support | 灵活 |
| 混合搜索 | 手动 RRF | 原生 RRF + RSF + 两阶段 | 简化代码 |

### 3.3 Bleve CJK 分析器细节

Bleve 内置 `cjk` 分析器：
```
输入文本 → unicode tokenizer → cjk_width filter → lowercase filter → cjk_bigram filter → token 流
```

**Bigram 中文分词示例**：
- `"内存数据库"` → `["内存", "存数", "数据", "据库"]`
- `"全文搜索"` → `["全文", "文搜", "搜索"]`

**优势**：
- 无需额外中文分词库（如 jieba），零依赖
- 对短文本（概念/标签）效果优于单字匹配
- 对中英混合文本友善

**可扩展**：支持注册自定义分析器，后续可集成 jieba-go 等分词库。

---

## 4. 架构设计

> **v1.1 修正（upstream-friendly）**：本方案的核心不是“让 Engine 认识 Bleve”，而是“让 Bleve 适配现有索引接口”。  
> 之前把 `BleveSearch` 注入 `EngineConfig`、替换 `ftsWorker`、改写 `Write/Forget/ClearVault/Reindex` 等生命周期，会导致大量核心源码 diff，不利于 fork 后继续合并 upstream。  
> 正确方向：新增一个 **SearchBackend/Factory 边界层**，在启动 wiring 处选择 native 或 bleve；Engine 内部仍只依赖现有 `FTSIndex` / `HNSWIndex` / 写入索引接口。

### 4.0 为什么原设计不好

| 问题 | 后果 | 修正原则 |
|------|------|----------|
| 把 `BleveSearch` 作为 `EngineConfig` 字段 | Engine 被第三方后端污染，upstream 合并冲突大 | Engine 不出现 bleve 包名 |
| 只复用 activation 的 `FTSIndex/HNSWIndex` | 搜索可替换，但写入、删除、清库、重建仍绑死 `*fts.Index` / `*hnsw.Registry` | 抽象完整索引生命周期接口 |
| 为 Bleve 改 `Write/Batch/Evolve/AddChild/Forget/ClearVault` | 触碰热点业务路径，风险和冲突都高 | 业务路径调用统一 backend 接口 |
| Bleve 自带 worker | 与现有 `fts.Worker`/写入队列职责重叠 | 让现有 worker 依赖接口，而不是具体 `*fts.Index` |
| 配置方案侵入 server.go 大块 if/else | 启动代码膨胀，测试困难 | 用 factory 返回一组 adapters |

**结论**：当前 MuninnDB 已经有搜索接口雏形，但接口边界不完整。应先做一个小型、通用、upstream 可接受的接口抽象层，再把 native 和 bleve 都做成实现。

### 4.1 目标架构

```
┌──────────────────────────────────────────────────────────────────────┐
│                    Engine（不导入 bleve）                              │
│                                                                      │
│   只依赖 search.Backend 接口：                                        │
│   - TextIndexer / TextSearcher                                       │
│   - VectorIndexer / VectorSearcher                                   │
│   - VaultLifecycle / io.Closer                                       │
└──────────────────────────────────────────────────────────────────────┘
             ▲                                  ▲
             │                                  │
┌────────────┴────────────┐        ┌────────────┴────────────┐
│ search/native.Backend   │        │ search/bleve.Backend    │
│ wraps fts.Index + HNSW  │        │ wraps Bleve per-vault   │
│ no behavior change      │        │ implements same iface   │
└─────────────────────────┘        └─────────────────────────┘
```

### 4.2 接口优先的 Backend 模式

```go
// internal/search/backend.go
package search

type TextHit struct {
    ID    [16]byte
    Score float64
}

type VectorHit = TextHit

type TextIndexer interface {
    IndexText(ctx context.Context, ws [8]byte, eng *storage.Engram) error
    DeleteText(ctx context.Context, ws [8]byte, id [16]byte) error
}

type TextSearcher interface {
    SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]TextHit, error)
}

type VectorIndexer interface {
    IndexVector(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error
    DeleteVector(ctx context.Context, ws [8]byte, id [16]byte) error
}

type VectorSearcher interface {
    SearchVector(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]VectorHit, error)
}

type VaultLifecycle interface {
    ResetVault(ctx context.Context, ws [8]byte) error
    ReindexVault(ctx context.Context, ws [8]byte, scan func(func(*storage.Engram) error) error) error
}

type Backend interface {
    TextIndexer
    TextSearcher
    VectorIndexer
    VectorSearcher
    VaultLifecycle
    io.Closer
}
```

### 4.3 兼容现有 activation/trigger 的薄 adapter

`activation.FTSIndex` / `activation.HNSWIndex` 保持不变，通过薄 adapter 包装 `search.Backend`：

```go
// internal/search/adapters/activation.go
type ActivationFTS struct{ B search.TextSearcher }
func (a ActivationFTS) Search(ctx context.Context, ws [8]byte, q string, k int) ([]activation.ScoredID, error)

type ActivationVector struct{ B search.VectorSearcher }
func (a ActivationVector) Search(ctx context.Context, ws [8]byte, v []float32, k int) ([]activation.ScoredID, error)
```

这样 activation、trigger、autoassoc 可以继续使用原接口，不需要知道 native/bleve。

### 4.4 启动 wiring（唯一选择点）

```
┌─────────────────────────────────────────────────────────────────┐
│                    cmd/muninn/server.go                          │
│                                                                  │
│  backend := searchfactory.Open(cfg.Search, deps)                 │
│  actEngine := activation.New(store,                              │
│      adapters.ActivationFTS{B: backend},                         │
│      adapters.ActivationVector{B: backend},                      │
│      embedder)                                                   │
│                                                                  │
│  eng := engine.NewEngine(engine.EngineConfig{                    │
│      Store: store,                                               │
│      Search: backend, // 通用接口，不是 BleveSearch              │
│  })                                                              │
└─────────────────────────────────────────────────────────────────┘
```

Engine 里最多新增一个 `Search search.Backend` 字段；不会新增 `BleveIndex`、`VectorBackend`、`FTSBackend` 等多个后端特定字段。

---

## 5. 配置集成

### 5.1 配置文件扩展

#### muninn.yaml 新增

```yaml
# {dataDir}/muninn.yaml

# === 现有配置 ===
cluster:
  enabled: false
  node_id: ""
  # ... 现有字段保持不变

# === 新增：搜索引擎配置 ===
search:
  # 引擎选择："native" | "bleve"
  #   native — 使用自建 FTS + HNSW（默认，向后兼容）
  #   bleve  — 使用 bleve FTS + Faiss KNN
  engine: "native"

  # === Bleve 引擎参数（engine=bleve 时生效）===
  bleve:
    # FTS 分析器配置
    fts:
      # 默认分析器："standard" | "cjk" | "simple" | custom_analyzer_name
      default_analyzer: "cjk"

      # 各字段分析器覆盖（可选，未指定字段使用 default_analyzer）
      field_analyzers:
        concept: ""      # 空 = 使用 default_analyzer
        content: ""      # 空 = 使用 default_analyzer
        tags: ""         # 空 = 使用 default_analyzer
        created_by: ""   # 空 = 使用 default_analyzer

      # 自定义分析器（可选，用于注册非内置分析器）
      custom_analyzers:
        # 示例：中文 + 英文混合分析器
        mixed_zh_en:
          type: "custom"
          char_filters: []
          tokenizer: "unicode"
          token_filters: ["cjk_width", "lowercase", "cjk_bigram"]

        # 示例：jieba 分词分析器（需引入 jieba-go）
        # jieba:
        #   type: "custom"
        #   tokenizer: "jieba"    # 需注册 jieba tokenizer
        #   token_filters: ["lowercase"]

    # KNN 向量搜索配置
    knn:
      # 向量索引优化方向："recall" | "latency" | "memory-efficient"
      #   recall          — 最高召回率（BIVF+Flat，默认）
      #   latency         — 最低查询延迟（BIVF+SQ8）
      #   memory-efficient — 最小内存（BIVF+SQ8，更激进的压缩）
      vector_index_optimized_for: "recall"

      # 相似度度量："cosine" | "l2_norm" | "dot_product"
      similarity: "cosine"

      # Faiss IVF 搜索参数
      search_params:
        ivf_nprobe_pct: 10     # 搜索时探测的聚类百分比
        ivf_max_codes_pct: 1.0 # 最大扫描向量百分比

    # 混合搜索配置
    hybrid:
      # 融合策略："rrf" | "rsf" | "none"
      #   rrf  — Reciprocal Rank Fusion（推荐）
      #   rsf  — Relative Score Fusion
      #   none — 独立返回 FTS 和 KNN 结果（手动融合）
      fusion_strategy: "rrf"

      # FTS 查询在混合搜索中的权重
      fts_weight: 1.0

      # KNN 查询在混合搜索中的权重
      knn_weight: 1.0

    # 索引存储路径（相对于 dataDir）
    data_subdir: "bleve"

  # === 自建引擎参数（engine=native 时生效，保持现有行为）===
  native:
    # HNSW 参数（现有，通过 env 控制）
    # MUNINN_HNSW_WARN_THRESHOLD_MB
    # MUNINN_HNSW_MAX_MB

    # FTS 参数（现有）
    # 硬编码 BM25 k1=1.2, b=0.75
```

#### plugin_config.json 新增

```jsonc
// {dataDir}/plugin_config.json
{
  // === 现有字段 ===
  "embed_provider": "local",
  "embed_url": "",
  "embed_api_key": "",
  // ...

  // === 新增：搜索引擎切换 ===
  "search_engine": "native",
  // "native" — 使用自建索引（默认）
  // "bleve"  — 使用 bleve 索引
  // 此字段优先级高于 muninn.yaml 中的 search.engine

  "search_engine_fallback": true
  // true (默认) — bleve 不可用时自动回退到 native
  // false      — bleve 不可用时报错
}
```

### 5.2 配置加载优先级

```
启动顺序:
  1. env: MUNINN_SEARCH_ENGINE="bleve"           (最高优先级)
  2. plugin_config.json: search_engine            (次优先级)
  3. muninn.yaml: search.engine                   (配置文件)
  4. 默认: "native"                                (硬编码默认值)
```

### 5.3 对应的 Go 配置结构体

```go
// internal/config/search.go (新增)

package config

// SearchConfig holds all search engine configuration.
type SearchConfig struct {
    // Engine selects the search backend: "native" | "bleve"
    Engine string `yaml:"engine" json:"engine"`

    // Bleve holds bleve-specific configuration (active when Engine="bleve").
    Bleve BleveConfig `yaml:"bleve" json:"bleve"`

    // Native holds native engine configuration (active when Engine="native").
    Native NativeSearchConfig `yaml:"native" json:"native"`
}

// BleveConfig configures the bleve search backend.
type BleveConfig struct {
    FTS    BleveFTSConfig    `yaml:"fts" json:"fts"`
    KNN    BleveKNNConfig    `yaml:"knn" json:"knn"`
    Hybrid BleveHybridConfig `yaml:"hybrid" json:"hybrid"`

    // DataSubdir is the directory under dataDir for bleve index files.
    // Default: "bleve"
    DataSubdir string `yaml:"data_subdir" json:"data_subdir"`
}

// BleveFTSConfig configures full-text search in bleve.
type BleveFTSConfig struct {
    // DefaultAnalyzer is the analyzer used for all text fields unless
    // overridden in FieldAnalyzers. Default: "cjk"
    DefaultAnalyzer string `yaml:"default_analyzer" json:"default_analyzer"`

    // FieldAnalyzers maps field names to their specific analyzers.
    // Unspecified fields use DefaultAnalyzer.
    // Keys: "concept", "content", "tags", "created_by"
    FieldAnalyzers map[string]string `yaml:"field_analyzers" json:"field_analyzers"`

    // CustomAnalyzers registers custom analysis pipelines.
    // Uses bleve's AddCustomAnalyzer config format.
    // Key = analyzer name, Value = bleve analyzer config map.
    CustomAnalyzers map[string]map[string]interface{} `yaml:"custom_analyzers" json:"custom_analyzers"`
}

// BleveKNNConfig configures KNN vector search in bleve.
type BleveKNNConfig struct {
    // VectorIndexOptimizedFor selects index optimization: "recall" | "latency" | "memory-efficient"
    VectorIndexOptimizedFor string `yaml:"vector_index_optimized_for" json:"vector_index_optimized_for"`

    // Similarity selects the similarity metric: "cosine" | "l2_norm" | "dot_product"
    Similarity string `yaml:"similarity" json:"similarity"`

    // SearchParams are Faiss IVF search parameters.
    SearchParams BleveKNNSearchParams `yaml:"search_params" json:"search_params"`
}

// BleveKNNSearchParams holds Faiss IVF search parameters.
type BleveKNNSearchParams struct {
    // IvlNprobePct is the percentage of clusters to probe during search.
    // Range: 1-100, default: 10
    IvlNprobePct int `yaml:"ivf_nprobe_pct" json:"ivf_nprobe_pct"`

    // IvlMaxCodesPct is the percentage of total vectors to scan.
    // Range: 0.0-1.0, default: 1.0
    IvlMaxCodesPct float64 `yaml:"ivf_max_codes_pct" json:"ivf_max_codes_pct"`
}

// BleveHybridConfig configures hybrid (text + vector) search.
type BleveHybridConfig struct {
    // FusionStrategy selects fusion method: "rrf" | "rsf" | "none"
    FusionStrategy string `yaml:"fusion_strategy" json:"fusion_strategy"`

    // FTSWeight is the relative weight of FTS results: 0.0-10.0
    FTSWeight float64 `yaml:"fts_weight" json:"fts_weight"`

    // KNNWeight is the relative weight of KNN results: 0.0-10.0
    KNNWeight float64 `yaml:"knn_weight" json:"knn_weight"`
}

// NativeSearchConfig holds native engine configuration.
type NativeSearchConfig struct {
    // Currently no exposed config — HNSW thresholds use env vars.
}

// PluginConfig 扩展
//   PluginConfig 新增字段：
//     SearchEngine         string `json:"search_engine"`          // "native" | "bleve"
//     SearchEngineFallback bool   `json:"search_engine_fallback"` // 回退开关
```

### 5.4 `EngineConfig` 最小扩展

```go
// internal/engine/config.go

type EngineConfig struct {
    // === 现有字段尽量保留，用于兼容测试和 native 默认路径 ===
    Store            *storage.PebbleStore
    AuthStore        *auth.Store
    ActivationEngine *activation.ActivationEngine
    TriggerSystem    *trigger.TriggerSystem
    HebbianWorker    *cognitive.HebbianWorker
    ContradictWorker *cognitive.Worker[cognitive.ContradictItem]
    ConfidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate]
    Embedder         activation.Embedder

    // === 新增：通用搜索后端（不出现 bleve 包名） ===
    Search search.Backend
}
```

兼容策略：

1. 第一步可保留 `FTSIndex *fts.Index` / `HNSWRegistry *hnsw.Registry`，但标记为 legacy wiring。
2. 新代码只使用 `Search search.Backend`。
3. 测试逐步迁移到 `search/native.Backend`，最后再考虑移除 legacy 字段。
4. 禁止新增 `BleveSearch *...` 这类后端专属字段。

---

## 6. 中文分析器支持

### 6.1 内置 CJK 分析器

Bleve 内置 `cjk` 分析器，直接在 mapping 中指定：

```go
mapping := bleve.NewIndexMapping()

// 设置全局默认分析器为 CJK
mapping.DefaultAnalyzer = "cjk"

// 或仅对特定字段使用 CJK
docMapping := bleve.NewDocumentMapping()
conceptField := bleve.NewTextFieldMapping()
conceptField.Analyzer = "cjk"          // 中文 + 英文混合
contentField := bleve.NewTextFieldMapping()
contentField.Analyzer = "standard"      // 纯英文可用 standard
```

### 6.2 CJK 分析器管线

```
输入: "MuninnDB 内存数据库 Go 语言"

Step 1 — unicode tokenizer:
  ["MuninnDB", "内存数据库", "Go", "语言"]

Step 2 — cjk_width filter:
  ["muninndb", "内存数据库", "go", "语言"]  (全角→半角, uppercase→lowercase)

Step 3 — lowercase filter:
  ["muninndb", "内存数据库", "go", "语言"]  (已是小写，无变化)

Step 4 — cjk_bigram filter:
  ["muninndb", "内存", "存数", "数据", "据库", "go", "语言"]

输出 tokens:
  ["muninndb", "内存", "存数", "数据", "据库", "go", "语言"]
```

### 6.3 搜索效果对比

| 查询 | 当前 MuninnDB FTS | Bleve + CJK Analyzer |
|------|------------------|---------------------|
| `"内存数据库"` | 精确匹配 `"内存数据库"` 整词 | 匹配 `"内存"`, `"存数"`, `"数据"`, `"据库"` 任意组合 |
| `"全文搜索"` | 精确匹配 `"全文搜索"` | 匹配 `"全文"`, `"文搜"`, `"搜索"` |
| `"向量"` | 不匹配（词太短，<2 长度过滤） | 匹配 `"向量"` (bigram 产生的) |
| `"Go memory"` | 匹配 `"go"`, `"memory"` (stem→`"memori"`) | 匹配 `"go"`, `"memory"` (更准确) |

### 6.4 自定义中文分析器（可选扩展）

```go
// 注册 jieba 分词分析器（需引入 gojieba 依赖）
func init() {
    // 使用 bleve 的 registry 注册
    registry.RegisterTokenizer("jieba", func(config map[string]interface{}, cache *registry.Cache) (analysis.Tokenizer, error) {
        return NewJiebaTokenizer(), nil
    })

    registry.RegisterAnalyzer("chinese_jieba", func(config map[string]interface{}, cache *registry.Cache) (*analysis.Analyzer, error) {
        tokenizer, _ := cache.TokenizerNamed("jieba")
        cjkWidth, _ := cache.TokenFilterNamed("cjk_width")
        lowercase, _ := cache.TokenFilterNamed("lowercase")
        return &analysis.DefaultAnalyzer{
            Tokenizer: tokenizer,
            TokenFilters: []analysis.TokenFilter{cjkWidth, lowercase},
        }, nil
    })
}
```

### 6.5 配置文件中的分析器选择

```yaml
# muninn.yaml
search:
  bleve:
    fts:
      # 全局默认 CJK（支持中英混合）
      default_analyzer: "cjk"

      # 每字段可独立配置
      field_analyzers:
        concept: "cjk"       # 混合中英概念名
        content: "cjk"       # 可能含中文的内容
        tags: "standard"     # 纯英文标签
        created_by: "keyword" # 不需要分析的标识符

      # 注册自定义分析器
      custom_analyzers:
        # 严格中文分词（精确匹配模式）
        strict_cjk:
          type: "custom"
          tokenizer: "unicode"
          token_filters: ["cjk_width", "lowercase", "cjk_bigram"]
          char_filters: []
```

---

## 7. 动态字段与向量维度处理

### 7.1 Bleve 动态字段机制

Bleve 的 `NewDocumentMapping()` **默认 `Dynamic: true`**，意味着索引文档时遇到未预定义的字段会自动映射和索引：

```go
// bleve 默认行为
docMapping := bleve.NewDocumentMapping()
// docMapping.Dynamic == true  ← 默认开启！
```

**文本/数值字段** — 完全自动，无需重建索引：

```
Index("doc1", { concept: "Go memory", content: "...", tags: ["go"] })
          │
          ▼  bleve Dynamic=true → newTextFieldMappingDynamic
          │  自动映射 concept/content/tags 为 text 字段
          │
Index("doc2", { concept: "Python tips", author: "alice", extra: 42 })
          │
          ▼  Dynamic=true → author 自动映射为 text，extra 自动映射为 number
          │  无需重建索引，无需预定义 mapping
```

**向量字段** — 需要维度信息：

- `[]float32` 在 Dynamic 模式下会被索引为**重复数值字段**，而非向量字段
- 向量字段必须通过 `NewVectorFieldMapping()` **显式定义**，且 `Dims` 必须在索引创建前已知
- bleve 不支持运行时修改已创建索引的 mapping

### 7.2 解决方案：启动时维度发现

MuninnDB 的 embedder 在 **启动时即完全初始化**，向量维度已知：

```go
// Embedder 接口要求 Dims() 方法
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
    Dims() int  // ← 维度在启动时就已确定
}

// 启动流程：
//   1. buildEmbedder() → 根据 env/plugin_config.json 创建 embedder
//   2. embedder.Dims() → 得到向量维度（384/1536/...）
//   3. DimProvider(ws) → 返回维度
//   4. bleveSearch.getOrCreate(ws) → 创建含正确 Dims 的 IndexMapping
//   5. 后续所有 IndexEngram → 携带向量字段，正常索引
```

**正常流程 — 零重建**：

```
┌──────────────────────────────────────────────────────────────┐
│                    search/bleve.Backend Startup              │
│                                                              │
│  Step 1: DimProvider(ws) 查询维度                             │
│          ├─ 查 Pebble 已持久化的 vault embedDim → 384        │
│          ├─ 或查 embedder.Dims() → 384                       │
│          └─ 默认: embedder 不存在 → 0 (仅 FTS)               │
│                                                              │
│  Step 2: buildIndexMapping(384)                              │
│          文本字段: concept/content/tags/created_by (显式)     │
│          向量字段: embedding (显式, Dims=384)                 │
│          其余字段: Dynamic=true 自动处理                      │
│                                                              │
│  Step 3: 正常索引 — 无需重建                                  │
│          IndexEngram → 构建 Document → bleve.Index()          │
│          - 文本字段: bleve 自动分析/倒排                      │
│          - 向量字段: bleve 自动加入 Faiss 索引                │
│          - 新字段: Dynamic=true 自动映射                      │
└──────────────────────────────────────────────────────────────┘
```

### 7.3 极端情况：启动时维度未知

**唯一会触发重建的场景**：embedder 在首次写入**之后**才配置（如先启动空数据库，再通过 API 设置 embedder）。

对于此极端情况（非正常运维路径），提供两个选项：

| 选项 | 策略 | 适用 |
|------|------|------|
| **A: 延迟初始化** | 首次 IndexEngram 时无 vector 字段 → 仅文本索引；embedder 配置后**重启服务**，DimProvider 返回正确维度，getOrCreate 创建正确 mapping | 推荐（运维标准操作） |
| **B: 运行时迁移** | 记录 dims 到 Pebble metadata → Close bleve Index → 重建 mapping → 从 Pebble 扫描已有 engram 重新索引 | 极端场景（零停机要求） |

**选项 A 的 server.go wiring**：

```go
// DimProvider 实现 — 启动时查询
dimProvider := func(ws [8]byte) int {
    // 1. 查已持久化的维度（vault 元数据）
    if dim, ok := store.GetVaultEmbedDim(ws); ok {
        return dim
    }
    // 2. 查当前 embedder（启动时已初始化）
    if embedder != nil {
        if dimmer, ok := embedder.(interface{ Dims() int }); ok {
            return dimmer.Dims()
        }
    }
    // 3. 无 embedder → 仅 FTS，不启用 KNN
    return 0
}

bleveCfg.DimProvider = dimProvider
```

### 7.4 Pebble 元数据新增键

当 embedder 首次产生向量时（Remember 携带 embedding），持久化维度信息：

```go
// internal/storage/keys 新增
func VaultEmbedDimKey(ws [8]byte) []byte {
    key := make([]byte, 1+8+4)
    key[0] = 0x2D  // 新的命名空间前缀 (向量维度)
    copy(key[1:9], ws[:])
    binary.BigEndian.PutUint32(key[9:13], uint32(dims))
    return key[:13]
}
```

此键在以下场景写入：
1. 首次 Remember 携带 embedding 时 → `store.PersistVaultEmbedDim(ws, dims)`
2. embedder 切换后重启 → 服务检测到维度变化 → 警告日志 + 可选择重建

### 7.5 总结：Dynamic 模式下的字段处理

| 字段类型 | 预定义 mapping | Dynamic 行为 | 需要重建？ |
|---------|---------------|-------------|-----------|
| `id` / `vault` | Store=true, Index=false | — | ❌ |
| `concept` / `content` / `tags` / `created_by` | 显式 text 字段 + 指定分析器 | 不会被 Dynamic 覆盖 | ❌ |
| `embedding` (vector) | 显式 vector 字段 (需 Dims) | `[]float32` 不自动映射为 vector | ❌ (维度已知时) |
| 任意新文本/数值字段 | 无预定义 | `Dynamic=true` 自动创建 | ❌ 无需重建 |

**核心理念**：bleve 的 `Dynamic: true` 使得文本索引**完全不需要"重建"**——只有 vector 字段需要在创建 IndexMapping 时知道维度，而维度在 MuninnDB 启动时已通过 embedder 确定。

---

## 8. Bleve Document Mapping

### 8.1 完整 IndexMapping

```go
func (b *Backend) buildIndexMapping(vectorDim int) *mapping.IndexMappingImpl {
    im := bleve.NewIndexMapping()

    // 全局默认分析器
    if b.cfg.Bleve.FTS.DefaultAnalyzer != "" {
        im.DefaultAnalyzer = b.cfg.Bleve.FTS.DefaultAnalyzer
    } else {
        im.DefaultAnalyzer = "cjk" // 默认 CJK，兼容中英文
    }

    // 注册自定义分析器
    for name, cfg := range b.cfg.Bleve.FTS.CustomAnalyzers {
        im.AddCustomAnalyzer(name, cfg) //nolint:errcheck
    }

    // === 文档映射 ===
    docMapping := bleve.NewDocumentMapping()

    // ID 字段 — 存储，不分析
    idField := bleve.NewTextFieldMapping()
    idField.Store = true
    idField.Index = false  // 不需要搜索 ID
    docMapping.AddFieldMappingsAt("id", idField)

    // Vault 标识 — 存储，不分析
    vaultField := bleve.NewTextFieldMapping()
    vaultField.Store = true
    vaultField.Index = false
    docMapping.AddFieldMappingsAt("vault", vaultField)

    // Concept 字段 — 高权重
    conceptField := bleve.NewTextFieldMapping()
    conceptField.Analyzer = b.fieldAnalyzer("concept")
    conceptField.Boost = 3.0 // 对应 fieldWeightConcept
    docMapping.AddFieldMappingsAt("concept", conceptField)

    // Content 字段 — 主文本
    contentField := bleve.NewTextFieldMapping()
    contentField.Analyzer = b.fieldAnalyzer("content")
    contentField.Boost = 1.0 // 对应 fieldWeightContent
    docMapping.AddFieldMappingsAt("content", contentField)

    // Tags 字段 — 中权重
    tagsField := bleve.NewTextFieldMapping()
    tagsField.Analyzer = b.fieldAnalyzer("tags")
    tagsField.Boost = 2.0 // 对应 fieldWeightTags
    docMapping.AddFieldMappingsAt("tags", tagsField)

    // CreatedBy 字段 — 低权重
    createdByField := bleve.NewTextFieldMapping()
    createdByField.Analyzer = b.fieldAnalyzer("created_by")
    createdByField.Boost = 0.5 // 对应 fieldWeightCreatedBy
    docMapping.AddFieldMappingsAt("created_by", createdByField)

    // Vector 字段 — 仅当维度已知时添加
    if vectorDim > 0 {
        vecField := bleve.NewVectorFieldMapping()
        vecField.Dims = vectorDim
        vecField.Similarity = b.cfg.Bleve.KNN.Similarity
        if vecField.Similarity == "" {
            vecField.Similarity = "cosine"
        }
        vecField.VectorIndexOptimizedFor = b.cfg.Bleve.KNN.VectorIndexOptimizedFor
        if vecField.VectorIndexOptimizedFor == "" {
            vecField.VectorIndexOptimizedFor = "recall"
        }
        docMapping.AddFieldMappingsAt("embedding", vecField)
    }

    im.DefaultMapping = docMapping
    return im
}

// fieldAnalyzer returns the analyzer for a given field, falling back to DefaultAnalyzer.
func (b *Backend) fieldAnalyzer(field string) string {
    if a, ok := b.cfg.Bleve.FTS.FieldAnalyzers[field]; ok && a != "" {
        return a
    }
    return b.cfg.Bleve.FTS.DefaultAnalyzer
}
```

### 8.2 Document 构建

```go
func (b *Backend) buildDocument(ws [8]byte, id [16]byte, eng *storage.Engram) *document.Document {
    doc := document.NewDocument(string(id[:]))

    // 标识字段
    doc.AddField(document.NewTextField("id", string(id[:])))
    doc.AddField(document.NewTextField("vault", string(ws[:])))

    // 文本字段
    doc.AddField(document.NewTextField("concept", eng.Concept))
    doc.AddField(document.NewTextField("content", eng.Content))
    for _, tag := range eng.Tags {
        doc.AddField(document.NewTextField("tags", tag))
    }
    doc.AddField(document.NewTextField("created_by", eng.CreatedBy))

    // 向量字段
    if len(eng.Embedding) > 0 {
        // bleve 会自动调用 processVector 处理 float32 slice
        field := document.NewVectorFieldWithIndexingOptions("embedding", eng.Embedding, document.IndexField)
        doc.AddField(field)
    }

    return doc
}
```

---

## 9. 索引与搜索流程

### 9.1 索引流程（Remember 路径）

```
Remember(ctx, vault, concept, content)
        │
        ▼
  Engine.Write()
        │
        ▼
  PebbleStore.WriteEngram()           ← 持久化到 Pebble (同步)
        │
        ▼
  [async] FTS Worker / Bleve Worker
        │
        ├─ native 路径:
        │    fts.Index.IndexEngram()    ← 写入 Pebble 倒排索引
        │
        └─ bleve 路径:
             bleveSearch.IndexEngram()  ← 写入 bleve Index (boltdb)
                  │
                  ├─ 构建 bleve Document (文本 + 向量字段)
                  ├─ bleve.Index.Index()
                  └─ [可选] 记录 vault embed dim 到 Pebble metadata
```

### 9.2 搜索流程（Recall 路径）

```
Recall/Activate(ctx, vault, query, embedding)
        │
        ▼
  ActivationEngine.Run()
        │
  Phase 2: 并行候选检索
        │
        ├─ FTS Search:
        │    ├─ native: fts.Index.Search(ctx, ws, query, topK)
        │    └─ bleve:  bleveSearch.searchFTS(ctx, ws, query, topK)
        │               └─ bleve.SearchRequest{
        │                    Query: matchQuery / queryStringQuery
        │                    Size: topK
        │                    Explain: req.IncludeWhy }
        │
        ├─ HNSW/KNN Search:
        │    ├─ native: hnsw.Registry.Search(ctx, ws, vec, topK)
        │    └─ bleve:  bleveSearch.searchKNN(ctx, ws, vec, topK)
        │               └─ bleve.SearchRequest{
        │                    Query: matchNoneQuery
        │                    KNN: [{Field: "embedding", Vector: vec, K: topK}]
        │                  }
        │
        └─ [Phase 3 可选] bleve 混合搜索:
             └─ bleveSearch.searchHybrid(ctx, ws, query, vec, topK)
                └─ bleve.SearchRequest{
                     Query: matchQuery
                     KNN: [{Field: "embedding", Vector: vec, K: topK}]
                     KNNOperator: "or"
                     // bleve 自动执行 RRF 融合
                     // 通过 KNNCollector + TopNCollector 统一排序
                   }
```

### 9.3 FTS 搜索实现

```go
func (b *Backend) SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]search.Hit, error) {
    idx := b.getOrCreate(ws)

    // 构建 bleve 查询 — 支持更多查询类型
    var q query.Query
    switch {
    case b.cfg.Bleve.FTS.DefaultQueryType == "match_phrase":
        q = bleve.NewMatchPhraseQuery(query)
    case strings.Contains(query, "*") || strings.Contains(query, "?"):
        q = bleve.NewWildcardQuery(query)
    default:
        q = bleve.NewMatchQuery(query)
    }

    sr := bleve.NewSearchRequest(q)
    sr.Size = topK
    sr.Fields = []string{"id"} // 仅返回 ID 字段
    sr.Explain = true          // 支持解释

    // 按 vault 过滤 (通过添加 term query)
    // 注意: 由于我们按 vault 分割索引，此过滤并非必需
    // 但如果使用单索引多 vault 模式，则需要:
    // sr.AddKNNWithFilter(...)

    result, err := idx.SearchInContext(ctx, sr)
    if err != nil {
        return nil, fmt.Errorf("bleve fts search: %w", err)
    }

    out := make([]ScoredID, 0, len(result.Hits))
    for _, hit := range result.Hits {
        uid, err := storage.ParseULID(hit.ID)
        if err != nil {
            continue
        }
        out = append(out, ScoredID{ID: uid, Score: hit.Score})
    }
    return out, nil
}
```

### 9.4 KNN 搜索实现

```go
func (b *Backend) SearchVector(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]search.Hit, error) {
    idx := b.getOrCreate(ws)

    sr := bleve.NewSearchRequest(bleve.NewMatchNoneQuery())
    sr.AddKNN("embedding", vec, int64(topK), 1.0)
    sr.Size = topK
    sr.Fields = []string{"id"}

    result, err := idx.SearchInContext(ctx, sr)
    if err != nil {
        return nil, fmt.Errorf("bleve knn search: %w", err)
    }

    out := make([]ScoredID, 0, len(result.Hits))
    for _, hit := range result.Hits {
        uid, err := storage.ParseULID(hit.ID)
        if err != nil {
            continue
        }
        out = append(out, ScoredID{ID: uid, Score: hit.Score})
    }
    return out, nil
}
```

### 9.5 删除流程

```go
func (b *Backend) DeleteText(ctx context.Context, ws [8]byte, id [16]byte) error {
    idx := b.getOrCreate(ws)
    return idx.Delete(string(id[:]))
}
```

---

## 10. 实施路线图

### Phase 1: 基础设施 (约 2 周)

```
□ 1.1 抽象通用搜索接口（先于 Bleve）
├── internal/search/backend.go
├── TextIndexer / TextSearcher
├── VectorIndexer / VectorSearcher
├── VaultLifecycle / Backend
├── adapters/activation.go / adapters/trigger.go
│
□ 1.2 Native backend 适配（行为零变化）
├── internal/search/native/backend.go
├── wraps *fts.Index + *hnsw.Registry
├── 现有 activation/trigger 通过 adapters 包装 search.Backend
├── 现有 fts.Worker 改为依赖 TextIndexer 接口（小 diff）
│
□ 1.3 引入 bleve/v2 依赖到 go.mod
├── go get github.com/blevesearch/bleve/v2
│
□ 1.4 创建 internal/search/bleve/ 包
├── config.go          — BleveConfig 结构体 + 默认值
├── backend.go         — Backend 主结构体（实现 search.Backend）
├── mapping.go         — buildIndexMapping + buildDocument
├── text.go            — IndexText / SearchText / DeleteText
├── vector.go          — IndexVector / SearchVector / DeleteVector
├── hybrid_search.go   — searchHybrid (统一搜索)
├── lifecycle.go       — Close / ResetVault / Stats
└── adapter_test.go    — 单元测试
│
□ 1.5 扩展配置系统
├── internal/config/search.go  — SearchConfig + 加载/保存
├── 扩展 PluginConfig (SearchEngine, SearchEngineFallback)
├── 扩展 muninn.yaml schema (search.bleve.*)
│
□ 1.6 最小集成到 EngineConfig
└── EngineConfig.Search search.Backend（禁止 Bleve 专属字段）
```

### Phase 2: FTS 集成 (约 2 周)

```
□ 2.1 实现 search/bleve.Backend (仅 FTS, 不含 Vector)
├── buildIndexMapping(vectorDim=0)
├── IndexText (文本字段)
├── SearchText (MatchQuery/WildcardQuery/MatchPhraseQuery)
├── DeleteText
├── 复用现有异步 worker / 后台索引入口
│
□ 2.2 集成到 search factory
├── feature flag: MUNINN_SEARCH_ENGINE=bleve
├── wiring: factory 返回 search.Backend
├── fallback: bleve 失败 → native
│
□ 2.3 测试
├── 单元测试: 对比 bleve Search 与 fts.Index.Search 结果
├── 集成测试: Remember → Recall 端到端
├── 中文测试: 中文概念/内容索引与搜索
├── 性能测试: 基准对比 (latency, throughput, memory)
└── 回归测试: native 路径不受影响
│
□ 2.4 文档
└── docs/design/bleve-integration.md (本文档)
```

### Phase 3: KNN 集成 (约 2 周)

```
□ 3.1 build tag 支持
├── //go:build vectors
├── knn_search.go        — SearchKNN (有 vectors tag)
├── knn_search_noop.go   — SearchKNN stub (无 vectors tag)
│
□ 3.2 启动时维度发现
├── DimProvider 接口（启动时即知维度）
├── VaultEmbedDim Pebble 元数据（持久化记录）
├── bleve Dynamic=true 覆盖新文本字段（无需重建）
│
□ 3.3 Faiss 搜索参数
├── ivf_nprobe_pct 配置
├── similarity 选择
│
□ 3.4 测试
├── 对比 HNSW vs Faiss KNN 召回率
├── 延迟/内存基准测试
└── 预过滤 (FilterQuery) 功能测试
```

### Phase 4: 混合搜索 (约 1 周)

```
□ 4.1 bleve 原生 RRF 融合
├── hybrid_search.go
├── 利用 SearchRequest.KNN + KNNOperator="or"
├── 对比手动 RRF (当前 phase3RRF) vs bleve RRF
│
□ 4.2 评分解释
├── bleve Explain → activation.ScoreComponents
├── 可视化诊断
│
□ 4.3 性能调优
├── scorch 索引参数
├── boltdb 配置优化
└── 缓存策略
```

### Phase 5: 生产准备 (约 1 周)

```
□ 5.1 数据迁移工具
├── 从 Pebble FTS 迁移到 bleve
├── 从 HNSW 迁移到 bleve/Faiss
│
□ 5.2 监控
├── bleve index stats → Prometheus metrics
├── 搜索延迟分布
├── bleve disk usage
│
□ 5.3 灰度发布
├── A/B 测试 native vs bleve
├── Canary deployment
└── Rollback plan
```

---

## 11. 风险与缓解

| # | 风险 | 影响 | 缓解措施 |
|---|------|------|---------|
| 1 | **双重存储开销** (Pebble + boltdb 共存) | 磁盘空间 ↑2× | Phase 4 后评估 Pebble-backed KV store for bleve；短期通过 `data_subdir` 独立管理 |
| 2 | **Faiss build tag 依赖** | `vectors` tag 不可用导致 KNN 降级 | `//go:build vectors` 分离文件 + 编译时 stub + 运行时 HNSW 回退 |
| 3 | **维度不匹配** (embedder 切换后) | 搜索失败 | vault-level embedDim 元数据 + 维度升级流程 + 配置校验 |
| 4 | **中文分词准确度** (CJK bigram 是通用方案) | 中文短词搜索不够精确 | Phase 2 后评估引入 jieba-go 或 sego 分词库作为可选分析器 |
| 5 | **Bleve API 破坏性变更** | 版本升级困难 | 锁定 bleve/v2 大版本，通过 `go.sum` 校验 |
| 6 | **锁竞争** (boltdb 写锁) | 高并发写入瓶颈 | 复用现有后台索引队列；必要时在 backend 内批量提交 |
| 7 | **迁移数据丢失** | 索引重建错误 | 保留 native 路径 + 先迁移再切换 + 校验工具 |
| 8 | **CJK 分析器不支持混合语言** | 英文还原不够准确 | 使用 cjk + standard 双分析器策略 + 字段级分析器分配 |

---

## 12. 接口定义参考

```go
// internal/search/backend.go (核心接口)

package search

import (
    "context"
    "io"

    "github.com/scrypster/muninndb/internal/storage"
)

type Hit struct {
    ID    [16]byte
    Score float64
}

type Backend interface {
    IndexText(ctx context.Context, ws [8]byte, eng *storage.Engram) error
    DeleteText(ctx context.Context, ws [8]byte, id [16]byte) error
    SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]Hit, error)

    IndexVector(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error
    DeleteVector(ctx context.Context, ws [8]byte, id [16]byte) error
    SearchVector(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]Hit, error)

    ResetVault(ctx context.Context, ws [8]byte) error
    ReindexVault(ctx context.Context, ws [8]byte, scan func(func(*storage.Engram) error) error) error
    io.Closer
}
```

---

## 附录 A: 依赖变更

```diff
# go.mod 新增 (Phase 1)
+ github.com/blevesearch/bleve/v2 v2.4.0
+ go.etcd.io/bbolt v1.4.0                # bleve 默认 KV store

# go.mod 新增 (Phase 3, 可选)
+ github.com/blevesearch/go-faiss v1.0.34 # Faiss 向量索引
```

## 附录 B: 文件清单

```
internal/search/
├── backend.go           # 通用 Backend 接口
├── adapters/
│   ├── activation.go    # search.Backend → activation.FTSIndex/HNSWIndex
│   └── trigger.go       # search.Backend → trigger.FTSIndex/HNSWIndex
├── native/
│   └── backend.go       # wraps fts.Index + hnsw.Registry
└── bleve/
    ├── backend.go       # Bleve 实现 search.Backend
    ├── config.go        # BleveConfig + 默认值
    ├── mapping.go       # buildIndexMapping + buildDocument
    ├── text.go          # IndexText/SearchText/DeleteText
    ├── vector.go        # IndexVector/SearchVector/DeleteVector
    ├── vector_noop.go   # 无 vectors build tag 时降级
    ├── hybrid.go        # 混合搜索 (RRF/RSF)
    ├── lifecycle.go     # Close / ResetVault / Stats
    └── backend_test.go  # 单元测试

internal/config/
└── search.go            # SearchConfig + LoadSearchConfig / SaveSearchConfig

docs/design/
└── bleve-integration.md # 本文档
```

## 附录 C: 环境变量参考

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `MUNINN_SEARCH_ENGINE` | `native` | 搜索引擎选择: `native` 或 `bleve` |
| `MUNINN_BLEVE_DATA_DIR` | `{dataDir}/bleve` | Bleve 索引数据目录 |
| `MUNINN_BLEVE_ANALYZER` | `cjk` | 默认分析器 |
| `MUNINN_BLEVE_VECTOR_SIMILARITY` | `cosine` | 向量相似度 |
| `MUNINN_HNSW_WARN_THRESHOLD_MB` | (disabled) | HNSW 内存警告阈值（现有） |
| `MUNINN_HNSW_MAX_MB` | (disabled) | HNSW 硬内存限制（现有） |
